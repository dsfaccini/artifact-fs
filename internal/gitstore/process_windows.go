//go:build windows

package gitstore

import (
	"io/fs"
	"os"
	"os/exec"
)

func configureCommandProcessGroup(_ *exec.Cmd) {}

func configureCommandSession(_ *exec.Cmd) {}

// terminateCommandProcessGroup kills cmd: Windows has no SIGTERM.
func terminateCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

// privateToEUID reports true. Windows file modes carry no owner bits, and the
// default temp directory lies in the user's profile.
func privateToEUID(fs.FileInfo) bool { return true }
