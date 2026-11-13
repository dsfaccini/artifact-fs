//go:build windows

package gitstore

// privateAncestors reports true. Windows file modes carry no owner bits.
func privateAncestors(string) bool { return true }
