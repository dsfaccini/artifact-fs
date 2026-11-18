package slimtree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/slimtree/mounts"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
	"github.com/cloudflare/artifact-fs/internal/version"
)

const (
	// artifactFSLockReason is the reason git worktree lock records for an
	// artifactfs worktree. git worktree list skips a locked worktree's .git,
	// so no listing reaches the mount, and never marks it prunable.
	artifactFSLockReason = "slimtree artifactfs"
	// gitfileName is the file in an artifactfs worktree's record directory
	// that keeps the gitfile git worktree add wrote. The mount point stays
	// empty, and the mount serves the same gitfile.
	gitfileName = "gitfile"
	// daemonLogName is the daemon's log in the record directory.
	daemonLogName = "daemon.log"
	// mountPoll is how often slimtree looks for a daemon's mount or exit.
	mountPoll = 100 * time.Millisecond
	// mountWait bounds the wait for a daemon slimtree started to mount: its
	// start, its preparation and its mount, which it bounds at 30 s.
	mountWait = time.Minute
	// mountCheckTimeout bounds the git command that reads a new mount.
	mountCheckTimeout = 30 * time.Second
)

// daemonTermWait is how long a daemon has to exit after SIGTERM. Tests
// lower it.
var daemonTermWait = daemon.ExitBound

// daemonKillWait bounds the reap of a daemon slimtree started, after SIGKILL.
const daemonKillWait = time.Minute

// daemonProgram returns the program a daemon of home runs. Tests set it to a
// fake daemon.
var daemonProgram = runtimeCopy

// errStillServed marks a failure that left the daemon slimtree started
// running, or its mount mounted, so nothing below the mount point may be
// removed.
var errStillServed = errors.New("slimtree could not stop what it started")

// localGitEnv are the variables git rev-parse --local-env-vars lists, which
// tie git to one repository, and GIT_NAMESPACE. A caller in a hook, an alias
// or another worktree can set them, and the daemon would then run git on the
// caller's repository.
var localGitEnv = []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR", "GIT_NAMESPACE"}

// newArtifactFS makes the artifactfs worktree rb.dest, which makeWorktree
// resolved: git worktree add --no-checkout from rb.base with addArgs and
// commit, under apfsclone's registration lock. It locks the worktree, moves
// its gitfile into the record directory, adopts its git dir into an
// artifact-fs state root there, with the shared blob cache and no remote
// refresh, and starts a daemon that mounts it. It writes the record once the
// mount serves, and returns the commit it checked out. When it fails, it
// removes what it made.
func newArtifactFS(ctx context.Context, home *state.Home, rb *rollback, goos, commit string, addArgs []string, stderr io.Writer) (head string, err error) {
	dir := home.RecordDir(rb.dest)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = undoArtifactFS(ctx, rb, dir, err)
		}
	}()
	head, err = apfsclone.Register(ctx, apfsclone.Options{Base: rb.base, Path: rb.dest, Commit: commit, AddArgs: addArgs, Stdout: stderr, Stderr: stderr})
	var incomplete *apfsclone.IncompleteError
	rb.added = err == nil || errors.As(err, &incomplete)
	if err != nil {
		return "", err
	}
	out, err := apfsclone.Git(ctx, rb.dest, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", err
	}
	r := state.Record{Backend: string(artifactFS), Path: rb.dest, CommonDir: rb.repo, Branch: rb.branch,
		Created: time.Now().UTC(), SlimtreeVersion: version.String(), StateDir: filepath.Join(dir, "state")}
	if r.GitDir, err = state.Canonical(strings.TrimSpace(string(out))); err != nil {
		return "", err
	}
	if _, err := apfsclone.Git(ctx, rb.base, "worktree", "lock", "--reason", artifactFSLockReason, rb.dest); err != nil {
		return "", err
	}
	if err := moveFile(filepath.Join(rb.dest, ".git"), filepath.Join(dir, gitfileName)); err != nil {
		return "", err
	}
	ctl, err := daemon.NewController(ctx, r.StateDir, logging.NewJSONLogger(stderr, slog.LevelWarn))
	if err != nil {
		return "", err
	}
	// The registry closes before undoArtifactFS removes the state root.
	defer ctl.Close()
	cfg := model.RepoConfig{
		RepoPaths:     model.RepoPaths{Name: r.Name(), GitDir: r.GitDir, MountRoot: filepath.Dir(r.Path), BlobCacheDir: filepath.Join(home.Dir(), "blobs")},
		RepoSpec:      model.RepoSpec{AdoptGitDir: true},
		RefreshPolicy: model.RefreshPolicy{RemoteRefreshDisabled: true},
		Enabled:       true,
	}
	if err := ctl.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{}); err != nil {
		return "", err
	}
	return head, serve(ctx, home, ctl, r, goos)
}

