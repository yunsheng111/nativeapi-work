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
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hostswitch"
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
	// ListBindings 返回粘性会话绑定快照（脱敏 ID），供「会话」视图；nil 时该端点 501。
	ListBindings func() []SessionBinding
	// RebindSession 把指定会话改绑到账号，返回命中条数（0 = 会话不存在/已过期）。
	RebindSession func(id, uid string) int
	// BindAllSessions 把全部会话改绑到账号（「全部会话切到此号」）。
	BindAllSessions func(uid string) int
	// StickyEnabled 读粘性热开关当前状态；SetStickyEnabled 热切换并落盘。
	// nil 时开关端点 501（理论不可达：路由器始终构建）。
	StickyEnabled   func() bool
	SetStickyEnabled func(bool) error

	// HostSwitch 宿主切号服务（写 WorkBuddy 官方客户端登录态文件）；nil 时宿主
	// 端点返回 501（非 Windows / 显式关闭）。指针直连而非闭包：hostswitch 是
	// 自包含模块（路径注入 + 无池依赖），与 pool/session 无耦合。
	HostSwitch *hostswitch.Service

	// ReqLog 请求日志库（内存窗口 + 磁盘分段持久化，见 reqlog.go）。
	// main 装配时注入（与 Ring 的 chatSink 对接）；nil 时 New 回退内存模式，
	// 监控端点功能照常（测试与裸用场景无需额外装配）。
	ReqLog *ReqLog

	// DevDir 面板开发模式：index.html/app.js 改从该目录实时读取（改完自动刷新，
	// 免重新编译），空 = 生产模式用 go:embed 内容。仅由环境变量 WB2API_PANEL_DEV 注入，
	// 正常部署不设置。
	DevDir string

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring
	dev     devStatic // 开发模式热读盘（DevDir 为空时退化为 embed 静态）

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

	// events 池变更广播器：pool 的 onChange 回调投递到这里，再由 /panel/api/events
	// （SSE）扇出给浏览器，使面板从"定时轮询 overview"改为"变更即拉取"。
	events *eventBroker
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
	// 请求日志 nil 回退内存模式：测试与裸用场景（不经 app.Build 装配）不落盘，
	// 监控端点照常可用。
	if cfg.ReqLog == nil {
		cfg.ReqLog = NewReqLog("")
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
		events:  newEventBroker(),
	}
	if cfg.DevDir != "" {
		p.dev.dir = cfg.DevDir
	}
	// 池变更 → 广播。注入必须在 p（含 p.events）构造完成之后：回调在请求热路径上被调用，
	// 若早于构造完成注入，首个变更就会撞上 nil 的 events。
	if cfg.Pool != nil {
		cfg.Pool.SetOnChange(p.events.Notify)
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/dev_version", p.withAuth(p.devVersionHandler))
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/events", p.withAuth(p.eventsHandler))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/chatlogs", p.withAuth(p.chatLogsHandler))
	p.mux.HandleFunc("GET /panel/api/usage_stats", p.withAuth(p.usageStatsHandler))
	p.mux.HandleFunc("GET /panel/api/metrics", p.withAuth(p.metricsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("GET /panel/api/models_all", p.withAuth(p.modelsAll))
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
	p.mux.HandleFunc("GET /panel/api/sessions", p.withAuth(p.sessionsList))
	p.mux.HandleFunc("POST /panel/api/sessions/rebind", p.withAuth(p.sessionRebind))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/adopt_sessions", p.withAuth(p.accountAdoptSessions))
	p.mux.HandleFunc("POST /panel/api/sticky", p.withAuth(p.stickyToggle))
	p.mux.HandleFunc("GET /panel/api/host/current", p.withAuth(p.hostCurrent))
	p.mux.HandleFunc("POST /panel/api/host/switch", p.withAuth(p.hostSwitch))
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
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
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
	// 粘性热开关状态：会话页开关按钮的当前态。
	if p.cfg.StickyEnabled != nil {
		resp["sticky_enabled"] = p.cfg.StickyEnabled()
	}
	writeJSON(w, http.StatusOK, resp)
}

// ssePingInterval SSE 心跳间隔。取值远小于常见中间层 60s 空闲超时，留足容错余量。
const ssePingInterval = 25 * time.Second

// eventsHandler 池状态变更的 SSE 流（text/event-stream）。只在池状态真的变化时
// 推一帧信号，前端收到后自行拉 overview —— 状态本体不走这条流，避免"推送内容"
// 与"拉取内容"两套口径漂移。
//
// 之所以需要心跳：SSE 是长连接，中间层（反代/NAT/浏览器）会按"空闲"掐断静默连接，
// 定期发注释帧维持活跃，客户端按 SSE 规范忽略注释帧（不触发 message 事件）。
func (p *Panel) eventsHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		// 无法逐帧 flush 时 SSE 会退化成"攒够缓冲才可见"，不如明确报错。
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 禁用反代缓冲：nginx 默认会把响应攒起来，SSE 帧到不了浏览器。
	h.Set("X-Accel-Buffering", "no")
	// 清掉写超时：http.Server 配了 WriteTimeout/IdleTimeout 时，长连接会在超时点被
	// 服务端单方面掐断。ResponseController 是 Go 1.20+ 的正规清法；部分 ResponseWriter
	// 实现不支持（返回 ErrNotSupported），此时沿用原超时——连接仍可用，只是会按时断开
	// 由客户端重连，故忽略错误而不中断本次订阅。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// 先发一帧注释，让客户端立刻确认"连上了"（否则要等首个变更才知道握手成功）。
	_, _ = io.WriteString(w, ": connected\n\n")
	fl.Flush()

	ch, cancel := p.events.Subscribe()
	defer cancel()

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			// 客户端断开（关标签页/网络中断）：退出并取消订阅，释放 channel。
			return
		case <-ch:
			_, _ = io.WriteString(w, "event: change\ndata: 1\n\n")
			fl.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// tryParseTime 解析监控查询参数的时间：依次尝试 RFC3339、无时区的本地时间
// （秒/分钟两种粒度）、unix 毫秒数；空串或全部解析失败返回零值（= 不限）。
// 无时区格式按本机时区解释：前端发的本地墙钟时间不应被隐式当作 UTC 平移。
// 容错：query 里未编码的 "+" 会被解码成空格（RFC3339 的 "+08:00" 时区段是
// 重灾区），而这些布局里空格本身不合法，解析前还原为 "+" 不会误伤。
func tryParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	s = strings.ReplaceAll(s, " ", "+")
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(n)
	}
	return time.Time{}
}

