package main

// layout_test.go —— 窗口自适应（宽度裁剪 / 行数折叠 / 安全网）的测试。
//
// 背景：分屏等窄窗口下，帧若宽于终端就会折行，watch 按逻辑行数上移光标随即
// 失准，于是每帧都在旧内容下方再画一份（表现为"刷新一次多一个统计窗口"）。
// 本文件锁死"给定宽度必然不折行"这一契约。

import (
	"fmt"
	"strings"
	"testing"
)

// maxFrameWidth 返回帧内最宽一行的显示列数。
func maxFrameWidth(frame []string) int {
	mx := 0
	for _, l := range frame {
		if w := displayWidth(l); w > mx {
			mx = w
		}
	}
	return mx
}

// multiRows 造一组覆盖典型形态的统计（长模型名、中文名、多模型）。
func multiRows() ([]modelStat, modelStat) {
	rows := []modelStat{
		mkModel("deepseek-v4.1-flash", 786, 3),
		mkModel("cn:deepseek-v4.1-flash", 29, 4),
		mkModel("deepseek-v4-pro", 13, 0),
	}
	return rows, mkModel("(all)", 828, 7)
}

// TestTableFitsWithinWidth 任意宽度下表格都不得超宽 —— 防折行的硬约束。
func TestTableFitsWithinWidth(t *testing.T) {
	rows, total := multiRows()
	for w := 28; w <= 200; w++ {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		if got := maxFrameWidth(tbl); got > w {
			t.Errorf("宽度上限 %d 时表格宽 %d（会折行）:\n%s", w, got, strings.Join(tbl, "\n"))
		}
	}
}

// TestTableDropsColumnsBeforeWrapping 变窄时应先淘汰次要列而非折行。
//
// 淘汰次序由 dropSeq 决定：缓存命中 → 输入/输出 → 吞吐 → 耗时 → 首字；
// 模型/请求/失败（keep）与扣费（dropSeq 最大）留到最后。
func TestTableDropsColumnsBeforeWrapping(t *testing.T) {
	rows, total := multiRows()

	wide := strings.Join(buildTable(rows, total, "requests", 200, 0, testNow), "\n")
	for _, h := range []string{"缓存命中", "输入/输出", "吞吐", "耗时", "首字", "扣费", "请求", "失败"} {
		if !strings.Contains(wide, h) {
			t.Errorf("宽窗口应含列 %q:\n%s", h, wide)
		}
	}

	// 逐级收窄：列数单调不增，且 keep 列与扣费始终在场。
	sawFewer := false
	prevCols := 99
	for _, w := range []int{200, 120, 100, 86, 70, 50} {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		joined := strings.Join(tbl, "\n")
		cols := strings.Count(tbl[0], "|") + 1
		if cols > prevCols {
			t.Errorf("w=%d 列数 %d 多于更宽时的 %d（不应随变窄而增加）", w, cols, prevCols)
		}
		if cols < prevCols {
			sawFewer = true
		}
		prevCols = cols
		for _, must := range []string{"模型", "请求", "失败", "扣费"} {
			if !strings.Contains(joined, must) {
				t.Errorf("w=%d 丢了必要列 %q:\n%s", w, must, joined)
			}
		}
	}
	if !sawFewer {
		t.Error("收窄过程应至少淘汰一列，否则说明裁剪逻辑未生效")
	}
}

// TestTableNoBracketBreak 裁剪后分隔线的 + 仍与各行的 | 逐列同位
// （TestTableSeparatorAlignsBars 的窄窗口版本）。
func TestTableNoBracketBreak(t *testing.T) {
	rows := []modelStat{mkModel("a", 131, 0), mkModel("中文模型名较长", 70, 3)}
	total := mkModel("(all)", 201, 3)
	for _, w := range []int{200, 120, 100, 86, 70, 50, 34} {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		var dataLines, sepLines []string
		for _, l := range tbl {
			switch {
			case strings.Contains(l, "|"):
				dataLines = append(dataLines, l)
			case strings.Contains(l, "+") && strings.Contains(l, "-"):
				sepLines = append(sepLines, l)
			}
		}
		if len(sepLines) == 0 {
			t.Fatalf("w=%d 未找到分隔线:\n%s", w, strings.Join(tbl, "\n"))
		}
		want := barColumns(sepLines[0], '+')
		for _, l := range dataLines {
			if got := barColumns(l, '|'); !equalInts(got, want) {
				t.Errorf("w=%d 竖线列位 %v ≠ 分隔线 %v:\n  %q", w, got, want, l)
			}
		}
	}
}