// undoArtifactFS removes what a newArtifactFS that failed with cause made:
// it puts the gitfile back, so that rb.run can remove the worktree, and
// removes the record directory. serve has stopped the daemon and unmounted
// its mount, unless cause wraps errStillServed. Then it removes nothing.
func undoArtifactFS(ctx context.Context, rb *rollback, dir string, cause error) error {
	if errors.Is(cause, errStillServed) {
		return fmt.Errorf("%w; slimtree left %s, its registration and %s", cause, rb.dest, dir)
	}
	if gitfile := filepath.Join(dir, gitfileName); exists(gitfile) {
		if err := moveFile(gitfile, filepath.Join(rb.dest, ".git")); err != nil {
			return errors.Join(cause, fmt.Errorf("slimtree left %s, its registration and %s: %w", rb.dest, dir, err))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.Join(cause, err)
	}
	if err := rb.run(ctx); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// identifyHook and unmountHook, when set, run in place of mounts.Identify
// and mounts.Unmount. Tests use them to show a mount where none is.
var (
	identifyHook func(path string) (mounts.Identity, error)
	unmountHook  func(ctx context.Context, path string, id mounts.Identity) error
)

func identify(path string) (mounts.Identity, error) {
	if identifyHook != nil {
		return identifyHook(path)
	}
	return mounts.Identify(path)
}

func unmount(ctx context.Context, path string, id mounts.Identity) error {
	if unmountHook != nil {
		return unmountHook(ctx, path, id)
	}
	return mounts.Unmount(ctx, path, id)
}

// renameHook, when set, renames in place of os.Rename in moveFile. Tests use
// it to fail a rename across file systems.
var renameHook func(src, dst string) error

// moveFile moves the small file src to dst. A worktree and the home can be
// on two file systems, where a rename fails with EXDEV, so moveFile then
// copies src and removes it.
func moveFile(src, dst string) error {
	rename := os.Rename
	if renameHook != nil {
		rename = renameHook
	}
	err := rename(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	created, err := copyFile(dst, data, fi.Mode().Perm())
	if err != nil {
		// A partial copy at dst would stand for src.
		if created {
			os.Remove(dst)
		}
		return err
	}
	return os.Remove(src)
}

// copyOpened, when set, runs on copyFile's file right after its create.
// Tests use it to fail the write that follows.
var copyOpened func(*os.File)

// copyFile writes data to the new file dst, and reports whether it created
// dst.
func copyFile(dst string, data []byte, perm fs.FileMode) (bool, error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return false, err
	}
	if copyOpened != nil {
		copyOpened(f)
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return true, err
}

// serve starts a daemon on r's state root, waits until it mounts r.Path,
// and writes r's record with the daemon and its mount. Until the record is
// written, a failure stops that daemon and unmounts what it mounted, and when
// that fails too, the error wraps errStillServed. Once the record is
// written, the daemon keeps serving.
func serve(ctx context.Context, home *state.Home, ctl *daemon.Controller, r state.Record, goos string) (err error) {
	started := time.Now()
	d, err := startDaemon(home, r, goos)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if stopErr := d.stop(ctx, r.Path); stopErr != nil {
				err = fmt.Errorf("%w; %w: %w", err, errStillServed, stopErr)
			}
		}
	}()
	id, err := awaitMount(ctx, ctl, r, d, started)
	if err != nil {
		return err
	}
	r.DaemonPID, r.DaemonToken, r.MountIdentity = d.proc.Pid, d.token, string(id)
	if err := home.Write(r); err != nil {
		return fmt.Errorf("writing the record of %s: %w", r.Path, err)
	}
	return nil
}

// launchedDaemon is a daemon slimtree started. No other process can take
// its PID until slimtree reaps it.
type launchedDaemon struct {
	proc  *os.Process
	token string
	log   string
	// done closes once the wait for the daemon returned waitErr.
	done    chan struct{}
	waitErr error
}

// startDaemon starts slimtree daemon on r's state root, detached in its own
// session, in r's record directory and with its log there. The caller's
// directory can be another worktree's mount, which the daemon would hold.
// It records the daemon's start token right after the start, from the
// start time, which fork sets: right after exec, Linux shows the daemon
// with an empty argv. Only the wait for the daemon tells that it exited.
func startDaemon(home *state.Home, r state.Record, goos string) (*launchedDaemon, error) {
	program, err := daemonProgram(home, goos)
	if err != nil {
		return nil, err
	}
	dir := home.RecordDir(r.Path)
	logPath := filepath.Join(dir, daemonLogName)
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	// Main forwards daemon only as its first argument.
	cmd := exec.Command(program, "daemon")
	cmd.Dir = dir
	cmd.Env = daemonEnv(os.Environ(), r.StateDir, goos)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := &launchedDaemon{proc: cmd.Process, log: logPath, done: make(chan struct{})}
	// The child is not reaped before the wait starts, so its PID is its own.
	token, tokenErr := mounts.StartToken(cmd.Process.Pid)
	go func() {
		d.waitErr = cmd.Wait()
		close(d.done)
	}()
	if tokenErr == nil && token != "" {
		d.token = token
		return d, nil
	}
	if err := d.end(); err != nil {
		return nil, errors.Join(tokenErr, fmt.Errorf("%w: %w", errStillServed, err))
	}
	if tokenErr != nil {
		return nil, tokenErr
	}
	// macOS gives an exited child no start token.
	return nil, fmt.Errorf("the daemon exited (%v); see %s", d.waitErr, logPath)
}

// end stops the daemon through its own process and reaps it: SIGTERM, then
// after daemonTermWait SIGKILL, and a reap bounded by daemonKillWait.
func (d *launchedDaemon) end() error {
	for _, step := range []struct {
		sig  os.Signal
		wait time.Duration
	}{{syscall.SIGTERM, daemonTermWait}, {syscall.SIGKILL, daemonKillWait}} {
		// A reaped daemon takes no signal.
		_ = d.proc.Signal(step.sig)
		select {
		case <-d.done:
			return nil
		case <-time.After(step.wait):
		}
	}
	return fmt.Errorf("the daemon, pid %d, did not exit within %s of SIGKILL", d.proc.Pid, daemonKillWait)
}

// stop ends the daemon, then unmounts the mount at path that a killed
// daemon leaves. slimtree holds the global lock, so an artifact-fs mount
// there is the daemon's.
func (d *launchedDaemon) stop(ctx context.Context, path string) error {
	if err := d.end(); err != nil {
		return err
	}
	id, err := identify(path)
	if err != nil || id == "" {
		return err
	}
	return unmount(context.WithoutCancel(ctx), path, id)
}

// awaitMount waits until d mounts r.Path, which status reports mounted and
// the mount table lists, and git finds r's worktree there. It returns the
// mount's identity. It fails once d exits, once d reports a mount failure
// after started, and after mountWait.
func awaitMount(ctx context.Context, ctl *daemon.Controller, r state.Record, d *launchedDaemon, started time.Time) (mounts.Identity, error) {
	timeout := time.NewTimer(mountWait)
	defer timeout.Stop()
	tick := time.NewTicker(mountPoll)
	defer tick.Stop()
	for {
		select {
		case <-d.done:
			return "", fmt.Errorf("the daemon exited (%v) before it mounted %s; see %s", d.waitErr, r.Path, d.log)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout.C:
			return "", fmt.Errorf("the daemon did not mount %s within %s; see %s", r.Path, mountWait, d.log)
		case <-tick.C:
		}
		st, err := ctl.Status(ctx, r.Name())
		if err != nil {
			return "", err
		}
		// A record outlives its daemon, so an earlier daemon's failure stays.
		if st.MountError != "" && st.LastErrorAt.After(started) {
			return "", fmt.Errorf("the daemon could not mount %s: %s", r.Path, st.MountError)
		}
		if st.State != "mounted" {
			continue
		}
		id, err := identify(r.Path)
		if err != nil {
			return "", err
		}
		if id == "" {
			continue
		}
		return id, checkMounted(ctx, r)
	}
}

// checkMounted checks that git finds r's worktree and git dir through the
// mount at r.Path.
func checkMounted(ctx context.Context, r state.Record) error {
	ctx, cancel := context.WithTimeout(ctx, mountCheckTimeout)
	defer cancel()
	out, err := apfsclone.Git(ctx, r.Path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir")
	if err != nil {
		return fmt.Errorf("the mount at %s: %w", r.Path, err)
	}
	top, gitDir, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !state.SamePath(top, r.Path) || !state.SamePath(gitDir, r.GitDir) {
		return fmt.Errorf("the mount at %s serves the worktree %s of the git dir %s, not the git dir %s", r.Path, top, gitDir, r.GitDir)
	}
	return nil
}

// daemonEnv returns environ for a daemon on stateDir, without localGitEnv
// and the GIT_CONFIG_KEY_ and GIT_CONFIG_VALUE_ variables. On macOS the
// daemon mounts through FSKit, whose mounts alone mounts.Identify knows.
func daemonEnv(environ []string, stateDir, goos string) []string {
	var env []string
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(localGitEnv, key) || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") ||
			key == "ARTIFACT_FS_ROOT" || key == "ARTIFACT_FS_FUSE_BACKEND" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "ARTIFACT_FS_ROOT="+stateDir)
	if goos == "darwin" {
		env = append(env, "ARTIFACT_FS_FUSE_BACKEND=fskit")
	}
	return env
}

// runtimeCopy returns the copy of the running binary that daemons run,
// <home>/runtimes/<SHA-256>/slimtree, and writes it when it is missing or
// does not match its hash, as after a power loss. A daemon links Git's
// FSMonitor hook to its own binary, so an upgrade that replaces slimtree
// never puts another version behind a running daemon's hook.
func runtimeCopy(home *state.Home, goos string) (string, error) {
	self, err := runningBinary(goos)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	dir := filepath.Join(home.Dir(), "runtimes", hex.EncodeToString(sum[:]))
	path := filepath.Join(dir, "slimtree")
	if copied, err := os.ReadFile(path); err == nil && sha256.Sum256(copied) == sum {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".slimtree-"+strconv.Itoa(os.Getpid()))
	if err := writeRuntime(tmp, path, data); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// runningBinary returns a path that reads the running binary on goos. On
// Linux /proc/self/exe reads the running file even after an upgrade replaced
// the file at its path. macOS has no such path.
func runningBinary(goos string) (string, error) {
	if goos == "linux" {
		return "/proc/self/exe", nil
	}
	return os.Executable()
}

// writeRuntime writes data to tmp, syncs it, and renames it to path.
func writeRuntime(tmp, path string, data []byte) error {
	if err := execfile.Write(tmp, data, 0o700); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// daemonAlive reports whether the daemon r records runs.
func daemonAlive(r state.Record) (bool, error) {
	if r.DaemonPID == 0 {
		return false, nil
	}
	switch err := mounts.Signal(r.DaemonPID, r.DaemonToken, 0); {
	case errors.Is(err, mounts.ErrGone):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// stopWorktree stops the daemon of the artifactfs worktree arg names and
// unmounts its mount. Its files stay in its overlay.
func stopWorktree(ctx context.Context, home *state.Home, arg string, stdout io.Writer) error {
	lock, err := home.Lock(ctx, "")
	if err != nil {
		return err
	}
	defer lock.Unlock()
	r, err := resolveArtifactFS(home, arg)
	if err != nil {
		return err
	}
	if err := healRemount(ctx, home, &r); err != nil {
		return err
	}
	if err := stopDaemon(ctx, r); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "stopped %s\n", r.Path)
	return nil
}

// healRemount records the identity of the mount at r.Path when r's recorded
// daemon runs and reports r mounted, that identity is not the recorded one,
// and git finds r's worktree and git dir through the mount. After an
// external unmount, the daemon mounts again, with a new identity. It refuses
// a mount where git finds another worktree.
func healRemount(ctx context.Context, home *state.Home, r *state.Record) error {
	id, remount, err := remountedBy(ctx, *r)
	if err != nil || !remount {
		return err
	}
	if err := checkMounted(ctx, *r); err != nil {
		return fmt.Errorf("%s holds another mount than the one slimtree recorded, and slimtree left it: %w", r.Path, err)
	}
	r.MountIdentity = string(id)
	return home.Write(*r)
}

// remountedBy returns the identity of the mount at r.Path, and whether r's
// recorded daemon runs and reports r mounted while that identity is not the
// recorded one.
func remountedBy(ctx context.Context, r state.Record) (mounts.Identity, bool, error) {
	alive, err := daemonAlive(r)
	if err != nil || !alive {
		return "", false, err
	}
	id, err := identify(r.Path)
	if err != nil || id == "" || string(id) == r.MountIdentity {
		return id, false, err
	}
	ctl, err := daemon.NewController(ctx, r.StateDir, logging.NewJSONLogger(io.Discard, slog.LevelWarn))
	if err != nil {
		return id, false, err
	}
	defer ctl.Close()
	st, err := ctl.Status(ctx, r.Name())
	if err != nil {
		return id, false, err
	}
	return id, st.State == "mounted", nil
}

// resolveArtifactFS returns the record of the artifactfs worktree arg names.
func resolveArtifactFS(home *state.Home, arg string) (state.Record, error) {
	r, err := home.Resolve(arg)
	if err == nil && r.Backend != string(artifactFS) {
		err = fmt.Errorf("%s is an %s worktree, which no daemon serves", r.Path, r.Backend)
	}
	return r, err
}

// stopDaemon stops r's recorded daemon and unmounts its recorded mount. It
// refuses another mount at r.Path, and a mount that processes other than
// the daemon use: a mount that a normal unmount cannot take is detached
// lazily on Linux, and stays alive for each of them. It signals the daemon
// only while its start token matches, and unmounts only the recorded mount.
func stopDaemon(ctx context.Context, r state.Record) error {
	alive, err := daemonAlive(r)
	if err != nil {
		return err
	}
	id, err := identify(r.Path)
	if err != nil {
		return err
	}
	if id != "" && string(id) != r.MountIdentity {
		return fmt.Errorf("%s holds another mount than the one slimtree recorded; slimtree left it", r.Path)
	}
	// Once the daemon is gone, the mount answers nothing, so a refusal
	// protects nothing.
	if alive && id != "" {
		if err := refuseHolders(ctx, r); err != nil {
			return err
		}
	}
	if alive {
		if err := mounts.Signal(r.DaemonPID, r.DaemonToken, syscall.SIGTERM); err != nil && !errors.Is(err, mounts.ErrGone) {
			return err
		}
		deadline := time.Now().Add(daemonTermWait)
		for alive {
			if time.Now().After(deadline) {
				return fmt.Errorf("the daemon of %s, pid %d, did not exit within %s of SIGTERM; slimtree left it running", r.Path, r.DaemonPID, daemonTermWait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(mountPoll):
			}
			if alive, err = daemonAlive(r); err != nil {
				return err
			}
		}
	}
	// A daemon that exits unmounts its mount, and one that died leaves it.
	if id, err = identify(r.Path); err != nil || id == "" {
		return err
	}
	if err := unmount(ctx, r.Path, mounts.Identity(r.MountIdentity)); err != nil {
		key, keyErr := mounts.KeyOf(r.Path)
		if keyErr != nil {
			return err
		}
		if holders, _ := mounts.Holders(ctx, r.Path, key, 0); len(holders) > 0 {
			return fmt.Errorf("%w; processes using it: %s", err, describeHolders(holders))
		}
		return err
	}
	return nil
}

// refuseHolders refuses r's mount while processes other than its daemon and
// the daemon's children use it.
func refuseHolders(ctx context.Context, r state.Record) error {
	key, err := mounts.KeyOf(r.Path)
	if err != nil {
		return err
	}
	holders, err := mounts.Holders(ctx, r.Path, key, r.DaemonPID)
	if err != nil || len(holders) == 0 {
		return err
	}
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("processes use %s, so slimtree left it mounted: %s", r.Path, describeHolders(holders)))
	ancestors := map[int]bool{}
	for pid := os.Getpid(); pid > 0 && !ancestors[pid]; {
		ancestors[pid] = true
		if pid, err = mounts.ParentPID(pid); err != nil {
			break
		}
	}
	for _, h := range holders {
		if ancestors[h.PID] {
			msg.WriteString(". slimtree, or a process it runs under, runs inside the worktree; run slimtree from a shell outside it")
			break
		}
	}
	return errors.New(msg.String())
}

func describeHolders(holders []mounts.Holder) string {
	described := make([]string, len(holders))
	for i, h := range holders {
		described[i] = fmt.Sprintf("%d %s (parent %d %s)", h.PID, h.Command, h.ParentPID, h.ParentCommand)
	}
	return strings.Join(described, ", ")
}

// resumeWorktree mounts the artifactfs worktree arg names again. A
// worktree whose recorded daemon serves its recorded mount stays as it is.
// Otherwise resumeWorktree stops what is left of the recorded daemon and
// its mount, and starts a daemon that mounts the empty mount point.
func resumeWorktree(ctx context.Context, home *state.Home, arg, goos string, stdout, stderr io.Writer) error {
	lock, err := home.Lock(ctx, "")
	if err != nil {
		return err
	}
	defer lock.Unlock()
	r, err := resolveArtifactFS(home, arg)
	if err != nil {
		return err
	}
	if err := healRemount(ctx, home, &r); err != nil {
		return err
	}
	alive, err := daemonAlive(r)
	if err != nil {
		return err
	}
	id, err := identify(r.Path)
	if err != nil {
		return err
	}
	if alive && id != "" && string(id) == r.MountIdentity {
		if err := checkMounted(ctx, r); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s is mounted\n", r.Path)
		return nil
	}
	if err := stopDaemon(ctx, r); err != nil {
		return err
	}
	// A second daemon on the state root fails to start.
	if pid, running, err := daemon.Running(r.StateDir); err != nil {
		return err
	} else if running {
		return fmt.Errorf("pid %d, a daemon slimtree did not record, serves %s; stop it, then resume", pid, r.StateDir)
	}
	if err := emptyMountPoint(r.Path); err != nil {
		return err
	}
	ctl, err := daemon.NewController(ctx, r.StateDir, logging.NewJSONLogger(stderr, slog.LevelWarn))
	if err != nil {
		return err
	}
	defer ctl.Close()
	if err := serve(ctx, home, ctl, r, goos); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "mounted %s\n", r.Path)
	return nil
}

// emptyMountPoint makes the directory path, where nothing is mounted, when
// it is missing, and refuses one that holds anything: a daemon mounts over
// it and would hide what it holds.
func emptyMountPoint(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Mkdir(path, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not mounted and holds files; slimtree mounts its worktree only on an empty directory", path)
	}
	return nil
}

// resumeAll resumes every artifactfs worktree, each under its own hold of
// the global lock. It reports each failure to stderr and goes on.
func resumeAll(ctx context.Context, home *state.Home, goos string, stdout, stderr io.Writer) error {
	records, err := home.Records(func(err error) { fmt.Fprintln(stderr, "slimtree:", err) })
	if err != nil {
		return err
	}
	failed, total := 0, 0
	for _, r := range records {
		if r.Backend != string(artifactFS) {
			continue
		}
		total++
		if err := resumeWorktree(ctx, home, r.Path, goos, stdout, stderr); err != nil {
			if ctx.Err() != nil {
				return err
			}
			failed++
			fmt.Fprintf(stderr, "slimtree: resume %s: %v\n", r.Path, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("slimtree resumed %d of %d artifactfs worktrees", total-failed, total)
	}
	return nil
}
