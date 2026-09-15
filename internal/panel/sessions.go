// sessions.go 面板「会话」视图（C）：展示粘性会话挂在哪个账号，并把会话改绑到指定账号。
//
// 与「换号」（eject）的职责边界：
//   - eject：把账号从选号里推开一段时间，被动等会话漂走；
//   - 本文件：直接指定"这条会话下一跳走谁"，主动且能指定目标。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
)

// SessionBinding 面板侧的会话绑定投影。类型定义在 panel 而非复用 session.Binding：
// 面板不引入 session 包，由装配层做一次结构转换（与 UnbindByUID/BoundUIDs 的闭包注入同风格）。
type SessionBinding struct {
	ID        string `json:"id"`
	UID       string `json:"uid"`
	Kind      string `json:"kind"`
	AgeSec    int64  `json:"age_sec"`
	TTLRemain int64  `json:"ttl_remain_sec"`
}

// poolAvailable 报告 uid 是否在可选用集合内（与选号同一口径）。
// 用途：让"切到一个正在冷却的号"这种注定漂走的操作在点击前后都可见。
func (p *Panel) poolAvailable(uid string) bool {
	for _, u := range p.cfg.Pool.AvailableUIDs() {
		if u == uid {
			return true
		}
	}
	return false
}

// sessionsList GET /panel/api/sessions —— 绑定列表 + 可作为目标的账号（带 available）。
func (p *Panel) sessionsList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ListBindings == nil {
		writeErr(w, http.StatusNotImplemented, "sticky routing disabled (no session router)")
		return
	}
	bindings := p.cfg.ListBindings()
	if bindings == nil {
		bindings = []SessionBinding{}
	}
	avail := map[string]bool{}
	for _, uid := range p.cfg.Pool.AvailableUIDs() {
		avail[uid] = true
	}
	accts := p.cfg.Pool.List()
	out := make([]map[string]any, 0, len(accts))
	for _, s := range accts {
		out = append(out, map[string]any{
			"uid":       s.UID,
			"nickname":  s.Nickname,
			"available": avail[s.UID],
			"locked":    s.Locked,
			"disabled":  s.Disabled,
			"cooling":   s.Cooling,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings, "accounts": out, "sticky_enabled": p.cfg.StickyEnabled != nil && p.cfg.StickyEnabled()})
}

// stickyBody 粘性热开关请求体。
type stickyBody struct {
	Enabled bool `json:"enabled"`
}

// stickyToggle POST /panel/api/sticky —— 粘性会话总开关（热生效 + 落盘）。
// 开启：会话按 conversationId 钉在所选账号（默认永久保留，仅账号不可用时漂移）；
// 关闭：请求按权重正常分配，**历史绑定保留**，重新开启后立即恢复粘性。
func (p *Panel) stickyToggle(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SetStickyEnabled == nil {
		writeErr(w, http.StatusNotImplemented, "sticky toggle unavailable")
		return
	}
	var body stickyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := p.cfg.SetStickyEnabled(body.Enabled); err != nil {
		log.Printf("panel: 粘性开关写入失败 enabled=%v: %v", body.Enabled, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 粘性会话已%s（热生效 + 已写入配置）", map[bool]string{true: "开启", false: "关闭"}[body.Enabled])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sticky_enabled": body.Enabled})
}

// rebindBody 会话改绑请求体。
type rebindBody struct {
	ID  string `json:"id"`
	UID string `json:"uid"`
}

// sessionRebind POST /panel/api/sessions/rebind —— 把一条会话改绑到指定账号。
// 会话不存在/已过期 404；目标号不可用时仍执行（绑定是先记偏好，账号恢复后立即生效），
// 但响应带 target_available=false，让面板提示"会先落到别的号"。
func (p *Panel) sessionRebind(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RebindSession == nil {
		writeErr(w, http.StatusNotImplemented, "sticky routing disabled (no session router)")
		return
	}
	var body rebindBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.ID == "" || body.UID == "" {
		writeErr(w, http.StatusBadRequest, "id and uid are required")
		return
	}
	if _, ok := p.cfg.Pool.Status(body.UID); !ok {
		writeErr(w, http.StatusNotFound, "target account not found")
		return
	}
	n := p.cfg.RebindSession(body.ID, body.UID)
	if n == 0 {
		writeErr(w, http.StatusNotFound, "session binding not found（可能已过期，刷新列表）")
		return
	}
	log.Printf("panel: 会话改绑 id=%s -> uid=%s（人工切换）", body.ID, body.UID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "rebound": n, "target_available": p.poolAvailable(body.UID),
	})
}

// accountAdoptSessions POST /panel/api/accounts/{uid}/adopt_sessions
// 把现有全部会话改绑到该账号（「全部会话切到此号」）。
func (p *Panel) accountAdoptSessions(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if p.cfg.BindAllSessions == nil {
		writeErr(w, http.StatusNotImplemented, "sticky routing disabled (no session router)")
		return
	}
	n := p.cfg.BindAllSessions(uid)
	log.Printf("panel: 全部会话改绑 uid=%s sessions=%d（人工收编）", uid, n)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "bound": n, "target_available": p.poolAvailable(uid),
	})
}
