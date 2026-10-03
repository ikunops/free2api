//go:build windows

package main

import (
	"testing"
	"unsafe"

)

// TestDrawFloatTextWritesPixels 直接对着一个内存 DIB 画一行字，数一下有多少像素
// 变白。GDI 文本这条路很容易因为参数/句柄问题**静默不画**，而浮窗那种场景下
// 「画不出来」和「画出来但看不清」在页面上长得一样——所以必须在单测里用像素说话。
func TestDrawFloatTextWritesPixels(t *testing.T) {
	const w, h = 300, 60
	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		t.Skip("no screen dc")
	}
	defer pReleaseDC.Call(0, screenDC)
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		t.Fatal("CreateCompatibleDC failed")
	}
	defer pDeleteDC.Call(memDC)
	bih := bitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: w, Height: -h,
		Planes: 1, BitCount: 32, Compression: biRGB,
	}
	var bits unsafe.Pointer
	dib, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bih)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == nil {
		t.Fatal("CreateDIBSection failed")
	}
	defer pDeleteObject.Call(dib)
	old, _, _ := pSelectObject.Call(memDC, dib)
	defer pSelectObject.Call(memDC, old)

	px := unsafe.Slice((*byte)(bits), w*h*4)
	for i := range px {
		px[i] = 0
	}
	drawFloatText(memDC, "123.6 tok/s", 8, 8, 280, 24, 16, fwBold, 232, 234, 240, antialiased)

	lit := 0
	for i := 0; i < len(px); i += 4 {
		if px[i] > 100 && px[i+1] > 100 && px[i+2] > 100 {
			lit++
		}
	}
	if lit < 20 {
		t.Fatalf("text drew %d bright pixels, want >=20 (GDI text silently no-op?)", lit)
	}
	t.Logf("lit pixels = %d", lit)
}

// TestLogFontFaceNameLayout 断言 LOGFONTW 布局没被改错：FaceName 必须紧跟在
// PitchFamily 后面（WCHAR[32]），一旦加了 padding 就会把字体名写到别处，
// CreateFontW 静默回落到 System 默认字体，窗口看起来「字不太对」。
func TestLogFontFaceNameLayout(t *testing.T) {
	var lf logFontW
	if !lf.setFace("Segoe UI") {
		t.Fatal("setFace failed")
	}
	// "Segoe UI" = S e g o e ␣ U I + NUL → 下标 8 是 0。
	if lf.FaceName[0] != 'S' || lf.FaceName[5] != ' ' || lf.FaceName[6] != 'U' || lf.FaceName[7] != 'I' || lf.FaceName[8] != 0 {
		t.Fatalf("FaceName = %v", lf.FaceName[:10])
	}
	if lf.setFace("这个字体名特别特别长长长长长长长长长长长长长长长长长长长长长长长长长") {
		t.Fatal("setFace should reject over-long name")
	}
	if got := unsafe.Sizeof(lf); got != 92 {
		t.Fatalf("sizeof(LOGFONTW) = %d, want 92", got)
	}
	if lf.FaceName[9] != 0 || lf.FaceName[10] != 0 {
		t.Fatalf("FaceName tail not clean: %v", lf.FaceName[:12])
	}
}

// TestFloatRenderProducesText 把一整帧 render 出来并数「比底色亮很多」的像素。
// 这是浮窗最关键的不变量：内容必须真的落在像素上。曾经出过一次「全窗只有背景、
// 一个字都没有」的故障（圆角 clip region 与绘制顺序的问题），从屏幕截图上完全
// 分不清是字太小还是压根没画——所以必须在这里用像素数量说话。
func TestFloatRenderProducesText(t *testing.T) {
	f := &floatWin{className: floatClassName, cfg: defaultFloatConfig(), snapOK: true}
	f.snap.Total.Requests = 8241
	f.snap.Total.TotalTokens = 12400000
	f.snap.Live.TokensPerSec = 123.6
	f.snap.Live.WindowSec = 60
	f.snap.Live.Requests = 3
	f.snap.LiveModels = map[string]int{"cn:deepseek-v4.1-flash-x0.11": 2}
	f.snap.Producers = []struct {
		Producer string `json:"producer"`
		Requests int64  `json:"requests"`
	}{{Producer: "workbuddy", Requests: 7000}}

	rows := f.probeRows()
	w, h := sc96(floatBaseWidth), floatHeightFor(rows)
	px := f.render(1.0)
	if px == nil {
		t.Fatal("render returned nil")
	}
	if len(px) != w*h*4 {
		t.Fatalf("pixel len = %d, want %d", len(px), w*h*4)
	}
	// 底色 dark = (20,21,26)。文本 fg=(232,234,240) / 主色 accent=(96,165,250)。
	lit := 0
	alphaOn := 0
	for i := 0; i < len(px); i += 4 {
		if px[i+3] > 0 {
			alphaOn++
		}
		if px[i] > 110 && px[i+1] > 110 && px[i+2] > 110 {
			lit++
		}
	}
	if alphaOn < w*h/2 {
		t.Errorf("only %d of %d pixels opaque; rounded corners should still be mostly opaque", alphaOn, w*h)
	}
	if lit < 200 {
		t.Errorf("only %d bright pixels in a full frame; text likely not drawn", lit)
	}
	// 圆角外必须完全透明（(0,0) 在圆角之外）。
	if px[3] != 0 {
		t.Errorf("corner alpha = %d, want 0", px[3])
	}
	t.Logf("lit=%d opaque=%d size=%dx%d", lit, alphaOn, w, h)
}

// TestFloatRenderLightTheme 浅色主题下底色是接近白的，文本是深色：
// 「亮像素计数」的口径必须反过来，否则浅色主题会被误判成「一个字都没画」。
func TestFloatRenderLightTheme(t *testing.T) {
	cfg := defaultFloatConfig()
	cfg.Theme = "light"
	f := &floatWin{className: floatClassName, cfg: cfg, snapOK: true}
	f.snap.Total.Requests = 100
	rows := f.probeRows()
	w, h := sc96(floatBaseWidth), floatHeightFor(rows)
	px := f.render(1.0)
	dark := 0
	for i := 0; i < len(px); i += 4 {
		if px[i+3] > 200 && px[i] < 90 && px[i+1] < 90 && px[i+2] < 90 {
			dark++
		}
	}
	if dark < 150 {
		t.Errorf("only %d dark pixels in light theme; text likely not drawn", dark)
	}
	t.Logf("dark=%d size=%dx%d", dark, w, h)
}
