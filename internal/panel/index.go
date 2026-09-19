// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// devStatic 面板开发模式：DevDir 非空时 index/app.js 改从磁盘实时读取，改完保存
// 前端轮询到新版本号即自动 reload——改 UI 不再需要重新编译 exe。
// 仅读这两个固定文件名（不提供任意路径服务，避免变成目录遍历面）；
// 缓存 1s：轮询窗口内反复保存不会抖动，1.5s 轮询下最多滞后一拍。
// 读失败（文件被编辑器半写入等）回退上次成功内容，等下一拍再换。
type devStatic struct {
	dir     string
	current atomic.Value // [2][]byte —— index, app（devDir 为空时永不读取）
	stamp   atomic.Value // devStamp{index, app time.Time}；零值 = 从未读过
	lastTry atomic.Int64 // 上次尝试读盘的 unix 秒（1s 节流）
}

type devStamp struct{ index, app time.Time }

const (
	devPollMs   = 1500           // 前端轮询间隔（与轮询节奏错开 0.5s 防共振）
	devCacheTTL = 1 * time.Second // mtime 检查节流
)

func (d *devStatic) load(now time.Time) (index, app []byte, devVersion string) {
	if d == nil || d.dir == "" {
		return indexHTML, appJS, ""
	}
	if now.Unix() != d.lastTry.Load() {
		d.lastTry.Store(now.Unix())
		var idx, js []byte
		var ti, tj time.Time
		if b, err := os.ReadFile(filepath.Join(d.dir, "index.html")); err == nil {
			idx = b
			if fi, e := os.Stat(filepath.Join(d.dir, "index.html")); e == nil {
				ti = fi.ModTime()
			}
		}
		if b, err := os.ReadFile(filepath.Join(d.dir, "app.js")); err == nil {
			js = b
			if fi, e := os.Stat(filepath.Join(d.dir, "app.js")); e == nil {
				tj = fi.ModTime()
			}
		}
		// 全部读取成功才换（半写入/暂态错误保持旧内容）；有任一文件更新才算新版本
		if idx != nil && js != nil {
			d.current.Store([2][]byte{idx, js})
			d.stamp.Store(devStamp{ti, tj})
		}
	}
	cur, ok := d.current.Load().([2][]byte)
	if !ok { // 首次读盘失败：退回编译期内容，避免空页
		return indexHTML, appJS, ""
	}
	st, _ := d.stamp.Load().(devStamp)
	// 版本串：两个 mtime 的 unix 秒拼接，任一文件保存即变化
	return cur[0], cur[1], "dev" + strconvFormat(st.index.Unix()) + strconvFormat(st.app.Unix())
}

// strconvFormat 极简 int → string（避免为一个小端点引入 strconv 全包名歧义）。
func strconvFormat(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
// API 响应额外禁缓存：监控的 chatlogs 查询串里 to= 是分钟精度，同一分钟内的
// 刷新 URL 相同，WebView2/浏览器会启发式缓存旧响应——SSE 通知前端刷新时拿到
// 的还是缓存里的旧数据，「实时刷新」表现为慢一拍。no-store 明确掐掉这条路径。
func setSecurityHeaders(w http.ResponseWriter, isAPI bool) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if isAPI {
		w.Header().Set("Cache-Control", "no-store")
	}
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// devDir 模式下注入版本号给前端轮询：内容变化 → 前端自动 reload，免构建免手动刷新。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	index, _, devVer := p.dev.load(time.Now())
	if devVer == "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
		return
	}
	// 开发模式：磁盘直读 + 强制重新验证。WebView2/Chromium 会磁盘缓存静态资源，
	// 不禁缓存的话 reload 后仍执行旧 app.js（实测踩到），轮询等于失效。
	w.Header().Set("Cache-Control", "no-cache")
	// 开发模式：在 <head> 的 </head> 前注入版本桥 <meta>。必须先于 app.js 执行——
	// app.js 是 </body> 前的同步脚本，meta 注入到 body 尾部（实测踩到：落在 script
	// 之后）会让 IIFE 查不到 meta、轮询静默失效；注入 head 则时序必然正确。
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(replaceOnce(string(index),
		"</head>", "<meta name=\"dev-version\" content=\""+devVer+"\"></head>")))
}

// replaceOnce 一次性替换（仅开发模式路径使用；找不到锚点原样返回）。
func replaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, app, devVer := p.dev.load(time.Now())
	if devVer != "" {
		w.Header().Set("Cache-Control", "no-cache") // 开发模式：reload 必须拿到新脚本
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(app)
}

// devVersionHandler 开发模式专用：返回当前版本串，前端 1.5s 轮询比对，变了就 reload。
// 生产模式（devDir 空）404——前端探到 404 也会停表，不留无谓请求。
func (p *Panel) devVersionHandler(w http.ResponseWriter, r *http.Request) {
	_, _, devVer := p.dev.load(time.Now())
	if devVer == "" {
		http.NotFound(w, r)
		return
	}
	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(devVer))
}
