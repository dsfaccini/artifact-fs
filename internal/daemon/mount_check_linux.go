package daemon

import (
	"os"
	"os/exec"
	"strings"
)

// mountTableSource reports whether the given path is an active mount point,
// and the source the mount table names for it. Reads /proc/mounts first (fast,
// no subprocess), falls back to mount(8).
func mountTableSource(path string) (string, bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return "", false, err
		}
		source, found := matchMountOutput(string(out), path)
		return source, found, nil
	}
	source, found := matchProcMounts(string(data), path)
	return source, found, nil
}

// procMountsEscaper undoes the octal escapes /proc/mounts writes for these bytes in a path.
var procMountsEscaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// matchProcMounts checks /proc/mounts where each line is:
// <device> <mountpoint> <fstype> <options> <dump> <pass>
// A later line for the same path is mounted on top of an earlier one.
func matchProcMounts(output, path string) (string, bool) {
	source, found := "", false
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && procMountsEscaper.Replace(fields[1]) == path {
			source, found = procMountsEscaper.Replace(fields[0]), true
		}
	}
	return source, found
}

// matchMountOutput checks mount(8) output where each line is:
// <device> on <mountpoint> type <fstype> (<options>)
func matchMountOutput(output, path string) (string, bool) {
	source, found := "", false
	for line := range strings.SplitSeq(output, "\n") {
		if device, _, ok := strings.Cut(line, " on "+path+" type "); ok {
			source, found = device, true
		}
	}
	return source, found
}
