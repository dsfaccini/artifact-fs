//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/meta"
)

// fskitMountBudget caps the FUSE mounts one e2e run starts on FSKit. macFUSE's
// FSKit extension can stop being found or enabled after a series of mounts in
// one session (https://github.com/macfuse/macfuse/issues/1232).
const fskitMountBudget = 8

// e2eMountsUseFSKit reports whether e2e mounts go through macFUSE's FSKit backend.
var e2eMountsUseFSKit = mountsThroughFSKit

// skipIfOverMountBudget skips t on FSKit when the mounts t starts would take
// the run past fskitMountBudget. e2eMountsStarted is the meter.
func skipIfOverMountBudget(t *testing.T, mounts int64) {
	t.Helper()
	if !e2eMountsUseFSKit() {
		return
	}
	if started := e2eMountsStarted.Load(); started+mounts > fskitMountBudget {
		t.Skipf("skipping: this test starts %d FUSE mounts, and the run has started %d of its %d-mount FSKit budget (https://github.com/macfuse/macfuse/issues/1232)",
			mounts, started, fskitMountBudget)
	}
}

// skipIfFSKitHEADMove skips t on FSKit, where mover moves HEAD. A HEAD move can
// shrink a file under the size the kernel holds, and FSKit pads the short read
// with bytes that are not file data.
func skipIfFSKitHEADMove(t *testing.T, mover string) {
	t.Helper()
	if e2eMountsUseFSKit() {
		t.Skipf("skipping on FSKit: %s moves HEAD, and FSKit pads a read that comes back short with bytes that are not file data (https://github.com/macfuse/macfuse/issues/1231)", mover)
	}
}

// useFSKitForTest makes e2eMountsUseFSKit report fskit until t ends.
func useFSKitForTest(t *testing.T, fskit bool) {
	t.Helper()
	previous := e2eMountsUseFSKit
	e2eMountsUseFSKit = func() bool { return fskit }
	t.Cleanup(func() { e2eMountsUseFSKit = previous })
}

func TestMountBudgetSkipsTestsPastItOnFSKit(t *testing.T) {
	previous := e2eMountsStarted.Swap(fskitMountBudget - 1)
	t.Cleanup(func() { e2eMountsStarted.Store(previous) })

	skippedWith := func(mounts int64) bool {
		t.Helper()
		skipped := false
		t.Run("mounting test", func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			skipIfOverMountBudget(t, mounts)
		})
		return skipped
	}

	useFSKitForTest(t, true)
	if skippedWith(1) {
		t.Fatal("a test that ends the run at the FSKit mount budget skipped")
	}
	if !skippedWith(2) {
		t.Fatal("a test that would take the run past the FSKit mount budget ran")
	}

	useFSKitForTest(t, false)
	if skippedWith(2) {
		t.Fatal("the FSKit mount budget skipped a test on another backend")
	}
}

func TestSharedMountSkipsTestsThatNeedRestoreOnFSKit(t *testing.T) {
	previous := wedgedMount.Swap(nil)
	t.Cleanup(func() { wedgedMount.Store(previous) })

	// A test that runs marks the mount wedged, so its release restores nothing.
	ran := func(register func(*sharedE2ERepo, *testing.T, string, func(*testing.T, *mountedE2ERepo))) bool {
		t.Helper()
		wedgedMount.Store(nil)
		called := false
		register(&sharedE2ERepo{}, t, "shared test", func(*testing.T, *mountedE2ERepo) {
			called = true
			_ = markMountWedged("/shared/mount")
		})
		return called
	}

	useFSKitForTest(t, true)
	if ran((*sharedE2ERepo).run) {
		t.Fatal("a shared-mount test that needs the restore ran on FSKit")
	}
	if !ran((*sharedE2ERepo).runReadOnly) {
		t.Fatal("a read-only shared-mount test skipped on FSKit")
	}

	useFSKitForTest(t, false)
	if !ran((*sharedE2ERepo).run) {
		t.Fatal("a shared-mount test that needs the restore skipped on another backend")
	}
}

// TestSharedMountFailsReadOnlyTestThatChangesMount runs
// TestSharedMountReadOnlyTestChangesMountHelper in a child process, because the
// failure it asserts fails the test that hits it.
func TestSharedMountFailsReadOnlyTestThatChangesMount(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run", "^TestSharedMountReadOnlyTestChangesMountHelper$")
	cmd.Env = append(os.Environ(), "AFS_SHARED_READ_ONLY_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a read-only shared-mount test that changed the mount passed:\n%s", out)
	}
	if !strings.Contains(string(out), "registered with runReadOnly but changed the mount: overlay entries") {
		t.Fatalf("read-only shared-mount test failed for another reason:\n%s", out)
	}
}

func TestSharedMountReadOnlyTestChangesMountHelper(t *testing.T) {
	if os.Getenv("AFS_SHARED_READ_ONLY_HELPER") != "1" {
		t.Skip("helper for TestSharedMountFailsReadOnlyTestThatChangesMount")
	}
	previous := wedgedMount.Swap(nil)
	t.Cleanup(func() { wedgedMount.Store(previous) })

	root := t.TempDir()
	// gitDir does not exist, so a restore that runs anyway fails without
	// touching a repository.
	s := &sharedE2ERepo{repo: &mountedE2ERepo{root: root}, gitDir: filepath.Join(root, "missing-git")}
	s.runReadOnly(t, "writes a file", func(t *testing.T, repo *mountedE2ERepo) {
		db, err := meta.OpenDB(filepath.Join(repo.root, "overlays", repoName, "meta.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE overlay_entries(path TEXT, kind TEXT); INSERT INTO overlay_entries VALUES('written.txt', 'file')`); err != nil {
			t.Fatal(err)
		}
	})
}
