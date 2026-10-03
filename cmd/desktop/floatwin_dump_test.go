//go:build windows

package main

import (
	"os"
	"testing"
)

// TestDumpFloatFrameForManualInspect 把一帧 render 的真实像素写成 BMP。
//
// 为什么需要它：浮窗是 WS_EX_LAYERED 窗口，屏幕截图工具（BitBlt / PrintWindow
// / PW_RENDERFULLCONTENT）**都抓不到它的内容**——分层窗口的合成发生在 DWM 里，
// 不经过任何一条常规抓屏路径。所以「肉眼验收浮窗长什么样」这件事没法靠截图，
// 只能让程序自己把将要交给 UpdateLayeredWindow 的那块像素原样吐出来。
//
// 用法：F2A_DUMP_FLOAT=<out.bmp> go test ./cmd/desktop -run DumpFloatFrame
func TestDumpFloatFrameForManualInspect(t *testing.T) {
	out := os.Getenv("F2A_DUMP_FLOAT")
	if out == "" {
		t.Skip("set F2A_DUMP_FLOAT=<path.bmp> to dump a frame")
	}
	cfg := defaultFloatConfig()
	f := &floatWin{className: floatClassName, cfg: cfg, snapOK: true}
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

	px := f.render(1.0)
	rows := f.probeRows()
	w, h := sc96(floatBaseWidth), floatHeightFor(rows)
	if err := writeBMP(out, px, w, h); err != nil {
		t.Fatalf("writeBMP: %v", err)
	}
	t.Logf("wrote %s (%dx%d)", out, w, h)
}

// writeBMP 写一个 32bpp 未压缩 BMP（BGRA，自上而下）。
func writeBMP(path string, bgra []byte, w, h int) error {
	const hdrSize = 14
	const infoSize = 40
	pix := len(bgra)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	put32 := func(v uint32) { _, _ = f.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}) }
	put16 := func(v uint16) { _, _ = f.Write([]byte{byte(v), byte(v >> 8)}) }
	_, _ = f.Write([]byte{'B', 'M'})
	put32(uint32(hdrSize + infoSize + pix))
	put32(0)
	put32(uint32(hdrSize + infoSize))
	put32(infoSize)
	put32(uint32(w))
	put32(uint32(-h)) // 负高度 = 自上而下
	put16(1)
	put16(32)
	put32(0) // BI_RGB
	put32(uint32(pix))
	put32(0)
	put32(0)
	put32(0)
	put32(0)
	_, err = f.Write(bgra)
	return err
}
