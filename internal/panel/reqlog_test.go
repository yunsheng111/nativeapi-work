package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEntry 构造一条完整的测试条目（正常路径字段齐全，需要缺失维度的用例直接改字段）。
func testEntry(seq int64, ts time.Time, uid, model string, status int) ChatEntry {
	return ChatEntry{
		TS: ts, Seq: seq, Model: model, Mode: "stream", UID: uid, Status: status,
		TTFBMs: 100, InTokens: 10, Tokens: 20, TokPs: 5, TotalSec: 2,
	}
}

func mustJSON(t *testing.T, ce ChatEntry) string {
	t.Helper()
	raw, err := json.Marshal(ce)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestReqLogMemoryOnly 纯内存模式（dir=""）：查询可用，不产生任何文件。
func TestReqLogMemoryOnly(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	rl.Write(testEntry(1, time.Now(), "aaaaaaaa", "cn:glm-5.2", 200))
	entries, total, st := rl.Query(QueryOpts{})
	if total != 1 || len(entries) != 1 {
		t.Fatalf("total=%d entries=%d want 1/1", total, len(entries))
	}
	if st.Req != 1 || st.Ok != 1 || st.Err != 0 || st.InTokens != 10 || st.OutTokens != 20 {
		t.Fatalf("stats mismatch: %+v", st)
	}
	if st.TTFBAvgMs != 100 || st.TotalAvgSec != 2 {
		t.Fatalf("averages mismatch: %+v", st)
	}
}

// TestReqLogPersistRoundTrip 写入落盘 → 关闭 → 重启载入：条目跨进程恢复。
func TestReqLogPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	rl := NewReqLog(dir)
	good := testEntry(2, now, "bbbbbbbb", "global:kimi-k2.7", 503)
	good.Err = "no_healthy_account: upstream 429"
	rl.Write(testEntry(1, now.Add(-time.Minute), "aaaaaaaa", "cn:glm-5.2", 200))
	rl.Write(good)
	rl.Close()

	// 当前小时的分段文件已生成。
	if _, err := os.Stat(filepath.Join(dir, segName(now))); err != nil {
		t.Fatalf("segment file missing: %v", err)
	}

	rl2 := NewReqLog(dir)
	defer rl2.Close()
	entries, total, st := rl2.Query(QueryOpts{})
	if total != 2 || len(entries) != 2 {
		t.Fatalf("total=%d want 2", total)
	}
	// 降序：最新在前；Err 与 token 维度经 JSON 往返后保留。
	if entries[0].Seq != 2 || entries[1].Seq != 1 {
		t.Fatalf("order not desc: %d %d", entries[0].Seq, entries[1].Seq)
	}
	if entries[0].Err != good.Err || entries[0].InTokens != 10 || entries[0].Tokens != 20 {
		t.Fatalf("err/tokens lost in round trip: %+v", entries[0])
	}
	if st.Err != 1 || st.Ok != 1 || st.Req != 2 {
		t.Fatalf("stats mismatch: %+v", st)
	}
}

