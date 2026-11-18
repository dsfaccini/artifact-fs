package slimtree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/mounts"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// fakeDaemon makes slimtree start a shell script with body in place of the
// daemon until t ends. Package tests never mount.
func fakeDaemon(t *testing.T, body string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "daemon")
	if err := execfile.Write(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := daemonProgram
	daemonProgram = func(*state.Home, string) (string, error) { return script, nil }
	t.Cleanup(func() { daemonProgram = previous })
}

// linux runs slimtree's commands with args as on a healthy Linux machine.
func linux(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(ctx, "linux", healthyLinux(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// slimtree waits for a daemon's exit after SIGTERM at least as long as the
// daemon's Close can take, so its SIGKILL never cuts the unmount short.
func TestDaemonTermWaitOutlastsTheDaemonsExit(t *testing.T) {
	if daemonTermWait < daemon.ExitBound {
		t.Fatalf("daemonTermWait = %v, want at least daemon.ExitBound %v", daemonTermWait, daemon.ExitBound)
	}
}

// A new that fails before it writes the record stops the daemon it started
// through the daemon's own process, also a daemon that ignores SIGTERM, and
// removes what it made. It starts the daemon in its own session, in the
// record directory, which holds the worktree's gitfile, on the worktree's
// state root, and without the variables that tie git to the caller's
// repository. The worktree and the home can be on two file systems.
func TestNewArtifactFSStopsItsDaemonWhenItFailsBeforeTheRecord(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	// git -c sets it for the commands git runs, such as a hook that runs
	// slimtree.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'slimtree.test=caller'")
	probe := filepath.Join(t.TempDir(), "probe")
	fakeDaemon(t, "trap '' TERM\n{ echo $$; pwd -P; cat gitfile; env; } >'"+probe+".tmp' && mv '"+probe+".tmp' '"+probe+"'\nexec sleep 60\n")
	renameHook = func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameHook = nil })
	previous := daemonTermWait
	daemonTermWait = 100 * time.Millisecond
	t.Cleanup(func() { daemonTermWait = previous })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, _, stderr := linux(ctx, "--backend", "artifactfs", "new", "feat")
		done <- result{code, stderr}
	}()
	var started []byte
	for started == nil {
		select {
		case r := <-done:
			t.Fatalf("new returned before its daemon started: exit %d, stderr %q", r.code, r.stderr)
		case <-time.After(10 * time.Millisecond):
		}
		started, _ = os.ReadFile(probe)
	}
	lines := strings.Split(strings.TrimSpace(string(started)), "\n")
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(filepath.Dir(main), "main-feat")
	dir := home.RecordDir(dest)
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		t.Errorf("the daemon's session = %d, %v; want its own, %d", sid, err, pid)
	}
	if lines[1] != dir {
		t.Errorf("the daemon runs in %s, want %s", lines[1], dir)
	}
	if !strings.HasPrefix(lines[2], "gitdir: ") {
		t.Errorf("the record directory's gitfile holds %q, want the worktree's gitfile", lines[2])
	}
	env := lines[3:]
	if !slices.Contains(env, "ARTIFACT_FS_ROOT="+filepath.Join(dir, "state")) || !slices.Contains(env, "GIT_CONFIG_GLOBAL="+os.DevNull) ||
		slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") }) {
		t.Errorf("the daemon's environment = %q; want ARTIFACT_FS_ROOT, GIT_CONFIG_GLOBAL and no GIT_CONFIG_PARAMETERS", env)
	}

	cancel()
	r := <-done
	if r.code != 1 || !strings.Contains(r.stderr, "context canceled") {
		t.Fatalf("a canceled new: exit %d, stderr %q; want exit 1 and the cancellation", r.code, r.stderr)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the daemon %d runs after new failed: %v", pid, err)
	}
	requireNoWorktree(t, main, dest)
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
}

