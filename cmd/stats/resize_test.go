package main

// resize_test.go —— 刷新过程中**改变窗口尺寸**时的显示正确性。
//
// 背景（用户实测）：`-watch` 运行中把命令行窗口缩小，屏幕会变成一堆半截的表格，
// "根本没法直观查看"。原因不是排版宽度算错，而是**覆盖重绘的记账失效**：
//
//	rewriteFrame 靠相对上移（\033[NA）回到帧首，前提是"上一帧在当前窗口下的
//	视觉行数 == len(frame)"。窗口一缩，旧帧内容在新宽度下会重新折行，视觉行数
//	暴涨，上移 N 行根本回不到帧首 —— 旧帧残留 + 新帧叠上，刷一次乱一分。
//
// 本文件用屏幕模型复现该场景（含终端重排语义），锁死"缩放后必须重建基线"。

import (
	"strings"
	"testing"
)

// mkStats 造一份用于缩放测试的多模型统计。
func mkStats() *statsResponse {
	return &statsResponse{
		Enabled: true, UptimeSec: 86400, Since: "2026-09-14T20:43:52+08:00",
		Total: mkModel("(all)", 1474, 9),
		Models: []modelStat{
			mkModel("deepseek-v4.1-flash", 1387, 3),
			mkModel("cn:deepseek-v4.1-flash", 29, 4),
			mkModel("cn:hy4-preview", 24, 0),
			mkModel("deepseek-v4-pro", 13, 0),
			mkModel("cn:glm-5.3-flash", 9, 0),
			mkModel("cn:glm-5.3", 8, 0),
			mkModel("cn:hy3-x", 4, 2),
		},
	}
}

// trimFrame 去掉帧各行的尾随空格（屏幕模型取回的内容也不含尾随空格）。
func trimFrame(frame []string) []string {
	out := make([]string, len(frame))
	for i, l := range frame {
		out[i] = strings.TrimRight(l, " ")
	}
	return out
}

