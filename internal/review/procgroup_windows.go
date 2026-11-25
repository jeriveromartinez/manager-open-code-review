//go:build windows

package review

import "os/exec"

// isolateProcessGroup is a no-op on Windows: the platform has no process
// groups, so CommandContext keeps its default behaviour of killing the direct
// child only.
func isolateProcessGroup(cmd *exec.Cmd) {}

// gracefulStopSupported is false on Windows: a child cannot be asked to close
// with a POSIX signal, so OCR keeps its own per-task timeout as the fallback.
func gracefulStopSupported() bool { return false }

// signalProcessGroup is a no-op on Windows for the same reason.
func signalProcessGroup(*exec.Cmd) error { return nil }

// killProcessGroup stops the direct child. Without process groups the helpers
// it spawned are not reachable, but WaitDelay closes the pipes so the slot is
// released.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
