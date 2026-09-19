// reqlog.go 请求日志库：内存窗口 + 磁盘 JSONL 按小时分段 + 24h（1 天）保留。
//
// 对话表格行（server/logChatRow）经 Ring.chatSink 回调进入这里：内存保留最近
// 24h 全量条目（上限 20000 条，超出淘汰最旧），同时按小时追加到 logs 目录的
// requests-YYYYMMDDHH.jsonl 分段文件。
//
// 保留口径只有一条：reqLogRetain（24h）。内存与磁盘都按它判过期，不做第二套规则。
//   - 内存：每次 Write 与每次 sweep 淘汰早于 cutoff 的条目。有流量时窗口恒 ≤24h；
//     完全静默时最长可到 24h + 一个 sweep 周期（15min）才被淘汰。
//   - 磁盘：sweep 删除「最后一次可能写入的时刻 <= cutoff」的分段文件。驻留长度由
//     分段跨度决定——按小时分段时最多 25 个文件、最旧数据约 25h（不是 24h，因为
//     最后一份落在窗口内的分段可能只被窗口覆盖了一小部分）；旧版按天分段（一个文件
//     覆盖 24h）最坏会留下两份「今天+昨天」文件，≈48h 原始数据，与「只留 1 天」不符，
//     故改为小时粒度。
//   - 触发时机：NewReqLog 启动即清理一次，此后每 sweepInterval（15min）一次。
//
// 重启种子：扫描目录内全部分段，只读「与保留窗口有交集」的文件，再按 cutoff 逐条
// 过滤，因此 24h 窗口跨重启连续。旧版按天分段仍可被识别与读取（按 24h 跨度参与
// 过期判定），升级不丢历史，且旧文件最终会被清掉。
//
// dir 为空 = 纯内存模式（测试与裸用场景），不落盘、不启动 sweep。
package panel

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// reqLogCap 内存条目上限：24h 高流量下不易触顶；触顶按 FIFO 淘汰最旧。
	// 注意这是容量护栏而非保留口径——极端流量下它会先于 24h 生效，此时「全部日志」
	// 指最近 reqLogCap 条。
	reqLogCap = 20000
	// reqLogRetain 条目保留窗口 = 1 天。内存与磁盘共用这一个口径。
	reqLogRetain = 24 * time.Hour
	// sweepInterval sweep 周期。它决定"到期后多久被删"，不决定驻留长度（后者由分段
	// 跨度决定）；15min 的代价是每分钟 1/4 次空转（一次 glob + 至多 2 万条内存过滤），
	// 可忽略。
	sweepInterval = 15 * time.Minute

	segLayout         = "2006010215" // 分段文件名里的时间戳段（小时粒度）
	legacySegLayout   = "20060102"   // 旧版按天分段的时间戳段：仅用于识别与清理
	segSpan           = time.Hour    // 小时分段覆盖的时长
	legacySegSpan     = 24 * time.Hour
	segPrefix         = "requests-" // 分段文件名前缀
	segSuffix         = ".jsonl"    // 分段文件名后缀
	queryLimitDefault = 200         // Query 未指定 limit 时的默认页大小
	queryLimitMax     = 2000        // Query 单页上限
)

// segName 分段文件名：requests-YYYYMMDDHH.jsonl（本地时间，小时粒度）。
func segName(t time.Time) string { return segPrefix + t.Format(segLayout) + segSuffix }

// parseSeg 解析分段文件名 → 覆盖区间 [start, start+span)。
// span 由名字里的时间戳精度决定：10 位 = 小时分段（1h），8 位 = 旧版按天分段（24h）。
// 不符合命名约定的名字返回 ok=false，调用方据此跳过——目录里人工放置的文件绝不误删。
func parseSeg(name string) (start time.Time, span time.Duration, ok bool) {
	if !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, segSuffix) {
		return time.Time{}, 0, false
	}
	stem := name[len(segPrefix) : len(name)-len(segSuffix)]
	layout, dur := segLayout, segSpan
	switch len(stem) {
	case len(legacySegLayout):
		layout, dur = legacySegLayout, legacySegSpan
	case len(segLayout):
	default:
		return time.Time{}, 0, false
	}
	t, err := time.ParseInLocation(layout, stem, time.Local)
	if err != nil {
		return time.Time{}, 0, false
	}
	// 回写比对：time.Parse 会把越界日期归一化（20260230 → 03-02），归一化后的名字
	// 不等于原串，说明这不是我们生成的文件——按约定拒绝，避免把它当合法分段删掉。
	if t.Format(layout) != stem {
		return time.Time{}, 0, false
	}
	return t, dur, true
}