// TestReqLogSeedSkipsStaleAndBadLines 种子载入：坏行跳过，超 24h 条目过滤，升序入内存。
func TestReqLogSeedSkipsStaleAndBadLines(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	lines := []string{
		"not a json line", // 坏行
		"{}",              // 零值条目（ts 零 → 过期）
		mustJSON(t, testEntry(1, now.Add(-25*time.Hour), "aaaaaaaa", "m", 200)), // 超窗
		mustJSON(t, testEntry(2, now.Add(-2*time.Hour), "bbbbbbbb", "m", 200)),  // 保留
		mustJSON(t, testEntry(3, now, "cccccccc", "m", 200)),                    // 保留
		"", // 空行
	}
	if err := os.WriteFile(filepath.Join(dir, segName(now)), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	rl := NewReqLog(dir)
	defer rl.Close()
	entries, total, _ := rl.Query(QueryOpts{})
	if total != 2 {
		t.Fatalf("total=%d want 2（坏行与超窗条目被过滤）", total)
	}
	if entries[0].Seq != 3 || entries[1].Seq != 2 {
		t.Fatalf("seed not sorted/filtered: %d %d", entries[0].Seq, entries[1].Seq)
	}
}

// TestReqLogHourSegments 按 ts 所在小时分段：跨小时的条目落到不同分段文件。
func TestReqLogHourSegments(t *testing.T) {
	dir := t.TempDir()
	rl := NewReqLog(dir)
	// 取整点对齐的两个相邻小时：直接用 now 减固定时长，在整点附近会落进同一小时。
	h1 := time.Now().Truncate(time.Hour)
	h0 := h1.Add(-time.Hour)
	rl.Write(testEntry(1, h0.Add(time.Minute), "aaaaaaaa", "m", 200))
	rl.Write(testEntry(2, h1.Add(time.Minute), "bbbbbbbb", "m", 200))
	rl.Close()
	for _, h := range []time.Time{h0, h1} {
		if _, err := os.Stat(filepath.Join(dir, segName(h))); err != nil {
			t.Errorf("segment %s missing: %v", segName(h), err)
		}
	}
}

// TestReqLogSeedReadsLegacyDailySegment 升级兼容：旧版按天分段仍能被载入，
// 其中超出 24h 的条目按统一口径过滤。
func TestReqLogSeedReadsLegacyDailySegment(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	lines := []string{
		mustJSON(t, testEntry(1, now.Add(-2*time.Hour), "aaaaaaaa", "m", 200)),  // 保留
		mustJSON(t, testEntry(2, now.Add(-30*time.Hour), "bbbbbbbb", "m", 200)), // 超窗
	}
	name := segPrefix + now.AddDate(0, 0, -1).Format(legacySegLayout) + segSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	rl := NewReqLog(dir)
	defer rl.Close()
	entries, total, _ := rl.Query(QueryOpts{})
	if total != 1 || len(entries) != 1 || entries[0].Seq != 1 {
		t.Fatalf("legacy segment not loaded/filtered: total=%d entries=%+v", total, entries)
	}
}

// TestReqLogSweepPrunesOldSegments sweep 按 segEnd 判过期：小时分段看 start+1h，
// 旧版按天分段看 mtime（文件已冻结）；无关文件与正在写的分段不碰。
func TestReqLogSweepPrunesOldSegments(t *testing.T) {
	dir := t.TempDir()
	rl := NewReqLog(dir)
	defer rl.Close()
	now := time.Now().Truncate(time.Hour)
	expiredHour := now.Add(-30 * time.Hour) // 区间 30h~29h 前：整体过期
	keptHour := now.Add(-23 * time.Hour)    // 区间 23h~22h 前：仍在窗内
	legacyExpired := segPrefix + now.AddDate(0, 0, -3).Format(legacySegLayout) + segSuffix
	legacyToday := segPrefix + now.Format(legacySegLayout) + segSuffix
	keep := []string{segName(keptHour), segName(now), legacyToday, "app.log"}
	for _, name := range append([]string{segName(expiredHour), legacyExpired}, keep...) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// legacy 分段的过期看 mtime：把 3 天前那份的最后写入时刻也挪到 3 天前，
	// 今天那份保持"刚写过"。
	old := now.AddDate(0, 0, -3)
	if err := os.Chtimes(filepath.Join(dir, legacyExpired), old, old); err != nil {
		t.Fatal(err)
	}
	rl.sweep()
	for _, name := range []string{segName(expiredHour), legacyExpired} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expired segment %s not removed (stat err=%v)", name, err)
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should survive sweep: %v", name, err)
		}
	}
}

// TestReqLogSweepKeepsActiveSegment 正在写的分段即使时间戳很旧也不删：Windows 上
// 句柄未关时删除必然失败（每轮 sweep 刷错误日志），Linux 上会静默丢后续写入。
func TestReqLogSweepKeepsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	rl := NewReqLog(dir)
	defer rl.Close()
	// 直塞一条 30h 前的条目：Write 会按它的 ts 打开对应分段并保持句柄。
	stale := time.Now().Add(-30 * time.Hour)
	rl.Write(testEntry(1, stale, "aaaaaaaa", "m", 200))
	active := segName(stale)
	if _, err := os.Stat(filepath.Join(dir, active)); err != nil {
		t.Fatalf("active segment missing: %v", err)
	}
	rl.sweep()
	if _, err := os.Stat(filepath.Join(dir, active)); err != nil {
		t.Errorf("active segment must not be pruned: %v", err)
	}
}

