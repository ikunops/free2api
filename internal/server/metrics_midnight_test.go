package server

import (
	"testing"
	"time"
)

// TestTodaySpecFromIsRecomputedPerFrame today 的 from 必须每帧重算，不能在建连时固化。
//
// 踩过的坑：/v1/stats/stream 把 rangeSpec 在建连时算一次就存着。SSE 是长连接，
// 跨零点不断开，于是 from 永远停在**建连那天的零点** —— 过了 0 点还在推送昨天的
// 桶，而且那个桶仍在被写入，页面上就出现「今天请求 1694 而且还在涨」，
// 可实际涨的是昨天的数据。左下角柱子因为走了另一条路径先对上，更显得诡异。
//
// 这里断言 parseRangeSpec 每次调用都取当下的零点：同一根指针隔一天再取，
// from 必须差 24 小时。
func TestTodaySpecFromIsRecomputedPerFrame(t *testing.T) {
	first := parseRangeSpec("today")
	second := parseRangeSpec("today")

	if first.id != "today" || second.id != "today" {
		t.Fatalf("id 丢了：%q / %q", first.id, second.id)
	}

	// 两个 spec 指向同一天的同一个零点（同一帧内多次调用必须一致）。
	if !first.from.Equal(second.from) {
		t.Fatalf("同一时刻两次解析的 from 不一致：%v vs %v", first.from, second.from)
	}

	// 关键断言：from 必须对齐到本地零点，而不是「当前时刻」。
	want := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(),
		0, 0, 0, 0, time.Now().Location())
	if !first.from.Equal(want) {
		t.Errorf("today.from=%v 不是本地零点 %v", first.from, want)
	}

	// 把 spec 的窗口挪到昨天，模拟「连接建于昨天、今天仍在推」。
	yesterday := first.from.AddDate(0, 0, -1)
	stale := rangeSpec{id: "today", from: yesterday}
	fresh := parseRangeSpec("today")

	if stale.from.Equal(fresh.from) {
		t.Fatal("测试没意义：固化 spec 与重算 spec 的 from 竟然相同")
	}
	if got := fresh.from.Sub(stale.from); got < 23*time.Hour || got > 25*time.Hour {
		t.Errorf("重算后的 from 应比固化值晚约一天，实测 %v", got)
	}
}

// TestRollingRangeWindowsSlide 7d / 30d 的窗口同样每天要滑动一格 —— 它们是
// 「最近 N 天」，不是「建连那天的最近 N 天」。
func TestRollingRangeWindowsSlide(t *testing.T) {
	today := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(),
		0, 0, 0, 0, time.Now().Location())

	for _, tc := range []struct {
		rng  string
		days int
	}{
		{"7d", 7},
		{"30d", 30},
	} {
		spec := parseRangeSpec(tc.rng)
		want := today.AddDate(0, 0, -(tc.days - 1))
		if !spec.from.Equal(want) {
			t.Errorf("%s: from=%v want %v（今天是第 1 天，往前数 %d 天）",
				tc.rng, spec.from, want, tc.days-1)
		}
	}
}
