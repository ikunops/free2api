//go:build !windows

package scheduler

import "os/exec"

func hideChildWindow(cmd *exec.Cmd) {}
