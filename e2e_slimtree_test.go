//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/slimtree/mounts"
)

// slimtreeBinary returns the slimtree binary e2e tests drive:
// $AFS_E2E_SLIMTREE, which CI builds before it runs the suite as root, where
// no go is on PATH, or else one built from ./cmd/slimtree.
func slimtreeBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("AFS_E2E_SLIMTREE"); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), "slimtree")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/slimtree").CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/slimtree: %v\n%s", err, out)
	}
	return bin
}

// slimtreeRecord is the part of an artifactfs worktree's record this test
// reads.
type slimtreeRecord struct {
	DaemonPID   int    `json:"daemon_pid"`
	DaemonToken string `json:"daemon_start_token"`
}

// TestE2ESlimtreeLifecycle drives the slimtree binary through an artifactfs
// worktree's life: new mounts it and runs its post-checkout hook, stop
// refuses while another process uses the mount and then unmounts it, resume
// mounts it again with its changes, and resume brings it back after its
// daemon was killed. Its daemons run in slimtree processes, so this test
// process serves no mount. Its subject is mounting, so it keeps its own
// mounts.
func TestE2ESlimtreeLifecycle(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	skipIfOverMountBudget(t, 3)
	bin := slimtreeBinary(t)

	base := filepath.Join(t.TempDir(), "base")
	run(t, "", "git", "clone", "-q", createLocalTestRepo(t), base)
	run(t, base, "git", "checkout", "-q", "main")
	hookLog := filepath.Join(t.TempDir(), "post-checkout")
	if err := execfile.Write(filepath.Join(base, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\necho \"$(pwd -P) $*\" >>'"+hookLog+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The hook is the link only under a home no other user can replace.
	home := filepath.Join(gittest.PrivateTempDir(t), "home")
	mountDir := newE2EMountDir(t)
	wt := filepath.Join(mountDir, repoName)
	slimtree := func(args ...string) (string, string, error) {
		cmd := commandIn(base, bin, args...)
		cmd.Env = append(os.Environ(), "SLIMTREE_HOME="+home)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	mustSlimtree := func(args ...string) string {
		t.Helper()
		stdout, stderr, err := slimtree(args...)
		if err != nil {
			t.Fatalf("slimtree %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout, stderr)
		}
		return stdout
	}
	record := func() slimtreeRecord {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(home, "w", "*", "worktree.json"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("records = %q, %v; want one", paths, err)
		}
		data, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		var r slimtreeRecord
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	listed := func() string {
		t.Helper()
		var l struct {
			Worktrees []struct{ Path, State string }
		}
		if err := json.Unmarshal([]byte(mustSlimtree("list", "--json")), &l); err != nil {
			t.Fatal(err)
		}
		if len(l.Worktrees) != 1 || l.Worktrees[0].Path != wt {
			t.Fatalf("slimtree list = %+v, want %s alone", l.Worktrees, wt)
		}
		return l.Worktrees[0].State
	}

	if got := mustSlimtree("--backend", "artifactfs", "new", "--path", wt, "feat"); got != wt+"\n" {
		t.Fatalf("slimtree new printed %q, want %s", got, wt)
	}
	t.Cleanup(func() {
		if _, stderr, err := slimtree("stop", wt); err != nil {
			t.Errorf("slimtree stop at cleanup: %v: %s", err, stderr)
			r := record()
			mounts.Signal(r.DaemonPID, r.DaemonToken, syscall.SIGKILL)
		}
	})
	if got := listed(); got != "mounted" {
		t.Fatalf("slimtree list after new: %s, want mounted", got)
	}
	head := strings.TrimSpace(gitCmd(t, wt, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(gitCmd(t, wt, "symbolic-ref", "--short", "HEAD")); got != "feat" {
		t.Fatalf("the worktree is on %s, want feat", got)
	}
	assertGitStatus(t, wt, map[string]string{})
	if data, err := os.ReadFile(hookLog); err != nil || string(data) != wt+" "+strings.Repeat("0", len(head))+" "+head+" 1\n" {
		t.Fatalf("post-checkout ran as %q (%v), want once in %s on %s", data, err, wt, head)
	}
	// The daemon runs, and links Git's FSMonitor hook to, its own copy of
	// slimtree.
	link := strings.TrimSpace(gitCmd(t, wt, "config", "--get", "core.fsmonitor"))
	target, err := os.Readlink(link)
	resolvedHome, homeErr := filepath.EvalSymlinks(home)
	if err != nil || homeErr != nil || !strings.HasPrefix(target, filepath.Join(resolvedHome, "runtimes")+"/") {
		t.Fatalf("core.fsmonitor %s links to %q (%v, %v), want a copy below %s/runtimes", link, target, err, homeErr, home)
	}
	if err := writeMountFile(filepath.Join(wt, "notes.txt"), []byte("kept across stops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertGitStatus(t, wt, map[string]string{"notes.txt": "??"})

	// A process with its working directory in the mount holds it.
	holder := commandIn(wt, "/bin/sh", "-c", "echo ready; exec sleep 60")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := slimtree("stop", wt); err == nil || !strings.Contains(stderr, "processes use "+wt) || !strings.Contains(stderr, strconv.Itoa(holder.Process.Pid)+" sleep") {
		t.Fatalf("slimtree stop while pid %d holds the mount: %v, stderr %q; want a refusal that names it", holder.Process.Pid, err, stderr)
	}
	if got := listed(); got != "mounted" {
		t.Fatalf("slimtree list after a refused stop: %s, want mounted", got)
	}
	holder.Process.Kill()
	holder.Wait()

	daemon := record()
	if got := mustSlimtree("stop", wt); got != "stopped "+wt+"\n" {
		t.Fatalf("slimtree stop printed %q", got)
	}
	if id, err := mounts.Identify(wt); err != nil || id != "" {
		t.Fatalf("after stop, the mount table holds %q at %s (%v)", id, wt, err)
	}
	if err := mounts.Signal(daemon.DaemonPID, daemon.DaemonToken, 0); !errors.Is(err, mounts.ErrGone) {
		t.Fatalf("after stop, the daemon %d: %v, want it gone", daemon.DaemonPID, err)
	}
	if entries, err := os.ReadDir(wt); err != nil || len(entries) != 0 {
		t.Fatalf("the mount point after stop holds %v (%v), want nothing", entries, err)
	}
	if got := listed(); got != "stopped" {
		t.Fatalf("slimtree list after stop: %s, want stopped", got)
	}

	if got := mustSlimtree("resume", "--all"); got != "mounted "+wt+"\n" {
		t.Fatalf("slimtree resume --all printed %q", got)
	}
	assertGitStatus(t, wt, map[string]string{"notes.txt": "??"})
	if got := listed(); got != "mounted" {
		t.Fatalf("slimtree list after resume: %s, want mounted", got)
	}
	if got := mustSlimtree("resume", wt); got != wt+" is mounted\n" {
		t.Fatalf("slimtree resume of a mounted worktree printed %q", got)
	}

	// A killed daemon leaves its mount, which answers nothing.
	daemon = record()
	if err := mounts.Signal(daemon.DaemonPID, daemon.DaemonToken, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 30*time.Second, "the killed daemon's exit", func() (bool, string) {
		err := mounts.Signal(daemon.DaemonPID, daemon.DaemonToken, 0)
		return errors.Is(err, mounts.ErrGone), "signal 0: " + errString(err)
	})
	if got := listed(); got != "stopped" {
		t.Fatalf("slimtree list after the daemon died: %s, want stopped", got)
	}
	if got := mustSlimtree("resume", wt); got != "mounted "+wt+"\n" {
		t.Fatalf("slimtree resume after the daemon died printed %q", got)
	}
	assertGitStatus(t, wt, map[string]string{"notes.txt": "??"})
	if data, err := readMountFile(filepath.Join(wt, "notes.txt")); err != nil || string(data) != "kept across stops\n" {
		t.Fatalf("notes.txt after resume = %q, %v", data, err)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
