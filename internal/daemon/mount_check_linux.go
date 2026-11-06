package daemon

import (
	"os"
	"os/exec"
	"strings"
)

// mountTableEntry reports whether the given path is an active mount point,
// and what the mount table lists for it. Reads /proc/mounts first (fast, no
// subprocess), falls back to mount(8).
func mountTableEntry(path string) (mountEntry, bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return mountEntry{}, false, err
		}
		entry, found := matchMountOutput(string(out), path)
		return entry, found, nil
	}
	entry, found := matchProcMounts(string(data), path)
	return entry, found, nil
}

// procMountsEscaper undoes the octal escapes /proc/mounts writes for these bytes in a path.
var procMountsEscaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// matchProcMounts checks /proc/mounts where each line is:
// <device> <mountpoint> <fstype> <options> <dump> <pass>
// A later line for the same path is mounted on top of an earlier one.
func matchProcMounts(output, path string) (mountEntry, bool) {
	var entry mountEntry
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && procMountsEscaper.Replace(fields[1]) == path {
			entry = mountEntry{source: procMountsEscaper.Replace(fields[0]), fstype: fields[2]}
			found = true
		}
	}
	return entry, found
}

// matchMountOutput checks mount(8) output where each line is:
// <device> on <mountpoint> type <fstype> (<options>)
func matchMountOutput(output, path string) (mountEntry, bool) {
	var entry mountEntry
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		device, rest, ok := strings.Cut(line, " on "+path+" type ")
		if !ok {
			continue
		}
		fstype, _, _ := strings.Cut(rest, " ")
		entry = mountEntry{source: device, fstype: fstype}
		found = true
	}
	return entry, found
}
