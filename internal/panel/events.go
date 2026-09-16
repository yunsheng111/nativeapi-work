// 池状态变更的广播器：把 pool 的"状态变了"信号扇出给面板内所有 SSE 订阅者。
// 与 pool 的解耦点：pool 只认一个无参回调，不感知订阅者数量、生命周期与投递语义。
package panel

import "sync"

// eventBroker 极简广播器：把"池状态变了"这一无载荷信号投递给所有订阅者。
// 无载荷是刻意的——订阅者收到信号后自行拉取 /panel/api/overview 取最新状态，
// 避免在广播器里维护/复制池状态带来的不一致。
// 语义是"合并"而非"计数"：投递满则丢弃，订阅者拿到的最新一次信号即可代表全部积压变更。
type eventBroker struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{subs: map[chan struct{}]struct{}{}}
}

// Notify 非阻塞通知所有订阅者。可在持锁路径调用（只取本结构体的锁，且不做任何阻塞操作）。
// 锁序安全：本结构体的锁永远是最内层——没有任何持 b.mu 的路径去取 pool 的锁，
// 故与 pool.mu 之间不存在环路（池侧持锁 → broker.Notify → b.mu 是单向的）。
func (b *eventBroker) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		// 带缓冲 + 非阻塞投递：订阅者处理慢（或已卡在写 SSE）时丢弃本次信号，
		// 因为它尚未处理的那次信号已经代表了"去重取最新状态"，补发没有增量信息。
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe 注册订阅者，返回事件 channel 与取消函数。
// channel 带 1 个缓冲：投递方永不阻塞；订阅者处理慢时事件被合并。
// 取消只把 channel 从订阅表摘除，**不关闭**它——关闭会让仍在 select 上的接收方
// 立刻收到零值，把"取消"误读成"有变更"（无限重取）；订阅者退出靠自身 ctx 感知。
// 取消幂等（重复 delete 是空操作）。
func (b *eventBroker) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return ch, cancel
}
