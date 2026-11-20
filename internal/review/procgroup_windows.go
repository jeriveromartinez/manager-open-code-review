//go:build windows

package review

import "os/exec"

// isolateProcessGroup is a no-op on Windows: the platform has no process
// groups, so CommandContext keeps its default behaviour of killing the direct
// child only.
func isolateProcessGroup(cmd *exec.Cmd) {}
