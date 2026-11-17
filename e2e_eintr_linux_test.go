package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A rename of a directory whose blobs are cold hydrates every file below it.
// A signal that reaches the thread waiting in rename(2) interrupts the op, and
// the mount answers EINTR and changes nothing. The test process serves the
// mount, so its threads take such signals, and renameInMount must retry. The
// fetch that hydrates the blobs waits for release, so the signal lands while
// the rename waits for it. The rename runs in the test process, on a locked
// thread, because the subject is a signal to the thread in rename(2), which
// no child process can take in its place.
func TestE2ERenameRetriesAnInterruptedHydration(t *testing.T) {
	skipIfOverMountBudget(t, 1)
	repo := newMountedE2ERepo(t)

	markers := t.TempDir()
	started, release := filepath.Join(markers, "started"), filepath.Join(markers, "release")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	uploadPack := filepath.Join(markers, "upload-pack")
	// The wait for release ends after about 60 s, so a regression leaves no
	// fetch running.
	script := "#!/bin/sh\n: > " + quote(started) + "\ni=0\nwhile [ ! -e " + quote(release) + " ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done\nexec git-upload-pack \"$@\"\n"
	if err := os.WriteFile(uploadPack, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })
	run(t, "", "git", "--git-dir", filepath.Join(repo.root, "repos", repoName, "git"), "config", "remote.origin.uploadpack", uploadPack)

	source := filepath.Join(repo.mountPath, "packages", "wrangler")
	destination := filepath.Join(repo.mountPath, "packages", "wrangler-pending")
	retries := renameEINTRRetries.Load()
	tids := make(chan int, 1)
	renamed := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- unix.Gettid()
		renamed <- renameInMount(source, destination)
	}()
	tid := <-tids
	await := func(what string, ready func() bool) {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for !ready() {
			select {
			case err := <-renamed:
				t.Fatalf("the rename returned %v before %s", err, what)
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	await("the fetch that hydrates its blobs started", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if err := unix.Tgkill(os.Getpid(), tid, unix.SIGURG); err != nil {
		t.Fatal(err)
	}
	// The retried rename waits for the same fetch and holds the directory
	// lock in packages, so nothing reads the tree before release.
	await("the rename retried after EINTR", func() bool { return renameEINTRRetries.Load() > retries })
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-renamed:
		if err != nil {
			t.Fatalf("rename once the fetch ran: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for the rename once the fetch ran")
	}
	assertPathExists(t, filepath.Join(destination, "nested", "deep", "config.json"))
	assertPathMissing(t, source)
}
