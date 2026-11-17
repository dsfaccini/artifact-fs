package mounts

import (
	"testing"

	"golang.org/x/sys/unix"
)

func zombie(t *testing.T, pid int) bool {
	t.Helper()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil || len(procs) == 0 {
		t.Fatalf("kern.proc.pid %d = %v, %v", pid, procs, err)
	}
	return procs[0].Proc.P_stat == szomb
}
