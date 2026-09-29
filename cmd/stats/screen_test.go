package main

// screen_test.go —— 用一个极简终端屏幕模型回放 rewriteFrame 的真实输出，
// 直接验证"刷新 N 次后屏幕上只剩一份表"。
//
// 为什么值得单独建模型：这个 bug 的本质是**视觉层**的（折行导致光标上移失准、
// 旧帧残留），单纯断言转义序列的字节形态（如 TestRewriteFrameEscapes）无法
// 覆盖它 —— 那些断言在 bug 存在时同样是绿的。只有在屏幕模型上回放，
// 才能把"堆叠"这件事本身变成可断言的事实。

import (
	"strconv"
	"strings"
	"testing"
)

// screen 是极简终端模型：按"单元格网格"存储（每格一个 rune + 其显示宽度），
// 支持本工具用到的转义序列。
//
// 用网格而非字符串拼接：覆盖重绘会把光标移回已有内容之上重写，字符串拼接式
// 的模型无法表达"覆盖"（会变成追加），那样就测不出堆叠也无法验证修复。
//
// 只实现被回放到的序列（光标上移/下移、回车、换行、擦到行尾、擦到屏尾、
// 光标显隐）；未识别的序列直接丢弃。
type screen struct {
	cells    [][]rune // 每行的 rune 序列
	widths   [][]int  // 与 cells 对应的显示宽度
	row, col int
	// pendingWrap 模拟"写在末列后终端置位待折行"：置位时再来一个可打印字符
	// 才真正换行。conhost 与 xterm 都如此，是本 bug 的成因之一。
	pendingWrap bool
	width       int
}

func newScreen(width int) *screen { return &screen{width: width} }

// ensureRow 保证第 r 行存在。
func (s *screen) ensureRow(r int) {
	for len(s.cells) <= r {
		s.cells = append(s.cells, nil)
		s.widths = append(s.widths, nil)
	}
}

// blankTo 把第 row 行补齐到 col 列（终端把跳过的位置渲染为空格）。
func (s *screen) blankTo() {
	s.ensureRow(s.row)
	for len(s.cells[s.row]) < s.col {
		s.cells[s.row] = append(s.cells[s.row], ' ')
		s.widths[s.row] = append(s.widths[s.row], 1)
	}
}

// put 在光标处写入一个显示宽度为 w 的字符（覆盖该位置的既有内容）。
func (s *screen) put(r rune, w int) {
	if s.pendingWrap {
		s.row++
		s.col = 0
		s.pendingWrap = false
	}
	s.blankTo()
	// 覆盖：从 col 起替换 w 个单元格（宽字符占两格，后一格置为占位）。
	line, wl := s.cells[s.row], s.widths[s.row]
	for i := 0; i < w; i++ {
		if s.col+i < len(line) {
			line[s.col+i] = ' '
			wl[s.col+i] = 1
		}
	}
	if s.col >= len(line) {
		line = append(line, make([]rune, s.col-len(line)+1)...)
		wl = append(wl, make([]int, s.col-len(wl)+1)...)
		for i := range wl {
			if wl[i] == 0 {
				wl[i] = 1
			}
		}
	}
	if s.col >= len(line) {
		line = append(line, r)
		wl = append(wl, w)
	} else {
		line[s.col] = r
		wl[s.col] = w
	}
	s.cells[s.row], s.widths[s.row] = line, wl
	s.col += w
	if s.width > 0 && s.col >= s.width {
		s.col = s.width
		s.pendingWrap = true
	}
}

// eraseToEnd 擦除当前行从 col 到行尾的内容。
func (s *screen) eraseToEnd() {
	s.ensureRow(s.row)
	if s.col < len(s.cells[s.row]) {
		s.cells[s.row] = s.cells[s.row][:s.col]
		s.widths[s.row] = s.widths[s.row][:s.col]
	}
	s.pendingWrap = false
}

// eraseToScreenEnd 擦除从光标处到屏幕末尾的所有内容（对应 \033[J）。
func (s *screen) eraseToScreenEnd() {
	s.eraseToEnd()
	if s.row+1 < len(s.cells) {
		s.cells = s.cells[:s.row+1]
		s.widths = s.widths[:s.row+1]
	}
}

// eraseAll 清空整屏（对应 \033[2J）—— 与光标位置无关。
func (s *screen) eraseAll() {
	s.cells = nil
	s.widths = nil
	s.pendingWrap = false
}

// home 把光标移到左上角（对应 \033[H）。
func (s *screen) home() {
	s.row, s.col = 0, 0
	s.pendingWrap = false
}

