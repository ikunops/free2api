//go:build windows

// term_windows.go 平台相关：Windows 控制台的 ANSI/VT 支持与可见窗口尺寸。
//
// 为什么需要 VT：Windows 控制台默认不解释 ANSI 转义序列，`\033[2K` 这类会被
// 静默丢弃 —— 表现为 watch 模式的逐帧输出堆叠刷屏（每帧都新起一段），
// 而不是原地覆盖。必须显式打开 ENABLE_VIRTUAL_TERMINAL_PROCESSING。
//
// 为什么需要尺寸：watch 靠"光标上移 N 行"回到帧首。若帧比窗口宽，终端会把一行
// 折成两行，该帧实际占用的行数就多于逻辑行数，上移量随之失准 —— 于是每帧都在
// 旧内容下方再画一份（窄窗口下的典型症状）。渲染方据此把帧裁进窗口。
//
// 只依赖标准库：ENABLE_* 两个常量与 SetConsoleMode/GetConsoleScreenBufferInfo
// 都不在 syscall 包里（常量需本地定义，函数经 kernel32 动态获取），因此不必引入
// golang.org/x/sys。
//
// 非 Windows 由 term_unix.go / term_other.go 提供实现。
package main

import (
	"syscall"
	"unsafe"
)

const (
	// 控制台输出模式位。标准库 syscall 未定义这两个常量，按 Win32 文档取值。
	enableProcessedOutput           = 0x0001
	enableVirtualTerminalProcessing = 0x0004
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// coord / smallRect / consoleScreenBufferInfo 对应 Win32 的 COORD、SMALL_RECT、
// CONSOLE_SCREEN_BUFFER_INFO。成员都是 SHORT（int16），Go 结构体对齐与 C 一致
// （最大字段对齐 2 字节，无填充差异），可直接把地址交给系统调用。
type coord struct{ x, y int16 }

type smallRect struct{ left, top, right, bottom int16 }

type consoleScreenBufferInfo struct {
	size              coord
	cursorPosition    coord
	attributes        uint16
	window            smallRect
	maximumWindowSize coord
}

// enableVT 尝试在当前标准输出上启用 VT 处理。
//
// 返回 false 表示"无法启用"——最常见的原因是 stdout 被重定向到文件或管道
// （此时 GetConsoleMode 直接失败，没有控制台可设置）。调用方据此回落为
// 不带转义的逐帧滚动输出，保证 `stats.exe | tee log` 之类的用法仍可读。
func enableVT() bool {
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Stdout, &mode); err != nil {
		return false // 无控制台（重定向/管道）或调用失败
	}
	// 目标位已开则无需再设置（幂等；也避免不必要的 SetConsoleMode 调用）。
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}

	// 注意：Call 返回的 error 恒非 nil（其语义是 GetLastError），
	// 因此必须判首个返回值 r1 是否为 0，而不能判 err != nil。
	r1, _, _ := procSetConsoleMode.Call(
		uintptr(syscall.Stdout),
		uintptr(mode|enableProcessedOutput|enableVirtualTerminalProcessing),
	)
	return r1 != 0
}

// termSize 返回控制台可见窗口的列数与行数；拿不到（重定向/管道/无窗口）返回 0, 0。
//
// 取"窗口"而非"缓冲区"尺寸：conhost 的缓冲区默认 120 列宽、可远大于可见区域，
// 按缓冲区排版仍会被折行 —— 可见区域才是排版的真实约束。
func termSize() (cols, rows int) {
	var info consoleScreenBufferInfo
	r1, _, _ := procGetConsoleScreenBufferInfo.Call(
		uintptr(syscall.Stdout),
		uintptr(unsafe.Pointer(&info)),
	)
	if r1 == 0 {
		return 0, 0
	}
	// SMALL_RECT 是闭区间，故跨度为 right-left+1。
	return int(info.window.right-info.window.left) + 1,
		int(info.window.bottom-info.window.top) + 1
}
