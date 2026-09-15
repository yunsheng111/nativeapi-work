// build.go 装配段：把配置变成一个可服务的实例（pool / upstream / scheduler /
// panel / http.Handler），供 cmd/server 与 cmd/desktop 共用。
//
// 职责边界：本文件只做"装配"，不做"生命周期"。起 HTTP、等信号（服务端）或
// 开窗口（桌面端）由各自入口决定；但装配期启动的后台 goroutine（pool 落盘
// flusher / session GC / scheduler）的清理由 Instance.Close 统一接管，避免
// 两个入口各写一遍。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/hostswitch"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// AppVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const AppVersion = "1.8.1-panel"

// Instance 一个已装配、尚未开始服务的网关实例。
// 入口拿到它之后自行决定如何服务（ListenAndServe 等信号 / 开 WebView2 窗口），
// 结束时调用 Close 释放装配期资源。
type Instance struct {
	Cfg      *Config
	Pool     *pool.Pool
	Upstream *upstream.Client
	Scheduler *scheduler.Scheduler
	Panel    *panel.Panel
	Handler  http.Handler
	// Server 预填好 Addr/Handler/超时的 http.Server，入口直接 ListenAndServe。
	Server *http.Server

	sessRouter *session.Router
	live       *livecfg.Holder
	schedCtx   context.Context
	schedStop  context.CancelFunc
}

// LoadOrInitConfig 加载配置；文件不存在时自动落一份推荐配置（含随机 api_key）再加载。
// 生成失败（目录只读等）时退回"纯默认 + env"并打日志，不阻塞启动——与旧 main 行为一致。
func LoadOrInitConfig(path string) (*Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
	if errors.Is(err, fs.ErrNotExist) {
		// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
		// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
		if key, werr := WriteDefault(path); werr == nil {
			log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", path, key)
			cfg2, err2 := Load(path)
			if err2 == nil {
				return cfg2, nil
			}
			err = err2
		} else {
			err = werr
		}
		// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
		log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", path, err)
		fallback, ferr := Load("")
		if ferr == nil {
			return fallback, nil
		}
		return nil, ferr
	}
	return nil, err
}

