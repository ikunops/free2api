//go:build windows

package main

import "testing"

// TestFloatAlphaEdgeIsFeatheredAllFourSides 四条直边都必须有羽化像素，不能是硬边。
//
// 这是用户实际报的那条缺陷：一开悬浮窗开关就看到圆角外面围着一圈直角残边。
// 根因：fillAlpha=238（不是 255），形状边界与窗口矩形重合时最外一列/一行是满的
// 238，17/255 的背景沿直边整条渗出且是 0→238 硬切。修法是四边对称内缩 0.5px，
// 让最外像素只被覆盖一半。
//
// 判据：四条边的最外那一行/列 alpha 必须落在 (0, alpha) 区间内 —— 有过渡但没被
// 吃掉；紧邻的内侧必须已经是满 alpha（羽化宽度就是 1px，不能糊成一片虚）。
func TestFloatAlphaEdgeIsFeatheredAllFourSides(t *testing.T) {
	const w, h = 300, 157
	const alpha = 238
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = 200, 200, 200
	}
	applyRoundedAlpha(px, w, h, 16, alpha)
	at := func(x, y int) int { return int(px[(y*w+x)*4+3]) }

	midX, midY := w/2, h/2
	cases := []struct {
		name       string
		outer, in  int
	}{
		{"左边", at(0, midY), at(1, midY)},
		{"右边", at(w-1, midY), at(w-2, midY)},
		{"上边", at(midX, 0), at(midX, 1)},
		{"下边", at(midX, h-1), at(midX, h-2)},
	}
	for _, c := range cases {
		if c.outer == 0 {
			t.Errorf("%s: 最外像素 alpha=0，窗口会显得比内容小一圈", c.name)
		}
		if c.outer >= alpha {
			t.Errorf("%s: 最外像素 alpha=%d 是硬边 —— 这就是那圈直角残边", c.name, c.outer)
		}
		if c.in != alpha {
			t.Errorf("%s: 内侧相邻像素 alpha=%d want %d（羽化只 1px，不该扩散更深）",
				c.name, c.in, alpha)
		}
	}
	t.Logf("四边最外/内侧 alpha：左 %d/%d 右 %d/%d 上 %d/%d 下 %d/%d",
		at(0, midY), at(1, midY), at(w-1, midY), at(w-2, midY),
		at(midX, 0), at(midX, 1), at(midX, h-1), at(midX, h-2))
}
