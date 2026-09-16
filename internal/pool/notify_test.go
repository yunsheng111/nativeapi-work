package pool

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNotifyFiresOnStateChanges 逐个入口验证变更回调：面板靠这一无载荷信号决定
// "要不要重取状态"，漏掉任何一个入口都会表现为"面板显示不动"的静默故障。
// 每个用例先跑 setup（把账号置入目标方法所需的前置状态），再清零计数，
// 最后只执行 target——否则 setup 自身触发的通知会掩盖 target 不通知的缺陷。
func TestNotifyFiresOnStateChanges(t *testing.T) {
	acquire := func(p *Pool, uid string) { p.Acquire(uid) }
	cases := []struct {
		name   string
		setup  func(p *Pool, uid string)
		target func(p *Pool, uid string)
	}{
		{"Acquire", nil, acquire},
		{"Release", acquire, func(p *Pool, uid string) { p.Release(uid) }},
		{"NoteSuccess", nil, func(p *Pool, uid string) { p.NoteSuccess(uid) }},
		{"NoteError", nil, func(p *Pool, uid string) { p.NoteError(uid) }},
		{"RecordTokenUsage", nil, func(p *Pool, uid string) { p.RecordTokenUsage(uid, TokenUsageDelta{}) }},
		{"Disable", nil, func(p *Pool, uid string) { p.Disable(uid, "test") }},
		{"Eject", nil, func(p *Pool, uid string) { p.Eject(uid, time.Minute, "test") }},
		{"Lock", nil, func(p *Pool, uid string) { p.Lock(uid, "test") }},
		{"Unlock", func(p *Pool, uid string) { p.Lock(uid, "test") }, func(p *Pool, uid string) { p.Unlock(uid) }},
		{"Revive", func(p *Pool, uid string) { p.Disable(uid, "test") }, func(p *Pool, uid string) { p.Revive(uid) }},
		{"ReviveDisabled", func(p *Pool, uid string) { p.Disable(uid, "test") }, func(p *Pool, uid string) { p.ReviveDisabled(uid) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			if c.setup != nil {
				c.setup(p, "u1")
			}
			var n atomic.Int64
			p.SetOnChange(func() { n.Add(1) })
			c.target(p, "u1")
			if n.Load() == 0 {
				t.Fatalf("%s 未触发 onChange", c.name)
			}
		})
	}
}

// TestAcquireFailureDoesNotNotify 名额已满时 Acquire 返回 false——没有状态变更，
// 不应通知（否则满载账号会在每次请求上放大一轮面板拉取）。
func TestAcquireFailureDoesNotNotify(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	p.Acquire("u1")
	var n atomic.Int64
	p.SetOnChange(func() { n.Add(1) })
	if p.Acquire("u1") {
		t.Fatal("名额已满时 Acquire 应返回 false")
	}
	if n.Load() != 0 {
		t.Fatalf("失败的 Acquire 不应通知，实际通知 %d 次", n.Load())
	}
}

// TestNotifyRunsOutsidePoolLock 守住"锁外触发"契约：回调里再取池锁不得死锁。
// 持锁方法的 defer 顺序一旦被写成"先解锁后通知"之外的形式（例如把 notify 挪进
// 临界区），回调取锁就会自锁；本用例用超时把死锁转成明确失败而不是挂住 CI。
func TestNotifyRunsOutsidePoolLock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.SetOnChange(func() { _ = p.List() }) // List 走 RLock：若回调在持写锁时被调用则自锁
		p.NoteSuccess("u1")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notify 似乎在持池锁时调用回调（回调内取锁自锁）")
	}
}
