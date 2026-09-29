//go:build solaris || aix

// term_ansi_nosize.go Solaris / AIX：终端支持 ANSI/VT，但其标准库未导出
// SYS_IOCTL（ioctl 需经 libc 调用），故拿不到窗口尺寸。
//
// 尺寸未知时渲染方不做宽度裁剪、watch 保持单帧输出 —— 这与"有尺寸"的平台相比
// 少了自适应，但不会因为按错误尺寸排版而产生错位堆叠（未知即不猜）。
// 需要窄窗口适配时可用 -width/-height 显式指定。
package main

// enableVT Solaris/AIX 终端原生支持 ANSI 转义。
func enableVT() bool { return true }

// termSize 返回 0, 0 表示"尺寸未知"（stdlib 不导出 SYS_IOCTL，无法查询）。
func termSize() (cols, rows int) { return 0, 0 }
