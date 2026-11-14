package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

// wedgedMountRecovery is how Linux clears a stalled mount: a lazy unmount
// detaches it without waiting for the stuck FUSE requests.
const wedgedMountRecovery = "clear it with fusermount3 -uz <path>, or umount -l <path> as root"

// skipIfNoFUSE skips the test if the FUSE kernel module is not available, and
// fails when ARTIFACT_FS_FUSE_BACKEND is invalid.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	skipIfMountWedged(t)
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("skipping: /dev/fuse not available")
	}
	if _, err := fusefs.MountBackend(); err != nil {
		t.Fatal(err)
	}
}

// mountsThroughFSKit reports false: FSKit is a macOS backend.
func mountsThroughFSKit() bool { return false }

// isMounted checks whether the given path is a FUSE mount point by reading /proc/mounts.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Fall back to mount(8)
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), path)
	}
	return strings.Contains(string(data), path)
}

// singleProcessorChildEnv marks the child process that
// TestE2EMountPollingWithSingleProcessor mounts in.
const singleProcessorChildEnv = "AFS_E2E_SINGLE_PROCESSOR_CHILD"

// singleProcessorBound is how long the child may take to mount and read.
const singleProcessorBound = 60 * time.Second

// Package os registers each file it opens with epoll, and epoll_ctl on a FUSE
// file sends FUSE_POLL to the server. Go makes that call holding its P, so at
// GOMAXPROCS(1) the serving process cannot answer it
// (https://github.com/golang/go/issues/21014), unless the mount already
// answered a FUSE_POLL with ENOSYS, as the fuse library's Mount arranges. A
// child mounts and reads the synthesized .git at GOMAXPROCS(1): its open
// hydrates and prefetches nothing, so the server forks no git meanwhile
// (e2e_files_test.go). A deadlock holds the child's only P, which no timer in
// the child can then preempt, so the test bounds the child from outside.
func TestE2EMountPollingWithSingleProcessor(t *testing.T) {
	if os.Getenv(singleProcessorChildEnv) == "1" {
		runtime.GOMAXPROCS(1)
		repo := newMountedE2ERepo(t)
		got, err := os.ReadFile(filepath.Join(repo.mountPath, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(got), "gitdir: ") {
			t.Fatalf(".git = %q, want a gitfile", got)
		}
		return
	}
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	// The child's state root holds its control socket, so its temp dir stays
	// short.
	tmp, err := os.MkdirTemp("", "afs-gomaxprocs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeAllWithRetry(t, tmp) })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestE2EMountPollingWithSingleProcessor$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), singleProcessorChildEnv+"=1", "TMPDIR="+tmp)
	var out synchronizedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child that mounted and read .git at GOMAXPROCS(1): %v\n%s", err, out.String())
		}
		return
	case <-time.After(singleProcessorBound):
	}
	// A process waiting on its own mount ends only once the mount's FUSE
	// connection is aborted.
	mounts := mountsUnder(t, tmp)
	for _, m := range mounts {
		if err := os.WriteFile(filepath.Join("/sys/fs/fuse/connections", m.conn, "abort"), []byte("1"), 0o200); err != nil {
			t.Logf("abort the FUSE connection of %s: %v", m.path, err)
		}
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("child did not exit within 10s of SIGKILL")
	}
	for _, m := range mounts {
		if out, err := exec.Command("fusermount3", "-uz", m.path).CombinedOutput(); err != nil {
			t.Errorf("fusermount3 -uz %s: %v: %s", m.path, err, out)
		}
	}
	t.Fatalf("child that mounts and reads .git at GOMAXPROCS(1) did not finish within %v: the mount deadlocked\n%s", singleProcessorBound, out.String())
}

type fuseMount struct{ path, conn string }

// mountsUnder returns each FUSE mount below dir, with the connection ID under
// /sys/fs/fuse/connections that its device number names.
func mountsUnder(t *testing.T, dir string) []fuseMount {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	var mounts []fuseMount
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.HasPrefix(fields[4], dir+"/") {
			continue
		}
		_, minor, ok := strings.Cut(fields[2], ":")
		if ok {
			mounts = append(mounts, fuseMount{path: fields[4], conn: minor})
		}
	}
	return mounts
}
