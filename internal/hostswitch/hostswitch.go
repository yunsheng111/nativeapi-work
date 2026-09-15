// Package hostswitch 把 wb2api 账号池的凭证写入 WorkBuddy 官方桌面客户端的登录态
// 文件（workbuddy-desktop.info），实现"宿主侧切号"：面板选一个池内账号 → 备份 →
// （可选）关闭 WorkBuddy → 写入 → （可选）重启 WorkBuddy，官方客户端即以该账号登录。
//
// 认证文件规格（对照 changexbc/workbuddy-switch 的 auth_file.rs，行为对齐）：
//   - Windows: %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info
//   - macOS:   ~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/…
//   - Linux:   ~/.local/share/CodeBuddyExtension/Data/Public/auth/…
//
// 文件是四段 JSON：account（profile）/ auth（token）/ accounts 与 allAccounts（并存的
// 全部账号）。写入语义：allAccounts 保留现有条目并把目标账号去重并入；account/auth
// 整体替换为目标账号。
//
// 时序约束：WorkBuddy 退出时会用内存态覆盖该文件——因此"先写后关"会被覆盖回旧账号，
// 必须按 workbuddy-switch 的顺序：备份 → 关进程 → 写入 → 重启。restart=false 的写入
// 只在"WorkBuddy 此后不登录其他账号直接退出"的前提下才会保留，属于高级用法。
package hostswitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Account 切号目标的归一化凭证（wb2api auth.Auth 的同构投影；ExpiresAtSec 为 Unix 秒）。
type Account struct {
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	TokenType    string // 空 = "Bearer"
	Domain       string
	ExpiresAtSec int64 // Unix 秒；<=0 表示未知（写入 expiresIn=0）
}

// Current 宿主登录态的只读快照（不含 token，供面板展示）。
type Current struct {
	FileExists  bool   `json:"file_exists"`
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	Domain      string `json:"domain"`
	ExpiresAtMs int64  `json:"expires_at_ms"`
}

