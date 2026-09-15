// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号（healthy 且未占满在途，
	// 且未被该模型限流/限额）。nil 时回落 Available（无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	r.stop = make(chan struct{})
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")），保留给不关心模型的调用方。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的 softRateModel 豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()
	available := r.availableSet(model)

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/占满 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 失效：清掉再分配
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// UnbindByUID 解除所有绑定到 uid 的会话（返回解绑条数），供"强制换号"使用：
// 把某个账号上的全部粘性会话一次性松绑，让这些会话下次请求重新分配。
//
// **必须与 pool.Eject 配合使用，单独调用无效**（这是本方法最容易踩的坑）：
// ResolveForModel 的分配是确定性哈希——hashIndex(key, len(pool2)) 对同一 key 与
// 同一候选集恒返回同一下标。若只解绑不避让，候选集（可用账号列表）没变，同一个
// 会话 key 会被哈希回**完全相同的 uid**，等于没换号。正确姿势是：
//
//	session.UnbindByUID(uid)      // 松开旧号上的会话
//	pool.Eject(uid, d, reason)    // 把旧号推出候选集 → 候选集变化 → 哈希落向别的号
//
// 两步都是必要的：解绑负责让**已绑定**的会话重新走分配路径；避让负责让重分配的
// 结果**不是旧号**。单做避让不够——已绑定会话在快路径命中时只校验"账号是否仍可用"，
// 而避让确实会让它失效并走慢路径，所以单做避让其实也能换动已绑定会话；但两者一起做
// 语义更清晰、且对"避让到期后旧号立刻回归"的场景仍能保持会话落在新号上。
//
// 与"在途请求成功后回绑"的竞态（无需额外处理，此处记录推理）：
// handler 在 chat 成功后会 Bind(sessKey, 实际成功号)。若换号发生在某请求在途期间，
// 该请求成功会把会话绑回旧号，看似"撤销"了换号。但旧号此时已被 Eject（until 冷却中）
// 或 Lock（永久不可选），而 ResolveForModel 的快路径**每次都要校验账号在 available 集内**，
// 于是下一个请求立刻发现绑定号不可用 → 走慢路径重分配 → 落到新号。
// 即：效果最多延迟一个请求，且不会被真正撤销。故 handler 无需感知换号操作。
func (r *Router) UnbindByUID(uid string) int {
	if uid == "" {
		return 0
	}
	r.mu.Lock()
	var keys []string
	for key, e := range r.entries {
		if e.uid == uid {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range keys {
		r.cfg.Store.DelBind(key)
	}
	return len(keys)
}

// BoundUIDs 返回每个 uid 当前绑定的会话数（供面板展示"该号上挂了多少会话"，
// 让"换号"的影响面在点击前可见）。无绑定返回空 map（非 nil）。
func (r *Router) BoundUIDs() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int)
	for _, e := range r.entries {
		out[e.uid]++
	}
	return out
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Binding 一条粘性绑定的对外投影（面板「会话」视图用）。
//
// 为什么给 ID 而不是原始会话键：键可能是客户端对话 id（metadata.conversation_id）
// 或内容派生哈希，属于用户隐私面，不该下发到浏览器；对外只给键的短哈希，
// 需要按会话操作时由服务端用 ID 反查（见 RebindByID）。
type Binding struct {
	ID        string `json:"id"`             // 会话键短哈希
	UID       string `json:"uid"`            // 当前绑定的账号
	Kind      string `json:"kind"`           // conversation（客户端给的 id）| derived（内容派生）
	AgeSec    int64  `json:"age_sec"`        // 距最后一次真实活跃的秒数
	TTLRemain int64  `json:"ttl_remain_sec"` // 绑定剩余存活时间；<=0 表示下轮 GC 清理
}

// keyIDLen 会话键短哈希字节数（10 字节 = 20 hex；对内存级会话表碰撞概率可忽略）。
const keyIDLen = 10

// keyID 会话键的稳定短标识（SHA-256 前 keyIDLen 字节，无盐——面板按 ID 回查要靠它确定）。
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:keyIDLen])
}

// keyKind 归类会话键来源，让面板能区分"客户端带 id 的对话"与"按内容派生的会话"。
func keyKind(key string) string {
	if strings.HasPrefix(key, derivedKeyPrefix) {
		return "derived"
	}
	return "conversation"
}

// Bindings 返回全部粘性绑定的脱敏快照（按最近活跃降序）。
// 此前面板只有各号的计数（BoundUIDs），看不出"我正在用的这条对话钉在谁身上"。
func (r *Router) Bindings() []Binding {
	now := time.Now()
	r.mu.RLock()
	out := make([]Binding, 0, len(r.entries))
	for key, e := range r.entries {
		out = append(out, Binding{
			ID:        keyID(key),
			UID:       e.uid,
			Kind:      keyKind(key),
			AgeSec:    int64(now.Sub(e.lastActive).Seconds()),
			TTLRemain: int64((r.cfg.TTL - now.Sub(e.lastActive)).Seconds()),
		})
	}
	r.mu.RUnlock()
	// 排序在锁外：快照已拷贝，不触碰共享状态。
	sort.Slice(out, func(i, j int) bool { return out[i].AgeSec < out[j].AgeSec })
	return out
}

// RebindByID 把 ID 对应会话改绑到 uid，返回命中条数（0 = 会话不存在/已过期）。
//
// 与 handler 成功回绑（Bind）的分工：那个持有原始键；本方法只有脱敏 ID，需要反查，
// 所以 keyID 必须是确定性的。
//
// 语义边界（必须让调用方知道，与 eject/lock 的区别）：改绑只改"这条会话下一跳走谁"，
// 不改变账号可用性；目标号随后冷却/锁定/占满时，快路径校验会让会话再次漂移
// （ResolveForModel 的既定行为）。要"钉死不走别的号"，需配合 lock 其它号。
//
// 这里**不刷新 lastActive**：活跃时间与 TTL 应反映真实请求活动，人工改绑不产生流量，
// 伪造它会让「最后活跃」失真、并让本该过期的绑定续命。
func (r *Router) RebindByID(id, uid string) int {
	if id == "" || uid == "" {
		return 0
	}
	r.mu.Lock()
	var keys []string
	for key, e := range r.entries {
		if keyID(key) == id {
			e.uid = uid
			r.entries[key] = e
			keys = append(keys, key)
		}
	}
	r.mu.Unlock()
	for _, key := range keys {
		r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	}
	return len(keys)
}

// BindAllTo 把现有全部绑定统一指向 uid（面板「全部会话切到此号」），返回条数。
// 只动粘性锚点：新会话（尚无绑定的）仍按双段哈希分配，所以它不是"全网关只走一个号"的
// 开关——那个语义要用 lock 其它号实现。同样不刷新 lastActive。
func (r *Router) BindAllTo(uid string) int {
	if uid == "" {
		return 0
	}
	r.mu.Lock()
	keys := make([]string, 0, len(r.entries))
	for key, e := range r.entries {
		e.uid = uid
		r.entries[key] = e
		keys = append(keys, key)
	}
	r.mu.Unlock()
	for _, key := range keys {
		r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	}
	return len(keys)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把 AvailableForModel(model) 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available（nil 视空池）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. metadata.user_id
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退）
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	return deriveKey(obj)
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 粘性完全失效；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话；
//   - 同一会话多轮请求 → 同一键 → 稳定粘住同一账号（上游 prompt 缓存命中）。
//
// 取不到用户文本（纯图片等）时返回空串：不粘性，退回普通轮换（安全降级）。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				firstUserText = text
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