// chatLogsHandler 监控视图的请求日志查询：服务端按时间/账号/模型/模式/状态筛选
// 并分页（数据源是 ReqLog 的 24h 持久化窗口，单页量有界），随响应附带过滤结果
// 的聚合指标（req/ok/err/token 合计/平均耗时），前端不再自行全量计算。
func (p *Panel) chatLogsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := QueryOpts{
		From:   tryParseTime(q.Get("from")),
		To:     tryParseTime(q.Get("to")),
		UID:    q.Get("uid"),
		Model:  q.Get("model"),
		Mode:   q.Get("mode"),
		Status: q.Get("status"),
		Limit:  atoiDefault(q.Get("limit"), 0),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	if v, err := strconv.ParseBool(q.Get("err_only")); err == nil && v {
		opts.ErrOnly = true
	}
	entries, total, stats := p.cfg.ReqLog.Query(opts)
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total, "stats": stats})
}

// usageStatsHandler 监控视图的用量聚合：时间桶序列（前端画图，空桶补零）+
// 按模型/账号的用量排行 + 总量指标。缺省窗口 = 最近 24h。
func (p *Panel) usageStatsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := tryParseTime(q.Get("from")), tryParseTime(q.Get("to"))
	if from.IsZero() {
		from = time.Now().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now()
	}
	writeJSON(w, http.StatusOK, p.cfg.ReqLog.UsageStats(from, to))
}

// metricsWindows 多窗口速率视角的窗口清单：60s 实时口径在低流量下常为 0，
// 补三个长窗口（请求数 + 折算 TPM）让"运行状态"在个人部署流量下也有参考值。
var metricsWindows = []time.Duration{10 * time.Minute, time.Hour, 24 * time.Hour}

