// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int
	// UnbindByUID 解除某账号上的全部粘性会话绑定，返回解绑条数；nil 时换号功能降级
	// （只做 pool 层避让、不解绑会话，见 forceSwitch 注释）。
	// 用闭包而非 *session.Router 直连：与 StickyCount 同风格，面板不持有 session 包依赖。
	UnbindByUID func(uid string) int
	// BoundUIDs 返回每个账号当前绑定的会话数（uid → 条数），供面板展示换号影响面；
	// nil 时该维度留空（不影响其他功能）。
	BoundUIDs func() map[string]int
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	// 换号与锁定：eject 临时避让（带时长）、lock/unlock 人工锁定、switch/force 一键换号。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/eject", p.withAuth(p.accountEject))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/lock", p.withAuth(p.accountLock))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/unlock", p.withAuth(p.accountUnlock))
	p.mux.HandleFunc("POST /panel/api/switch/force", p.withAuth(p.forceSwitch))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/status", p.withAuth(p.schoolStatus))
	p.mux.HandleFunc("POST /panel/api/school/run_all", p.withAuth(p.schoolRunAll))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	accounts := p.cfg.Pool.List()
	// locked 从明细派生（pool.CountsDetailed 的 disabled 桶含 locked，不单列——保持
	// total == healthy+cooling+disabled 恒等式）。面板导航要显示"锁定 N"，故单独数一遍。
	locked := 0
	for i := range accounts {
		if accounts[i].Locked {
			locked++
		}
	}
	resp := map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"locked":          locked,
		"in_flight_full":  inFlightFull,
		"accounts":        accounts,
	}
	// 各账号粘性会话数：面板在「换号」按钮上展示"将影响 N 个会话"，让影响面点击前可见。
	if p.cfg.BoundUIDs != nil {
		resp["bound_uids"] = p.cfg.BoundUIDs()
	}
	writeJSON(w, http.StatusOK, resp)
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 无可用账号 503（先添加账号）；上游失败 502。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	acct := p.cfg.Pool.Pick()
	if acct == nil {
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	infos, err := p.cfg.Upstream.FetchModels(acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch models: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		out = append(out, map[string]any{
			"id":                   mi.ID,
			"name":                 mi.Name,
			"context_length":       mi.ContextWindow,
			"max_output_tokens":    mi.MaxTokens,
			"max_allowed_size":     mi.MaxAllowedSize,
			"default_effort":       mi.DefaultEffort,
			"supported_efforts":    mi.Efforts,
			"can_disable_thinking": mi.CanDisableThinking,
			"supports_reasoning":   mi.SupportsReasoning,
			"supports_images":      mi.SupportsImages,
			"credits":              mi.Credits,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
	}
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResource → SetCredits（不触碰冷却状态）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 换号与锁定（面板「换号」/「锁定」/「强制换号」）
// ---------------------------------------------------------------------------

// ejectBody 换号接口的可选请求体：{"duration_sec": N, "reason": "..."}。
// 全部字段可省——空体 / 非法 JSON 都按默认时长处理（面板只发空体，带时长是给脚本用）。
type ejectBody struct {
	DurationSec int    `json:"duration_sec"`
	Reason      string `json:"reason"`
}

// accountEject 单号临时避让（面板「换号」按钮）：解绑该号上的粘性会话 + 把该号
// 推出选号候选集 duration_sec 秒（默认 pool 侧 5 分钟）。两步必须同时做，理由见
// session.Router.UnbindByUID 注释（确定性哈希下只解绑会算回同号）。
//
// 与 lock 的区别：eject 是**有时长的避让**，到期自动回归；lock 是**人工终态**，
// 需显式解锁。运维"这个号现在响应慢，先换掉"用 eject；"这个号别再用"用 lock。
func (p *Panel) accountEject(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body ejectBody
	if r.Body != nil {
		// 空体/非法 JSON 不报错（面板发空体）：解析成功才采用字段。
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual eject (panel)"
	}
	d := time.Duration(body.DurationSec) * time.Second
	if d <= 0 {
		d = defaultEjectSeconds * time.Second
	}
	p.cfg.Pool.Eject(uid, d, reason)
	unbound := 0
	if p.cfg.UnbindByUID != nil {
		unbound = p.cfg.UnbindByUID(uid)
	}
	log.Printf("panel: eject uid=%s duration=%s unbound_sessions=%d（人工换号）", uid, d, unbound)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"unbound_sessions": unbound,
		"duration_sec":     int(d.Seconds()),
	})
}

// accountLock 人工锁定：该号退出选号，直到 unlock（持久化，重启不丢）。
// 只动 locked 维度、不清冷却 —— 解锁后账号回到锁定前的冷却/健康状态（符合直觉）。
func (p *Panel) accountLock(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Lock(uid, "manual lock (panel)")
	unbound := 0
	if p.cfg.UnbindByUID != nil {
		// 锁定即不可选，绑定在该号上的会话本就无法命中；顺手解绑让下一次请求
		// 立即重新分配，而不是等快路径校验失败后再走慢路径（少一跳延迟）。
		unbound = p.cfg.UnbindByUID(uid)
	}
	log.Printf("panel: lock uid=%s unbound_sessions=%d（人工锁定）", uid, unbound)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound_sessions": unbound})
}

// accountUnlock 解除人工锁定。账号无其他冷却/熔断时立即可选并参与会话分配。
// 未锁定/不存在返回 404（供面板区分"确实解锁了"与"本来就没事"）。
func (p *Panel) accountUnlock(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if !p.cfg.Pool.Unlock(uid) {
		writeErr(w, http.StatusNotFound, "account not locked")
		return
	}
	log.Printf("panel: unlock uid=%s（解除人工锁定）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// forceSwitch 一键强制换号：解绑**全部**粘性会话，让所有会话在下一个请求重新分配。
//
// 与单号 eject 的适用场景不同：
//   - 单号 eject："这个号有问题，把它换掉"——针对账号。
//   - 一键换号："我现在想整体换一批/换开一个"——针对会话，不预设哪个号不好。
//     典型用法：上游开始对某号限流但还没到熔断，用户想立刻把流量挪开；
//     或调试时想让所有会话重新洗牌。
//
// 实现只做"解绑全部会话"而不做 pool 层避让：因为这里没有"要避开哪个号"的语义，
// 重分配本身就会按三因子权重重新散列（credits/idle/successRate），于是流量自然
// 从前一次集中的号散开。若调用方还想顺带把某个具体号推开，用 eject 接口。
//
// 额外返回"下一个可用号"作为即时反馈：Pick 会真实占用一个在途名额，故用
// AuthByUID + AvailableUIDs 的只读口径取 top1，避免为了一次展示而空占租约。
func (p *Panel) forceSwitch(w http.ResponseWriter, r *http.Request) {
	// 解绑计数：遍历前先取快照数与各号绑定情况，供响应展示影响面。
	before := 0
	if p.cfg.StickyCount != nil {
		before = p.cfg.StickyCount()
	}
	unbound := 0
	if p.cfg.UnbindByUID != nil {
		// 按当前池内全部账号逐个解绑（不能只解"有绑定的号"——BoundUIDs 可能为 nil，
		// 且逐个 uid 解绑的语义比"清空全表"更精确：不动无关账号的绑定）。
		for _, st := range p.cfg.Pool.List() {
			unbound += p.cfg.UnbindByUID(st.UID)
		}
	} else {
		// 降级路径：无 session 解绑能力时，一键换号无实际效果（粘性会话仍钉在旧号）。
		// 明确 501 而不是假装成功——静默成功会让运维以为换号了，实际流量没动。
		writeErr(w, http.StatusNotImplemented, "session unbind not available (sticky routing disabled?)")
		return
	}
	avail := p.cfg.Pool.AvailableUIDs()
	nextUID := ""
	if len(avail) > 0 {
		nextUID = avail[0]
	}
	log.Printf("panel: force switch（一键换号）unbound_sessions=%d available=%d next=%s",
		unbound, len(avail), nextUID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"unbound_sessions": unbound,
		"sticky_before":    before,
		"available":        len(avail),
		"next_uid":         nextUID,
	})
}

// defaultEjectSeconds 面板「换号」按钮的默认避让时长，与 pool.defaultEjectDuration 对齐。
// 面板层另立常量而非导出 pool 常量：面板是独立的 API 契约（duration_sec 参数），
// 其默认值可独立演进；真正兜底仍在 pool.Eject（d<=0 时用池侧默认）。
const defaultEjectSeconds = 300

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