// Result 一次切换的结果（供面板展示）。
type Result struct {
	Backup    string `json:"backup"`
	ClosedWB  bool   `json:"closed_workbuddy"`
	Launched  bool   `json:"launched_workbuddy"`
	AllCount  int    `json:"all_accounts"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
}

// Service 宿主切号服务。authPath/backupDir 由装配层注入：生产传官方路径与
// data/host-auth-backups/，测试传临时路径（绝不触碰真实宿主文件）。
type Service struct {
	authPath  string
	backupDir string
	mu        sync.Mutex // 串行化切换（并发切换 = 备份/写入交错，无意义且危险）
}

// NewService 构造；authPath 为空时用当前平台的官方默认路径。
func NewService(authPath, backupDir string) *Service {
	if authPath == "" {
		authPath = DefaultAuthFilePath()
	}
	return &Service{authPath: authPath, backupDir: backupDir}
}

// DefaultAuthFilePath 当前平台 WorkBuddy 认证文件路径。
func DefaultAuthFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "workbuddy-desktop.info" // 异常环境退化为相对路径，让后续报错带上可见上下文
	}
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info")
	default:
		return filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info")
	}
}

// AuthFilePath 返回本服务实际使用的认证文件路径（诊断用）。
func (s *Service) AuthFilePath() string { return s.authPath }

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// Current 读当前登录态（文件不存在返回 FileExists=false 的零值，不报错）。
func (s *Service) Current() (*Current, error) {
	root, err := s.readAuth()
	if err != nil {
		return nil, err
	}
	c := &Current{}
	if root == nil {
		return c, nil
	}
	c.FileExists = true
	c.UID = strOf(root["uid"], objOf(root["account"])["uid"])
	c.Nickname = firstStr(root["nickname"], objOf(root["account"])["nickname"])
	auth := objOf(root["auth"])
	c.Domain = firstStr(auth["domain"], root["domain"])
	c.ExpiresAtMs = numOf(auth["expiresAt"])
	return c, nil
}

// readAuth 读并解析认证文件；不存在返回 (nil, nil)，解析失败报错（写坏比读不出更糟，
// 这里宁可不切）。
func (s *Service) readAuth() (map[string]any, error) {
	raw, err := os.ReadFile(s.authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read auth file: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse auth file %s: %w", s.authPath, err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 切换
// ---------------------------------------------------------------------------

// Switch 把 acc 写入宿主登录态。restart=true 时按标准时序执行（关 WorkBuddy → 写 → 启动）；
// progress 非空则逐步回调进度文案。同进程内串行（互斥）。
func (s *Service) Switch(acc Account, restart bool, progress func(string)) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	say := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}

	if strings.TrimSpace(acc.UID) == "" || strings.TrimSpace(acc.AccessToken) == "" {
		return nil, errors.New("uid 与 accessToken 均不能为空")
	}

	res := &Result{UID: acc.UID, Nickname: acc.Nickname}

	// 1) 备份（文件存在才备份；备份失败即中止——没有退路的写操作不做）。
	say("备份宿主认证文件…")
	backup, err := s.backup()
	if err != nil {
		return nil, err
	}
	res.Backup = backup

	// 2) restart 时先关闭 WorkBuddy（顺序约束见包注释：退出会覆盖登录态文件）。
	if restart {
		say("关闭 WorkBuddy…")
		closed, err := closeWorkBuddy(20 * time.Second)
		if err != nil {
			return nil, fmt.Errorf("close WorkBuddy: %w", err)
		}
		res.ClosedWB = closed
	}

	// 3) 写入 + 写后校验。
	say("写入认证文件…")
	allCount, err := s.writeAccount(acc)
	if err != nil {
		return nil, err
	}
	res.AllCount = allCount

	// 4) restart 时拉起 WorkBuddy。
	if restart {
		say("启动 WorkBuddy…")
		launched, err := launchWorkBuddy()
		if err != nil {
			return nil, fmt.Errorf("launch WorkBuddy: %w", err)
		}
		res.Launched = launched
	}

	say("切换完成")
	return res, nil
}

// backup 复制当前认证文件到备份目录（workbuddy-desktop.<RFC3339 时间戳>.info）。
// 文件不存在返回 ("", nil)——首次运行宿主从未登录属正常状态。
func (s *Service) backup() (string, error) {
	raw, err := os.ReadFile(s.authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read auth file for backup: %w", err)
	}
	if err := os.MkdirAll(s.backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	name := fmt.Sprintf("workbuddy-desktop.%s.info", time.Now().Format("20060102-150405"))
	dst := filepath.Join(s.backupDir, name)
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return dst, nil
}

// writeAccount 构造四段 JSON 并原子写入，随后重读校验 accessToken。返回并入后的 allAccounts 长度。
func (s *Service) writeAccount(acc Account) (int, error) {
	existing, err := s.readAuth()
	if err != nil {
		return 0, err
	}

	accountObj := buildAccountObj(acc)
	authObj := buildAuthObj(acc, time.Now())

	// allAccounts：保留现有全部账号，目标账号去重后并入（按 uid 识别）。
	all := accountsOf(existing)
	kept := all[:0]
	for _, a := range all {
		m, ok := a.(map[string]any)
		if !ok || strOf(m["uid"]) != acc.UID {
			kept = append(kept, a)
		}
	}
	all = append(kept, accountObj)

	session := map[string]any{
		"account":     accountObj,
		"auth":        authObj,
		"accounts":    all,
		"allAccounts": all,
	}
	content, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal auth file: %w", err)
	}
	if err := atomicWrite(s.authPath, content); err != nil {
		return 0, fmt.Errorf("write auth file: %w", err)
	}

	// 写后校验：重读比对 accessToken，防止半写/被外部覆盖。
	check, err := s.readAuth()
	if err != nil {
		return 0, fmt.Errorf("verify auth file: %w", err)
	}
	if got := strOf(objOf(check["auth"])["accessToken"]); got != acc.AccessToken {
		return 0, errors.New("认证文件写后校验失败：accessToken 与目标不一致（可能被并发写入）")
	}
	return len(all), nil
}

// buildAccountObj 构造官方 account 段（profile）。对齐 workbuddy-switch 的默认字段：
// wb2api 池凭证没有完整 profile，缺省字段按官方默认值补齐（与 auth_file.rs setdefault 一致）。
func buildAccountObj(acc Account) map[string]any {
	nickname := acc.Nickname
	if nickname == "" {
		nickname = acc.UID // 宿主侧展示兜底：无昵称时用 uid
	}
	return map[string]any{
		"uid":                        acc.UID,
		"nickname":                   nickname,
		"type":                       "personal",
		"accountType":                "",
		"idp":                        "",
		"oneidAccountId":             "",
		"areaInfoComplete":           false,
		"isCurrentOneIdEnterprise":   false,
		"isCurrentOneIdPersonal":     false,
		"isFirstLogin":               false,
		"isCreator":                  false,
		"isAdmin":                    false,
		"uin":                        "",
		"phoneNumber":                "",
		"lastLogin":                  true,
		"pluginEnabled":              true,
		"deployStatus":               map[string]any{"statusCode": 0, "statusMsg": "", "detailMsg": ""},
		"sso":                        map[string]any{"domain": "", "domainModifiedTimes": 0},
	}
}

// buildAuthObj 构造官方 auth 段（token）。wb2api 的 ExpiresAtSec 是 Unix 秒，官方文件
// 是毫秒（实测 expiresAt=1794666709478），这里 ×1000 换算；refresh 过期未知时沿用
// access 过期（与 auth_file.rs 的默认一致），scope 对齐官方默认。
func buildAuthObj(acc Account, now time.Time) map[string]any {
	tokenType := acc.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	nowMs := now.UnixMilli()
	expMs := acc.ExpiresAtSec * 1000
	m := map[string]any{
		"accessToken":     acc.AccessToken,
		"refreshToken":    acc.RefreshToken,
		"tokenType":       tokenType,
		"domain":          acc.Domain,
		"lastRefreshTime": nowMs,
		"scope":           "openid profile offline_access email",
		"notBeforePolicy": 0,
		"sessionState":    "",
	}
	if expMs > 0 {
		m["expiresAt"] = expMs
		m["expiresIn"] = max64(0, (expMs-nowMs)/1000)
		m["refreshExpiresAt"] = expMs // refresh 过期未知：与 access 同期（官方默认口径）
		m["refreshExpiresIn"] = max64(0, (expMs-nowMs)/1000)
	} else {
		m["expiresIn"] = 0
		m["refreshExpiresIn"] = 0
	}
	return m
}

// atomicWrite 先写同目录临时文件再 rename 替换（Go 的 os.Rename 在 Windows 用
// MOVEFILE_REPLACE_EXISTING，可覆盖已存在目标）。
func atomicWrite(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// WorkBuddy 进程管理（Windows 原生实现；非 Windows 平台 restart 降级为不可用）
// ---------------------------------------------------------------------------

const workbuddyProcName = "WorkBuddy.exe"

// workBuddyExePath 探测 WorkBuddy 可执行文件：优先当前运行中的进程镜像路径
// （最可靠——安装位置任意，如本机 D:\WorkBuddy），否则常见安装位。
// 必须在关闭进程**之前**调用并记住结果。
func workBuddyExePath() (string, bool) {
	if pids := findWorkBuddyPIDs(); len(pids) > 0 {
		if exe := processImageName(pids[0]); exe != "" {
			return exe, true
		}
	}
	for _, p := range candidateExePaths() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

func candidateExePaths() []string {
	local := os.Getenv("LOCALAPPDATA")
	var out []string
	if local != "" {
		out = append(out, filepath.Join(local, "Programs", "WorkBuddy", workbuddyProcName))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, "Desktop", "WorkBuddy", workbuddyProcName),
			filepath.Join("D:\\", "WorkBuddy", workbuddyProcName),
			filepath.Join("C:\\", "WorkBuddy", workbuddyProcName),
		)
	}
	return out
}

// closeWorkBuddy 终止全部 WorkBuddy 进程并等待退出（超时报错）。
// 返回 false = 本来就没在运行（不算错误）。
func closeWorkBuddy(wait time.Duration) (bool, error) {
	pids := findWorkBuddyPIDs()
	if len(pids) == 0 {
		return false, nil
	}
	for _, pid := range pids {
		if err := terminateProcess(pid); err != nil {
			return true, fmt.Errorf("terminate pid=%d: %w", pid, err)
		}
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if len(findWorkBuddyPIDs()) == 0 {
			return true, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return true, fmt.Errorf("WorkBuddy 进程在 %v 内未退出，请手动关闭后重试", wait)
}

// launchWorkBuddy 启动官方客户端（分离进程；wb2api 退出不影响它）。
func launchWorkBuddy() (bool, error) {
	exe, ok := workBuddyExePath()
	if !ok {
		return false, errors.New("未找到 WorkBuddy.exe（请确认已安装；切换本身已成功，手动启动即可）")
	}
	// DETACHED_PROCESS：脱离 wb2api 的控制台/作业，窗口正常显示。
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	go func() { _ = cmd.Wait() }() // 回收句柄，避免僵尸进程表项
	return true, nil
}

// findWorkBuddyPIDs 枚举系统进程，返回名字匹配 WorkBuddy.exe 的 PID 列表。
func findWorkBuddyPIDs() []uint32 {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []uint32
	for err := windows.Process32First(snapshot, &pe); err == nil; err = windows.Process32Next(snapshot, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), workbuddyProcName) {
			out = append(out, pe.ProcessID)
		}
	}
	return out
}

// processImageName 读进程可执行文件全路径（QUERY_LIMITED_INFORMATION 权限即可）。
func processImageName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var buf = make([]uint16, 32768)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// terminateProcess 强制结束进程（TerminateProcess；WorkBuddy 无单实例协议可优雅通知，
// 与 workbuddy-switch 的 taskkill /F 同级语义）。
func terminateProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		// 打开失败常见于进程刚好自行退出（竞态）：再查一次，已消失则不算失败。
		if !containsPID(findWorkBuddyPIDs(), pid) {
			return nil
		}
		return fmt.Errorf("open pid=%d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

func containsPID(pids []uint32, pid uint32) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// JSON 小工具
// ---------------------------------------------------------------------------

func objOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func strOf(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstStr(vals ...any) string { return strOf(vals...) }

// numOf 宽松取数字（json.Unmarshal 的数字是 float64；接受字符串形态的时间戳）。
func numOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%f", &f); err == nil {
			return int64(f)
		}
	}
	return 0
}

// accountsOf 取 existing 的 allAccounts（缺失回落 accounts），保证数组形态。
func accountsOf(existing map[string]any) []any {
	for _, key := range []string{"allAccounts", "accounts"} {
		if arr, ok := existing[key].([]any); ok {
			return arr
		}
	}
	return []any{}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
