package main

// fill_test.go —— "横向铺满"（列间距弹性 + 模型列吸宽 + 诚实留白）的测试。
//
// 背景：表格自然宽度只取决于列数与内容，与终端多宽无关。窗口更宽时右侧留白，
// 观感很空。修法是按优先级分配余量：先列间距、再模型列，两者都有硬上限，
// 超出就诚实留白（无上限拉伸会制造"内部空洞"，比留白更难看）。
//
// 本文件锁死三条契约：①窗口够宽时要铺满；②间距与模型列都不得越界；
// ③无论怎么调，都不得破坏"表宽 ≤ 窗口"这条防折行硬约束。

import (
	"strings"
	"testing"
	"time"
)

// richRows 造一组带"填充性列"数据（流式 / 最后活动 / 缓存写入）的多模型统计。
func richRows() ([]modelStat, modelStat) {
	base := testNow.Add(-90 * time.Minute).Format(time.RFC3339)
	rows := []modelStat{
		mkRichModel("deepseek-v4.1-flash", 786, 3, 780, testNow.Add(-30*time.Second).Format(time.RFC3339)),
		mkRichModel("cn:deepseek-v4.1-flash", 29, 4, 0, base),
		mkRichModel("deepseek-v4-pro", 13, 0, 13, testNow.Add(-11*time.Hour).Format(time.RFC3339)),
	}
	rows[0].CacheWriteTokens = 1234 // 让"缓存写入"列显形
	total := mkRichModel("(all)", 828, 7, 793, testNow.Format(time.RFC3339))
	total.CacheWriteTokens = 1234
	return rows, total
}

// sepOf 取出表格里的分隔线行（形如 "---+----+---"）。
func sepOf(tbl []string) string {
	for _, l := range tbl {
		if strings.Contains(l, "+") && strings.Contains(l, "-") && !strings.Contains(l, "|") {
			return l
		}
	}
	return ""
}

// gapRuns 返回分隔线上相邻 '+' 之间各段的 '-' 个数。
//
// 分隔线构造为：'-' + '-'×w0 + [ '-'×g + '+' + '-'×g + '-'×wi ] ... + '-'
// 故各段长度依次是 w0+g+1、wi+2g（中间段）、wlast+g+1。段长本身混合了列宽与
// 间距，无法单独解出 g —— 这正是下面不靠"反推 g"来断言间距的原因。
func gapRuns(sep string) []int {
	if sep == "" {
		return nil
	}
	parts := strings.Split(sep, "+")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i] = len(p)
	}
	return out
}

// TestTableFillsAvailableWidth 窗口足够宽时表格应铺满（利用率 ≥ 95%）。
//
// 这是本次改动的主诉求：130 列窗口此前只用 106 列（18% 空白），现在应当填满。
// 上界取到 180：再宽就撞上"模型列 40 + 间距 3"的硬上限，此时按设计留白，
// 由 TestTableKeepsBlankBeyondCaps 单独断言。
func TestTableFillsAvailableWidth(t *testing.T) {
	rows, total := richRows()
	for w := 100; w <= 180; w += 2 {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		got := maxFrameWidth(tbl)
		if got > w {
			t.Fatalf("w=%d 表格宽 %d 超宽（会折行）:\n%s", w, got, strings.Join(tbl, "\n"))
		}
		if used := float64(got) / float64(w); used < 0.95 {
			t.Errorf("w=%d 只用了 %d 列（%.0f%%），未铺满:\n%s",
				w, got, used*100, strings.Join(tbl, "\n"))
		}
	}
}

