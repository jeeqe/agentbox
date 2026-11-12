//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the agent in its own process group and kills the whole group on cancel.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