// startNew runs new feat with the artifactfs backend until cancel, once the
// daemon wrote probe, and returns new's exit code and stderr.
func startNew(t *testing.T, probe string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, _, stderr := linux(ctx, "--backend", "artifactfs", "new", "feat")
		done <- result{code, stderr}
	}()
	for {
		select {
		case r := <-done:
			t.Fatalf("new returned before its daemon started: exit %d, stderr %q", r.code, r.stderr)
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Lstat(probe); err == nil {
			break
		}
	}
	cancel()
	r := <-done
	return r.code, r.stderr
}

// A new whose failure leaves a mount it cannot unmount removes nothing: the
// gitfile stays in the record directory, and the worktree stays registered,
// so nothing reaches the mount.
func TestNewArtifactFSRemovesNothingWhileAMountStays(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	probe := filepath.Join(t.TempDir(), "probe")
	fakeDaemon(t, "touch '"+probe+"'\nexec sleep 60\n")
	identifyHook = func(string) (mounts.Identity, error) { return "stuck", nil }
	unmountHook = func(context.Context, string, mounts.Identity) error { return errors.New("device busy") }
	t.Cleanup(func() { identifyHook, unmountHook = nil, nil })

	code, stderr := startNew(t, probe)
	if code != 1 || !strings.Contains(stderr, "slimtree could not stop what it started") || !strings.Contains(stderr, "device busy") {
		t.Fatalf("new that cannot unmount: exit %d, stderr %q; want exit 1 and the unmount failure", code, stderr)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(filepath.Dir(main), "main-feat")
	if data, err := os.ReadFile(filepath.Join(home.RecordDir(dest), "gitfile")); err != nil || !strings.HasPrefix(string(data), "gitdir: ") {
		t.Errorf("the record directory's gitfile = %q, %v; want it kept", data, err)
	}
	if _, err := os.Stat(filepath.Join(home.RecordDir(dest), "state")); err != nil {
		t.Errorf("the state root: %v; want it kept", err)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+dest+"\n") {
		t.Errorf("git no longer lists %s:\n%s", dest, list)
	}
	if got := records(t); len(got) != 0 {
		t.Errorf("records = %+v, want none", got)
	}
}

// A gitfile copy across file systems that fails leaves no partial gitfile in
// the record directory, so the rollback keeps the worktree's own .git and
// removes the worktree.
func TestNewArtifactFSRollsBackAFailedGitfileCopy(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	marker := filepath.Join(t.TempDir(), "started")
	fakeDaemon(t, "touch '"+marker+"'\nexec sleep 60\n")
	renameHook = func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
	}
	copyOpened = func(f *os.File) {
		// The first copy fails after its create.
		f.Close()
		copyOpened = nil
	}
	t.Cleanup(func() { renameHook, copyOpened = nil, nil })
	if code, stdout, stderr := linux(t.Context(), "--backend", "artifactfs", "new", "feat"); code != 1 || stdout != "" || !strings.Contains(stderr, "file already closed") {
		t.Fatalf("new with a failed gitfile copy: exit %d, stdout %q, stderr %q; want exit 1 and the copy's failure", code, stdout, stderr)
	}
	requireNoWorktree(t, main, filepath.Join(filepath.Dir(main), "main-feat"))
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("new started a daemon: %v", err)
	}
}

// A copy across file systems to a path that exists leaves that path as it
// was, as when a rollback puts the gitfile back over an intact .git.
func TestMoveFileKeepsAnExistingDestination(t *testing.T) {
	renameHook = func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameHook = nil })
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "gitfile"), filepath.Join(dir, ".git")
	if err := os.WriteFile(src, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("gitdir: intact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("moveFile over an existing file = %v, want it to exist", err)
	}
	for path, want := range map[string]string{src: "partial", dst: "gitdir: intact\n"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", path, data, err, want)
		}
	}
}

