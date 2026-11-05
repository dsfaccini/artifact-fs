package daemon

import (
	"os/exec"
	"strings"
)

// mountTableHas reports whether the given path is an active mount point.
// On macOS, mount(8) output format is: <device> on <path> (<options>)
// We match the " on <path> (" pattern to avoid substring false positives.
// macOS reports /private/tmp even for /tmp paths.
func mountTableHas(path string) (bool, error) {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, " on "+path+" (") || strings.Contains(line, " on /private"+path+" (") {
			return true, nil
		}
	}
	return false, nil
}