// Build 装配一个网关实例。cfgPath 用于面板的配置读写端点（LoadConfig/SaveConfig）。
//
// 装配顺序刻意与旧 main 一致（store → pool → session → upstream → scheduler →
// panel → handler），因为其中存在真实依赖：session 需要 pool 的可用性闭包，
// handler 需要 session，panel 需要 scheduler。
func Build(cfg *Config, cfgPath string) (*Instance, error) {
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return nil, err
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由：路由器**始终构建**（轻量），是否生效由 live.StickyEnabled 热开关
	// 门控（面板一键切换，无需重启）。默认关闭——粘性改变分配行为，由用户手动开启。
	// TTL 从 config 解析（"0" = 永久保留，仅账号不可用时漂移）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	sessRouter = session.New(session.Config{
		TTL:        cfg.SessionTTL,
		GCInterval: cfg.SessionGCInterval,
		Store:      store,
		Available:  p.AvailableUIDs,
		// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
		// 裸名走 cn（现状零回归）。闭包内部 ResolveModel 剥前缀，再按 realm 过滤。
		AvailableForModel: RealmAwareAvailableForModel(p),
	})
	sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
	sessRouter.StartGC()
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
	})
	logScheduleSummary(cfg)

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		StickyEnabled:        cfg.SessionSticky.Enabled,
	})
	// 换号能力：仅在粘性路由启用时注入解绑闭包（粘性关闭时没有会话可解绑，
	// 闭包留 nil 让面板的 switch/force 明确返回 501，而不是假装成功）。
	var unbindByUID func(string) int
	var boundUIDs func() map[string]int
	var listBindings func() []panel.SessionBinding
	var rebindSession func(id, uid string) int
	var bindAllSessions func(uid string) int
	if sessRouter != nil {
		unbindByUID = sessRouter.UnbindByUID
		boundUIDs = sessRouter.BoundUIDs
		// 会话视图：结构转换放装配层，面板只认自己的投影类型（不引入 session 包）。
		listBindings = func() []panel.SessionBinding {
			bs := sessRouter.Bindings()
			out := make([]panel.SessionBinding, 0, len(bs))
			for _, b := range bs {
				out = append(out, panel.SessionBinding{
					ID: b.ID, UID: b.UID, Kind: b.Kind, AgeSec: b.AgeSec, TTLRemain: b.TTLRemain,
				})
			}
			return out
		}
		rebindSession = sessRouter.RebindByID
		bindAllSessions = sessRouter.BindAllTo
	}
	// 粘性热开关：先改 live 快照（本进程立即生效），再把 session_sticky.enabled
	// 写回 config.json（重启后保持）。两步都必须做——只热更会在重启后回退，
	// 只落盘则要重启才生效。
	setStickyEnabled := func(v bool) error {
		s := live.Load()
		s.StickyEnabled = v
		live.Store(s)
		cfg.SessionSticky.Enabled = v
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
		ss, ok := m["session_sticky"].(map[string]any)
		if !ok || ss == nil {
			ss = map[string]any{}
			m["session_sticky"] = ss
		}
		ss["enabled"] = v
		out, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		return nil
	}
	pn := panel.New(panel.Config{
		Pool:        p,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		// 换号能力：解绑指定账号上的粘性会话 + 展示各号会话数。闭包注入与 StickyCount
		// 同风格，面板不直接依赖 session 包。
		UnbindByUID: unbindByUID,
		BoundUIDs:   boundUIDs,
		// 会话视图：绑定列表 / 单会话改绑 / 全量收编，与换号（eject）互补。
		ListBindings:    listBindings,
		RebindSession:   rebindSession,
		BindAllSessions: bindAllSessions,
		// 粘性热开关的读写点：读走 live（含启动时 config 值），写 = live + 落盘。
		StickyEnabled:   func() bool { return live.Load().StickyEnabled },
		SetStickyEnabled: setStickyEnabled,
		// 宿主切号：把池内账号写入 WorkBuddy 官方客户端登录态（备份→关客户端→写→重启）。
		// 备份落在数据目录内；认证文件路径默认用 hostswitch 官方位置，可用
		// WB_HOST_AUTH_FILE 覆盖（测试注入临时文件 / 便携部署自定义位置）。
		HostSwitch: hostswitch.NewService(os.Getenv("WB_HOST_AUTH_FILE"), filepath.Join(filepath.Dir(cfg.StateFile), "host-auth-backups")),
		Version:    AppVersion,
		Live:       live,
		ConfigPath: cfgPath,
		LoadConfig: func() (any, error) { return Load(cfgPath) },
		SaveConfig: func(raw []byte) ([]string, error) { return SaveConfig(raw, cfgPath, live, p, up, sch) },
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		MaxBodyBytes:  int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
	})

	inst := &Instance{
		Cfg:        cfg,
		Pool:       p,
		Upstream:   up,
		Scheduler:  sch,
		Panel:      pn,
		Handler:    h,
		sessRouter: sessRouter,
		live:       live,
	}
	// 日志镜像延迟到实例构建完成后接线：让装配期的日志也能进面板缓冲。
	inst.StartLogMirror()

	// 排程后台协程：随 Instance.Close 一同停止。
	inst.schedCtx, inst.schedStop = context.WithCancel(context.Background())
	go sch.Run(inst.schedCtx)
	sch.StartBalanceRefresh(inst.schedCtx, cfg.BalanceRefreshInterval)

	inst.Server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 取值大于 MaxBodyMB 在常规带宽下的上传耗时；聊天请求体上限默认 8MB。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}

	// 面板日志捕获提前到装配开始前不可行（面板本身要在装配中构造），故此处
	// 补记一条"装配完成"，让面板日志视图从第一行起就是完整的。
	log.Printf("装配完成：%d 个账号，粘性会话=%v，redis=%s",
		len(auths), cfg.SessionSticky.Enabled, redisMode)
	return inst, nil
}

// StartLogMirror 把标准 log（stderr）与 chat 表格日志（stdout）镜像进面板缓冲。
// 控制台输出行为完全不变——是 MultiWriter，不是重定向。
func (i *Instance) StartLogMirror() {
	if i.Panel == nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, i.Panel.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, i.Panel.Logs()))
}

// Serve 在 cfg.Listen 上开始服务（阻塞）；返回非 ErrServerClosed 的错误。
func (i *Instance) Serve() error {
	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/",
		i.Cfg.Listen, i.Cfg.APIKey != "", panelListenPath(i.Cfg.Listen))
	err := i.Server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown 优雅停机：先落盘池状态，再关 HTTP（限时 5s）。
func (i *Instance) Shutdown() {
	i.Pool.Flush()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = i.Server.Shutdown(ctx)
}

// Close 释放装配期资源（后台协程 + 池落盘）。幂等，入口 defer 调用即可。
func (i *Instance) Close() {
	if i.schedStop != nil {
		i.schedStop()
		i.schedStop = nil
	}
	if i.sessRouter != nil {
		i.sessRouter.StopGC()
	}
	i.Pool.Close()
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// logScheduleSummary 打印各排程项的启用状态与时点（启动日志的一部分，便于运维确认）。
func logScheduleSummary(cfg *Config) {
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
}

// SaveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func SaveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("replace config: %w", err)
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
	})
	up.SanitizeFingerprints = newCfg.Features.SanitizeBlacklistFingerprints
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)

	return restartRequiredFields(newCfg), nil
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
