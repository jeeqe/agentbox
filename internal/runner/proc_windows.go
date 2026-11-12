//go:build windows

package runner

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
