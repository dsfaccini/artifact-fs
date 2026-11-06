package daemon

import (
	"os"
	"os/exec"
	"strings"
)

// mountTableHas reports whether the given path is an active mount point.
// Reads /proc/mounts first (fast, no subprocess), falls back to mount(8).
func mountTableHas(path string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false, err
		}
		return matchMountOutput(string(out), path), nil
	}
	return matchProcMounts(string(data), path), nil
}

// procMountsEscaper undoes the octal escapes /proc/mounts writes for these bytes in a path.
var procMountsEscaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// matchProcMounts checks /proc/mounts where each line is:
// <device> <mountpoint> <fstype> <options> <dump> <pass>
func matchProcMounts(output, path string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && procMountsEscaper.Replace(fields[1]) == path {
			return true
		}
	}
	return false
}

// matchMountOutput checks mount(8) output where each line is:
// <device> on <mountpoint> type <fstype> (<options>)
func matchMountOutput(output, path string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, " on "+path+" type ") {
			return true
		}
	}
	return false
}
