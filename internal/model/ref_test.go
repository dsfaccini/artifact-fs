package model

import "testing"

func TestCanonicalRefNamesBranchesInFullAndKeepsOtherNamespaces(t *testing.T) {
	for ref, want := range map[string]string{
		"main":                     "refs/heads/main",
		" refs/heads/main ":        "refs/heads/main",
		"origin/main":              "refs/heads/main",
		"refs/remotes/origin/main": "refs/heads/main",
		"refs/heads/origin/main":   "refs/heads/origin/main",
		"refs/tags/release":        "refs/tags/release",
		"refs/pull/1/head":         "refs/pull/1/head",
	} {
		if got := CanonicalRef(ref); got != want {
			t.Errorf("CanonicalRef(%q) = %q, want %q", ref, got, want)
		}
	}
}
