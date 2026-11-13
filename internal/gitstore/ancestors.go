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
// Debian's OpenSSH decides: every user whose primary group is gid is uid,
// every group entry for gid lists no member but uid's name, and uid is a
// member. It reads only groupFile and passwdFile, compares IDs as numbers,
// and returns false when either file holds an ID that is not a number or an
// entry from another source, such as NIS, or lacks gid or uid.
func privateGroup(gid, uid uint32) bool {
	users, ok := readDatabase(passwdFile)
	if !ok {
		return false
	}
	name, members := "", 0
	for _, user := range users {
		if len(user) < 4 {
			continue
		}
		userID, uidOK := parseID(user[2])
		groupID, gidOK := parseID(user[3])
		if !uidOK || !gidOK {
			return false
		}
		if userID == uid {
			name = user[0]
		}
		if groupID == gid {
			if userID != uid {
				return false
			}
			members++
		}
	}
	groups, ok := readDatabase(groupFile)
	if !ok || name == "" {
		return false
	}
	found := false
	for _, group := range groups {
		if len(group) < 4 {
			continue
		}
		groupID, ok := parseID(group[2])
		if !ok {
			return false
		}
		if groupID != gid {
			continue
		}
		found = true
		if group[3] != "" {
			if group[3] != name {
				return false
			}
			members++
		}
	}
	return found && members > 0
}

// parseID parses a decimal user or group ID.
func parseID(field string) (uint32, bool) {
	id, err := strconv.ParseUint(field, 10, 32)
	return uint32(id), err == nil
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