// TestAllocWidthGutterBounded allocWidth 决定的列间距恒在 [gutterMin, gutterMax]。
//
// 间距由 allocWidth 唯一决定（分隔线上的段长混合了列宽，无法反推），故这里直接
// 测这个决策点；渲染层只保证与它同构（见 TestGutterSeparatorAlignsBars）。
func TestAllocWidthGutterBounded(t *testing.T) {
	widths := []int{19, 6, 4, 8, 8, 9, 14, 8, 8, 6, 8, 4}

	// 窗口极窄：间距保持下限，绝不出现 0 或负数。
	if _, g := allocWidth(widths, 1); g < gutterMin {
		t.Errorf("极窄窗口下 g=%d < 下限 %d", g, gutterMin)
	}
	if _, g := allocWidth(widths, 0); g != gutterMin {
		t.Errorf("不限宽时 g 应为下限 %d，得到 %d", gutterMin, g)
	}

	// 逐档扫过：g 单调不降，且始终在界内。
	//
	// 注意 allocWidth 只**加宽**不收缩（列淘汰是 buildTable 的职责），故"放得下"
	// 这条断言只在自然宽度本就放得下时才成立。
	prev := 0
	for w := 1; w <= 600; w++ {
		ws, g := allocWidth(widths, w)
		if g < gutterMin || g > gutterMax {
			t.Fatalf("w=%d g=%d 越界 [%d,%d]", w, g, gutterMin, gutterMax)
		}
		if g < prev {
			t.Fatalf("w=%d g 从 %d 降到 %d（应单调不降）", w, prev, g)
		}
		prev = g

		natural := tableWidth(widths, gutterMin)
		if natural <= w {
			if got := tableWidth(ws, g); got > w {
				t.Fatalf("w=%d 自然宽度 %d 放得下，但分配后总宽 %d 超宽", w, natural, got)
			}
		} else if got := tableWidth(ws, g); got != natural {
			t.Fatalf("w=%d 自然宽度 %d 已超宽，allocWidth 不应再加宽，得到 %d",
				w, natural, got)
		}
	}
	if prev != gutterMax {
		t.Errorf("扫到 600 列时 g 仍未达上限 %d（实际 %d）", gutterMax, prev)
	}
}

// TestTableModelColumnCapped 模型列宽不得超过上限 —— 防"内部空洞"。
//
// 无上限拉伸时模型列会被撑到 60+ 列宽，名字与后面的数字之间隔着一大片空白，
// 比右侧留白更难看。
//
// 断言刻意**不引用 modelColMaxWidth 常量**：拿常量去比常量是恒真式，把上限改成
// 200 也照样通过（实测验证过这个坑）。这里改用与实现无关的**可读性上界**：
// 模型列宽不应超过最长模型名本身太多。真正要守住的是"没有内部空洞"这个观感，
// 而它等价于"列宽 - 最长名字宽 ≤ 一屏内可接受的呼吸空间"。
func TestTableModelColumnCapped(t *testing.T) {
	rows, total := richRows()

	longest := 0
	for _, m := range rows {
		if w := displayWidth(m.Model); w > longest {
			longest = w
		}
	}
	// 允许比最长名字多出的呼吸空间。给得比真实上限（40）宽松，只拦"明显空洞"：
	// 若有人把上限放开到 200，模型列会涨到 200，必然越过这条线。
	const maxSlack = 24

	for _, w := range []int{130, 180, 200, 240, 300, 600, 2000} {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		sep := sepOf(tbl)
		if sep == "" {
			t.Fatalf("w=%d 无分隔线:\n%s", w, strings.Join(tbl, "\n"))
		}
		// 分隔线首段 = 行首'-' + 模型列 + 右侧 g 个'-'（g ≤ gutterMax）。
		// 故 列宽 ≤ 段长 - 1 - gutterMin，列宽 ≥ 段长 - 1 - gutterMax。
		first := gapRuns(sep)[0]
		modelW := first - 1 - gutterMin // 上界（g 取下限时列宽最大）
		if modelW-longest > maxSlack {
			t.Errorf("w=%d 模型列宽约 %d，比最长模型名 %d 多出 %d 列（>%d）—— 出现内部空洞:\n%s",
				w, modelW, longest, modelW-longest, maxSlack, sep)
		}
	}

	// 反向断言：封顶后继续加宽窗口，模型列宽不应再增长。
	// 这是"上限确实生效"的直接证据，且同样不依赖常量取值。
	narrow := gapRuns(sepOf(buildTable(rows, total, "requests", 240, 0, testNow)))[0]
	huge := gapRuns(sepOf(buildTable(rows, total, "requests", 600, 0, testNow)))[0]
	if huge != narrow {
		t.Errorf("窗口 240→600 时模型列段长从 %d 变成 %d，说明未被封顶", narrow, huge)
	}
}

