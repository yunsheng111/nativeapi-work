package panel

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newMonitorPanel 构造带内存 ReqLog 的面板（免鉴权），并注入固定时间的测试条目。
// 返回面板、ReqLog 与条目写入时刻的基准 now。
func newMonitorPanel(t *testing.T) (*Panel, *ReqLog, time.Time) {
	t.Helper()
	rl := NewReqLog("")
	now := time.Now()
	ok := ChatEntry{Seq: 1, TS: now.Add(-2 * time.Hour), Model: "cn:glm-5.2", Mode: "stream",
		UID: "aaaaaaaa", Status: 200, TTFBMs: 100, InTokens: 10, Tokens: 20, TokPs: 5, TotalSec: 2}
	// bad 行模拟轮转耗尽：TTFB/usage 维度缺失（-1），err 段落非空。
	bad := ChatEntry{Seq: 2, TS: now.Add(-time.Hour), Model: "global:kimi-k2.7", Mode: "sync",
		UID: "bbbbbbbb", Status: 503, TTFBMs: -1, InTokens: -1, Tokens: -1, Err: "no_healthy_account: upstream 429"}
	rl.Write(ok)
	rl.Write(bad)
	p := New(Config{Version: "test", ReqLog: rl})
	t.Cleanup(func() { rl.Close() })
	return p, rl, now
}

func getJSON(t *testing.T, p *Panel, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("%s: code=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%s: decode: %v body=%s", path, err, rec.Body.String())
	}
	return m
}

// TestChatLogsAPIQuery 服务端筛选与分页 + stats 聚合随响应返回。
func TestChatLogsAPIQuery(t *testing.T) {
	p, _, now := newMonitorPanel(t)

	// 无筛选：total=2，降序（最新在前）。
	resp := getJSON(t, p, "/panel/api/chatlogs")
	if int(resp["total"].(float64)) != 2 {
		t.Fatalf("total=%v want 2", resp["total"])
	}
	entries := resp["entries"].([]any)
	first := entries[0].(map[string]any)
	if first["seq"].(float64) != 2 || first["err"] != "no_healthy_account: upstream 429" {
		t.Fatalf("first entry: %+v", first)
	}
	if v, ok := first["in_tokens"].(float64); !ok || v != -1 {
		t.Fatalf("bad entry in_tokens: %v", first["in_tokens"])
	}

	// stats 字段齐全且数值正确。
	st := resp["stats"].(map[string]any)
	if st["req"].(float64) != 2 || st["ok"].(float64) != 1 || st["err"].(float64) != 1 {
		t.Fatalf("stats counts: %+v", st)
	}
	if st["in_tokens"].(float64) != 10 || st["out_tokens"].(float64) != 20 {
		t.Fatalf("stats tokens: %+v", st)
	}
	if st["ttfb_avg_ms"].(float64) != 100 {
		t.Fatalf("stats ttfb avg: %+v", st)
	}

	// uid 前缀筛选 + status=bad 组合。
	resp = getJSON(t, p, "/panel/api/chatlogs?uid=aaaa&status=ok")
	if int(resp["total"].(float64)) != 1 {
		t.Fatalf("uid+status filter total=%v", resp["total"])
	}

	// err_only。
	resp = getJSON(t, p, "/panel/api/chatlogs?err_only=true")
	if int(resp["total"].(float64)) != 1 {
		t.Fatalf("err_only total=%v", resp["total"])
	}

	// model 双向包含（查询串比条目名长）。
	resp = getJSON(t, p, "/panel/api/chatlogs?model=kimi-k2.7-flash")
	if int(resp["total"].(float64)) != 1 {
		t.Fatalf("model contains total=%v", resp["total"])
	}

	// from RFC3339 过滤旧条目。
	resp = getJSON(t, p, "/panel/api/chatlogs?from="+now.Add(-90*time.Minute).Format(time.RFC3339))
	if int(resp["total"].(float64)) != 1 {
		t.Fatalf("from filter total=%v", resp["total"])
	}

	// 分页：limit=1 第一页取最新，offset=1 取次新。
	resp = getJSON(t, p, "/panel/api/chatlogs?limit=1")
	entries = resp["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["seq"].(float64) != 2 {
		t.Fatalf("page1: %+v", entries)
	}
	resp = getJSON(t, p, "/panel/api/chatlogs?limit=1&offset=1")
	entries = resp["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["seq"].(float64) != 1 {
		t.Fatalf("page2: %+v", entries)
	}
}