// TestTableFoldsRowsWhenHeightLimited 行数受限时保留前若干行并给出折叠提示。
//
// 提示必须显式说明"还有多少未显示"，避免静默丢数据；合计行是汇总，不该被折叠。
func TestTableFoldsRowsWhenHeightLimited(t *testing.T) {
	rows := make([]modelStat, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, mkModel(fmt.Sprintf("m%02d", i), int64(100-i), 0))
	}
	total := mkModel("(all)", 1200, 0)

	// 行数预算 8：表头+分隔线+分隔线+合计 = 4 固定，明细可用 4 行。
	tbl := buildTable(rows, total, "requests", 200, 8, testNow)
	if len(tbl) > 8 {
		t.Errorf("行数 = %d 超出预算 8:\n%s", len(tbl), strings.Join(tbl, "\n"))
	}
	joined := strings.Join(tbl, "\n")
	if !strings.Contains(joined, "另有") || !strings.Contains(joined, "未显示") {
		t.Errorf("被裁剪时应给出折叠提示:\n%s", joined)
	}
	if !strings.Contains(joined, "合计") {
		t.Errorf("合计行应保留:\n%s", joined)
	}
	// 不限行数时不得出现折叠提示。
	if full := strings.Join(buildTable(rows, total, "requests", 200, 0, testNow), "\n"); strings.Contains(full, "另有") {
		t.Errorf("不限行数时不应折叠:\n%s", full)
	}
}

// TestFrameFitsWithinLayout 端到端：给定 layout，帧必然放得进去。
func TestFrameFitsWithinLayout(t *testing.T) {
	rows, total := multiRows()
	st := &statsResponse{
		Enabled: true, UptimeSec: 3600, Since: "2026-09-14T20:43:52+08:00",
		Total: total, Models: rows,
	}
	for _, lay := range []layout{
		{width: 200, height: 50}, {width: 120, height: 50}, {width: 100, height: 50},
		{width: 86, height: 40}, {width: 70, height: 30}, {width: 50, height: 20},
		{width: 40, height: 12}, {width: 34, height: 10},
	} {
		frame := buildFrame(st, "requests", nil, lay)
		if !frameFits(frame, lay) {
			t.Errorf("layout %+v 下帧放不下（最宽 %d / %d 行）:\n%s",
				lay, maxFrameWidth(frame), len(frame), strings.Join(frame, "\n"))
		}
	}
}

