package daemon

import (
	"runtime"
	"testing"
)

// The mount table names each volume's type, which tells a macFUSE mount from
// any other.
func TestMountTableReadsTheMountSourceAndType(t *testing.T) {
	entry, found, err := mountTableEntry("/")
	if err != nil || !found || entry.fstype == "" || entry.source == "" || ownsLeftover(runtime.GOOS, entry, "repo") {
		t.Fatalf("mount table entry for / = %+v, %t, %v; want a listed volume that is not macFUSE", entry, found, err)
	}
}
