//go:build windows

package main

import (
	"encoding/json"
	"math"
	"unsafe"
	"os"
	"path/filepath"
	"testing"
)

// TestFloatConfigSanitizeDropsUnknownItems 配置里混进未知项 / 非法区间 / 重复项时，
// 必须被清洗掉再落盘：前端就算传了脏值也不会让窗口画错。
func TestFloatConfigSanitizeDropsUnknownItems(t *testing.T) {
	c := floatConfig{
		Range: "hoge",
		Theme: "neon",
		Items: []string{"rate", "rate", floatItemClick, "不存在的项"},
	}
	c.sanitize()
	if c.Range != "today" {
		t.Errorf("Range = %q, want today", c.Range)
	}
	if c.Theme != "dark" {
		t.Errorf("Theme = %q, want dark", c.Theme)
	}
	want := []string{"rate", floatItemClick}
	if len(c.Items) != len(want) {
		t.Fatalf("Items = %v, want %v", c.Items, want)
	}
	for i := range want {
		if c.Items[i] != want[i] {
			t.Fatalf("Items = %v, want %v", c.Items, want)
		}
	}
}

// TestFloatConfigEmptyItemsGetsDefault 全关也要留至少一项：全空的悬浮窗只剩
// 一个空壳，用户会以为是坏的。
func TestFloatConfigEmptyItemsGetsDefault(t *testing.T) {
	c := floatConfig{Items: []string{}}
	c.sanitize()
	if len(c.Items) == 0 {
		t.Fatal("Items empty after sanitize, want at least one default")
	}
}