// resize 改变模型窗口宽度，并**模拟真实终端的重排**：已画出的内容会按新宽度
// 重新折行（这正是缩小窗口后旧帧"视觉行数"暴涨、相对上移失准的根源）。
//
// 若不模拟重排，屏幕模型就测不出这个 bug —— 这也是它此前漏判的原因。
//
// 关键细节：重排后**光标停在内容的末尾**（而不是回到左上角）。真实终端里光标
// 是跟着文本走的：窗口缩窄使文本折行变长后，光标仍在最后一行。若这里把光标
// 归零，`\033[NA` 会因越界被钳到顶行，于是"上移不到位"被掩盖成"恰好重画"，
// 反证用例就会假通过（实测踩过这个坑）。
func (s *screen) resize(width int) {
	if width == s.width || width <= 0 {
		s.width = width
		return
	}
	// 把现有内容按逻辑行取出，再按新宽度硬折行重建。
	var logical []string
	for i := range s.cells {
		logical = append(logical, s.line(i))
	}
	s.width = width
	s.cells, s.widths = nil, nil
	s.row, s.col, s.pendingWrap = 0, 0, false
	for _, l := range logical {
		if l == "" {
			s.row++
			s.col = 0
			continue
		}
		col := 0
		for _, r := range l {
			w := runeWidth(r)
			if col+w > width { // 折行：终端把放不下的部分挪到下一行
				s.row++
				s.col = 0
				col = 0
			}
			s.col = col
			s.put(r, w)
			col += w
		}
		s.row++
		s.col = 0
	}
	// 光标留在内容末尾（最后一行行首），与真实终端一致。
	s.col = 0
	s.pendingWrap = false
}

// write 回放一段带转义序列的输出。
func (s *screen) write(b string) {
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			j := i + 2
			for j < len(b) && (b[j] == '?' || (b[j] >= '0' && b[j] <= '9') || b[j] == ';') {
				j++
			}
			if j >= len(b) {
				return
			}
			params, final := b[i+2:j], b[j]
			switch final {
			case 'A': // 光标上移
				n := atoiDefault(strings.TrimPrefix(params, "?"), 1)
				s.row -= n
				if s.row < 0 {
					s.row = 0
				}
				s.pendingWrap = false
			case 'B': // 光标下移
				n := atoiDefault(params, 1)
				s.row += n
				s.pendingWrap = false
			case 'H': // 光标归位（左上角）
				s.home()
			case 'J': // 擦除：参数 2 = 整屏，其余 = 光标到屏尾
				// 注意：\033[2J 只擦内容，**不移动光标**（真实终端语义）。
				// 归位要靠单独的 \033[H。若这里顺手把光标归零，就会掩盖
				// "清屏后忘了归位"这类缺陷（实测踩过这个坑）。
				if strings.TrimPrefix(params, "?") == "2" {
					s.eraseAll()
				} else {
					s.eraseToScreenEnd()
				}
			case 'K': // 擦到行尾
				s.eraseToEnd()
			case 'h', 'l': // 光标显隐 / 备用屏切换：不影响本模型的单元格内容
			}
			i = j + 1
			continue
		}
		if b[i] == '\r' {
			s.col = 0
			s.pendingWrap = false
			i++
			continue
		}
		if b[i] == '\n' {
			s.row++
			s.col = 0
			s.pendingWrap = false
			i++
			continue
		}
		r, size := decodeRune(b[i:])
		s.put(r, runeWidth(r))
		i += size
	}
}

// atoiDefault 解析十进制参数；空串或非法时返回 def。
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// decodeRune 从 b 解出首个 rune 及其字节长度（仅需处理本工具用到的 BMP 字符）。
func decodeRune(b string) (rune, int) {
	if b[0] < 0x80 {
		return rune(b[0]), 1
	}
	for _, sz := range []int{2, 3, 4} {
		if len(b) >= sz {
			rs := []rune(b[:sz])
			if len(rs) == 1 && rs[0] != 0xFFFD {
				return rs[0], sz
			}
		}
	}
	return rune(b[0]), 1
}

