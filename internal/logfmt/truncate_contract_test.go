package logfmt

import (
	"strings"
	"testing"
)

// Truncate 的非正 n 契约测试（#146）：文档契约自述「n<=0 返回空串」，但实现里
// `if len(s) > n` 对负数恒成立（len(s) >= 0 > n），内层 `for n > 0` 不进循环体，
// 落到 `return s[:n]` → s[:负数] panic: slice bounds out of range。n==0 不崩
// （len(s) > 0 时 s[:0] 合法），坑只在负数。兄弟函数 Pad 有 width<=0 守卫，
// Truncate 缺同一道——本组测试把该守卫钉进契约，防回归。

// TestTruncateNegativePanicsBeforeFix 负数 n 在修复前 panic（RED 阶段证据）：
// 用 recover 捕获并转为失败，修复后（n<=0 入口返回空串）不会进 panic 分支。
func TestTruncateNegativePanicsBeforeFix(t *testing.T) {
	for _, n := range []int{-1, -5} {
		// Arrange
		s := "将在 24 小时后重置限额"

		// Act
		got, panicked := func() (out string, panicked bool) {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			out = Truncate(s, n)
			return
		}()

		// Assert：契约要求返回空串，绝不允许 panic。
		if panicked {
			t.Errorf("Truncate(s, %d) panicked (slice bounds out of range), want 空串", n)
		}
		if got != "" {
			t.Errorf("Truncate(s, %d)=%q want 空串（n<=0 返回空串）", n, got)
		}
	}
}

// TestTruncateNonPositiveReturnsEmpty n<=0 的完整契约：负数与 0 都返回空串
// （对齐文档注释与 Pad 的 width<=0 守卫风格）。空串输入也覆盖——空串 + 负数
// 同样命中 len(s) > n 分支（0 > -1 成立）。
func TestTruncateNonPositiveReturnsEmpty(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
	}{
		{"negative n on ascii", "hello", -1},
		{"negative n on cjk", "将在 24 小时后重置限额", -5},
		{"negative n on empty", "", -1},
		{"zero n on ascii", "hello", 0},
		{"zero n on cjk", "重置限额", 0},
		{"zero n on empty", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.s / tc.n
			// Act
			got := Truncate(tc.s, tc.n)

			// Assert
			if got != "" {
				t.Errorf("Truncate(%q, %d)=%q want 空串", tc.s, tc.n, got)
			}
		})
	}
}

// TestTruncatePositivePathUnchanged 正数路径回归锚：修守卫后 n>=1 行为逐字节
// 不变（TrimSpace 前置、rune 边界回退、短于 n 原样、精确字节整切）。
func TestTruncatePositivePathUnchanged(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"ascii truncate", "abcdefghijklmnopqrstuvwxyz", 10, "abcdefghij"},
		{"cjk rune boundary fallback", "将在 24 小时后重置限额", 4, "将"},
		{"cjk exact rune boundary", strings.Repeat("中", 5), 9, strings.Repeat("中", 3)},
		{"shorter than n unchanged", "短", 100, "短"},
		{"empty string with positive n", "", 10, ""},
		{"trim space then unchanged", "  error body  ", 20, "error body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.s / tc.n
			// Act
			got := Truncate(tc.s, tc.n)

			// Assert
			if got != tc.want {
				t.Errorf("Truncate(%q, %d)=%q want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}