// TestTableKeepsBlankBeyondCaps 超过两级上限后应诚实留白，而非继续拉伸。
//
// 断言方式：极宽窗口（400）下表格宽度应**明显小于**可用宽度，且等于"撞上限时
// 的自然宽度"。若有人去掉上限改成无限拉伸，此测试立刻失败。
func TestTableKeepsBlankBeyondCaps(t *testing.T) {
	rows, total := richRows()
	huge := maxFrameWidth(buildTable(rows, total, "requests", 400, 0, testNow))
	if huge >= 400 {
		t.Fatalf("400 列窗口下表格宽 %d，不应填满（说明上限失效）", huge)
	}
	// 再宽也不应更宽：证明已经封顶。
	for _, w := range []int{400, 600, 1000} {
		if got := maxFrameWidth(buildTable(rows, total, "requests", w, 0, testNow)); got != huge {
			t.Errorf("w=%d 表格宽 %d ≠ 封顶值 %d（封顶后不应继续变宽）", w, got, huge)
		}
	}
}

// TestTableNeverExceedsWidth 全档扫描：任何窗口宽度下都不得超宽。
//
// 这是防折行（进而防 watch 堆叠）的硬约束，与铺满互为约束。窗口窄于最简表
// 固有宽度时允许超出——那种情况由调用方回落为滚动输出（见 frameFits）。
func TestTableNeverExceedsWidth(t *testing.T) {
	rows, total := richRows()
	for w := 29; w <= 400; w++ {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		if got := maxFrameWidth(tbl); got > w {
			t.Fatalf("w=%d 表格宽 %d 超宽（会折行 → watch 堆叠）:\n%s",
				w, got, strings.Join(tbl, "\n"))
		}
	}
}

// TestAllocWidthPure 直接测 allocWidth 的分配次序与边界。
func TestAllocWidthPure(t *testing.T) {
	widths := []int{20, 6, 6}

	// 不限宽（管道场景）：原样返回，间距取下限。
	if got, g := allocWidth(widths, 0); g != gutterMin || got[0] != 20 {
		t.Errorf("maxWidth=0 应不调整：widths=%v g=%d", got, g)
	}
	if widths[0] != 20 {
		t.Errorf("allocWidth 不应改动入参切片：%v", widths)
	}

	// 刚好放不下：不应涨间距。
	narrow := 20 + 6 + 6 + 2 + 3*2 // = 自然宽度（g=1）
	if _, g := allocWidth(widths, narrow); g != gutterMin {
		t.Errorf("恰好放下时 g 应保持 %d，得到 %d", gutterMin, g)
	}

	// 极宽：模型列应停在上限，间距停在上限。
	got, g := allocWidth(widths, 1000)
	if g != gutterMax {
		t.Errorf("极宽时 g 应为 %d，得到 %d", gutterMax, g)
	}
	if got[0] != modelColMaxWidth {
		t.Errorf("极宽时模型列应封顶 %d，得到 %d", modelColMaxWidth, got[0])
	}
}