// TestResizeNarrowDoesNotStack 刷新中途**缩小**窗口后，屏幕仍应恰好是一帧。
//
// 这是用户实测缺陷的直接回归断言（缩小命令行窗口 → 满屏半截表格）。
// 断言方式与 TestWatchDoesNotStack 一致：屏幕内容必须严格等于**按新宽度排版的
// 那一帧**，多一行少一行都算失败 —— 直接对应"没法看"这个观感。
func TestResizeNarrowDoesNotStack(t *testing.T) {
	st := mkStats()
	wide := layout{width: 129, height: 40}
	narrow := layout{width: 85, height: 40}

	sc := newScreen(wide.width)

	// 第一帧：按宽窗口排版。
	f1 := buildFrame(st, "requests", nil, wide)
	sc.write(captureStdout(t, func() { rewriteFrame(f1, 0, false) }))
	// 再刷两帧，确认稳态无堆叠（此时还没有尺寸变化）。
	for i := 1; i < 3; i++ {
		sc.write(captureStdout(t, func() { rewriteFrame(f1, len(f1), true) }))
	}
	if got := sc.nonEmpty(); !sameLines(got, trimFrame(f1)) {
		t.Fatalf("缩放前就已堆叠，用例前提不成立:\n%s", strings.Join(got, "\n"))
	}

	// —— 缩小窗口 ——

	// 终端先把已画内容按新宽度重排（这正是"视觉行数 ≠ len(frame)"的来源）。
	sc.resize(narrow.width)

	// 下一轮：探测到新尺寸 → 必须清屏重建基线，再按新宽度画。
	f2 := buildFrame(st, "requests", nil, narrow)
	emitResizeFrame(t, sc, f2)

	got := sc.nonEmpty()
	if want := trimFrame(f2); !sameLines(got, want) {
		t.Errorf("缩窗后屏幕 ≠ 一帧（%d 行 vs %d 行）—— 旧帧残留/新帧叠上:\n"+
			"--- 屏幕 ---\n%s\n--- 期望 ---\n%s",
			len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestResizeWidenDoesNotStack 反向：放大窗口同样不得堆叠。
func TestResizeWidenDoesNotStack(t *testing.T) {
	st := mkStats()
	narrow := layout{width: 85, height: 40}
	wide := layout{width: 129, height: 40}

	sc := newScreen(narrow.width)
	f1 := buildFrame(st, "requests", nil, narrow)
	sc.write(captureStdout(t, func() { rewriteFrame(f1, 0, false) }))

	sc.resize(wide.width)
	f2 := buildFrame(st, "requests", nil, wide)
	emitResizeFrame(t, sc, f2)

	got := sc.nonEmpty()
	if want := trimFrame(f2); !sameLines(got, want) {
		t.Errorf("放大后屏幕 ≠ 一帧（%d 行 vs %d 行）:\n--- 屏幕 ---\n%s\n--- 期望 ---\n%s",
			len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestResizeOscillationStaysClean 反复缩放（拖分屏来回拉）后仍应只剩一帧。
//
// 拖动分屏是连续动作，会触发一串尺寸变化。若每次变化都留下残渣，几下之后就
// 没法看了 —— 用户截图正是这个状态。
func TestResizeOscillationStaysClean(t *testing.T) {
	st := mkStats()
	sizes := []int{129, 85, 120, 70, 110, 95, 129}

	sc := newScreen(sizes[0])
	for i, w := range sizes {
		lay := layout{width: w, height: 40}
		frame := buildFrame(st, "requests", nil, lay)
		if i > 0 {
			sc.resize(w)
		}
		if i == 0 {
			sc.write(captureStdout(t, func() { rewriteFrame(frame, 0, false) }))
		} else {
			emitResizeFrame(t, sc, frame)
		}

		got := sc.nonEmpty()
		if want := trimFrame(frame); !sameLines(got, want) {
			t.Fatalf("第 %d 次缩放到 %d 列后屏幕 ≠ 一帧（%d 行 vs %d 行）:\n"+
				"--- 屏幕 ---\n%s\n--- 期望 ---\n%s",
				i, w, len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// TestResizeWithoutResetWouldStack 反证：若尺寸变化后**不做**清屏重建（即修复前的
// 行为），屏幕模型确实会表现出堆叠 —— 证明上面几条断言不是因模型太宽松才通过。
//
// 这里刻意沿用"相对上移 + 直接叠画"的老路径，断言屏幕上出现多份标题。
func TestResizeWithoutResetWouldStack(t *testing.T) {
	st := mkStats()
	wide := layout{width: 129, height: 40}
	narrow := layout{width: 85, height: 40}

	sc := newScreen(wide.width)
	f1 := buildFrame(st, "requests", nil, wide)
	sc.write(captureStdout(t, func() { rewriteFrame(f1, 0, false) }))

	// 缩小窗口 → 终端重排 → 老逻辑仍按 len(f1) 上移（未清屏）。
	sc.resize(narrow.width)
	f2 := buildFrame(st, "requests", nil, narrow)
	sc.write(captureStdout(t, func() { rewriteFrame(f2, len(f1), true) }))

	got := sc.nonEmpty()
	n := 0
	for _, l := range got {
		if strings.Contains(l, "网关请求统计") {
			n++
		}
	}
	if n < 2 {
		t.Errorf("缩窗后沿老路径绘制应出现标题堆叠（模型需对此敏感），实际 %d 次:\n%s",
			n, strings.Join(got, "\n"))
	}
}

// TestClearScreenResetsBaseline 清屏后光标归位、内容清空 —— 重建基线的语义。
//
// 单独拆两个断言（内容 / 光标）是有意的：\033[2J 只擦内容、不移动光标，
// 归位靠 \033[H。若 clearScreen 漏掉 \033[H，内容看着是空的，但下一帧会从
// 屏幕中部画起 —— 仍是一屏错位。这种缺陷只有光标断言能拦住。
func TestClearScreenResetsBaseline(t *testing.T) {
	sc := newScreen(60)
	sc.write("hello\r\nworld\r\nthird")
	if len(sc.nonEmpty()) != 3 {
		t.Fatalf("前置条件不成立：应有 3 行，得到 %d", len(sc.nonEmpty()))
	}
	// 先把光标留在非顶行，确保"归位"这件事真的被验证到。
	if sc.row == 0 {
		t.Fatalf("前置条件不成立：光标应在非顶行，实际 row=%d", sc.row)
	}

	sc.write(captureStdout(t, func() { clearScreen() }))

	if got := sc.nonEmpty(); len(got) != 0 {
		t.Errorf("清屏后不应有任何内容，得到 %v", got)
	}
	if sc.row != 0 || sc.col != 0 {
		t.Errorf("清屏后光标应在左上角（需 \\033[H 归位），实际 row=%d col=%d", sc.row, sc.col)
	}
}

// TestClearScreenEscapeSequence 断言 clearScreen 发出的序列**不污染滚动缓冲**。
//
// 这是"调整窗口大小后 scrollback 里叠一堆表"的直接回归断言。
//
// 背景：Windows conhost 把 ED(2)（\033[2J）实现成"把当前屏幕滚入滚动缓冲"
// （即 cls 的行为）。而 clearScreen 每次尺寸变化都会被调用 —— 于是用户每调整
// 一次窗口，历史里就多一份完整表格（实测 4 次缩放 = 4 份表）。
//
// 故这里断言：**绝不允许出现 \033[2J**，且必须归位（\033[H），否则清屏后
// 会从屏幕中部画起。用 ED(0)（\033[J）擦光标之后的内容即可 —— 它不产生新历史。
func TestClearScreenEscapeSequence(t *testing.T) {
	got := captureStdout(t, func() { clearScreen() })

	if strings.Contains(got, "\033[2J") {
		t.Errorf("clearScreen 不得使用 ED(2) \\033[2J —— conhost 会把它实现为滚屏，\n"+
			"每调整一次窗口就往 scrollback 里留一份表格。得到 %q", got)
	}
	if !strings.Contains(got, "\033[J") {
		t.Errorf("clearScreen 应发出 ED(0) \\033[J 擦除光标之后的内容，得到 %q", got)
	}
	if !strings.Contains(got, "\033[H") {
		t.Errorf("clearScreen 应发出光标归位 \\033[H（\\033[J 不移动光标），得到 %q", got)
	}
}

// ─── 辅助 ────────────────────────────────────────────────────────────────

// emitResizeFrame 模拟 runWatch 在"检测到尺寸变化"时的那一步：
// 清屏重建基线，再以 prevLines=0 / overlap=false 绘制新帧。
func emitResizeFrame(t *testing.T, sc *screen, frame []string) {
	t.Helper()
	sc.write(captureStdout(t, func() { clearScreen() }))
	sc.write(captureStdout(t, func() { rewriteFrame(frame, 0, false) }))
}