// segEnd 分段"最后一次可能写入"的时刻，过期判定以它为准。
//   - 小时分段：写满即封存，端点就是 start+1h。
//   - 旧版按天分段：文件已冻结不再增长，用 mtime（= 最后一次写入）比 start+24h 更准。
//     按 start+24h 判定时，升级当天那份文件要等到 D+2 零点才过期（最坏残留 ~48h），
//     与"只留 1 天"不符；mtime 取不到时回退到 start+span——偏晚，只会多留不会误删。
func segEnd(path string, start time.Time, span time.Duration) time.Time {
	if span == legacySegSpan {
		if fi, err := os.Stat(path); err == nil {
			return fi.ModTime()
		}
	}
	return start.Add(span)
}

// ChatEntry 增补说明：InTokens（prompt tokens，-1 缺失）与 Err（失败原因短文本）
// 由 server/logChatRow 的表格行解析而来，见 ring.parseChatRow。

// ReqLog 并发安全的请求日志库。
type ReqLog struct {
	mu      sync.Mutex
	dir     string      // 空 = 纯内存模式
	entries []ChatEntry // 时间升序（追加序即时间序；种子载入时已排序）

	file    *os.File // 当前小时分段文件句柄（内存模式恒 nil）
	fileSeg string   // 句柄对应的分段起点（segLayout 串），换小时重开
	warned  bool     // 写盘失败告警防抖：首个失败打一次，成功重开后复位

	sweepTimer *time.Timer
	closed     bool
}

// NewReqLog 构建请求日志库。dir 为空 = 纯内存模式；否则创建目录、清一次过期分段、
// 载入种子数据并启动周期 sweep。
func NewReqLog(dir string) *ReqLog {
	rl := &ReqLog{dir: dir}
	if dir == "" {
		return rl
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// 目录建不出来（只读盘等）：落盘必然持续失败，退化为纯内存并明确告警。
		log.Printf("reqlog: 创建日志目录 %s 失败: %v（请求日志退化为纯内存）", dir, err)
		return rl
	}
	rl.pruneFiles() // 启动即清一次：停机期间过期的分段不必等到第一个 sweep 周期
	rl.mu.Lock()
	rl.seed()
	rl.scheduleSweepLocked()
	rl.mu.Unlock()
	return rl
}

// seed 启动载入：读「与保留窗口有交集」的全部分段（存在才读），坏行跳过，只保留
// 24h 内条目，按 ts 升序放回内存（容量超限淘汰最旧）。
func (rl *ReqLog) seed() {
	now := time.Now()
	cutoff := now.Add(-reqLogRetain)
	matches, err := filepath.Glob(filepath.Join(rl.dir, segPrefix+"*"+segSuffix))
	if err != nil {
		return
	}
	var loaded []ChatEntry
	for _, path := range matches {
		start, span, ok := parseSeg(filepath.Base(path))
		if !ok || !start.Add(span).After(cutoff) {
			continue // 不合命名约定，或整段都落在窗口之外：不必读
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ce ChatEntry
			if json.Unmarshal([]byte(line), &ce) != nil {
				continue // 坏行（半截写/手工编辑）：跳过不致命
			}
			loaded = append(loaded, ce)
		}
	}
	// 升序排序后再过滤与截尾：淘汰的一定是最旧的那头。
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].TS.Before(loaded[j].TS) })
	kept := loaded[:0]
	for _, e := range loaded {
		if !e.TS.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	if overflow := len(kept) - reqLogCap; overflow > 0 {
		kept = kept[overflow:]
	}
	rl.entries = kept
}

