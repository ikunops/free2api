package logfmt

import "testing"

// 本文件所有 uid / 昵称均为虚构构造值，与任何真实账号无关。
//
// TestLabel 覆盖账号标签的五种形态。标签用于请求流水行与调度日志，
// 目的是让人眼直接看出「刚才那个 429 / 6004 是哪个号」，因此昵称必须保留、
// uid8 必须保留（供 grep），两者都不能缺。
func TestLabel(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		nick string
		want string
	}{
		{"昵称 + uid8", "a1b2c3d4-0000-4000-8000-000000000001", "示例昵称甲", "示例昵称甲(a1b2c3d4)"},
		{"昵称为空退回 uid8", "e5f60718-0000-4000-8000-000000000002", "", "e5f60718"},
		{"昵称只有空白视为空", "e5f60718-b", "   ", "e5f60718"},
		{"uid 与昵称皆空", "", "", "-"},
		{"短 uid 不补零", "abc", "sample", "sample(abc)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Label(tc.uid, tc.nick); got != tc.want {
				t.Errorf("Label(%q, %q) = %q, want %q", tc.uid, tc.nick, got, tc.want)
			}
		})
	}
}

// TestLabelNeverLeaksFullUID 全量 uid 不得出现在日志标签里（与 server 侧
// 「full uid leaked」断言同一口径：日志只留 8 位，全量去 state.json 查）。
func TestLabelNeverLeaksFullUID(t *testing.T) {
	const uid = "a1b2c3d4-0000-4000-8000-000000000001"
	got := Label(uid, "示例昵称甲")
	if got != "示例昵称甲(a1b2c3d4)" {
		t.Fatalf("Label = %q", got)
	}
}

// TestDisplayWidth 中文/全角按 2 列算——这是表格能对齐的前提。
// 若错算成 1 列，中文昵称列会比英文昵称列少补一半空格，整张表往上缩。
func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"sample", 6},
		{"猫", 2},
		{"示例昵称甲", 10},
		{"示例昵称甲(a1b2c3d4)", 20},
		{"ｆｕｌｌ", 8},    // 全角 ASCII
		{"（全角括号）", 12}, // 全角标点
		{"a猫b", 4},
	}
	for _, tc := range tests {
		if got := DisplayWidth(tc.in); got != tc.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestPad 只补不截：超宽时原样返回，绝不丢信息（昵称是排查主线索）。
func TestPad(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"ascii 右补空格", "sample", 10, "sample    "},
		{"中文按显示宽补", "猫", 6, "猫    "},
		{"等宽不加空格", "sample", 6, "sample"},
		{"超宽原样返回不截断", "示例昵称甲(a1b2c3d4)", 8, "示例昵称甲(a1b2c3d4)"},
		{"width<=0 原样返回", "abc", 0, "abc"},
		{"空串补满", "", 3, "   "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Pad(tc.in, tc.width)
			if got != tc.want {
				t.Errorf("Pad(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
			// 不变量：补完后显示宽至少达到 width（超宽输入除外）
			if w := DisplayWidth(got); w < tc.width && w < DisplayWidth(tc.in) {
				t.Errorf("Pad(%q, %d) width=%d < %d", tc.in, tc.width, w, tc.width)
			}
		})
	}
}
