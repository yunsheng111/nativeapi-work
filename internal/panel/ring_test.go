package panel

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyLine(t *testing.T) {
	cases := map[string]string{
		"| #001 | 15:04:05 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=120ms | in=10 | out=20 | 5.0tok/s | total=1.0s |": ChChat,
		"school c8a3e793: ★ 分享任务完成":           ChTask,
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c": ChTask,
		"blackcat c8a3e793: 完成 3 次夜间对话":       ChTask,
		"checkin 5c162cc9: 已签到":               ChTask,
		"panel: 任务动作 uid=x code=chat_5":       ChTask,
		"panel: 队列启动：6 项（并发 2）":               ChTask,
		"panel: revive uid=x":                 ChSys,
		"workbuddy2api listening on :7863":    ChSys,
		"scheduler: 余额后台刷新每 5m0s":             ChSys,
	}
	for line, want := range cases {
		if got := classifyLine(line); got != want {
			t.Errorf("classifyLine(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRingWriteStripsTimestamp(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte("2026/09/14 00:12:34 school x: done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("| #002 | glm | stream | 200 | ok |")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d want 2", len(es))
	}
	if strings.HasPrefix(es[0].Text, "2026/") {
		t.Errorf("timestamp not stripped: %q", es[0].Text)
	}
	if es[0].Ch != ChTask || es[1].Ch != ChChat {
		t.Errorf("channels: %q %q", es[0].Ch, es[1].Ch)
	}
	if time.Since(es[0].TS) > 5*time.Second {
		t.Errorf("stale ts: %v", es[0].TS)
	}
}

func TestParseChatRow(t *testing.T) {
	full := "| #042 | 15:04:05 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=412ms | in=1234 | out=567 | 152.9tok/s | total=8.1s |"
	e, ok := parseChatRow(full)
	if !ok {
		t.Fatalf("parse failed: %q", full)
	}
	if e.Seq != 42 || e.Model != "glm-5.2" || e.Mode != "stream" || e.Status != 200 ||
		e.UID != "c8a3e793" || e.TTFBMs != 412 || e.InTokens != 1234 || e.Tokens != 567 ||
		e.TokPs != 152.9 || e.TotalSec != 8.1 || e.Err != "" {
		t.Fatalf("fields mismatch: %+v", e)
	}

	// 失败行：行尾带 err 段落（净化后的文本可含空格），各 token 维度缺失。
	errRow := "| #003 | 01:02:03 | - | sync | 503 | uid=- | TTFB=- | in=- | out=- | -tok/s | total=0.4s | err=no_healthy_account: upstream 429 rate limit |"
	er, ok := parseChatRow(errRow)
	if !ok {
		t.Fatalf("parse failed for err row")
	}
	if er.Seq != 3 || er.Status != 503 || er.TTFBMs != -1 || er.InTokens != -1 || er.Tokens != -1 ||
		er.TokPs != -1 || er.Err != "no_healthy_account: upstream 429 rate limit" {
		t.Fatalf("err row mismatch: %+v", er)
	}

	missing, ok := parseChatRow("| #007 | 01:02:03 | - | sync | 502 | uid=- | TTFB=- | in=- | out=- | -tok/s | total=0.4s |")
	if !ok {
		t.Fatalf("parse failed for missing-field row")
	}
	if missing.Seq != 7 || missing.Model != "-" || missing.Mode != "sync" || missing.Status != 502 ||
		missing.UID != "-" || missing.TTFBMs != -1 || missing.InTokens != -1 || missing.Tokens != -1 ||
		missing.TokPs != -1 || missing.TotalSec != 0.4 {
		t.Fatalf("missing-field row mismatch: %+v", missing)
	}

	for _, bad := range []string{
		"school x: done",                     // 非对话行
		"| #001 | glm | stream | 200 | ok |", // 截断样例
		"| #abc | 15:04:05 | m | stream | 200 | uid=u | TTFB=1ms | in=1 | out=1 | 1tok/s | total=1s |",
		"| #009 | 15:04:05 | m | stream | 200 | uid=u | TTFB=1ms | tok=1 | 1tok/s | total=1s |", // 旧 tok= 格式
	} {
		if _, ok := parseChatRow(bad); ok {
			t.Errorf("unexpected parse success: %q", bad)
		}
	}
}

func TestRingChatSink(t *testing.T) {
	r := NewRing(2)
	var chat []ChatEntry
	r.SetChatSink(func(ce ChatEntry) { chat = append(chat, ce) })
	lines := "| #001 | 15:04:05 | glm-5.2 | stream | 200 | uid=aaaaaaaa | TTFB=10ms | in=100 | out=10 | 5tok/s | total=2s |\n" +
		"| #002 | 15:04:06 | glm-5.2 | sync | 502 | uid=bbbbbbbb | TTFB=- | in=- | out=- | -tok/s | total=1s | err=no_healthy_account |\n" +
		"panel: 任务动作 uid=x code=chat_5\n" +
		"| #003 | 15:04:07 | glm-5.2 | stream | 200 | uid=cccccccc | TTFB=20ms | in=200 | out=20 | 10tok/s | total=2s |\n"
	if _, err := r.Write([]byte(lines)); err != nil {
		t.Fatal(err)
	}
	// 文本环容量 2：只留最后两行（任务行 + 对话#003）；对话行全部回调外送，不占环容量。
	if got := len(r.Snapshot()); got != 2 {
		t.Fatalf("text entries=%d want 2", got)
	}
	if len(chat) != 3 {
		t.Fatalf("chat sink calls=%d want 3", len(chat))
	}
	if chat[0].Seq != 1 || chat[2].Seq != 3 {
		t.Fatalf("chat seq order: %d..%d", chat[0].Seq, chat[2].Seq)
	}
	if chat[0].InTokens != 100 || chat[0].Tokens != 10 {
		t.Fatalf("chat in/out: %+v", chat[0])
	}
	if chat[1].Status != 502 || chat[1].TTFBMs != -1 || chat[1].Tokens != -1 || chat[1].Err != "no_healthy_account" {
		t.Fatalf("chat missing fields: %+v", chat[1])
	}
	if time.Since(chat[2].TS) > 5*time.Second {
		t.Errorf("stale chat ts: %v", chat[2].TS)
	}
}

func TestRingWithoutChatSink(t *testing.T) {
	// 未注入 sink（纯日志回显场景）：对话行只进文本环，不 panic。
	r := NewRing(4)
	if _, err := r.Write([]byte("| #001 | 15:04:05 | glm | stream | 200 | uid=aaaaaaaa | TTFB=10ms | in=1 | out=1 | 1tok/s | total=1s |\n")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 1 || es[0].Ch != ChChat {
		t.Fatalf("entries=%+v", es)
	}
}
