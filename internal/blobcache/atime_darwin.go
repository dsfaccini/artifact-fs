package blobcache

import (
	"os"
	"syscall"
	"time"
)

// accessTime returns info's access time, or its modification time when the
// platform reports none.
func accessTime(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atimespec.Unix())
	}
	return info.ModTime()
}
