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

// lowerPriority makes a started tool yield the CPU to the server and to
// uploads (nice 10). Its helpers inherit it. Failure is not an error.
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}
