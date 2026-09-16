// host.go 宿主切号端点：把池内账号写入 WorkBuddy 官方桌面客户端的登录态文件。
//
// 与网关侧换号（eject/lock/switch force）的分工：网关换号只影响 API 流量走池里
// 哪个账号；宿主切号改变的是 WorkBuddy 客户端**本身登录的账号**（改官方认证文件
// + 重启客户端）。两边共用同一账号池凭证（auths/），面板上可以从任意一行账号
// 卡片一键"设为宿主"。
//
// 官方客户端分国内版/国际版两个独立安装（cn → WorkBuddy.exe / workbuddy-desktop.info，
// global → WorkBuddyAI.exe / workbuddy-desktop-ai.info）：宿主状态与切号都按账号
// realm 分派到对应客户端，两个版本的宿主互不影响、可同时各登录一个池内账号。
//
// 安全顺序由 hostswitch.Service 保证：备份 → 关客户端 → 写入 → 重启。
// 切换必然中断目标客户端的当前会话（客户端机制决定，无法热切），
// 前端 confirm 已明示。
package panel

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hostswitch"
)

// hostCurrent 两个版本宿主的当前登录账号（只读，不含 token），附池内匹配状态：
// in_pool（国内版宿主 uid 是否在账号池里，旧字段名兼容保留）与 in_pool_global。
// 便于运维确认"宿主用的就是池里的号"。
func (p *Panel) hostCurrent(w http.ResponseWriter, r *http.Request) {
	if p.cfg.HostSwitch == nil {
		writeErr(w, http.StatusNotImplemented, "host switch not available (disabled or non-windows)")
		return
	}
	cn, err := p.cfg.HostSwitch.CurrentForRealm("cn")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	gl, err := p.cfg.HostSwitch.CurrentForRealm("global")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"ok": true, "current": cn, "current_global": gl}
	if cn != nil && cn.FileExists && cn.UID != "" {
		resp["in_pool"] = p.cfg.Pool.AuthByUID(cn.UID) != nil
	}
	if gl != nil && gl.FileExists && gl.UID != "" {
		resp["in_pool_global"] = p.cfg.Pool.AuthByUID(gl.UID) != nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// hostSwitchBody 请求体：{"uid": "...", "restart": true}。restart 缺省 true——
// WorkBuddy 退出时会覆盖登录态文件，"写完不重启"的结果不可预期，默认走完整时序。
type hostSwitchBody struct {
	UID     string `json:"uid"`
	Restart *bool  `json:"restart"`
}

// hostSwitch 把池内指定账号设为其 realm 对应客户端的宿主登录账号（cn 账号 → 国内版
// WorkBuddy，global 账号 → 国际版 WorkBuddyAI）。同步执行（全程数秒）：
// 备份 → 关客户端 → 写认证文件 → 重启，进度写日志（面板日志视图可见）。
func (p *Panel) hostSwitch(w http.ResponseWriter, r *http.Request) {
	if p.cfg.HostSwitch == nil {
		writeErr(w, http.StatusNotImplemented, "host switch not available (disabled or non-windows)")
		return
	}
	var body hostSwitchBody
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json body")
			return
		}
	}
	if body.UID == "" {
		writeErr(w, http.StatusBadRequest, "uid required")
		return
	}
	restart := true
	if body.Restart != nil {
		restart = *body.Restart
	}

	a := p.cfg.Pool.AuthByUID(body.UID)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	acc := hostswitch.Account{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		Domain:       a.Domain,
		Realm:        a.Realm(),
		ExpiresAtSec: a.ExpiresAt,
	}
	res, err := p.cfg.HostSwitch.Switch(acc, restart, func(msg string) {
		log.Printf("panel: host switch realm=%s uid=%s: %s", acc.Realm, body.UID, msg)
	})
	if err != nil {
		log.Printf("panel: host switch realm=%s uid=%s FAILED: %v", acc.Realm, body.UID, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: host switch 完成 realm=%s uid=%s restart=%v backup=%s", acc.Realm, body.UID, restart, res.Backup)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}