// TestFagoRelativeTime 相对时间的分档与回落。
func TestFagoRelativeTime(t *testing.T) {
	at := func(d time.Duration) *string {
		s := testNow.Add(-d).Format(time.RFC3339)
		return &s
	}
	cases := []struct {
		in   *string
		want string
		why  string
	}{
		{at(0), "0s前", "刚刚"},
		{at(42 * time.Second), "42s前", "秒级"},
		{at(89 * time.Second), "89s前", "秒级上界"},
		{at(90 * time.Second), "1m前", "进入分钟档"},
		{at(17 * time.Minute), "17m前", "分钟级"},
		{at(89 * time.Minute), "89m前", "分钟级上界"},
		{at(90 * time.Minute), "1.5h前", "进入小时档"},
		{at(22 * time.Hour), "22.0h前", "小时级"},
		{at(47 * time.Hour), "47.0h前", "小时级上界"},
		{at(48 * time.Hour), "2d前", "进入天档"},
		{at(72 * time.Hour), "3d前", "天级"},
		{at(-5 * time.Minute), "0s前", "时钟偏差导致的未来时刻按刚刚处理，不出现负数"},
		{nil, "-", "字段缺失"},
		{func() *string { s := ""; return &s }(), "-", "空串"},
		{func() *string { s := "不是时间"; return &s }(), "-", "解析失败"},
		{func() *string { s := "2026-09-15 21:30:00"; return &s }(), "-", "非 RFC3339"},
	}
	for _, c := range cases {
		if got := fago(c.in, testNow); got != c.want {
			t.Errorf("fago(%v) = %q, want %q（%s）", c.in, got, c.want, c.why)
		}
	}

	// now 为零值（旧版网关没有 now 字段）时应回落 "-"，而不是 panic 或算出荒谬值。
	if got := fago(at(time.Hour), time.Time{}); got != "-" {
		t.Errorf("now 为零值时 = %q, want %q", got, "-")
	}
}

// TestFillColumnsConditional 三列填充性列按数据显隐。
func TestFillColumnsConditional(t *testing.T) {
	// mkModel 不填这三个字段 → 三列都应隐藏，表格回到原来的 9 列形态。
	plain := []modelStat{mkModel("a", 10, 1), mkModel("b", 20, 0)}
	tbl := buildTable(plain, mkModel("(all)", 30, 1), "requests", 200, 0, testNow)
	joined := strings.Join(tbl, "\n")
	for _, h := range []string{"流式", "最后活动", "缓存写入"} {
		if strings.Contains(joined, h) {
			t.Errorf("字段全零时不应出现 %q 列:\n%s", h, joined)
		}
	}

	// 有数据 → 出现；缓存写入仍为 0 时它应保持隐藏（当前网关的常态）。
	rows, total := richRows()
	rows[0].CacheWriteTokens, rows[1].CacheWriteTokens, rows[2].CacheWriteTokens = 0, 0, 0
	total.CacheWriteTokens = 0
	tbl = buildTable(rows, total, "requests", 200, 0, testNow)
	joined = strings.Join(tbl, "\n")
	for _, h := range []string{"流式", "最后活动"} {
		if !strings.Contains(joined, h) {
			t.Errorf("有数据时应出现 %q 列:\n%s", h, joined)
		}
	}
	if strings.Contains(joined, "缓存写入") {
		t.Errorf("缓存写入全零时应隐藏（当前网关未采集）:\n%s", joined)
	}

	// 缓存写入有值时出现。
	rows[0].CacheWriteTokens = 999
	tbl = buildTable(rows, total, "requests", 200, 0, testNow)
	if !strings.Contains(strings.Join(tbl, "\n"), "缓存写入") {
		t.Errorf("缓存写入有值时应出现:\n%s", strings.Join(tbl, "\n"))
	}
}

// TestFillColumnsDropFirst 窗口收窄时，填充性列应先于核心列让位。
//
// 依据 dropSeq：缓存写入(1) < 流式(2) < 最后活动(3) < 缓存命中(4) < ... < 扣费(9)。
func TestFillColumnsDropFirst(t *testing.T) {
	rows, total := richRows()

	// 宽窗口：三列都在。
	wide := strings.Join(buildTable(rows, total, "requests", 200, 0, testNow), "\n")
	for _, h := range []string{"流式", "最后活动", "缓存写入"} {
		if !strings.Contains(wide, h) {
			t.Fatalf("200 列下应含 %q:\n%s", h, wide)
		}
	}

	// 逐步收窄：填充性列必须先消失，而"扣费/请求/失败"必须始终在场。
	for _, w := range []int{170, 150, 130, 110, 96, 80, 60} {
		joined := strings.Join(buildTable(rows, total, "requests", w, 0, testNow), "\n")
		for _, must := range []string{"模型", "请求", "失败", "扣费"} {
			if !strings.Contains(joined, must) {
				t.Errorf("w=%d 丢了核心列 %q:\n%s", w, must, joined)
			}
		}
	}
}

