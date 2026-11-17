package apfsclone

import "golang.org/x/sys/unix"

func clonefile(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
