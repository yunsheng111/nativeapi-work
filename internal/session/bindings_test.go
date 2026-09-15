// bindings_test.go 面板「会话」视图依赖的三个能力：脱敏快照 / 按 ID 改绑 / 全量改绑。
package session

import (
	"testing"
	"time"
)

func TestBindingsSnapshotIsMasked(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, 30*time.Minute)
	r.Resolve("conversation-A")
	r.Resolve("conversation-B")
	bs := r.Bindings()
	if len(bs) != 2 {
		t.Fatalf("bindings=%d want 2", len(bs))
	}
	for _, b := range bs {
		if len(b.ID) != keyIDLen*2 {
			t.Fatalf("ID 应为 %d 位 hex，得到 %q", keyIDLen*2, b.ID)
		}
		if b.ID == "conversation-A" || b.ID == "conversation-B" {
			t.Fatal("快照泄露了原始会话键")
		}
		if b.Kind != "conversation" {
			t.Fatalf("kind=%q want conversation", b.Kind)
		}
		if b.TTLRemain <= 0 {
			t.Fatalf("ttl_remain_sec=%d want >0", b.TTLRemain)
		}
	}
}

func TestRebindByIDMovesOnlyThatSession(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, 30*time.Minute)
	uidA, _ := r.Resolve("conversation-A")
	uidB, _ := r.Resolve("conversation-B")

	target := "a1"
	if uidA == target {
		target = "a2"
	}
	if n := r.RebindByID(keyID("conversation-A"), target); n != 1 {
		t.Fatalf("rebound=%d want 1", n)
	}
	if got, _ := r.Resolve("conversation-A"); got != target {
		t.Fatalf("A 应落在 %s，得到 %s", target, got)
	}
	if got, _ := r.Resolve("conversation-B"); got != uidB {
		t.Fatalf("改绑不应波及其它会话：want %s got %s", uidB, got)
	}
	if n := r.RebindByID("00000000000000000000", "a1"); n != 0 {
		t.Fatalf("未知 ID 应返回 0，得到 %d", n)
	}
}

func TestBindAllToKeepsActivityHonest(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, 30*time.Minute)
	r.Resolve("conversation-A")
	r.Resolve("conversation-B")
	before := map[string]int64{}
	for _, b := range r.Bindings() {
		before[b.ID] = b.TTLRemain
	}
	time.Sleep(1100 * time.Millisecond)
	if n := r.BindAllTo("a2"); n != 2 {
		t.Fatalf("bound=%d want 2", n)
	}
	// 先取快照再验证改绑生效：验证用的 Resolve 命中快路径会 touch 刷新 lastActive，
	// 混在一起会让"改绑不刷新活跃时间"的断言被自己的验证动作污染。
	after := r.Bindings()
	for _, b := range after {
		if b.TTLRemain >= before[b.ID] {
			t.Fatalf("改绑刷新了 lastActive（TTL %d → %d）：活跃时间被伪造", before[b.ID], b.TTLRemain)
		}
		if b.AgeSec < 1 {
			t.Fatalf("age_sec=%d 应反映真实空闲时间", b.AgeSec)
		}
	}
	if got, _ := r.Resolve("conversation-A"); got != "a2" {
		t.Fatalf("A 应改绑到 a2，得到 %s", got)
	}
}

func TestBindingsKindDerived(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, 30*time.Minute)
	// 带派生前缀的键应归类为 derived（来源：deriveKey）。
	r.Resolve("d-0123456789abcdef0123456789abcdef")
	bs := r.Bindings()
	if len(bs) != 1 || bs[0].Kind != "derived" {
		t.Fatalf("kind=%v want derived", bs)
	}
}
