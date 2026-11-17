package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/dsfaccini/fuse"
)

// System returns the Probe that reads this machine.
func System() Probe {
	return Probe{
		LookPath:  exec.LookPath,
		Stat:      os.Stat,
		ReadWrite: readWrite,
		ReadFile:  os.ReadFile,
		Output: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()
			// Output keeps a failed command's stderr in its ExitError, whose
			// message is only the exit status.
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				if stderr := bytes.TrimSpace(exitErr.Stderr); len(stderr) > 0 {
					err = fmt.Errorf("%w: %s", err, stderr)
				}
			}
			return string(out), err
		},
		EUID:           os.Geteuid(),
		OSVersion:      osVersion,
		FSKitAvailable: fuse.FSKitAvailable,
		APFS:           apfs,
	}
}

// readWrite opens path as the mount opens /dev/fuse. Unlike access(2), an
// open reaches the device's driver, so a /dev/fuse node without the fuse
// module fails it.
func readWrite(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
