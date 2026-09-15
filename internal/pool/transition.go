// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、人工锁定(locked)、
// 账号级冷却(until/coolKind)、模型级冷却(modelCooldowns)、熔断(breakerUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口
// （applyErrorPolicy / refresh / keepalive / 签到 / 选号 / 面板）对状态的改动
// 都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等封装
// （它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	locked             ← lockLocked / unlockLocked（面板人工锁定/解锁，与自动路径正交）
//	until/coolKind     ← Cooldown(CoolSoft/Hard) / CooldownSoftForModel 无解析分支 / ejectLocked
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked/ejectLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（Cooldown/NoteError 喂入）；NoteSuccess 清
//	softStreak         ← Cooldown(CoolSoft)/CooldownSoftForModel；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
//   - locked 与 disabled 同属"不可选"但不同源：disabled 由错误策略自动写入（可被
//     ReviveDisabled 自动复活），locked 纯人工意图（只由 Unlock 翻转）。因此
//     ReviveDisabled / ReenableIfCredits / NoteSuccess 都**不**触碰 locked——否则
//     "锁住一个号别再被用"会被下一次签到或成功请求默默推翻。
//   - ejectLocked 复用冷却域字段但**不喂熔断**（fails/softStreak 均不动）：人工换号
//     时上游没有失败，把健康号计入连续失败是伪造观测信号（连点换号能把好号推进熔断）。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，会出现
// 「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个先被
// 硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 只清冷却域（until/coolKind/reason/softStreak/modelCooldowns）
// 并更新 credits/creditsTotal，不动熔断器（fails/retryCount/breakerUntil）。签到解冻走这里：
// 签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx
// 信号）不应被签到覆盖。
// softStreak 属冷却域（与 until/coolKind 同域），随冷却一并清零——与「解冻只清冷却
// 不清熔断」的既有语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是
// 历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	e.clearCoolingLocked()
}

// ejectLocked 临时避让迁移：把账号从选号中推开 d 时长，**不喂熔断器**。
//
// 与 Cooldown(CoolSoft) 的语义差异（这是本原语存在的全部理由）：
//   - Cooldown 会调 recordBreakerFailureLocked 累加 e.fails —— 那是"上游确实失败了"
//     的观测信号。而 Eject 是**人工主动换号**，上游没有任何错误，把一个健康的号
//     计入连续失败是伪造信号：连点几次"换号"就能把一个好号推到熔断（fails=3），
//     完全违背运维意图。
//   - Cooldown 自增 softStreak（软退避指数），人工换号同样不该污染退避累积。
//
// 复用冷却域字段（until/coolKind）而非新开维度：healthy() 已有 until 判定，
// 写成 until 即天然被 Pick/AvailableUIDs/pickEarliestExpiryLocked 遵守，零额外判定点。
// coolKind 取 CoolSoft 表达"短时避让"而非 CoolHard——CoolHard 在
// pickEarliestExpiryLocked 里被显式排除（全冷却兜底时不参与），避让号应能在
// 无健康号时兜底参与（它本身是好的，只是被推开了 d 时长）。
//
// 清 modelCooldowns：与 Cooldown 一致（非模型级入口），避免旧模型豁免泄漏到本次避让。
// 调用方必须已持有 p.mu。
func (p *Pool) ejectLocked(e *entry, d time.Duration, reason string) {
	e.until = time.Now().Add(d)
	e.coolKind = CoolSoft
	e.reason = reason
	e.modelCooldowns = nil
	// 有意不调用 recordBreakerFailureLocked / 不自增 softStreak。
	p.dirty.Store(true)
}

// lockLocked 人工锁定：置 locked 与原因（面板可读）。
// 不动冷却域与熔断器——锁定是正交的人工维度，解除锁定时账号应回到锁定前
// 的冷却/健康状态（若期间冷却自然到期，则解锁后即可用，符合直觉）。
// 调用方必须已持有 p.mu。
func (p *Pool) lockLocked(e *entry, reason string) {
	e.locked = true
	if reason != "" {
		e.reason = reason
	}
	p.dirty.Store(true)
}

// unlockLocked 人工解锁：清 locked 标记与锁定原因。
// 一并清 reason 的前提是"该 reason 由锁定写入"——若账号同时处于自动冷却，
// 其 reason 会被本操作覆盖丢失。为保持"解锁不篡改自动状态"，仅在 locked
// 为 true 时清 reason（下面的实现遵循此约束），冷却自身的 reason 由
// 冷却字段（until/coolKind）承载，面板仍能读到原因文案。
// 调用方必须已持有 p.mu。
func (p *Pool) unlockLocked(e *entry) {
	if !e.locked {
		return
	}
	e.locked = false
	// reason 只在该账号确实被锁过时清；且仅当它不是冷却原因（until 已过期）时清，
	// 避免抹掉仍在生效的冷却故障描述。
	if e.until.IsZero() || !time.Now().Before(e.until) {
		e.reason = ""
	}
	p.dirty.Store(true)
}
