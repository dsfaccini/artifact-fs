//go:build !darwin

package apfsclone

import "golang.org/x/sys/unix"

// clonefile fails: only macOS has clonefile(2).
func clonefile(src, dst string) error { return unix.ENOTSUP }
