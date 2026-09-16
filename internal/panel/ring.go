// ring.go 固定容量的文本日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
//
// 对话行（"| #..." 表格行）额外解析为结构化 ChatEntry，经 chatSink 回调外送
// 给 ReqLog（持久化 + 24h 窗口，供「监控」视图按时间/账号/模型/状态筛选）；
// 解析失败的对话行只进文本环，不影响原始日志完整性。Ring 自身不再持有
// 对话数据：全量结构化保留是 ReqLog 的职责，文本环只承担回显。
package panel

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// ChatEntry 对话请求的结构化日志（解析自 server 包的 chat 表格行）。
// 数值字段 -1 表示该维度缺失（TTFB 未计 / usage 缺失），与表格行的 "-" 对应；
// Err 为失败原因短文本，成功为空。
type ChatEntry struct {
	TS       time.Time `json:"ts"`
	Seq      int64     `json:"seq"`
	Model    string    `json:"model"`
	Mode     string    `json:"mode"` // "stream" / "sync"
	UID      string    `json:"uid"`  // 表格行只打前 8 位（"-" = 空）
	Status   int       `json:"status"`
	TTFBMs   int       `json:"ttfb_ms"`
	InTokens int       `json:"in_tokens"` // prompt tokens，-1 = 缺失
	Tokens   int       `json:"tokens"`    // completion tokens，-1 = 缺失
	TokPs    float64   `json:"tokps"`
	TotalSec float64   `json:"total_sec"`
	Err      string    `json:"err,omitempty"`
}

// chatRowRe 对话表格行的解析规则，与 server/logging.go logChatRow 的输出格式
// 一一对应；TTFB / in / out / tok/s 四处可能是 "-"（缺失），err 段落仅失败行
// 存在（err 文本里的 '|' 与换行已在 logChatRow 净化为空格）。
var chatRowRe = regexp.MustCompile(
	`^\| #(\d+) \| \d{2}:\d{2}:\d{2} \| (.+?) \| (stream|sync) \| (\d+) \| uid=(\S+) \|` +
		` TTFB=([^|]+?) \| in=([^|]+?) \| out=([^|]+?) \| ([^|]+?)tok/s \| total=([0-9.]+)s \|` +
		`(?: err=(.+?) \|)?$`)

// parseChatRow 把对话表格行解析为结构化条目；格式不匹配（上游格式演进/被截断）返回 false。
func parseChatRow(line string) (ChatEntry, bool) {
	m := chatRowRe.FindStringSubmatch(line)
	if m == nil {
		return ChatEntry{}, false
	}
	e := ChatEntry{Model: m[2], Mode: m[3], UID: m[5], TTFBMs: -1, InTokens: -1, Tokens: -1, TokPs: -1}
	e.Seq, _ = strconv.ParseInt(m[1], 10, 64)
	e.Status, _ = strconv.Atoi(m[4])
	if ms, err := strconv.Atoi(strings.TrimSuffix(m[6], "ms")); err == nil {
		e.TTFBMs = ms
	}
	if n, err := strconv.Atoi(m[7]); err == nil {
		e.InTokens = n
	}
	if n, err := strconv.Atoi(m[8]); err == nil {
		e.Tokens = n
	}
	if f, err := strconv.ParseFloat(m[9], 64); err == nil {
		e.TokPs = f
	}
	e.TotalSec, _ = strconv.ParseFloat(m[10], 64)
	e.Err = m[11]
	return e, true
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 开学季",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲（文本环）。对话结构化行解析成功后经 chatSink 外送（见
// SetChatSink），Ring 只保留原始文本供 /panel/api/logs 回显。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
	// chatSink 对话行解析成功后的回调（ReqLog.Write）。仅启动期注入一次，
	// 此后只读、不与 Write 的调用方产生写竞争，故不加锁。
	chatSink func(ChatEntry)
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// SetChatSink 注入对话结构化行的回调（ReqLog.Write）。仅启动期（装配接线时）
// 调用一次，此后不再变更——无并发写竞争，读取端不加锁。
func (r *Ring) SetChatSink(fn func(ChatEntry)) {
	r.chatSink = fn
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
// 对话行解析成功后回调 chatSink（锁外调用：回调可能落盘，不应阻塞快照读取）。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	var chatRows []ChatEntry
	r.mu.Lock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		r.entries = append(r.entries, LogEntry{TS: now, Ch: classifyLine(text), Text: text})
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
		if ce, ok := parseChatRow(text); ok {
			ce.TS = now
			chatRows = append(chatRows, ce)
		}
	}
	r.mu.Unlock()
	for _, ce := range chatRows {
		if r.chatSink != nil {
			r.chatSink(ce)
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
