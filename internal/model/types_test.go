package model

import "testing"

// Every store keys a path by CleanPath, so each spelling of a path must clean
// to one key, and the root to ".".
func TestCleanPathGivesEachPathOneKey(t *testing.T) {
	for path, want := range map[string]string{
		"":        ".",
		".":       ".",
		"/":       ".",
		"a/..":    ".",
		"a":       "a",
		"/a/b/":   "a/b",
		"a//b":    "a/b",
		"./a/./b": "a/b",
		"a/../b":  "b",
		"/../a":   "a",
	} {
		if got := CleanPath(path); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", path, got, want)
		}
	}
}
