package panel

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 广播器语义：通知可达、积压被合并（不排队）、取消后不再投递。
func TestEventBrokerNotifyCoalesceCancel(t *testing.T) {
	b := newEventBroker()
	ch, cancel := b.Subscribe()

	b.Notify()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Notify 未投递到订阅者")
	}

	// 连发三次只应留下一个待处理信号：订阅者拿到信号后会重取全量状态，
	// 排队补发没有增量信息（合并语义）。
	b.Notify()
	b.Notify()
	b.Notify()
	select {
	case <-ch:
	default:
		t.Fatal("应有 1 个待处理信号")
	}
	select {
	case <-ch:
		t.Fatal("信号应被合并为 1 个，而不是排队 3 个")
	default:
	}

	cancel()
	b.Notify()
	select {
	case <-ch:
		t.Fatal("取消订阅后不应再收到通知")
	default:
	}
}

// 无订阅者时 Notify 不得阻塞或 panic（池变更先于面板首个订阅者到达是常态）。
func TestEventBrokerNotifyWithoutSubscribers(t *testing.T) {
	b := newEventBroker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Notify()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("无订阅者时 Notify 阻塞了")
	}
}

// SSE 端点端到端：响应头、握手帧、以及池状态变更推来的 change 帧。
func TestEventsSSEStreamsPoolChanges(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	p := New(Config{Version: "test", Pool: pl}) // APIKey 空 = 不鉴权
	srv := httptest.NewServer(p)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/panel/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control=%q want no-cache", cc)
	}

	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	if got := awaitLine(t, lines, 5*time.Second); got != ": connected" {
		t.Fatalf("首帧=%q want \": connected\"", got)
	}

	// handler 是先发握手帧再订阅，首个变更可能早于订阅建立（无丢失保证，只有重取保证），
	// 故循环触发直到收到事件帧——这也验证了"信号可丢、状态最终一致"的设计语义。
	deadline := time.Now().Add(5 * time.Second)
	for {
		pl.RecordTokenUsage("u1", pool.TokenUsageDelta{})
		for {
			line, ok := tryLine(lines, 300*time.Millisecond)
			if !ok {
				break // 本轮超时：说明订阅还没建立，重新触发
			}
			if line == "" {
				continue // 帧结束的空行
			}
			if line != "event: change" {
				t.Fatalf("事件帧=%q want \"event: change\"", line)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("5s 内未收到 change 事件帧")
		}
	}
}

// awaitLine 取一行 SSE 帧，超时即失败。
func awaitLine(t *testing.T, lines <-chan string, d time.Duration) string {
	t.Helper()
	line, ok := tryLine(lines, d)
	if !ok {
		t.Fatal("等待 SSE 帧超时")
	}
	return line
}

// tryLine 取一行 SSE 帧；流结束或超时返回 ok=false。
func tryLine(lines <-chan string, d time.Duration) (string, bool) {
	select {
	case line, ok := <-lines:
		return line, ok
	case <-time.After(d):
		return "", false
	}
}

// TestChatSinkWriteNotifies 覆盖装配层的组合语义：对话日志落库后必须补推一帧变更信号。
//
// 回归场景：失败请求可能在**选号之前**就终止（无可用账号/模型不被支持），全程不经过
// Acquire/NoteError/Release，因而没有任何池状态变更通知——只靠池事件驱动时，「错误
// 请求」页签会一直停在旧数据上。这里用 uid=- 的失败行复刻该场景。
func TestChatSinkWriteNotifies(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	p := New(Config{Version: "test", ReqLog: rl})
	ch, cancel := p.events.Subscribe()
	defer cancel()

	// 复刻 app.StartLogMirror 的接线：先落库，再通知。
	p.Logs().SetChatSink(func(ce ChatEntry) {
		rl.Write(ce)
		p.NotifyRequestLogged()
	})

	// 一行选号失败的对话日志：uid=- 表示未选中任何账号。
	p.Logs().Write([]byte("| #1 | 19:00:00 | cn:glm-5.2 | sync | 503 | uid=- |" +
		" TTFB=- | in=- | out=- | -tok/s | total=0.0s | err=no_healthy_account |\n"))

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("chatSink 落库后未推送变更信号（错误请求页签将无法自动刷新）")
	}
	if _, total, _ := rl.Query(QueryOpts{}); total != 1 {
		t.Fatalf("对话日志未落库：total=%d want 1", total)
	}
}