// metricsHandler 轻量指标快照（前端顶栏轮询）：速率（rpm/tpm 及其每秒折算）、
// 在途、池健康度、粘性会话数与运行时长。单次调用只读内存窗口与池快照，开销可忽略。
func (p *Panel) metricsHandler(w http.ResponseWriter, r *http.Request) {
	inFlight := 0
	for _, s := range p.cfg.Pool.List() {
		inFlight += s.InFlight
	}
	rpm, tpm := p.cfg.ReqLog.WindowCount(time.Minute)
	accountsTotal, accountsHealthy, _, _, _ := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	windows := make([]map[string]any, 0, len(metricsWindows))
	for _, win := range metricsWindows {
		req, tokens := p.cfg.ReqLog.WindowCount(win)
		windows = append(windows, map[string]any{
			"sec": int(win.Seconds()),
			"req": req,
			// 窗口内 tokens 折算成"每分钟 Token"；窗口不足一分钟按实际秒数折算
			"tpm": round1(float64(tokens) / (win.Minutes())),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"in_flight":        inFlight,
		"rpm":              rpm,
		"tpm":              tpm,
		"qps":              round1(float64(rpm) / 60),
		"tps":              round1(float64(tpm) / 60),
		"windows":          windows,
		"uptime_sec":       int(time.Since(p.started).Seconds()),
		"accounts_total":   accountsTotal,
		"accounts_healthy": accountsHealthy,
		"sticky_sessions":  sticky,
		"version":          p.cfg.Version,
	})
}

// round1 保留 1 位小数（速率展示口径）。
func round1(f float64) float64 {
	return math.Round(f*10) / 10
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
		entry := map[string]any{
			"id":                   mi.ID,
			"name":                 mi.Name,
			"default_effort":       mi.DefaultEffort,
			"supported_efforts":    mi.Efforts,
			"can_disable_thinking": mi.CanDisableThinking,
			"supports_reasoning":   mi.SupportsReasoning,
			"supports_images":      mi.SupportsImages,
			"credits":              mi.Credits,
			"description":          mi.Description,
			"tags":                 mi.Tags,
			"vendor":               mi.Vendor,
			"is_default":           mi.IsDefault,
			"supports_tool_call":   mi.SupportsToolCall,
			"only_reasoning":       mi.OnlyReasoning,
			"reasoning_effort":     mi.ReasoningEffort,
			"reasoning_summary":    mi.ReasoningSummary,
		}
		if mi.MaxAllowedSize > 0 {
			entry["max_allowed_size"] = mi.MaxAllowedSize
		}
		// 与 /v1/models 同口径：context_length / max_output_tokens 走四级查找链
		// （上游动态值 → 静态知识表 → model.json → models.dev → 1M 兜底），
		// effort 档位走 EffortListing（远端权威 ∪ CN 静态兜底表）——面板展示的
		// 数值即客户端实际拿到的数值，两侧不再漂移。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, p.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, p.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["supported_efforts"] = efforts
			if def != "" {
				entry["default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// modelsAll 聚合全部账号的模型名录（「监控」模型筛选下拉的数据源）。
// 逐号实时拉取上游目录，按账号 realm 补 cn:/global: 前缀后并集去重——同一产品各号
// 目录基本一致，但按域拆开才与客户端实际可请求的模型名对齐；单号失败不影响其余，
// 全部失败才 502（前端有日志观测值兜底）。
func (p *Panel) modelsAll(w http.ResponseWriter, r *http.Request) {
	list := p.cfg.Pool.List()
	if len(list) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "没有账号：请先在面板添加账号")
		return
	}
	var (
		mu   sync.Mutex
		seen = make(map[string]bool)
		okN  int
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, 4) // 并发上限：别把上游目录接口打爆
	for _, s := range list {
		wg.Add(1)
		go func(uid, realm string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			infos, err := p.cfg.Upstream.FetchModels(a)
			if err != nil {
				return
			}
			prefix := "cn:"
			if realm == "global" {
				prefix = "global:"
			}
			mu.Lock()
			defer mu.Unlock()
			okN++
			for _, mi := range infos {
				id := mi.ID
				if id == "" {
					continue
				}
				if !strings.HasPrefix(id, "cn:") && !strings.HasPrefix(id, "global:") {
					id = prefix + id
				}
				seen[id] = true
			}
		}(s.UID, s.Realm)
	}
	wg.Wait()
	if okN == 0 {
		writeErr(w, http.StatusBadGateway, "全部账号拉取模型目录失败")
		return
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids, "accounts_queried": okN, "accounts_total": len(list)})
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
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

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
