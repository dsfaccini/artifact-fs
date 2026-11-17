package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// ReadWrite opens the path for reading and writing, so it fails where an open
// fails though access(2) passes, as for a directory.
func TestSystemReadWriteOpensForReadingAndWriting(t *testing.T) {
	dir := t.TempDir()
	rw, ro := filepath.Join(dir, "rw"), filepath.Join(dir, "ro")
	if err := os.WriteFile(rw, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ro, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	readWrite := System().ReadWrite
	if err := readWrite(rw); err != nil {
		t.Errorf("ReadWrite of a 0600 file: %v", err)
	}
	if err := readWrite(dir); err == nil {
		t.Error("ReadWrite of a directory succeeded")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	if err := readWrite(ro); err == nil {
		t.Error("ReadWrite of a 0400 file succeeded")
	}
}

// A failed command's error carries its stderr, which says why it failed.
func TestSystemOutputKeepsStderr(t *testing.T) {
	_, err := System().Output("sh", "-c", "echo out; echo 'fatal: broken install' >&2; exit 3")
	if err == nil || err.Error() != "exit status 3: fatal: broken install" {
		t.Fatalf("Output of a failing command = %v, want its exit status and stderr", err)
	}
}