// TestFloatApplyPersistsAndOpens 配置写盘 + 开关生效（开了就会建窗，测完立刻关）。
func TestFloatApplyPersistsAndOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "floatwin.json")
	f := newFloatWin(path, "127.0.0.1:1", "")
	floatOwner = f
	defer func() { f.closeWin(); floatOwner = nil }()

	on := true
	items := []string{floatItemRate, floatItemTokens}
	if err := f.apply(floatApply{Enabled: &on, Items: items, Range: "7d", Theme: "light"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !f.running() {
		t.Fatal("window not running after apply(enabled=true)")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var saved floatConfig
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !saved.Enabled || saved.Range != "7d" || saved.Theme != "light" {
		t.Errorf("saved = %+v, want enabled/7d/light", saved)
	}

	off := false
	if err := f.apply(floatApply{Enabled: &off}); err != nil {
		t.Fatalf("apply off: %v", err)
	}
	if f.running() {
		t.Fatal("window still running after apply(enabled=false)")
	}
}

// TestFloatSnapshotDecode 校验 SSE 载荷里我们真用的那几个字段能解出来；
// 后端改字段名时这里先炸，而不是让浮窗默默显示 0。
func TestFloatSnapshotDecode(t *testing.T) {
	raw := []byte(`{"range":"today",
	  "total":{"requests":8241,"total_tokens":12400000},
	  "live":{"window_sec":60,"requests":3,"completion_tokens":7400,"tokens_per_sec":123.6},
	  "live_models":{"cn:deepseek-v4.1-flash-x0.11":2},
	  "producers":[{"producer":"workbuddy","requests":7000}]}`)
	var s floatSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Total.Requests != 8241 || s.Total.TotalTokens != 12400000 {
		t.Errorf("total = %+v", s.Total)
	}
	if s.Live.TokensPerSec != 123.6 || s.Live.WindowSec != 60 {
		t.Errorf("live = %+v", s.Live)
	}
	if s.LiveModels["cn:deepseek-v4.1-flash-x0.11"] != 2 {
		t.Errorf("live_models = %v", s.LiveModels)
	}
	if len(s.Producers) != 1 || s.Producers[0].Producer != "workbuddy" {
		t.Errorf("producers = %+v", s.Producers)
	}
}

// TestFloatItemDefsAreUnique 后端定死的开关清单不能有重 ID（前端按 ID 匹配，重了会串）。
func TestFloatItemDefsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, it := range floatItemDef() {
		id := it["ID"]
		if id == "" || it["Name"] == "" || it["Hint"] == "" {
			t.Fatalf("incomplete item def: %+v", it)
		}
		if seen[id] {
			t.Fatalf("duplicate float item id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 5 {
		t.Fatalf("float item defs too few: %d", len(seen))
	}
}

// TestFmtInt64 千分位与负号（浮窗唯一的格式化入口，错了整窗数字都丑）。
func TestFmtInt64(t *testing.T) {
	cases := map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4200: "-4,200"}
	for in, want := range cases {
		if got := fmtInt64(in); got != want {
			t.Errorf("fmtInt64(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyRoundedAlphaCenterAndCorner 圆心不透明、圆角外全透明。
func TestApplyRoundedAlphaCenterAndCorner(t *testing.T) {
	const w, h = 40, 40
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = 200, 200, 200
	}
	applyRoundedAlpha(px, w, h, 12, 238)
	ctr := (20*w + 20) * 4
	if px[ctr+3] != 238 {
		t.Fatalf("center alpha = %d, want 238", px[ctr+3])
	}
	if px[ctr] < 180 {
		t.Fatalf("center R = %d after premultiply, want >=180 (was 200)", px[ctr])
	}
	if px[3] != 0 {
		t.Fatalf("corner alpha = %d, want 0", px[3])
	}
}

// TestApplyRoundedAlphaCornerIsFeathered 圆角弧上必须有过渡像素，不能 0/238 硬切。
//
// 这是一条真实修过的 bug：早先「圆内 238、圆外 0」硬切，屏幕上浮窗四周出现一圈
// 肉眼可见的锯齿 + 一圈发暗的黑边。
//
// 判定要看**圆角弧**而不是直边：直边本来就该是硬的（一条直线不需要羽化），
// 只有拐角处才有斜切边。做法是从左上角沿对角线往里扫，alpha 必须单调不减，
// 并且中途出现中间值。
func TestApplyRoundedAlphaCornerIsFeathered(t *testing.T) {
	const w, h = 60, 40
	const alpha = 238
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = 200, 200, 200
	}
	applyRoundedAlpha(px, w, h, 14, alpha)

	// 沿**顶边**从左往右扫：这条线横切左上角的圆角弧，是真正会羽化的斜边。
	// （沿 45° 对角线扫是不行的——那条线正好过圆弧的切点，弧在该处与对角线相切，
	//   覆盖率会一步到位，看不到中间值。）
	scan := []int{}
	for x := 0; x < 16; x++ {
		scan = append(scan, int(px[(0*w+x)*4+3]))
	}
	diag := scan
	// 单调不减：不能「先全透明、再突然有、又再透明」。
	for i := 1; i < len(diag); i++ {
		if diag[i] < diag[i-1] {
			t.Fatalf("alpha not monotonic along corner diagonal: %v", diag)
		}
	}
	// 必须有中间值（羽化）。
	inter := 0
	for _, a := range diag {
		if a > 4 && a < alpha-4 {
			inter++
		}
	}
	if inter < 2 {
		t.Fatalf("corner edge not feathered: top-row alpha scan = %v", diag)
	}
	t.Logf("top-row alpha scan through corner = %v", diag)

	// 直边仍然是硬的：顶边中点附近应该一步到位到 alpha（一条直线不需要羽化）。
	// 如果这里也羽化，说明覆盖率算错了——那会让整条上边看起来发虚。
	mid := 30
	hard := false
	for x := 15; x < w-15; x++ {
		if px[(0*w+x)*4+3] == alpha && px[(1*w+x)*4+3] == alpha {
			hard = true
			break
		}
	}
	if !hard {
		t.Error("straight top edge should be fully opaque well past the corner")
	}
	_ = mid
}

// TestApplyRoundedAlphaRadiusClamped 半径超过边长一半时夹紧，且夹紧后仍然是
// 一个合法形状（圆心有内容、左右两列没有被整列吃掉）。
func TestApplyRoundedAlphaRadiusClamped(t *testing.T) {
	const w, h = 20, 60
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = 200, 200, 200
	}
	applyRoundedAlpha(px, w, h, 999, 255)

	center := (30*w + w/2) * 4 + 3
	if px[center] != 255 {
		t.Fatalf("center alpha = %d, want 255 (radius clamp broke the shape)", px[center])
	}
	// 夹到 w/2=10 之后，行中心左右应当都是满 alpha（形状退化成胶囊）。
	l := (30*w + 0) * 4 + 3
	r := (30*w + w-1) * 4 + 3
	if px[l] == 0 && px[r] == 0 {
		t.Fatalf("clamped radius ate the whole row (l=%d r=%d)", px[l], px[r])
	}
}

// TestFloatHeightFitsAllRows 全部开关都打开时，窗口高度必须容得下每一行。
//
// 这是一条真实修过的 bug：早先「先按猜的行数定高、再按开关逐行画」，用户多开两个
// 开关后内容就被画到窗口外面，屏幕上只看得见第一行。而从截图上看，「文字没画出来」
// 和「文字画在窗口外」长得一模一样——所以必须在布局层就断言。
func TestFloatHeightFitsAllRows(t *testing.T) {
	cfg := defaultFloatConfig()
	cfg.Items = []string{
		floatItemInFlight, floatItemRate, floatItemRequests,
		floatItemTokens, floatItemSource, floatItemTopmost, floatItemClick,
	}
	f := &floatWin{className: floatClassName, cfg: cfg, snapOK: true}
	pal := paletteFor(cfg.Theme)
	rows := f.layout(pal, floatSnapshot{
		Total: struct {
			Requests    int64 `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
		}{Requests: 8241, TotalTokens: 12400000},
		LiveModels: map[string]int{"cn:deepseek-v4.1-flash-x0.11": 2},
		Producers: []struct {
			Producer string `json:"producer"`
			Requests int64  `json:"requests"`
		}{{Producer: "workbuddy", Requests: 7000}},
	}, true, 0.55, 0)

	// topmost / passthru 是行为开关，不占行。
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 (title + 5 content)", len(rows))
	}
	h := floatHeightFor(rows)
	var sum int
	for _, r := range rows {
		sum += int(rowHeight(r))
	}
	if h < sum {
		t.Fatalf("window height %d < content height %d; rows will be clipped", h, sum)
	}
	if h > 240 {
		t.Fatalf("window height %d too tall for a corner widget", h)
	}

	// 反过来：渲染出的位图高度必须正好等于算出来的高度。
	px := f.render(1.0)
	if got := len(px) / 4 / sc96(floatBaseWidth); got != h {
		t.Fatalf("rendered height %d, want %d", got, h)
	}

	// 最后一行必须真的落在位图里面（底部至少留得下一个字高的一半）。
	last := rows[len(rows)-1]
	needBottom := int(last.fontSize)
	if h-sum < needBottom/2 {
		t.Fatalf("bottom padding %d too small for last row (fontSize %d)", h-sum, last.fontSize)
	}
}

// TestRoundedRectSDFSigns 钉住 SDF 的符号约定：内负外正、边界恰为 0。
//
// 这个函数写错过一次（漏了末尾的 -r），后果是「窗子只剩一圈圆角」这种
// 从截图上一眼看不出来的故障。把符号直接写成断言，以后改错立刻炸。
func TestRoundedRectSDFSigns(t *testing.T) {
	const w, h, r = 60.0, 40.0, 14.0
	if d := roundedRectSDF(w/2, h/2, w, h, r); d >= 0 {
		t.Errorf("center SDF = %v, want negative (inside)", d)
	}
	if d := roundedRectSDF(0.5, 0.5, w, h, r); d <= 0 {
		t.Errorf("outside-corner SDF = %v, want positive (outside)", d)
	}
	// 边界点：顶边中点上方 0.5 像素处 d 应恰好为 -0.5（像素中心在形状边界内侧半像素）。
	if d := roundedRectSDF(w/2, 0.5, w, h, r); math.Abs(d-(-0.5)) > 1e-9 {
		t.Errorf("top-edge SDF = %v, want -0.5", d)
	}
	// 对称性：左右镜像点距离必须相等。
	for _, y := range []float64{1, 10, 20.5, 39} {
		l := roundedRectSDF(1, y, w, h, r)
		rr := roundedRectSDF(w-1, y, w, h, r)
		if math.Abs(l-rr) > 1e-9 {
			t.Errorf("asymmetric at y=%v: left=%v right=%v", y, l, rr)
		}
	}
	// 半径夹紧后仍是合法形状：中心在内。
	if d := roundedRectSDF(10.5, 30.5, 20, 60, 10); d >= 0 {
		t.Errorf("clamped-shape center SDF = %v, want negative", d)
	}
}

// TestFloatWindowStylesNotVisibleAtCreation 建窗样式里**不能**有 WS_VISIBLE。
//
// 这是一条真实修过的 bug：早先建窗时带了 WS_VISIBLE，于是 CreateWindowExW 一返回，
// 窗口就已经显示在调用里给的占位矩形 (0,0,10,10) 上、而且还没画过任何内容——
// 屏幕左上角会「啪」地闪出一个小方块，再被后面的 SetWindowPos 挪到右下角。
// 截图里红框标出的那个方块就是它。
//
// 光靠肉眼看代码很容易改回去，所以把「不可见」直接写成断言。
func TestFloatWindowStylesNotVisibleAtCreation(t *testing.T) {
	style, exStyle := floatWindowStyles()
	if style&wsVisible != 0 {
		t.Errorf("creation style has WS_VISIBLE (0x%x); the window would flash at (0,0,10,10)", style)
	}
	if style&wsPopup == 0 {
		t.Errorf("style should be WS_POPUP (borderless), got 0x%x", style)
	}
	if exStyle&wsExLayered == 0 {
		t.Error("exStyle must have WS_EX_LAYERED (per-pixel alpha)")
	}
	if exStyle&wsExNoActivate == 0 {
		t.Error("exStyle must have WS_EX_NOACTIVATE (must never steal focus)")
	}
	if exStyle&wsExToolWindow == 0 {
		t.Error("exStyle must have WS_EX_TOOLWINDOW (keep out of the taskbar/Alt-Tab)")
	}
}

// TestFloatWindowShowedUpAtItsRealRect 窗口一旦显示，就必须已经在最终位置上——
// 不能还停在建窗时的占位矩形。这条把「摆位在显示之前」这个顺序钉住。
func TestFloatWindowShowedUpAtItsRealRect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "floatwin.json")
	f := newFloatWin(path, "127.0.0.1:1", "")
	floatOwner = f
	defer func() { f.closeWin(); floatOwner = nil }()

	on := true
	if err := f.apply(floatApply{Enabled: &on}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !f.running() {
		t.Fatal("window not running")
	}
	f.mu.Lock()
	hwnd := f.hwnd
	f.mu.Unlock()

	var r rECT
	if ret, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		t.Fatal("GetWindowRect failed")
	}
	w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
	if w < 100 || h < 40 {
		t.Fatalf("window is %dx%d — still sitting at the creation placeholder size", w, h)
	}
	// 也不该停在左上角（那正是「闪一下」时它出现的位置）。
	if r.Left < 40 && r.Top < 40 {
		t.Errorf("window at (%d,%d): visible at the top-left placeholder position", r.Left, r.Top)
	}
}
