package doctor

import (
	"os"
	"os/exec"

	"github.com/dsfaccini/fuse"
	"golang.org/x/sys/unix"
)

// System returns the Probe that reads this machine.
func System() Probe {
	return Probe{
		LookPath:  exec.LookPath,
		Stat:      os.Stat,
		ReadWrite: func(path string) error { return unix.Access(path, unix.R_OK|unix.W_OK) },
		ReadFile:  os.ReadFile,
		Output: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()
			return string(out), err
		},
		EUID:           os.Geteuid(),
		OSVersion:      osVersion,
		FSKitAvailable: fuse.FSKitAvailable,
		APFS:           apfs,
	}
}
