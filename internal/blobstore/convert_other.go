//go:build !unix

package blobstore

import "os/exec"

// isolateProcess: no process groups here; the context kills the tool itself.
func isolateProcess(cmd *exec.Cmd) {}

// lowerPriority: no portable priority here.
func lowerPriority(pid int) {}