// TestParseSegRejectsMalformedNames 命名约定之外的串一律拒绝：长度不对、日期非法
// （time.Parse 会把 20260230 归一化成 03-02）、前缀后缀不完整。
func TestParseSegRejectsMalformedNames(t *testing.T) {
	bad := []string{
		"requests-20260230.jsonl", // 归一化日期：回写比对必须拒绝
		"requests-202602301.jsonl",
		"requests-202609.jsonl",
		"requests-.jsonl",
		"2026091723.jsonl",
		"requests-2026091723.log",
		"other-2026091723.jsonl",
	}
	for _, name := range bad {
		if _, _, ok := parseSeg(name); ok {
			t.Errorf("parseSeg(%q) 应拒绝", name)
		}
	}
	good := []struct {
		name string
		span time.Duration
	}{
		{"requests-2026091723.jsonl", segSpan},     // 小时分段
		{"requests-20260917.jsonl", legacySegSpan}, // 旧版按天分段
	}
	for _, tc := range good {
		start, span, ok := parseSeg(tc.name)
		if !ok {
			t.Errorf("parseSeg(%q) 应接受", tc.name)
			continue
		}
		if start.IsZero() || span != tc.span {
			t.Errorf("parseSeg(%q) start=%v span=%v want span=%v", tc.name, start, span, tc.span)
		}
	}
}

// TestReqLogQuerySameTSStableOrder 同 ts 条目的分页顺序必须稳定（Seq 降序兜底），
// 否则 offset 分页会在相邻页之间重复或漏行。
func TestReqLogQuerySameTSStableOrder(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	ts := time.Now() // 同一时刻：模拟同一毫秒内完成的多个请求
	for i := 1; i <= 6; i++ {
		rl.Write(testEntry(int64(i), ts, "aaaaaaaa", "m", 200))
	}
	var got []int64
	for offset := 0; offset < 6; offset += 2 {
		page, total, _ := rl.Query(QueryOpts{Limit: 2, Offset: offset})
		if total != 6 {
			t.Fatalf("offset=%d total=%d want 6", offset, total)
		}
		for _, e := range page {
			got = append(got, e.Seq)
		}
	}
	want := []int64{6, 5, 4, 3, 2, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("同 ts 分页顺序 %v，want %v（重复或漏行）", got, want)
		}
	}
}

// TestReqLogSweepPrunesMemory 内存 sweep：超 24h 的条目被淘汰。
func TestReqLogSweepPrunesMemory(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	rl.Write(testEntry(2, now, "bbbbbbbb", "m", 200))
	// 直塞一条过期条目（绕过 Write 的即时淘汰），让 sweep 承担清理。
	rl.entries = append(rl.entries, testEntry(1, now.Add(-25*time.Hour), "aaaaaaaa", "m", 200))
	rl.sweep()
	_, total, _ := rl.Query(QueryOpts{})
	if total != 1 {
		t.Fatalf("total=%d want 1 after sweep", total)
	}
}

// TestReqLogMemoryCap 内存容量上限：超限淘汰最旧。
func TestReqLogMemoryCap(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	for i := 0; i < reqLogCap+500; i++ {
		rl.Write(testEntry(int64(i+1), now.Add(time.Duration(i)*time.Millisecond), "aaaaaaaa", "m", 200))
	}
	if len(rl.entries) != reqLogCap {
		t.Fatalf("entries=%d want cap %d", len(rl.entries), reqLogCap)
	}
	// 最旧的 500 条被淘汰：最老现存条目 seq = 501。
	last, n, _ := rl.Query(QueryOpts{Offset: reqLogCap - 1, Limit: 1})
	if n != reqLogCap || len(last) != 1 || last[0].Seq != 501 {
		t.Fatalf("oldest kept seq=%d n=%d, want 501/%d", last[0].Seq, n, reqLogCap)
	}
}

