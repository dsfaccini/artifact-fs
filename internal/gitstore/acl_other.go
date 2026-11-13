//go:build !linux && !windows

package gitstore

// hasACL reports false: only Linux shows an ACL as a separate attribute.
func hasACL(string) bool { return false }
