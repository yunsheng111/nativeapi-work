package hostswitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	return NewService(
		filepath.Join(dir, "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info"),
		filepath.Join(dir, "backups"),
	), dir
}

// TestBuildAuthObjMillisecond 验证 wb2api 秒级时间戳 → 官方毫秒的换算与 expiresIn 推导。
func TestBuildAuthObjMillisecond(t *testing.T) {
	now := time.Unix(1794666709, 0) // 与实测官方文件 expiresAt=1794666709478ms 对齐的秒
	acc := Account{
		UID: "u-1", AccessToken: "AT", RefreshToken: "RT",
		Domain: "copilot.tencent.com", ExpiresAtSec: now.Unix() + 3600,
	}
	m := buildAuthObj(acc, now)

	if got := m["expiresAt"].(int64); got != (now.Unix()+3600)*1000 {
		t.Fatalf("expiresAt = %d, want %d", got, (now.Unix()+3600)*1000)
	}
	if got := m["expiresIn"].(int64); got != 3600 {
		t.Fatalf("expiresIn = %d, want 3600", got)
	}
	if m["refreshExpiresAt"].(int64) != m["expiresAt"].(int64) {
		t.Fatalf("refreshExpiresAt 应与 expiresAt 同期（官方默认口径）")
	}
	if m["tokenType"] != "Bearer" || m["domain"] != "copilot.tencent.com" {
		t.Fatalf("tokenType/domain 不符: %v", m)
	}
	// 未知过期时间：expiresIn=0，不写 expiresAt
	m2 := buildAuthObj(Account{UID: "u", AccessToken: "a"}, now)
	if _, ok := m2["expiresAt"]; ok {
		t.Fatalf("ExpiresAtSec<=0 时不应写 expiresAt")
	}
	if v, ok := m2["expiresIn"].(int); !ok || v != 0 {
		t.Fatalf("expiresIn 应为 0, got %v", m2["expiresIn"])
	}
}

// TestBuildAccountObjDefaults 对齐官方默认字段（workbuddy-switch auth_file.rs setdefault）。
func TestBuildAccountObjDefaults(t *testing.T) {
	obj := buildAccountObj(Account{UID: "u-9", Nickname: ""})
	if obj["nickname"] != "u-9" {
		t.Fatalf("无昵称时应以 uid 兜底，got %v", obj["nickname"])
	}
	if obj["lastLogin"] != true || obj["pluginEnabled"] != true {
		t.Fatalf("lastLogin/pluginEnabled 默认应为 true")
	}
	if obj["type"] != "personal" {
		t.Fatalf("type 默认 personal")
	}
}

// TestSwitchWriteAndMerge 完整写入路径：已有 allAccounts 保留、同 uid 去重并入、
// 写后 Current 能读回目标账号。
func TestSwitchWriteAndMerge(t *testing.T) {
	svc, _ := newTestService(t)

	// 预置"现有登录态"：两个旧账号（其中 stale 与新目标同 uid → 应被去重替换）。
	old := map[string]any{
		"account": map[string]any{"uid": "old-1", "nickname": "旧号"},
		"auth":    map[string]any{"accessToken": "OLD-AT"},
		"allAccounts": []any{
			map[string]any{"uid": "old-1", "nickname": "旧号"},
			map[string]any{"uid": "stale-2", "nickname": "过期镜像"},
		},
	}
	b, _ := json.Marshal(old)
	if err := os.MkdirAll(filepath.Dir(svc.authPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.authPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	acc := Account{
		UID: "stale-2", Nickname: "新目标",
		AccessToken: "NEW-AT", RefreshToken: "NEW-RT",
		Domain: "copilot.tencent.com", ExpiresAtSec: time.Now().Add(time.Hour).Unix(),
	}
	res, err := svc.Switch(acc, false, nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if res.AllCount != 2 {
		t.Fatalf("去重并入后 allAccounts 应为 2（old-1 + stale-2 替换镜像），got %d", res.AllCount)
	}

	// Current 读回目标账号。
	cur, err := svc.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.UID != "stale-2" || cur.Nickname != "新目标" {
		t.Fatalf("current 读回不符: %+v", cur)
	}
	if cur.ExpiresAtMs <= 0 {
		t.Fatalf("expiresAtMs 应 > 0")
	}

	// 写盘内容结构：auth.accessToken 校验、旧账号仍在 allAccounts。
	raw, _ := os.ReadFile(svc.authPath)
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("written file not json: %v", err)
	}
	if got := root["auth"].(map[string]any)["accessToken"]; got != "NEW-AT" {
		t.Fatalf("written accessToken = %v", got)
	}
	foundOld := false
	for _, a := range root["allAccounts"].([]any) {
		if a.(map[string]any)["uid"] == "old-1" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Fatalf("旧账号 old-1 应保留在 allAccounts")
	}

	// 备份文件已生成，内容为切换前状态。
	if res.Backup == "" {
		t.Fatalf("已有登录态时应生成备份")
	}
	bk, err := os.ReadFile(res.Backup)
	if err != nil {
		t.Fatalf("backup 不可读: %v", err)
	}
	if !strings.Contains(string(bk), "OLD-AT") {
		t.Fatalf("备份应是切换前内容")
	}
}

// TestSwitchEmptyFileNoBackup 首次运行（无认证文件）：切换成功、无备份、allAccounts 只有目标。
func TestSwitchEmptyFileNoBackup(t *testing.T) {
	svc, _ := newTestService(t)
	res, err := svc.Switch(Account{UID: "u-1", AccessToken: "AT"}, false, nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if res.Backup != "" {
		t.Fatalf("无既有文件不应有备份")
	}
	if res.AllCount != 1 {
		t.Fatalf("allAccounts 应为 1, got %d", res.AllCount)
	}
}

// TestSwitchValidation 缺 uid/accessToken 拒绝。
func TestSwitchValidation(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Switch(Account{AccessToken: "AT"}, false, nil); err == nil {
		t.Fatalf("缺 uid 应报错")
	}
	if _, err := svc.Switch(Account{UID: "u"}, false, nil); err == nil {
		t.Fatalf("缺 accessToken 应报错")
	}
}

// TestCurrentMissingFile 文件不存在：FileExists=false 且不报错。
func TestCurrentMissingFile(t *testing.T) {
	svc, _ := newTestService(t)
	c, err := svc.Current()
	if err != nil {
		t.Fatalf("current on missing file: %v", err)
	}
	if c.FileExists {
		t.Fatalf("FileExists 应为 false")
	}
}

// TestAccountsOfFallback accounts 缺 allAccounts 时回落 accounts。
func TestAccountsOfFallback(t *testing.T) {
	got := accountsOf(map[string]any{"accounts": []any{map[string]any{"uid": "a"}}})
	if len(got) != 1 {
		t.Fatalf("回落失败: %v", got)
	}
	if len(accountsOf(map[string]any{})) != 0 {
		t.Fatalf("空对象应为空数组")
	}
}