// After an external unmount, the recorded daemon mounts the worktree again,
// with a new identity. While the daemon reports the worktree mounted and git
// finds the worktree's git dir through the mount, stop records the new
// identity and stops the daemon. Without either, stop refuses the other
// mount. list changes nothing, so it calls the mount unknown and names
// resume.
func TestStopFollowsARemountByTheRecordedDaemon(t *testing.T) {
	homeAfterFirstRun(t)
	parent, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command("sleep", "60")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})
	token, err := mounts.StartToken(daemon.Process.Pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken(%d) = %q, %v", daemon.Process.Pid, token, err)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	r := state.Record{Version: 1, Backend: "artifactfs", Path: filepath.Join(parent, "wt"), Branch: "wt",
		DaemonPID: daemon.Process.Pid, DaemonToken: token, MountIdentity: "before"}
	r.StateDir = filepath.Join(home.RecordDir(r.Path), "state")
	gitIn(t, parent, "init", "-q", filepath.Join(parent, "repo"))
	gitIn(t, filepath.Join(parent, "repo"), "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, filepath.Join(parent, "repo"), "worktree", "add", "-q", "--lock", "-b", "wt", r.Path)
	r.CommonDir = filepath.Join(parent, "repo", ".git")
	gitDir, err := state.Canonical(filepath.Join(r.CommonDir, "worktrees", "wt"))
	if err != nil {
		t.Fatal(err)
	}
	// First, the record names a git dir that git does not find at r.Path.
	r.GitDir = filepath.Join(parent, "elsewhere")
	if err := home.Write(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.RecordDir(r.Path), "gitfile"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon's registry, its runtime record, and the daemon lock it holds.
	if err := os.MkdirAll(filepath.Join(r.StateDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(t.Context(), filepath.Join(r.StateDir, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := reg.AddRepo(t.Context(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "wt", ID: model.RepoID("wt")}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(r.StateDir, "config", "daemon.lock")
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(daemon.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
	})
	// The daemon's new mount is at the worktree while the daemon runs.
	identifyHook = func(string) (mounts.Identity, error) {
		if mounts.Signal(daemon.Process.Pid, token, 0) != nil {
			return "", nil
		}
		return "after", nil
	}
	t.Cleanup(func() { identifyHook = nil })
	listed := func() (string, string) {
		t.Helper()
		code, stdout, stderr := linux(t.Context(), "list", "--json")
		var l struct {
			Worktrees []struct{ State, Reason string }
		}
		if err := json.Unmarshal([]byte(stdout), &l); code != 0 || err != nil || len(l.Worktrees) != 1 {
			t.Fatalf("list --json: exit %d, stdout %q, stderr %q (%v)", code, stdout, stderr, err)
		}
		return l.Worktrees[0].State, l.Worktrees[0].Reason
	}

	if err := reg.PutRuntime(t.Context(), model.RepoRuntimeRecord{RepoID: model.RepoID("wt"), State: "degraded"}); err != nil {
		t.Fatal(err)
	}
	if got, reason := listed(); got != "unknown" || !strings.Contains(reason, "slimtree resume records the new mount") {
		t.Fatalf("list while the daemon reports no mount: %s (%s), want unknown and resume", got, reason)
	}
	if code, _, stderr := linux(t.Context(), "stop", r.Path); code != 1 || !strings.Contains(stderr, "holds another mount than the one slimtree recorded") {
		t.Fatalf("stop while the daemon reports no mount: exit %d, stderr %q; want the refusal", code, stderr)
	}

	if err := reg.PutRuntime(t.Context(), model.RepoRuntimeRecord{RepoID: model.RepoID("wt"), State: "mounted"}); err != nil {
		t.Fatal(err)
	}
	if got, reason := listed(); got != "unknown" || !strings.Contains(reason, "slimtree resume records the new mount") {
		t.Fatalf("list after the daemon mounted again: %s (%s), want unknown and resume", got, reason)
	}
	if code, _, stderr := linux(t.Context(), "stop", r.Path); code != 1 || !strings.Contains(stderr, "holds another mount than the one slimtree recorded, and slimtree left it") {
		t.Fatalf("stop where git finds another git dir: exit %d, stderr %q; want the refusal", code, stderr)
	}
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Fatalf("records after a refused stop = %+v, want %+v", got, r)
	}

	r.GitDir = gitDir
	if err := home.Write(r); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := linux(t.Context(), "stop", r.Path); code != 0 || stdout != "stopped "+r.Path+"\n" {
		t.Fatalf("stop after the daemon mounted again: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if err := daemon.Wait(); err == nil || err.Error() != "signal: terminated" {
		t.Fatalf("the daemon's wait = %v, want SIGTERM", err)
	}
	r.MountIdentity = "after"
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Fatalf("records = %+v, want %+v", got, r)
	}
}

// The runtime copy is the running binary, under its hash. A copy that does
// not match its hash, as after a power loss, is written again, and a write
// that fails leaves no temporary file. On Linux the copy reads the running
// file through /proc/self/exe, which an upgrade that replaces the file at its
// path does not change.
func TestRuntimeCopyIsTheRunningBinary(t *testing.T) {
	homeAfterFirstRun(t)
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := runningBinary("linux"); got != "/proc/self/exe" || err != nil {
		t.Fatalf(`runningBinary("linux") = %q, %v`, got, err)
	}
	self, err := runningBinary(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	path, err := runtimeCopy(home, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the runtime copy %s holds %d bytes (%v), want the %d of the running binary", path, len(got), err, len(want))
	}
	if err := os.Truncate(path, 16); err != nil {
		t.Fatal(err)
	}
	if again, err := runtimeCopy(home, runtime.GOOS); err != nil || again != path {
		t.Fatalf("runtimeCopy after a truncation = %q, %v; want %s", again, err, path)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the truncated runtime copy holds %d bytes (%v) after runtimeCopy, want %d", len(got), err, len(want))
	}

	// A directory with a file in it takes the copy's name, so the rename fails.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeCopy(home, runtime.GOOS); err == nil {
		t.Fatal("runtimeCopy over a directory succeeded")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "slimtree" {
		t.Fatalf("the runtime directory holds %v after a failed write, want slimtree alone", entries)
	}
}

// Only the wait for the daemon tells new that it exited, and new then fails
// at once and removes what it made.
func TestNewArtifactFSFailsWhenItsDaemonExits(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	fakeDaemon(t, "exit 3\n")
	code, stdout, stderr := linux(t.Context(), "--backend", "artifactfs", "new", "feat")
	if code != 1 || stdout != "" || !regexp.MustCompile(`the daemon exited \(exit status 3\)`).MatchString(stderr) {
		t.Fatalf("new with a daemon that exits 3: exit %d, stdout %q, stderr %q; want exit 1 and the daemon's exit", code, stdout, stderr)
	}
	requireNoWorktree(t, main, filepath.Join(filepath.Dir(main), "main-feat"))
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
}

// new refuses an artifactfs worktree before it starts a daemon or changes
// anything when a mount check fails, when artifact-fs cannot name the
// worktree's directory, and when the record directory holds an earlier
// worktree's state.
func TestNewArtifactFSRefusesBeforeItMounts(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	marker := filepath.Join(t.TempDir(), "started")
	fakeDaemon(t, "touch '"+marker+"'\nexec sleep 60\n")
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(main)
	retained := filepath.Join(parent, "retained")
	kept := filepath.Join(home.RecordDir(retained), "state", "kept")
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("overlay\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noFUSE := healthyLinux()
	noFUSE.ReadWrite = func(string) error { return os.ErrPermission }
	for _, tc := range []struct {
		probe doctor.Probe
		args  []string
		want  string
	}{
		{probe: noFUSE, args: []string{"new", "feat"}, want: "dev-fuse"},
		{probe: healthyLinux(), args: []string{"new", "--path", filepath.Join(parent, "a,b"), "feat"}, want: "must not contain ','"},
		{probe: healthyLinux(), args: []string{"new", "--path", retained, "feat"}, want: "holds the state of an earlier worktree"},
	} {
		var stdout, stderr bytes.Buffer
		// git worktree add prints "Preparing worktree".
		if code := run(t.Context(), "linux", tc.probe, append([]string{"--backend", "artifactfs"}, tc.args...), &stdout, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "Preparing worktree") {
			t.Fatalf("%q: exit %d, stderr %q; want exit 1 and %q before git worktree add", tc.args, code, stderr.String(), tc.want)
		}
		gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	}
	requireNoWorktree(t, main, filepath.Join(parent, "main-feat"))
	requireNoWorktree(t, main, filepath.Join(parent, "a,b"))
	if data, err := os.ReadFile(kept); err != nil || string(data) != "overlay\n" {
		t.Errorf("the earlier worktree's state = %q, %v; want it kept", data, err)
	}
	if _, err := os.Lstat(retained); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists: %v", retained, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused new started a daemon: %v", err)
	}
}

// stop and resume refuse an apfsclone worktree. resume refuses, before it
// starts a daemon, a mount point that holds files and a state root that a
// daemon it did not record serves.
func TestStopAndResumeRefuseBeforeTheyMount(t *testing.T) {
	homeAfterFirstRun(t)
	parent, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	fakeDaemon(t, "touch '"+marker+"'\nexec sleep 60\n")
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	var written []state.Record
	for _, r := range []state.Record{
		{Backend: "apfsclone", Path: filepath.Join(parent, "cloned")},
		{Backend: "artifactfs", Path: filepath.Join(parent, "full")},
		{Backend: "artifactfs", Path: filepath.Join(parent, "served")},
	} {
		r.Version, r.Branch = 1, r.Name()
		if r.Backend == "artifactfs" {
			r.StateDir = filepath.Join(home.RecordDir(r.Path), "state")
		}
		if err := os.Mkdir(r.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := home.Write(r); err != nil {
			t.Fatal(err)
		}
		written = append(written, r)
	}
	if err := os.WriteFile(filepath.Join(parent, "full", "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Another daemon holds the daemon lock of served's state root.
	lockPath := filepath.Join(written[2].StateDir, "config", "daemon.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
	})
	t.Chdir(parent)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"stop", "cloned"}, want: filepath.Join(parent, "cloned") + " is an apfsclone worktree, which no daemon serves"},
		{args: []string{"resume", "cloned"}, want: filepath.Join(parent, "cloned") + " is an apfsclone worktree, which no daemon serves"},
		{args: []string{"resume", "full"}, want: filepath.Join(parent, "full") + " is not mounted and holds files"},
		{args: []string{"resume", "served"}, want: "pid 4242, a daemon slimtree did not record, serves " + written[2].StateDir},
		{args: []string{"resume"}, want: "slimtree resume takes one worktree, by name or path, or --all"},
		{args: []string{"resume", "--all", "full"}, want: "slimtree resume takes one worktree, by name or path, or --all"},
	} {
		if code, stdout, stderr := linux(t.Context(), tc.args...); code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit 1 and %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
	if got := records(t); !reflect.DeepEqual(got, written) {
		t.Errorf("records = %+v, want %+v", got, written)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused resume started a daemon: %v", err)
	}
}

// stop sends the recorded daemon SIGTERM, matched by its start token, waits
// until it exits, and keeps the record.
func TestStopEndsTheRecordedDaemon(t *testing.T) {
	homeAfterFirstRun(t)
	parent, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command("sleep", "60")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})
	token, err := mounts.StartToken(daemon.Process.Pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken(%d) = %q, %v", daemon.Process.Pid, token, err)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	r := state.Record{Version: 1, Backend: "artifactfs", Path: filepath.Join(parent, "wt"), Branch: "wt", DaemonPID: daemon.Process.Pid, DaemonToken: token}
	if err := os.Mkdir(r.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := home.Write(r); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := linux(t.Context(), "stop", r.Path); code != 0 || stdout != "stopped "+r.Path+"\n" {
		t.Fatalf("stop: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if err := daemon.Wait(); err == nil || err.Error() != "signal: terminated" {
		t.Fatalf("the daemon's wait = %v, want SIGTERM", err)
	}
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Fatalf("records = %+v, want %+v", got, r)
	}
}
