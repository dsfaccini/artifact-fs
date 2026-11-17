package slimtree

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/execfile"
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
	daemonProgram = func(*state.Home) (string, error) { return script, nil }
	t.Cleanup(func() { daemonProgram = runtimeCopy })
}

// linux runs slimtree's commands with args as on a healthy Linux machine.
func linux(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(ctx, "linux", healthyLinux(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// A new that fails before it writes the record stops the daemon it started
// through the daemon's own process, also a daemon that ignores SIGTERM, and
// removes what it made. It starts the daemon in its own session, in the
// record directory, on the worktree's state root, and without the variables
// that tie git to the caller's repository.
func TestNewArtifactFSStopsItsDaemonWhenItFailsBeforeTheRecord(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	// git -c sets it for the commands git runs, such as a hook that runs
	// slimtree.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'slimtree.test=caller'")
	probe := filepath.Join(t.TempDir(), "probe")
	fakeDaemon(t, "trap '' TERM\n{ echo $$; pwd -P; env; } >'"+probe+".tmp' && mv '"+probe+".tmp' '"+probe+"'\nexec sleep 60\n")
	previous := daemonStopWait
	daemonStopWait = 100 * time.Millisecond
	t.Cleanup(func() { daemonStopWait = previous })

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
	env := lines[2:]
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
