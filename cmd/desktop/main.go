// main.go wb2api 桌面端入口（GUI 子系统）：进程内起网关 + WebView2 窗口显示管理面板。
//
// 与 cmd/server 共用 internal/app 的配置与装配；本文件只保留桌面端特有行为：
//   - 工作目录锚定 exe 所在目录（config.json / auths / data 跟着 exe 走）；
//   - GUI 无控制台，stdout/stderr 落到 data/desktop.log；
//   - 无论 config 怎么写，强制 127.0.0.1 回环监听（不暴露局域网）；
//   - WebView2 窗口加载 /panel/，关窗即优雅停机，不留后台进程。
//
// 线程约束：webview2 包 init() 已对主线程 LockOSThread + CoInitializeEx，
// New/Navigate/Run 必须全部留在 main goroutine，不得另起。
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/linguo2625469/workbuddy2api-panel/internal/app"
	"golang.org/x/sys/windows"
)

const (
	windowTitle  = "WorkBuddy2API 控制台"
	logPath      = "data/desktop.log"
	logRotateMax = 8 << 20 // 超过 8MB 轮转一次，避免 GUI 日志无限增长
	readyWait    = 15 * time.Second
)

func main() {
	// 工作目录锚定 exe 所在目录：双击运行时 CWD 不确定，先钉死再谈相对路径。
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	// GUI 子系统下 stderr 是无效句柄；而 app.Build 的 StartLogMirror 用
	// io.MultiWriter(os.Stderr, 面板缓冲)——MultiWriter 遇错即停，若 stderr
	// 仍是无效句柄，面板日志会一行都收不到。先把两路标准流指到日志文件。
	// log 包随后由 StartLogMirror 重新接管（读到的是已替换后的 os.Stderr）。
	lf, err := openLogFile(logPath)
	if err != nil {
		die("初始化日志失败", err, "")
	}
	os.Stdout, os.Stderr = lf, lf
	log.SetOutput(lf)

	cfg, err := app.LoadOrInitConfig("config.json")
	if err != nil {
		die("加载配置失败", err, logPath)
	}

	// 桌面端只服务本机：端口沿用 config，host 强制回环。
	cfg.Listen = forceLoopback(cfg.Listen)

	inst, err := app.Build(cfg, "config.json")
	if err != nil {
		die("网关装配失败", err, logPath)
	}
	defer inst.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- inst.Serve() }()

	base := "http://" + cfg.Listen // forceLoopback 后必为 127.0.0.1:port 形态
	if err := waitReady(base, serveErr); err != nil {
		die("网关启动失败（端口 "+portOf(cfg.Listen)+"）", err, logPath)
	}
	log.Printf("桌面端就绪：%s/panel/（关窗即停止服务）", base)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		// WebView2 用户数据随程序目录走（便携）：缓存/LocalStorage 不散落到 AppData。
		DataPath: absPath(filepath.Join("data", "wv2")),
		WindowOptions: webview2.WindowOptions{
			Title:  windowTitle,
			Width:  1200,
			Height: 800,
			Center: true,
		},
	})
	if w == nil {
		die("WebView2 初始化失败",
			errors.New("缺少 Microsoft Edge WebView2 运行时，请到 https://developer.microsoft.com/microsoft-edge/webview2/ 安装后重试"),
			"")
	}
	w.Navigate(base + "/panel/")
	w.Run() // 阻塞到窗口关闭（WMQuit）

	// 关窗即停：先落盘池状态 + 优雅停 HTTP，再由 defer inst.Close() 收尾后台协程。
	inst.Shutdown()
	log.Printf("bye")
}

// waitReady 轮询 /healthz 直到服务就绪（200 正常；503 = 服务已起但暂无可用账号，
// 同样算就绪——与 scripts/verify-desktop.ps1 的判据一致）。Serve 失败则立即返回其错误。
func waitReady(base string, serveErr <-chan error) error {
	// 显式 Proxy: nil：防本机 HTTP_PROXY 把 127.0.0.1 的请求劫走（已知会 502）。
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	deadline := time.Now().Add(readyWait)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable {
				return nil
			}
		}
		select {
		case e := <-serveErr:
			return e
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("等待 %v 超时", readyWait)
}

// forceLoopback 把 listen 的 host 部分替换为 127.0.0.1（端口保留）。
// config normalize 已保证 ":port" 或 "host:port" 形态；异常输入原样返回交由
// ListenAndServe 报错。
func forceLoopback(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func portOf(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	return port
}

// openLogFile 打开追加式日志文件；超过上限轮转为 .old（保留上一份便于对照）。
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > logRotateMax {
		_ = os.Rename(path, path+".old")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// die GUI 下没有 stderr 可看，致命错误只能弹窗告知，然后非零退出。
func die(title string, err error, logHint string) {
	msg := fmt.Sprintf("%s\n\n%v", title, err)
	if logHint != "" {
		msg += "\n\n详细日志：" + absPath(logHint)
	}
	t, _ := windows.UTF16PtrFromString(windowTitle)
	m, _ := windows.UTF16PtrFromString(msg)
	_ = messageBox(0, m, t, mbIconError)
	os.Exit(1)
}

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const mbIconError = 0x10

func messageBox(hwnd uintptr, text, caption *uint16, flags uint) int {
	r, _, _ := procMessageBoxW.Call(
		hwnd,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(caption)),
		uintptr(flags),
	)
	return int(r)
}
