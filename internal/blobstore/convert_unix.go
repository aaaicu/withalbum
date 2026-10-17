//go:build unix

package blobstore

import (
	"os/exec"
	"syscall"
)

// isolateProcess starts the tool in its own process group and makes the
// context cancellation kill the whole group, so helpers it spawned do not
// outlive a timeout.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