// TestUsageStatsAPI 桶结构与补零、排行顺序、缺省窗口回退 24h。
func TestUsageStatsAPI(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	e1 := ChatEntry{Seq: 1, TS: now.Add(-30 * time.Minute), Model: "cn:glm-5.2", Mode: "stream",
		UID: "aaaaaaaa", Status: 200, TTFBMs: 100, InTokens: 10, Tokens: 20, TotalSec: 2}
	e2 := ChatEntry{Seq: 2, TS: now.Add(-5 * time.Minute), Model: "global:kimi-k2.7", Mode: "sync",
		UID: "bbbbbbbb", Status: 503, TTFBMs: -1, InTokens: -1, Tokens: -1, Err: "boom"}
	rl.Write(e1)
	rl.Write(e2)
	p := New(Config{Version: "test", ReqLog: rl})

	// 显式 1h 窗口：两条都在窗内。
	path := "/panel/api/usage_stats?from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(now.Format(time.RFC3339))
	m := getJSON(t, p, path)
	if int(m["bucket_sec"].(float64)) != 300 {
		t.Fatalf("bucket_sec=%v want 300", m["bucket_sec"])
	}
	buckets := m["buckets"].([]any)
	if len(buckets) != 12 {
		t.Fatalf("buckets=%d want 12", len(buckets))
	}
	empty := buckets[0].(map[string]any)
	if empty["req"].(float64) != 0 || empty["ok"].(float64) != 0 || empty["err"].(float64) != 0 {
		t.Fatalf("empty bucket not zero-filled: %+v", empty)
	}
	// e1 落在第 6 桶（30m/300s），e2 落在第 11 桶（55m/300s）。
	if int(buckets[6].(map[string]any)["req"].(float64)) != 1 {
		t.Fatalf("bucket[6]: %+v", buckets[6])
	}
	if int(buckets[11].(map[string]any)["err"].(float64)) != 1 {
		t.Fatalf("bucket[11]: %+v", buckets[11])
	}
	totals := m["totals"].(map[string]any)
	if totals["req"].(float64) != 2 || totals["ok"].(float64) != 1 || totals["err"].(float64) != 1 {
		t.Fatalf("totals: %+v", totals)
	}
	// 排行：req 并列时按名称稳定排序；账号 bbbbbbbb 只有 1 条。
	if len(m["models"].([]any)) != 2 || m["models"].([]any)[0].(map[string]any)["model"] != "cn:glm-5.2" {
		t.Fatalf("models: %+v", m["models"])
	}
	if len(m["accounts"].([]any)) != 2 || m["accounts"].([]any)[0].(map[string]any)["uid"] != "aaaaaaaa" {
		t.Fatalf("accounts: %+v", m["accounts"])
	}

	// 缺省窗口 = 最近 24h → 3600s 桶；两条都在窗内。
	m = getJSON(t, p, "/panel/api/usage_stats")
	if int(m["bucket_sec"].(float64)) != 3600 {
		t.Fatalf("default bucket_sec=%v want 3600", m["bucket_sec"])
	}
	totals = m["totals"].(map[string]any)
	if totals["req"].(float64) != 2 {
		t.Fatalf("default window totals: %+v", totals)
	}
}

// TestMetricsAPI metrics 字段齐全：速率、在途、池健康度、粘性、版本。
func TestMetricsAPI(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	// 30 条请求（每条 in 10 + out 20 = 30 token，全部落在最近 1 分钟）：
	// rpm=30 → qps=0.5；tpm=900 → tps=15.0。
	for i := 0; i < 30; i++ {
		rl.Write(testEntry(int64(i+1), now.Add(-time.Duration(i)*time.Second), "aaaaaaaa", "m", 200))
	}

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	pnl := New(Config{Version: "1.9.0-test", ReqLog: rl, Pool: p, StickyCount: func() int { return 3 }})

	m := getJSON(t, pnl, "/panel/api/metrics")
	for _, key := range []string{"in_flight", "rpm", "tpm", "qps", "tps", "uptime_sec",
		"accounts_total", "accounts_healthy", "sticky_sessions", "version"} {
		if _, ok := m[key]; !ok {
			t.Errorf("metrics missing %q: %+v", key, m)
		}
	}
	if int(m["rpm"].(float64)) != 30 || int(m["tpm"].(float64)) != 900 {
		t.Fatalf("rpm/tpm: %v/%v want 30/900", m["rpm"], m["tpm"])
	}
	if qps := m["qps"].(float64); qps != 0.5 {
		t.Fatalf("qps=%v want 0.5（rpm/60 保留 1 位小数）", qps)
	}
	if tps := m["tps"].(float64); tps != 15.0 {
		t.Fatalf("tps=%v want 15.0", tps)
	}
	if int(m["sticky_sessions"].(float64)) != 3 {
		t.Fatalf("sticky: %v", m["sticky_sessions"])
	}
	if m["version"] != "1.9.0-test" {
		t.Fatalf("version: %v", m["version"])
	}
	if int(m["accounts_total"].(float64)) != 0 || int(m["accounts_healthy"].(float64)) != 0 {
		t.Fatalf("accounts: %v/%v", m["accounts_total"], m["accounts_healthy"])
	}
}
