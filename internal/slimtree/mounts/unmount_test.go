package mounts

import (
	"errors"
	"testing"
)

func TestUnmountRefusesAMountArtifactFSDidNotMake(t *testing.T) {
	t.Parallel()
	if id, err := Identify("/"); !errors.Is(err, ErrUnrelatedMount) {
		t.Fatalf("Identify(/) = %q, %v; want ErrUnrelatedMount", id, err)
	}
	if err := Unmount(t.Context(), "/", "any"); !errors.Is(err, ErrUnrelatedMount) {
		t.Fatalf("Unmount(/) = %v; want ErrUnrelatedMount", err)
	}
	dir := t.TempDir()
	if id, err := Identify(dir); id != "" || err != nil {
		t.Fatalf("Identify of a directory with no mount = %q, %v; want none", id, err)
	}
	if err := Unmount(t.Context(), dir, "any"); err != nil {
		t.Fatalf("Unmount of a directory with no mount = %v; want nothing to do", err)
	}
}
