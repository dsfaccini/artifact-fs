package model

import "strings"

// BranchName returns the branch on the remote that ref names: X for
// refs/heads/X, refs/remotes/origin/X, origin/X or a bare name X, and "" for
// any other refs/ ref.
func BranchName(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/remotes/origin/", "origin/"} {
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			return after
		}
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
	return ref
}

// CanonicalRef returns the full name of ref on the remote: refs/heads/X for a
// ref that names branch X, and ref itself for any other refs/ ref.
func CanonicalRef(ref string) string {
	if branch := BranchName(ref); branch != "" {
		return "refs/heads/" + branch
	}
	return strings.TrimSpace(ref)
}
