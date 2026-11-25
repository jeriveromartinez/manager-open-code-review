//go:build unix

package review

import (
	"errors"
	"os/exec"
	"syscall"
)

// isolateProcessGroup makes the child the leader of its own process group so a
// timeout can kill it together with any helper it spawned.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext kills only the direct child by default. Killing the
	// process group also stops shells and helpers that outlive it.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// gracefulStopSupported reports whether OCR can be asked to close with a
// signal before the hard stop. OCR 1.14 turns SIGINT into a JSON report with
// the work completed so far.
func gracefulStopSupported() bool { return true }

// signalProcessGroup asks the child's process group to finish gracefully,
// giving OCR time to write its partial JSON before the hard stop.
func signalProcessGroup(cmd *exec.Cmd) error {
	return signalGroup(cmd, syscall.SIGINT)
}

// killProcessGroup hard-stops the child and every helper in its group, so a
// process that ignores the graceful signal cannot outlive the deadline.
func killProcessGroup(cmd *exec.Cmd) error {
	return signalGroup(cmd, syscall.SIGKILL)
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	killErr := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(killErr, syscall.ESRCH) {
		return nil
	}
	return killErr
}