// TestFrameFitsDetectsOverflow frameFits 必须能识别超宽/超高 ——
// watch 依赖它决定是否回落滚动输出（检测不到就会继续错位堆叠）。
func TestFrameFitsDetectsOverflow(t *testing.T) {
	cases := []struct {
		name  string
		frame []string
		lay   layout
		want  bool
	}{
		{"恰好放下", []string{"abc"}, layout{width: 3, height: 1}, true},
		{"宽一列", []string{"abcd"}, layout{width: 3, height: 1}, false},
		{"高一行", []string{"a", "b"}, layout{width: 3, height: 1}, false},
		{"宽不限", []string{"abcdefgh"}, layout{height: 1}, true},
		{"高不限", []string{"a", "b", "c"}, layout{width: 3}, true},
		{"都限且都放得下", []string{"ab", "cd"}, layout{width: 2, height: 2}, true},
	}
	for _, c := range cases {
		if got := frameFits(c.frame, c.lay); got != c.want {
			t.Errorf("%s: frameFits = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFrameNarrowerThanMinimumStillHonest 窄到放不下最简表时，帧仍应保持内容
// 完整可读（此时 watch 会回落滚动输出，不再尝试原地覆盖）。
func TestFrameNarrowerThanMinimumStillHonest(t *testing.T) {
	st := &statsResponse{Enabled: true, UptimeSec: 60,
		Total:  mkModel("(all)", 10, 0),
		Models: []modelStat{mkModel("m", 10, 0)}}
	lay := layout{width: 20, height: 40}
	frame := buildFrame(st, "requests", nil, lay)
	joined := strings.Join(frame, "\n")
	for _, must := range []string{"网关请求统计", "模型", "请求"} {
		if !strings.Contains(joined, must) {
			t.Errorf("窄窗口下丢了关键内容 %q:\n%s", must, joined)
		}
	}
	// 单行也不应被裁到远超最低固有宽度（约 28 列）。
	if w := maxFrameWidth(frame); w > 30 {
		t.Errorf("最宽行 %d 远超最低固有宽度（说明裁剪未尽力）:\n%s", w, joined)
	}
}

// TestFitLineTruncates 非表格行（标题/尾注）同样受宽度约束 ——
// 它们超宽一样会折行并破坏光标上移量。
func TestFitLineTruncates(t *testing.T) {
	long := "📈 网关请求统计 · 窗口 23h39m（自 09-14 20:43）这是一段很长的补充说明文字"
	got := fitLine(long, 20)
	if displayWidth(got) > 20 {
		t.Errorf("fitLine 超宽: %d 列, %q", displayWidth(got), got)
	}
	if !strings.Contains(got, "网关请求统计") {
		t.Errorf("截断应保留开头信息，得到 %q", got)
	}
	if got := fitLine(long, 0); got != long {
		t.Errorf("width=0 应原样返回，得到 %q", got)
	}
}

// TestRewriteFrameNoOverlap overlap=false 表示上一帧是滚动输出而非覆盖绘制，
// 此时不得上移光标 —— 否则会吃掉与该帧无关的既有输出。
func TestRewriteFrameNoOverlap(t *testing.T) {
	got := captureStdout(t, func() { rewriteFrame([]string{"a"}, 5, false) })
	if strings.Contains(got, "[5A") {
		t.Errorf("overlap=false 时不应上移光标，得到 %q", got)
	}
	if !strings.Contains(got, "a") {
		t.Errorf("内容仍应写出，得到 %q", got)
	}

	// overlap=true 时行为不变（回归保护）。
	got = captureStdout(t, func() { rewriteFrame([]string{"a"}, 5, true) })
	if !strings.HasPrefix(got, "\033[5A") {
		t.Errorf("overlap=true 应上移 5 行，得到 %q", got)
	}
}

// TestUsableWidth 排版宽度 = 终端列数 - 右边缘留白（规避末列待折行）。
func TestUsableWidth(t *testing.T) {
	if got := usableWidth(0); got != 0 {
		t.Errorf("usableWidth(0) = %d，0 表示不限应原样返回", got)
	}
	if got := usableWidth(80); got != 79 {
		t.Errorf("usableWidth(80) = %d, want 79", got)
	}
	if got := usableWidth(1); got != 1 {
		t.Errorf("usableWidth(1) = %d，应保底 1 不为负", got)
	}
}

// TestResolveLayoutPinsExplicit 显式 -width/-height 不被终端探测覆盖。
func TestResolveLayoutPinsExplicit(t *testing.T) {
	lay := resolveLayout(123, 45)
	if !lay.pinW || !lay.pinH {
		t.Errorf("显式指定的维度应标记为 pinned: %+v", lay)
	}
	if lay.width != 123 || lay.height != 45 {
		t.Errorf("显式值被改写: %+v", lay)
	}
	// 只指定宽度：宽度保持 pinned，高度交由探测。
	lay = resolveLayout(100, 0)
	if !lay.pinW || lay.pinH {
		t.Errorf("仅 -width 时 pinH 应为 false: %+v", lay)
	}
	if lay.width != 100 {
		t.Errorf("显式宽度被改写: %+v", lay)
	}
}

// TestRefreshLayoutRespectsPins 重新探测时只更新自动维度；探测失败不应清零。
func TestRefreshLayoutRespectsPins(t *testing.T) {
	lay := layout{width: 123, height: 45, pinW: true, pinH: true}
	if got := refreshLayout(lay); got != lay {
		t.Errorf("全 pinned 时不应变化: %+v → %+v", lay, got)
	}
	lay = layout{width: 80, height: 24}
	if got := refreshLayout(lay); got.width == 0 {
		t.Errorf("探测失败不应把宽度清零: %+v", got)
	}
}
