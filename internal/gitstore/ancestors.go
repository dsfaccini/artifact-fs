package gitstore

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// groupFile and passwdFile are the databases privateGroup reads. Tests
// replace them.
var groupFile, passwdFile = "/etc/group", "/etc/passwd"

// privateGroup reports whether uid is the only member of group gid, as
// Debian's OpenSSH decides: every user whose primary group is gid is uid, the
// group lists no member but uid's name, and uid is a member. It reads only
// groupFile and passwdFile, and returns false when either names an entry from
// another source, such as NIS, or lacks gid or uid.
func privateGroup(gid, uid uint32) bool {
	gidText, uidText := strconv.FormatUint(uint64(gid), 10), strconv.FormatUint(uint64(uid), 10)
	users, ok := readDatabase(passwdFile)
	if !ok {
		return false
	}
	name, members := "", 0
	for _, user := range users {
		if len(user) < 4 {
			continue
		}
		if user[2] == uidText {
			name = user[0]
		}
		if user[3] == gidText {
			if user[2] != uidText {
				return false
			}
			members++
		}
	}
	groups, ok := readDatabase(groupFile)
	if !ok || name == "" {
		return false
	}
	for _, group := range groups {
		if len(group) < 4 || group[2] != gidText {
			continue
		}
		if group[3] != "" {
			if group[3] != name {
				return false
			}
			members++
		}
		return members > 0
	}
	return false
}

// readDatabase returns the colon-separated fields of each entry in an
// /etc/passwd or /etc/group file. It returns false when the file cannot be
// read or holds a NIS compat entry, which starts with + or -.
func readDatabase(path string) ([][]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var entries [][]string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			return nil, false
		}
		entries = append(entries, strings.Split(line, ":"))
	}
	return entries, scanner.Err() == nil
}
