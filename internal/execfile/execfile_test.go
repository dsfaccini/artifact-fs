package execfile

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A fork while Write holds the file open would hand the child the write
// descriptor, and an exec of the file would fail with ETXTBSY until the child
// execs.
func TestWriteExcludesForksWhileTheFileIsOpen(t *testing.T) {
	ran, forkable := false, false
	testHookWhileOpen = func() {
		ran = true
		if syscall.ForkLock.TryLock() {
			forkable = true
			syscall.ForkLock.Unlock()
		}
	}
	t.Cleanup(func() { testHookWhileOpen = nil })
	path := filepath.Join(t.TempDir(), "script")
	if err := Write(path, []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Write never ran the hook")
	}
	if forkable {
		t.Fatal("a fork could start while Write held the file open for writing")
	}
	out, err := exec.Command(path).Output()
	if err != nil || string(out) != "ran\n" {
		t.Fatalf("script output = %q, %v, want %q", out, err, "ran\n")
	}
}
