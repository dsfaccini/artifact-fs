package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// skipIfNoFUSE skips the test if the FUSE kernel module is not available.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("skipping: /dev/fuse not available")
	}
}

// configGitSafeDir adds a safe.directory entry for the mount path.
func configGitSafeDir(t *testing.T, path string) {
	t.Helper()
	exec.Command("git", "config", "--global", "--add", "safe.directory", path).Run()
}

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

// primeE2EMountPolling caches FUSE's unsupported poll response before Go can
// block registering a descriptor with raw epoll_ctl. See https://github.com/golang/go/issues/21014
// and https://github.com/hanwen/go-fuse/commit/4f10e248ebabd3cdf9c0aa3ae58fd15235f82a79.
func primeE2EMountPolling(t *testing.T, mountPath string) {
	t.Helper()
	path := filepath.Join(mountPath, ".git")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	for errors.Is(err, unix.EINTR) {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		t.Fatalf("open synthesized .git for FUSE poll priming: %v", err)
	}
	defer func() {
		if err := unix.Close(fd); err != nil {
			t.Errorf("close synthesized .git after FUSE poll priming: %v", err)
		}
	}()

	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLPRI | unix.POLLOUT}}
	for {
		_, err := unix.Poll(fds, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatalf("poll synthesized .git for FUSE poll priming: %v", err)
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			t.Fatal("poll synthesized .git returned POLLNVAL")
		}
		return
	}
}

func TestE2EMountPollingWithSingleProcessor(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	repo := newMountedE2ERepo(t)
	readmePath := filepath.Join(repo.mountPath, "README.md")
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readmePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("README after write = %q, want %q", got, content)
	}
}