// Write 追加一条请求日志：先淘汰内存里超 24h 的旧条目，再入内存、写当天分段。
// 落盘失败只告警一次（防刷屏），不影响内存窗口。Close 之后到达的迟到回调丢弃。
func (rl *ReqLog) Write(ce ChatEntry) {
	if ce.TS.IsZero() {
		ce.TS = time.Now()
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.closed {
		return
	}
	rl.pruneLocked()
	rl.entries = append(rl.entries, ce)
	if overflow := len(rl.entries) - reqLogCap; overflow > 0 {
		rl.entries = rl.entries[overflow:]
	}
	rl.appendFileLocked(ce)
}

// appendFileLocked 追加到当前小时分段；跨小时（或首写）重开句柄。
// 调用方必须已持 rl.mu：句柄的打开/关闭与 Query/Close 共享同一临界区。
func (rl *ReqLog) appendFileLocked(ce ChatEntry) {
	if rl.dir == "" {
		return
	}
	seg := ce.TS.Format(segLayout)
	if rl.file == nil || seg != rl.fileSeg {
		rl.closeFileLocked()
		f, err := os.OpenFile(filepath.Join(rl.dir, segName(ce.TS)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			rl.warnWriteFailLocked(err)
			return
		}
		rl.file, rl.fileSeg = f, seg
		rl.warned = false // 重开成功即恢复告警能力：下次持续失败仍能提醒一次
	}
	data, err := json.Marshal(ce)
	if err != nil {
		rl.warnWriteFailLocked(err)
		return
	}
	if _, err := rl.file.Write(append(data, '\n')); err != nil {
		rl.warnWriteFailLocked(err)
	}
}

// warnWriteFailLocked 写盘失败告警（一次性）：磁盘故障每行刷一条日志只会淹没
// 真正的信息，首个失败提醒一次即可，内存窗口照常工作。
func (rl *ReqLog) warnWriteFailLocked(err error) {
	if rl.warned {
		return
	}
	rl.warned = true
	log.Printf("reqlog: 请求日志落盘失败（本次运行内不再重复告警）: %v", err)
}

// closeFileLocked 关闭当前分段句柄（幂等）。
func (rl *ReqLog) closeFileLocked() {
	if rl.file != nil {
		_ = rl.file.Close()
		rl.file = nil
		rl.fileSeg = ""
	}
}

// Close 停止 sweep、关闭分段文件句柄。幂等；Close 后 Write 变为丢弃。
func (rl *ReqLog) Close() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.closed {
		return
	}
	rl.closed = true
	if rl.sweepTimer != nil {
		rl.sweepTimer.Stop()
		rl.sweepTimer = nil
	}
	rl.closeFileLocked()
}

// scheduleSweepLocked 安排下一次 sweep（time.AfterFunc 循环驱动，Close 停止）。
func (rl *ReqLog) scheduleSweepLocked() {
	if rl.closed {
		return
	}
	rl.sweepTimer = time.AfterFunc(sweepInterval, func() {
		rl.sweep()
		rl.mu.Lock()
		defer rl.mu.Unlock()
		rl.scheduleSweepLocked()
	})
}

// sweep 周期清理：内存淘汰超 24h 条目；磁盘删除整段都在 24h 窗口之外的分段文件。
func (rl *ReqLog) sweep() {
	rl.mu.Lock()
	rl.pruneLocked()
	rl.mu.Unlock()
	rl.pruneFiles()
}

// pruneLocked 原地淘汰内存中超过保留窗口的条目。索引遍历避免逐条拷贝结构体
// （Write 热路径上每次全扫 2 万条，拷贝会让开销放大一个量级）。
func (rl *ReqLog) pruneLocked() {
	cutoff := time.Now().Add(-reqLogRetain)
	kept := rl.entries[:0]
	for i := range rl.entries {
		if e := &rl.entries[i]; !e.TS.Before(cutoff) {
			kept = append(kept, *e)
		}
	}
	rl.entries = kept
}

// pruneFiles 删除过期分段文件。判定口径与 seed 同源（parseSeg 决定区间），过期以
// segEnd 为准。只匹配分段命名模式，目录里的其他文件一概不碰；正在写的分段跳过——
// Windows 上句柄未关时 os.Remove 必然失败（每轮 sweep 刷一条错误日志），Linux 上
// unlink 会成功但后续写入静默落进已删除的 inode（丢数据且不报错）。
func (rl *ReqLog) pruneFiles() {
	if rl.dir == "" {
		return
	}
	cutoff := time.Now().Add(-reqLogRetain)
	rl.mu.Lock()
	active := ""
	if rl.fileSeg != "" {
		active = segPrefix + rl.fileSeg + segSuffix
	}
	rl.mu.Unlock()
	matches, err := filepath.Glob(filepath.Join(rl.dir, segPrefix+"*"+segSuffix))
	if err != nil {
		return
	}
	for _, path := range matches {
		name := filepath.Base(path)
		if name == active {
			continue
		}
		start, span, ok := parseSeg(name)
		if !ok {
			continue
		}
		if !segEnd(path, start, span).After(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("reqlog: 删除过期分段 %s 失败: %v", path, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// QueryOpts 监控视图的筛选与分页参数（零值字段 = 不限）。
type QueryOpts struct {
	From, To time.Time
	UID      string
	Model    string
	Mode     string // "stream" / "sync"
	Status   string // "ok" / "bad" / ""（不限）
	ErrOnly  bool   // true = 只看 status>=400（优先于 Status）
	Limit    int    // <=0 默认 200，上限 2000
	Offset   int
}

// Stats 过滤结果上的聚合指标（监控顶栏与 usage_stats.totals 共用）。
// TTFBAvgMs/TotalAvgSec 为 -1 表示窗口内无样本。
type Stats struct {
	Req         int     `json:"req"`
	Ok          int     `json:"ok"`
	Err         int     `json:"err"`
	InTokens    int64   `json:"in_tokens"`
	OutTokens   int64   `json:"out_tokens"`
	TTFBAvgMs   float64 `json:"ttfb_avg_ms"`
	TotalAvgSec float64 `json:"total_avg_sec"`
}

// Query 按条件过滤请求日志，返回按 ts 降序的一页条目、过滤后总数与聚合指标。
// 全部计算在锁内快照后进行，调用方可安全持有返回值。
func (rl *ReqLog) Query(o QueryOpts) ([]ChatEntry, int, Stats) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	filtered := make([]ChatEntry, 0, len(rl.entries))
	for _, e := range rl.entries {
		if !o.From.IsZero() && e.TS.Before(o.From) {
			continue
		}
		if !o.To.IsZero() && e.TS.After(o.To) {
			continue
		}
		if o.UID != "" && !uidFilterMatch(o.UID, e.UID) {
			continue
		}
		if o.Model != "" && !modelFilterMatch(o.Model, e.Model) {
			continue
		}
		if o.Mode != "" && e.Mode != o.Mode {
			continue
		}
		if o.ErrOnly {
			if e.Status < 400 {
				continue
			}
		} else {
			switch o.Status {
			case "ok":
				if e.Status < 200 || e.Status >= 400 {
					continue
				}
			case "bad":
				if e.Status >= 200 && e.Status < 400 {
					continue
				}
			}
		}
		filtered = append(filtered, e)
	}
	st := computeStats(filtered)
	// 降序（最新在前）后切片分页：监控视图首屏只看最近一页。
	// 同 ts 条目按 Seq 降序兜底：sort.Slice 不稳定，只比 ts 时同 ts 的相对次序可能
	// 逐次不同（同一毫秒完成的多个请求很常见），offset 分页下会让相邻两页重复或漏行。
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].TS.Equal(filtered[j].TS) {
			return filtered[i].TS.After(filtered[j].TS)
		}
		return filtered[i].Seq > filtered[j].Seq
	})
	limit := o.Limit
	if limit <= 0 {
		limit = queryLimitDefault
	}
	if limit > queryLimitMax {
		limit = queryLimitMax
	}
	offset := o.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := make([]ChatEntry, end-offset)
	copy(page, filtered[offset:end])
	return page, len(filtered), st
}

// computeStats 对条目集合聚合指标（不做拷贝，只读）。
func computeStats(entries []ChatEntry) Stats {
	st := Stats{Req: len(entries), TTFBAvgMs: -1, TotalAvgSec: -1}
	var (
		ttfbSum, ttfbN int
		totalSum       float64
	)
	for _, e := range entries {
		if e.Status >= 400 {
			st.Err++
		} else {
			st.Ok++
		}
		if e.InTokens >= 0 {
			st.InTokens += int64(e.InTokens)
		}
		if e.Tokens >= 0 {
			st.OutTokens += int64(e.Tokens)
		}
		if e.TTFBMs >= 0 {
			ttfbSum += e.TTFBMs
			ttfbN++
		}
		if e.TotalSec >= 0 {
			totalSum += e.TotalSec
		}
	}
	if ttfbN > 0 {
		st.TTFBAvgMs = float64(ttfbSum) / float64(ttfbN)
	}
	if len(entries) > 0 {
		st.TotalAvgSec = totalSum / float64(len(entries))
	}
	return st
}

// uidFilterMatch uid 维度匹配：条目 uid 非空且非 "-"（未选号行不算任何账号），
// 参数与条目任一为另一方前缀即命中——面板传 8 位短号或完整 uid 都能对上。
func uidFilterMatch(param, uid string) bool {
	if uid == "" || uid == "-" {
		return false
	}
	return strings.HasPrefix(param, uid) || strings.HasPrefix(uid, param)
}

// modelFilterMatch model 维度匹配：双方剥 cn:/global: 前缀、转小写后双向包含
// （条目名较短且 >=4 字符时也允许被查询串包含），与面板既有筛选口径一致。
func modelFilterMatch(param, model string) bool {
	q := strings.ToLower(param)
	q = strings.TrimPrefix(strings.TrimPrefix(q, "cn:"), "global:")
	em := strings.ToLower(model)
	em = strings.TrimPrefix(strings.TrimPrefix(em, "cn:"), "global:")
	if q == "" {
		return true
	}
	if strings.Contains(em, q) {
		return true
	}
	return len(em) >= 4 && strings.Contains(q, em)
}

// Bucket usage_stats 的时间桶（t 为桶起点 unix 秒）。
type Bucket struct {
	T         int64 `json:"t"`
	Req       int   `json:"req"`
	Ok        int   `json:"ok"`
	Err       int   `json:"err"`
	InTokens  int64 `json:"in_tokens"`
	OutTokens int64 `json:"out_tokens"`
}

// ModelStat 按模型聚合的用量行。
type ModelStat struct {
	Model     string `json:"model"`
	Req       int    `json:"req"`
	InTokens  int64  `json:"in_tokens"`
	OutTokens int64  `json:"out_tokens"`
}

// AccountStat 按账号聚合的用量行（uid 为 8 位短号，与表格行口径一致）。
type AccountStat struct {
	UID       string `json:"uid"`
	Req       int    `json:"req"`
	InTokens  int64  `json:"in_tokens"`
	OutTokens int64  `json:"out_tokens"`
}

// UsageReport UsageStats 的响应体。
type UsageReport struct {
	Totals    Stats         `json:"totals"`
	BucketSec int           `json:"bucket_sec"`
	Buckets   []Bucket      `json:"buckets"`
	Models    []ModelStat   `json:"models"`
	Accounts  []AccountStat `json:"accounts"`
}

// UsageStats 聚合 [from, to] 窗口内的请求用量：总数指标 + 时间桶序列（画图用，
// 空桶补零）+ 按模型/账号的用量排行（req 降序）。桶尺寸按跨度自选。
func (rl *ReqLog) UsageStats(from, to time.Time) UsageReport {
	now := time.Now()
	if from.IsZero() {
		from = now.Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = now
	}
	span := to.Sub(from)
	bucketSec := 3600
	switch {
	case span <= 0:
		bucketSec = 300
	case span <= time.Hour:
		bucketSec = 300
	case span <= 3*time.Hour:
		bucketSec = 900
	}
	bucketDur := time.Duration(bucketSec) * time.Second
	// 覆盖 [from, to) 需要的桶数（向上取整）；边界值 ts==to 的条目并入最后一桶。
	n := int(span / bucketDur)
	if span%bucketDur > 0 {
		n++
	}
	if n < 1 {
		n = 1
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	report := UsageReport{
		BucketSec: bucketSec,
		Buckets:   make([]Bucket, n),
		Models:    []ModelStat{},
		Accounts:  []AccountStat{},
	}
	for i := range report.Buckets {
		report.Buckets[i] = Bucket{T: from.Add(time.Duration(i) * bucketDur).Unix()}
	}
	models := map[string]*ModelStat{}
	accounts := map[string]*AccountStat{}
	var samples []ChatEntry
	for _, e := range rl.entries {
		if e.TS.Before(from) || e.TS.After(to) {
			continue
		}
		samples = append(samples, e)
		idx := int(e.TS.Sub(from) / bucketDur)
		if idx >= n {
			idx = n - 1 // ts==to 且跨度恰为整桶数时落进最后一桶
		}
		b := &report.Buckets[idx]
		b.Req++
		if e.Status >= 400 {
			b.Err++
		} else {
			b.Ok++
		}
		if e.InTokens >= 0 {
			b.InTokens += int64(e.InTokens)
		}
		if e.Tokens >= 0 {
			b.OutTokens += int64(e.Tokens)
		}
		if e.Model != "" && e.Model != "-" { // "-"（未带模型名的失败行）不进模型分布
			// 归并展示名：裸名=cn（resolveModel 协议），与 cn: 前缀请求合为一行，
			// 否则同一模型因客户端写法不同在分布里碎成两行；global: 保持独立
			//（跨域路由目标不同，合并会掩盖真实的 realm 流量分布）。
			disp := e.Model
			if !strings.HasPrefix(disp, "global:") {
				disp = "cn:" + strings.TrimPrefix(disp, "cn:")
			}
			ms := models[disp]
			if ms == nil {
				ms = &ModelStat{Model: disp}
				models[disp] = ms
			}
			ms.Req++
			if e.InTokens >= 0 {
				ms.InTokens += int64(e.InTokens)
			}
			if e.Tokens >= 0 {
				ms.OutTokens += int64(e.Tokens)
			}
		}
		if e.UID != "" && e.UID != "-" { // 未选号行（未轮到账号就失败）不计入账号排行
			as := accounts[e.UID]
			if as == nil {
				as = &AccountStat{UID: e.UID}
				accounts[e.UID] = as
			}
			as.Req++
			if e.InTokens >= 0 {
				as.InTokens += int64(e.InTokens)
			}
			if e.Tokens >= 0 {
				as.OutTokens += int64(e.Tokens)
			}
		}
	}
	report.Totals = computeStats(samples)
	for _, ms := range models {
		report.Models = append(report.Models, *ms)
	}
	for _, as := range accounts {
		report.Accounts = append(report.Accounts, *as)
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Req != report.Models[j].Req {
			return report.Models[i].Req > report.Models[j].Req
		}
		return report.Models[i].Model < report.Models[j].Model
	})
	sort.Slice(report.Accounts, func(i, j int) bool {
		if report.Accounts[i].Req != report.Accounts[j].Req {
			return report.Accounts[i].Req > report.Accounts[j].Req
		}
		return report.Accounts[i].UID < report.Accounts[j].UID
	})
	return report
}

// WindowCount 返回最近 window 内的请求数与 token 总量（in+out，缺失算 0）。
// metrics 端点的 rpm/tpm 数据源。
func (rl *ReqLog) WindowCount(window time.Duration) (req, tokens int) {
	cutoff := time.Now().Add(-window)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for _, e := range rl.entries {
		if e.TS.Before(cutoff) {
			continue
		}
		req++
		if e.InTokens > 0 {
			tokens += e.InTokens
		}
		if e.Tokens > 0 {
			tokens += e.Tokens
		}
	}
	return req, tokens
}

// atoiDefault 查询参数取整数，空/非法回退 def。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
