//go:build unix && !solaris && !aix

// term_unix.go 非 Windows 的 Unix 平台：终端原生支持 ANSI/VT，无需启用动作；
// 窗口尺寸经 TIOCGWINSZ ioctl 取得。
//
// 为什么需要尺寸：watch 靠"光标上移 N 行"回到帧首。帧若比窗口宽，终端会把一行
// 折成两行，实际占用行数多于逻辑行数，上移量随之失准 —— 于是每帧都在旧内容
// 下方再画一份。渲染方据此把帧裁进窗口。
//
// 只依赖标准库：不引入 golang.org/x/term（本工具刻意保持零额外依赖）。
// solaris/aix 的 stdlib 未导出 SYS_IOCTL（其 ioctl 需经 libc），故排除在外、
// 由 term_other.go 按"尺寸未知"处理；非 Unix 由 term_windows.go / term_other.go 提供。
package main

import (
	"syscall"
	"unsafe"
)

// winsize 对应 ioctl TIOCGWINSZ 的返回结构 struct winsize。
type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// enableVT 在 Unix 终端恒返回 true：终端默认解释 ANSI 转义序列，不存在开关。
func enableVT() bool { return true }

// termSize 返回终端窗口的列数与行数；拿不到（重定向/管道/无 tty）返回 0, 0。
func termSize() (cols, rows int) {
	ws := &winsize{}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(syscall.Stdout),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if errno != 0 || ws.cols == 0 || ws.rows == 0 {
		return 0, 0
	}
	return int(ws.cols), int(ws.rows)
}