// line 把第 i 行还原为字符串（宽字符按 UCS-2 语义还原，占位格跳过）。
func (s *screen) line(i int) string {
	var b strings.Builder
	for j := 0; j < len(s.cells[i]); j++ {
		w := s.widths[i][j]
		if j > 0 && s.widths[i][j-1] == 2 && w <= 1 && s.cells[i][j] == ' ' {
			continue // 前一宽字符的占位格
		}
		if s.cells[i][j] != 0 {
			b.WriteRune(s.cells[i][j])
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// nonEmpty 返回屏幕上非空行。
func (s *screen) nonEmpty() []string {
	var out []string
	for i := range s.cells {
		if t := s.line(i); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ─── 屏幕模型上的回放断言 ───────────────────────────────────────────────
//
// 判定标准用"屏幕内容是否恰好等于一帧"来表达，而不是数某个关键词出现几次：
// 关键词计数会因模型名互为子串（cn:deepseek-v4.1-flash 含 deepseek-v4.1-flash）
// 而失真，而"屏幕 == 一帧"既精确又直接对应"没有堆叠"这个事实。

// sameLines 比较两组行是否逐一相同。
func sameLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestWatchDoesNotStack 回放 5 帧后，屏幕内容应恰好等于一帧（无堆叠、无残留）。
//
// 这是本 bug 的直接回归断言：修复前第二帧起就会在下方多画一份，屏幕行数会
// 随刷新次数增长。
func TestWatchDoesNotStack(t *testing.T) {
	st := &statsResponse{
		Enabled: true, UptimeSec: 86400, Since: "2026-09-14T20:43:52+08:00",
		Total: mkModel("(all)", 828, 7),
		Models: []modelStat{
			mkModel("deepseek-v4.1-flash", 786, 3),
			mkModel("cn:deepseek-v4.1-flash", 29, 4),
			mkModel("deepseek-v4-pro", 13, 0),
		},
	}

	// 覆盖用户实测分屏宽度附近，以及更窄的档位。
	for _, lay := range []layout{{width: 86, height: 40}, {width: 100, height: 30}, {width: 60, height: 24}} {
		frame := buildFrame(st, "requests", nil, lay)
		if !frameFits(frame, lay) {
			t.Fatalf("布局 %+v 下帧放不下，用例前提不成立", lay)
		}
		want := make([]string, len(frame))
		for i, l := range frame {
			want[i] = strings.TrimRight(l, " ")
		}

		sc := newScreen(lay.width)
		// 首次绘制（prevLines=0）与后续覆盖绘制要分别验：首帧不清屏，直接从顶部开始。
		sc.write(captureStdout(t, func() { rewriteFrame(frame, 0, true) }))
		for i := 1; i < 5; i++ {
			sc.write(captureStdout(t, func() { rewriteFrame(frame, len(frame), true) }))
		}

		got := sc.nonEmpty()
		if !sameLines(got, want) {
			t.Errorf("layout %+v：刷新 5 次后屏幕内容 ≠ 一帧（%d 行 vs %d 行，堆叠或残留）:\n--- 屏幕 ---\n%s\n--- 期望 ---\n%s",
				lay, len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// TestWatchStackingReproducedByWrapping 反证：帧若宽于窗口，屏幕模型确实会
// 表现出堆叠 —— 证明上面那条断言不是因为模型太宽松才通过。
//
// 这里绕过 frameFits 直接回放（模拟修复前的行为）：窗口 60 列、帧 100 列，
// 终端折行使逻辑行数（9）远小于视觉行数（17），上移 9 行回不到帧首。
func TestWatchStackingReproducedByWrapping(t *testing.T) {
	st := &statsResponse{
		Enabled: true, UptimeSec: 3600,
		Total: mkModel("(all)", 828, 7),
		Models: []modelStat{
			mkModel("deepseek-v4.1-flash", 786, 3),
			mkModel("cn:deepseek-v4.1-flash", 29, 4),
		},
	}
	frame := buildFrame(st, "requests", nil, layout{width: 120, height: 40})
	if maxFrameWidth(frame) <= 60 {
		t.Fatalf("用例前提：帧（%d 列）应远宽于 60 列窗口", maxFrameWidth(frame))
	}

	sc := newScreen(60)
	sc.write(captureStdout(t, func() { rewriteFrame(frame, 0, true) }))
	sc.write(captureStdout(t, func() { rewriteFrame(frame, len(frame), true) }))
	got := sc.nonEmpty()

	// 折行导致上移量不足：第二次绘制落在首帧中部而非帧首，于是标题被留在
	// 上方，屏幕上出现两份标题。这正是用户截图里的"每刷新一次多一个统计窗口"。
	n := 0
	for _, l := range got {
		if strings.Contains(l, "网关请求统计") {
			n++
		}
	}
	if n < 2 {
		t.Errorf("折行应导致标题堆叠（模型需对此敏感），实际只出现 %d 次:\n%s",
			n, strings.Join(got, "\n"))
	}
}
