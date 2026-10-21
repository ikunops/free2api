//go:build windows

package scheduler

import (
	"os/exec"
	"syscall"
)

// createNoWindow 让 CUI 子进程（python.exe）不分配控制台窗口。
// 父进程是 windowsgui 时，Windows 默认会给控制台子进程新建一个终端；
// 定时任务（开学季/夜猫子）因此会在整点闪窗。
const createNoWindow = 0x08000000

func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