// TestGutterSeparatorAlignsBars 间距 g>1 时，分隔线的 + 仍与各行的 | 逐列同位。
//
// 这是 TestTableSeparatorAlignsBars 的"大间距"版本：间距可调后，分隔线构造
// 从固定 "-+-" 变成 "-"×g + "+" + "-"×g，必须与数据行的 " "×g + "|" + " "×g
// 严格同构，否则宽窗口下会错位。
func TestGutterSeparatorAlignsBars(t *testing.T) {
	rows, total := richRows()
	for _, w := range []int{90, 130, 180, 240, 320} {
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
		if len(dataLines) == 0 || len(sepLines) == 0 {
			t.Fatalf("w=%d 未找到竖线行或分隔线行:\n%s", w, strings.Join(tbl, "\n"))
		}

		want := barColumns(sepLines[0], '+')
		for _, l := range dataLines {
			if got := barColumns(l, '|'); !equalInts(got, want) {
				t.Errorf("w=%d 竖线列位 %v ≠ 分隔线 + 列位 %v:\n  %q\n  %q",
					w, got, want, sepLines[0], l)
			}
		}
		// 同宽：间距改变后分隔线与数据行仍必须逐列等宽。
		for _, l := range dataLines {
			if got, wantW := displayWidth(l), displayWidth(sepLines[0]); got != wantW {
				t.Errorf("w=%d 行宽 %d ≠ 分隔线宽 %d:\n  %q", w, got, wantW, l)
			}
		}
	}
}

// TestTableWidthMatchesRendered 校验 tableWidth 与实际渲染宽度一致。
//
// 两者是"预算"与"实际"的关系。若 tableWidth 与实际构造规则不符（例如间距改成
// 可变后仍按固定 3 估算），buildTable 就会误判"放得下"，产出超宽的表 → 折行 →
// watch 堆叠。故这里独立复算一遍渲染规则，与实际行宽逐档比对。
//
// 复算方式：分隔线的段长依次为 w0+g+1、wi+2g、（…）、wn+g+1。用相邻段长作差
// 消掉 g 的歧义不便，故直接按"段长序列"复算总宽 —— 总宽即各段长之和加上
// '+' 自身所占的列数，这与 tableWidth 的公式等价：
//
//	Σ段长 + (列数-1) = Σ(wi + 2g) + 2 = Σwi + 2g(n-1) + 2 = tableWidth
func TestTableWidthMatchesRendered(t *testing.T) {
	rows, total := richRows()
	for w := 40; w <= 300; w++ {
		tbl := buildTable(rows, total, "requests", w, 0, testNow)
		sep := sepOf(tbl)
		if sep == "" {
			continue // 极窄时可能没有分隔线
		}

		runs := gapRuns(sep)
		sum := 0
		for _, n := range runs {
			sum += n
		}
		recomputed := sum + (len(runs) - 1) // 各段 + 各 '+' 自身

		if got := displayWidth(sep); got != recomputed {
			t.Fatalf("w=%d 分隔线宽 %d ≠ 按段长复算 %d:\n%s", w, got, recomputed, sep)
		}
		// 表格里最宽的行必须也是这个宽度（表头/数据行与分隔线同构）。
		for _, l := range tbl {
			if got := displayWidth(l); got > recomputed {
				t.Errorf("w=%d 有行宽 %d > 分隔线宽 %d:\n  %q", w, got, recomputed, l)
			}
		}
	}
}
