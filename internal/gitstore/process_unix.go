//go:build !windows

package gitstore

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// configureCommandSession starts cmd in a new session, which has no
// controlling terminal. cmd still leads its own process group.
func configureCommandSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := cmd.Process.Pid
	err := cmd.Process.Kill()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return err
}
