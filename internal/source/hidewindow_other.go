//go:build !windows

package source

import "os/exec"

func hideChildWindow(cmd *exec.Cmd) {}
