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
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			return nil
		}
		return killErr
	}
}
