package mounts

import (
	"fmt"
	"strconv"
	"strings"
)

// The parsers build on every platform, so build-test and darwin-test both
// run their tests.

// mountinfoEntry is one line of /proc/self/mountinfo.
type mountinfoEntry struct {
	id           uint64
	major, minor uint32
	point        string
	fstype       string
	source       string
}

// parseMountinfo parses /proc/self/mountinfo. Each line is: mount ID, parent
// ID, major:minor, root, mount point, options, optional fields, "-", file
// system type, source, super options.
func parseMountinfo(text string) ([]mountinfoEntry, error) {
	var entries []mountinfoEntry
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 6 || sep < 0 || sep+2 >= len(fields) {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed mountinfo line %q: %w", line, err)
		}
		major, minor, err := parseDevice(fields[2], 10)
		if err != nil {
			return nil, fmt.Errorf("malformed mountinfo line %q: %w", line, err)
		}
		entries = append(entries, mountinfoEntry{id: id, major: major, minor: minor, point: unescapeOctal(fields[4]), fstype: fields[sep+1], source: unescapeOctal(fields[sep+2])})
	}
	return entries, nil
}

// parseDevice parses major:minor in base.
func parseDevice(s string, base int) (major, minor uint32, err error) {
	ma, mi, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("device %q is not major:minor", s)
	}
	m, err := strconv.ParseUint(ma, base, 32)
	if err != nil {
		return 0, 0, err
	}
	n, err := strconv.ParseUint(mi, base, 32)
	return uint32(m), uint32(n), err
}

// unescapeOctal undoes the \ooo escapes the kernel writes for a space, a
// tab, a newline and a backslash in a mount table path.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountAt returns the mount at point: the last entry there, which is
// mounted on top of the earlier ones.
func mountAt(entries []mountinfoEntry, point string) (mountinfoEntry, bool) {
	var found mountinfoEntry
	ok := false
	for _, e := range entries {
		if e.point == point {
			found, ok = e, true
		}
	}
	return found, ok
}

// holdingMount returns the mount that holds path: the deepest mount point at
// or above path, and of the mounts stacked there, the last.
func holdingMount(entries []mountinfoEntry, path string) (mountinfoEntry, bool) {
	var found mountinfoEntry
	depth := -1
	for _, e := range entries {
		if d := len(e.point); under(path, e.point) && d >= depth {
			found, depth = e, d
		}
	}
	return found, depth >= 0
}

// statFields returns the fields of /proc/<pid>/stat from field 3, the state,
// on. The command in field 2 can hold spaces and parentheses, so they start
// after its last ")".
func statFields(text string) ([]string, error) {
	i := strings.LastIndexByte(text, ')')
	if i < 0 {
		return nil, fmt.Errorf("malformed stat %q", text)
	}
	fields := strings.Fields(text[i+1:])
	// Field 22, the start time, is the last one read.
	if len(fields) < 20 {
		return nil, fmt.Errorf("malformed stat %q", text)
	}
	return fields, nil
}

// parseLsof returns the holders of path that the output of
// lsof -F pcnRD names: each process with a file named path or a path below
// it on device dev, except ignore and its descendants. lsof prints a process
// set, p then c and R, before the file sets of the process, each f then D
// and n. dev keeps the low 32 bits, as lsof prints them.
func parseLsof(out, path string, dev uint64, ignore int) []Holder {
	parents := map[int]int{}
	commands := map[int]string{}
	var pids []int
	held := map[int]bool{}
	pid := 0
	fileDev, haveDev := uint64(0), false
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		tag, value := line[0], line[1:]
		switch tag {
		case 'p':
			pid, _ = strconv.Atoi(value)
		case 'R':
			parents[pid], _ = strconv.Atoi(value)
		case 'c':
			commands[pid] = value
		case 'f':
			haveDev = false
		case 'D':
			d, err := strconv.ParseUint(value, 0, 64)
			fileDev, haveDev = d&0xffffffff, err == nil
		case 'n':
			if haveDev && fileDev == dev && under(value, path) && !held[pid] {
				held[pid] = true
				pids = append(pids, pid)
			}
		}
	}
	return holders(pids, ignore, parents, commands)
}
