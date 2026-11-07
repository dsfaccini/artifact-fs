//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// On FSKit mounts, artifact-fs checks a file's owner permission bits itself
// when the file is opened or truncated. The subtests share one mount: `run`
// skips a shared-mount test that changes the mount on FSKit.
func TestE2EFSKitMountChecksOwnerPermissions(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	if !e2eMountsUseFSKit() {
		t.Skip("skipping: artifact-fs checks permissions itself only on FSKit mounts, and this run does not mount through FSKit")
	}
	repo := newMountedE2ERepo(t)
	if !mountedThroughFSKit(t, repo.mountPath) {
		t.Fatalf("%s is not an FSKit mount", repo.mountPath)
	}
	// newFile writes content to a new file in the mount and chmods it to mode.
	newFile := func(t *testing.T, name, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(repo.mountPath, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	requireContent := func(t *testing.T, path, want string) {
		t.Helper()
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", filepath.Base(path), got, err, want)
		}
	}
	requireEACCES := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, syscall.EACCES) {
			t.Errorf("%s = %v, want EACCES", what, err)
		}
	}

	t.Run("new opens of a read-only file", func(t *testing.T) {
		path := newFile(t, "read-only.txt", "abc", 0o444)
		for name, flag := range map[string]int{"O_WRONLY": os.O_WRONLY, "O_RDWR": os.O_RDWR} {
			f, err := os.OpenFile(path, flag, 0)
			if err == nil {
				f.Close()
			}
			requireEACCES(t, "open "+name, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open O_RDONLY = %v", err)
		}
		f.Close()
	})

	t.Run("O_TRUNC open of a read-only file", func(t *testing.T) {
		path := newFile(t, "read-only-trunc.txt", "abc", 0o444)
		f, err := os.OpenFile(path, os.O_RDONLY|os.O_TRUNC, 0)
		if err == nil {
			f.Close()
		}
		requireEACCES(t, "open O_RDONLY|O_TRUNC", err)
		requireContent(t, path, "abc")
	})

	t.Run("second writer after a chmod", func(t *testing.T) {
		path := newFile(t, "second-writer.txt", "abc", 0o644)
		first, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
		second, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			second.Close()
		}
		requireEACCES(t, "second open O_WRONLY", err)
		if _, err := first.WriteAt([]byte("def"), 3); err != nil {
			t.Fatalf("write through the earlier descriptor = %v", err)
		}
		if err := first.Truncate(2); err != nil {
			t.Fatalf("ftruncate through the earlier descriptor = %v", err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		requireContent(t, path, "ab")
	})

	t.Run("truncate of a read-only file", func(t *testing.T) {
		path := newFile(t, "truncate.txt", "abc", 0o444)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		// macOS refuses ftruncate(2) through a read-only descriptor before the
		// call reaches the file system, so only the refusal is asserted.
		if err := f.Truncate(0); err == nil {
			t.Error("ftruncate through a read-only descriptor succeeded")
		}
		requireEACCES(t, "truncate(path)", os.Truncate(path, 0))
		requireContent(t, path, "abc")
	})

	t.Run("create a read-only file and write it", func(t *testing.T) {
		path := filepath.Join(repo.mountPath, "created-read-only.txt")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o444)
		if err != nil {
			t.Fatalf("open O_CREAT|O_WRONLY 0444 = %v", err)
		}
		if _, err := f.Write([]byte("abc")); err != nil {
			f.Close()
			t.Fatalf("write = %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close = %v", err)
		}
		requireContent(t, path, "abc")
	})

	t.Run("cp -p of a read-only file", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "source.txt")
		if err := os.WriteFile(src, []byte("abc"), 0o444); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(repo.mountPath, "copied-read-only.txt")
		// -X skips extended attributes, which the mount rejects (README).
		if out, err := exec.Command("cp", "-p", "-X", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("cp -p -X = %v: %s", err, out)
		}
		requireContent(t, dst, "abc")
		if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o444 {
			t.Fatalf("copy = %v, %v; want mode 0444", st, err)
		}
	})
}

// mountedThroughFSKit reports whether the mount table lists path as an FSKit
// mount.
func mountedThroughFSKit(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.Command("mount").Output()
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	paths := []string{path, "/private" + path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		paths = append(paths, resolved)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		for _, p := range paths {
			if strings.Contains(line, " on "+p+" (") && strings.Contains(line, "fskit") {
				return true
			}
		}
	}
	return false
}