// TestReqLogQueryFilters 筛选维度：uid 前缀双向、model 双向包含、mode、status、err_only、时间窗。
func TestReqLogQueryFilters(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	bad := testEntry(2, now.Add(-20*time.Minute), "bbbbbbbb", "cn:glm-5.2", 500)
	bad.Mode = "sync"
	bad.InTokens = 5
	bad.Tokens = -1
	bad.Err = "boom"
	rl.Write(testEntry(1, now.Add(-30*time.Minute), "aaaaaaaa", "cn:glm-5.2", 200))
	rl.Write(bad)
	rl.Write(testEntry(3, now, "cccccccc", "global:kimi-k2.7", 503))

	cases := []struct {
		name string
		opts QueryOpts
		want []int64 // 期望命中的 seq（按返回顺序，ts 降序）
	}{
		{"no filter", QueryOpts{}, []int64{3, 2, 1}},
		{"uid exact 8char", QueryOpts{UID: "aaaaaaaa"}, []int64{1}},
		{"uid prefix of entry", QueryOpts{UID: "bbbb"}, []int64{2}},
		{"uid entry prefix of full", QueryOpts{UID: "cccccccc12345678"}, []int64{3}},
		{"uid dash never matches", QueryOpts{UID: "-"}, nil},
		{"model contains", QueryOpts{Model: "cn:glm"}, []int64{2, 1}},
		{"model case-insensitive", QueryOpts{Model: "GLM-5.2"}, []int64{2, 1}},
		{"model query contains entry", QueryOpts{Model: "kimi-k2.7-flash"}, []int64{3}},
		{"model no match", QueryOpts{Model: "deepseek"}, nil},
		{"mode stream", QueryOpts{Mode: "stream"}, []int64{3, 1}},
		{"status ok", QueryOpts{Status: "ok"}, []int64{1}},
		{"status bad", QueryOpts{Status: "bad"}, []int64{3, 2}},
		{"err only", QueryOpts{ErrOnly: true}, []int64{3, 2}},
		{"from excludes old", QueryOpts{From: now.Add(-25 * time.Minute)}, []int64{3, 2}},
		{"to excludes new", QueryOpts{To: now.Add(-25 * time.Minute)}, []int64{1}},
		{"combined", QueryOpts{Model: "glm", Status: "bad"}, []int64{2}},
	}
	for _, tc := range cases {
		entries, total, _ := rl.Query(tc.opts)
		if total != len(tc.want) {
			t.Errorf("%s: total=%d want %d", tc.name, total, len(tc.want))
			continue
		}
		for i, seq := range tc.want {
			if entries[i].Seq != seq {
				t.Errorf("%s: entries[%d].Seq=%d want %d", tc.name, i, entries[i].Seq, seq)
			}
		}
	}
}

// TestReqLogQueryPaging 分页：limit 默认/上限、offset 越界收敛。
func TestReqLogQueryPaging(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	for i := 0; i < 5; i++ {
		rl.Write(testEntry(int64(i+1), now.Add(-time.Duration(4-i)*time.Minute), "aaaaaaaa", "m", 200))
	}
	page, total, _ := rl.Query(QueryOpts{Limit: 2})
	if total != 5 || len(page) != 2 || page[0].Seq != 5 || page[1].Seq != 4 {
		t.Fatalf("page1: total=%d seqs=%d,%d", total, page[0].Seq, page[1].Seq)
	}
	page, _, _ = rl.Query(QueryOpts{Limit: 2, Offset: 4})
	if len(page) != 1 || page[0].Seq != 1 {
		t.Fatalf("tail page: %+v", page)
	}
	page, _, _ = rl.Query(QueryOpts{Offset: 99})
	if len(page) != 0 {
		t.Fatalf("offset beyond end: %+v", page)
	}
	page, _, _ = rl.Query(QueryOpts{Limit: -1})
	if len(page) != 5 { // limit<=0 回退默认 200；只有 5 条时全部返回
		t.Fatalf("default limit page: %d want 5", len(page))
	}
}

// TestReqLogStatsSentinels 无样本时平均值哨兵 -1；缺失 token 不计入合计。
func TestReqLogStatsSentinels(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	_, _, st := rl.Query(QueryOpts{})
	if st.TTFBAvgMs != -1 || st.TotalAvgSec != -1 {
		t.Fatalf("empty stats sentinels: %+v", st)
	}
	now := time.Now()
	ce := testEntry(1, now, "aaaaaaaa", "m", 200)
	ce.TTFBMs = -1
	ce.InTokens = -1
	ce.Tokens = -1
	rl.Write(ce)
	_, _, st = rl.Query(QueryOpts{})
	if st.InTokens != 0 || st.OutTokens != 0 {
		t.Fatalf("missing tokens must not count: %+v", st)
	}
	if st.TTFBAvgMs != -1 {
		t.Fatalf("missing ttfb must be excluded from avg: %+v", st)
	}
	if st.TotalAvgSec != 2 {
		t.Fatalf("total avg: %v want 2", st.TotalAvgSec)
	}
}

