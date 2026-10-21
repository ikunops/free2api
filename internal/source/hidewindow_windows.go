//go:build windows

package source

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hideChildWindow 防止 WorkBuddy 客户端探测子进程在 GUI 父进程下弹出终端。
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
