package server

// metrics_backfill_test.go —— 历史流水日志回填（BackfillFromLogs）。
//
// 背景：逐日统计是后加的特性，启用之前的请求只留在网关自己的流水日志里。
// 回填要能从那些行重建「每天每模型多少请求 / 成功失败 / token / 耗时」，
// 且必须幂等（已有那天不重复计）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backfillRow 造一行与 logChatRow 同形的流水日志（列宽用空格补齐，与生产一致）。
func backfillRow(seq int, hhmmss, model, mode string, status int, ttfbMS, toks int, totalSec float64) string {
	ttfb := "-"
	if ttfbMS > 0 {
		ttfb = itoa(ttfbMS) + "ms"
	}
	tok := "-"
	if toks >= 0 {
		tok = itoa(toks)
	}
	return "| #" + pad3(seq) + " | " + hhmmss + " | " + model + " | " + mode + " | " +
		itoa(status) + " | acct(12345678)    | TTFB=" + ttfb + " | tok=" + tok +
		" | 10.0tok/s | total=" + ftoa(totalSec) + "s |"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func pad3(n int) string {
	s := itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func ftoa(f float64) string {
	// 只要一位小数，够本测试用
	whole := int(f)
	frac := int((f-float64(whole))*10 + 0.5)
	return itoa(whole) + "." + itoa(frac)
}

// TestParseChatRow 解析一行流水日志：各字段逐个核对。
func TestParseChatRow(t *testing.T) {
	line := backfillRow(7, "19:29:17", "global:deepseek-v4.1-flash", "stream", 200, 4148, 529, 6.6)
	clock, model, mode, status, toks, ttfb, total, ok := parseChatRow(line)
	if !ok {
		t.Fatalf("parse failed: %q", line)
	}
	if model != "global:deepseek-v4.1-flash" {
		t.Errorf("model=%q", model)
	}
	if mode != "stream" {
		t.Errorf("mode=%q", mode)
	}
	if status != 200 {
		t.Errorf("status=%d", status)
	}
	if toks != 529 {
		t.Errorf("toks=%d", toks)
	}
	if ttfb != 4148*time.Millisecond {
		t.Errorf("ttfb=%v", ttfb)
	}
	if total != 66*100*time.Millisecond {
		t.Errorf("total=%v want 6.6s", total)
	}
	if clock.Hour() != 19 || clock.Minute() != 29 || clock.Second() != 17 {
		t.Errorf("clock=%v", clock)
	}
}

// TestParseChatRowNoUsage 无 usage 时 tok=-，解析后 toks<0（缺失≠0）。
func TestParseChatRowNoUsage(t *testing.T) {
	line := backfillRow(1, "07:32:02", "cn:auto", "sync", 503, 0, -1, 16.1)
	_, _, _, status, toks, ttfb, _, ok := parseChatRow(line)
	if !ok {
		t.Fatalf("parse failed: %q", line)
	}
	if status != 503 {
		t.Errorf("status=%d", status)
	}
	if toks != -1 {
		t.Errorf("toks=%d want -1（缺失）", toks)
	}
	if ttfb != 0 {
		t.Errorf("ttfb=%v want 0", ttfb)
	}
}

// TestParseChatRowRejectsGarbage 非请求行 / 坏行一律不解析。
func TestParseChatRowRejectsGarbage(t *testing.T) {
	for _, s := range []string{
		"",
		"2026/10/02 19:29:17 loaded 17 account(s) from ./auths",
		"| #001 | 99:99:99 | m | sync | 200 | a | TTFB=- | tok=1 | x | total=1.0s |",
		"| #001 | 10:00:00 | m | sync | abc | a | TTFB=- | tok=1 | x | total=1.0s |",
		"| #001 | 10:00:00 | m | sync | 200 | a | TTFB=- | tok=1 | x | total=xxxs |",
	} {
		if _, _, _, _, _, _, _, ok := parseChatRow(s); ok {
			t.Errorf("should not parse: %q", s)
		}
	}
}

// TestBackfillFromLogs 从日志回填出逐日桶，且模型/来源/小时都落对。
func TestBackfillFromLogs(t *testing.T) {
	resetMetricsForTest(t)
	hardResetMetrics()
	t.Cleanup(hardResetMetrics)

	dir := t.TempDir()
	// 造两天的日志：昨天 1 条，今天 1 条（1 成功 1 失败）。
	today := time.Now()
	content := backfillRow(1, "23:10:00", "global:deepseek-v4.1-flash", "stream", 200, 1000, 100, 2.0) + "\n" +
		backfillRow(2, "23:20:00", "cn:auto", "sync", 503, 0, -1, 3.0) + "\n"
	p := filepath.Join(dir, "stdout.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 文件 mtime 设为今天：最后一行落在今天，往前遇到时刻倒退即昨天。
	os.Chtimes(p, today, today)

	// 只扫一个目录，且不扫轮转备份。
	days, rows := BackfillFromLogs(dir)
	if rows != 2 {
		t.Errorf("rows=%d want 2", rows)
	}
	if days == 0 {
		t.Fatalf("days=0，应该补出日桶")
	}

	snap := metricsSnapshotRange(parseRangeSpec("all"))
	if snap.Total.Requests != 2 {
		t.Fatalf("total requests=%d want 2", snap.Total.Requests)
	}
	if snap.Total.Success != 1 || snap.Total.Failed != 1 {
		t.Errorf("success=%d failed=%d want 1/1", snap.Total.Success, snap.Total.Failed)
	}
	// 来源：global: 前缀归 workbuddy，cn:auto 也归 workbuddy（裸名默认）。
	if len(snap.Producers) != 1 || snap.Producers[0].Producer != "workbuddy" {
		t.Errorf("producers=%+v", snap.Producers)
	}
	// 模型：两个不同键各 1 次。
	if len(snap.Models) != 2 {
		t.Errorf("models=%d want 2: %+v", len(snap.Models), snap.Models)
	}
}

// TestBackfillFromLogsIdempotent 同一天已有桶时不重复计。
func TestBackfillFromLogsIdempotent(t *testing.T) {
	resetMetricsForTest(t)
	hardResetMetrics()
	t.Cleanup(hardResetMetrics)

	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "stdout.log")
	content := backfillRow(1, "12:00:00", "cn:auto", "stream", 200, 100, 50, 1.0) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, now, now)

	if _, rows := BackfillFromLogs(dir); rows == 0 {
		t.Fatal("第一次应该解析到行")
	}
	first := metricsSnapshotRange(parseRangeSpec("all")).Total.Requests
	if first != 1 {
		t.Fatalf("first=%d want 1", first)
	}
	// 第二次回填：那天已有桶，整日跳过 → 请求数不变。
	days, _ := BackfillFromLogs(dir)
	if days != 0 {
		t.Errorf("第二次 days=%d want 0（已有日不重复补）", days)
	}
	if again := metricsSnapshotRange(parseRangeSpec("all")).Total.Requests; again != first {
		t.Errorf("幂等被破坏：again=%d want %d", again, first)
	}
}

// TestBackfillFromLogsNoFiles 没有日志文件时安静返回零值，不报错。
func TestBackfillFromLogsNoFiles(t *testing.T) {
	resetMetricsForTest(t)
	dir := t.TempDir()
	days, rows := BackfillFromLogs(dir)
	if days != 0 || rows != 0 {
		t.Errorf("days=%d rows=%d want 0/0", days, rows)
	}
}

// TestModelProducerFromName 来源推断：显式前缀优先，裸名回落 workbuddy。
func TestModelProducerFromName(t *testing.T) {
	cases := map[string]string{
		"zcode:glm-5.3-flash":        "zcode",
		"opencode:big-pickle":        "opencode",
		"kilo:kilo-auto/free":        "kilo",
		"cn:deepseek-v4.1-flash":     "workbuddy",
		"global:deepseek-v4.1-flash": "workbuddy",
		"deepseek-v4.1-flash":        "workbuddy",
	}
	for in, want := range cases {
		if got := modelProducerFromName(in); got != want {
			t.Errorf("modelProducerFromName(%q)=%q want %q", in, got, want)
		}
	}
}