// TestReqLogUsageStats 桶结构：尺寸按跨度自选、空桶补零、排行按 req 降序。
func TestReqLogUsageStats(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	from := now.Add(-time.Hour)
	m1 := testEntry(1, now.Add(-30*time.Minute), "aaaaaaaa", "cn:glm-5.2", 200)
	m2 := testEntry(2, now.Add(-5*time.Minute), "aaaaaaaa", "cn:glm-5.2", 500)
	m2.InTokens = 7
	m3 := testEntry(3, now.Add(-time.Minute), "bbbbbbbb", "global:kimi-k2.7", 200)
	rl.Write(m1)
	rl.Write(m2)
	rl.Write(m3)

	rep := rl.UsageStats(from, now)
	if rep.BucketSec != 300 {
		t.Fatalf("bucket_sec=%d want 300 (跨度 1h)", rep.BucketSec)
	}
	if len(rep.Buckets) != 12 {
		t.Fatalf("buckets=%d want 12", len(rep.Buckets))
	}
	// 桶起点从 from 起步、等距递增；窗口内空桶补零。
	if rep.Buckets[0].T != from.Unix() {
		t.Fatalf("bucket[0].t=%d want %d", rep.Buckets[0].T, from.Unix())
	}
	if rep.Buckets[0].Req != 0 {
		t.Fatalf("empty bucket not zero-filled: %+v", rep.Buckets[0])
	}
	// m1 落在第 6 桶（30m / 300s），m2/m3 落在最后两桶。
	if rep.Buckets[6].Req != 1 {
		t.Fatalf("bucket[6].req=%d want 1", rep.Buckets[6].Req)
	}
	if rep.Buckets[11].Req+rep.Buckets[10].Req != 2 {
		t.Fatalf("tail buckets: %+v", rep.Buckets[10:12])
	}
	if rep.Totals.Req != 3 || rep.Totals.Ok != 2 || rep.Totals.Err != 1 || rep.Totals.InTokens != 27 {
		t.Fatalf("totals: %+v", rep.Totals)
	}
	// 排行：模型 glm(req=2) 在前；账号 aaaaaaaa(req=2) 在前。
	if len(rep.Models) != 2 || rep.Models[0].Model != "cn:glm-5.2" || rep.Models[0].Req != 2 {
		t.Fatalf("models: %+v", rep.Models)
	}
	if len(rep.Accounts) != 2 || rep.Accounts[0].UID != "aaaaaaaa" || rep.Accounts[0].Req != 2 {
		t.Fatalf("accounts: %+v", rep.Accounts)
	}
	if rep.Accounts[0].InTokens != 17 || rep.Accounts[0].OutTokens != 40 {
		t.Fatalf("account tokens: %+v", rep.Accounts[0])
	}
}

// TestReqLogUsageStatsBucketSizes 桶尺寸档位：<=1h→300s，<=3h→900s，否则 3600s。
func TestReqLogUsageStatsBucketSizes(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	cases := []struct {
		span      time.Duration
		bucketSec int
	}{
		{30 * time.Minute, 300},
		{time.Hour, 300},
		{2 * time.Hour, 900},
		{3 * time.Hour, 900},
		{24 * time.Hour, 3600},
	}
	for _, tc := range cases {
		rep := rl.UsageStats(now.Add(-tc.span), now)
		if rep.BucketSec != tc.bucketSec {
			t.Errorf("span=%v bucket_sec=%d want %d", tc.span, rep.BucketSec, tc.bucketSec)
		}
	}
}

// TestReqLogWindowCount 最近窗口的条数与 token 合计（缺失算 0）。
func TestReqLogWindowCount(t *testing.T) {
	rl := NewReqLog("")
	defer rl.Close()
	now := time.Now()
	fresh := testEntry(1, now.Add(-30*time.Second), "aaaaaaaa", "m", 200)
	rl.Write(fresh)
	rl.Write(testEntry(2, now.Add(-2*time.Hour), "bbbbbbbb", "m", 200)) // 窗口外
	noTok := testEntry(3, now.Add(-10*time.Second), "cccccccc", "m", 503)
	noTok.InTokens, noTok.Tokens = -1, -1
	rl.Write(noTok)
	req, tokens := rl.WindowCount(time.Minute)
	if req != 2 || tokens != 30 {
		t.Fatalf("req=%d tokens=%d want 2/30", req, tokens)
	}
}
