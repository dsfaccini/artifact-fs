//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

// sshSessionReportEnv makes this test binary act as an SSH command that
// appends a line with its session ID, whether it got BatchMode=yes, and
// SSH_ASKPASS_REQUIRE to the named file, then fails.
const sshSessionReportEnv = "AFS_TEST_SSH_SESSION_REPORT"

// sshServeOnceEnv makes the first run of that SSH command serve the named
// repository with git-upload-pack instead of failing, so a command that
// contacts the remote twice reaches its second contact.
const sshServeOnceEnv = "AFS_TEST_SSH_SERVE_ONCE"

func init() {
	if report := os.Getenv(sshSessionReportEnv); report != "" {
		_, statErr := os.Stat(report)
		first := errors.Is(statErr, os.ErrNotExist)
		sid, err := unix.Getsid(0)
		if err == nil {
			var f *os.File
			if f, err = os.OpenFile(report, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, err = fmt.Fprintf(f, "%d %t %s\n", sid, slices.Contains(os.Args[1:], "BatchMode=yes"), os.Getenv("SSH_ASKPASS_REQUIRE"))
				err = errors.Join(err, f.Close())
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if repo := os.Getenv(sshServeOnceEnv); repo != "" && first {
			cmd := exec.Command("git-upload-pack", repo)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
		os.Exit(1)
	}
}

// sshContact is one run of the SSH command that sshSessionReportEnv names.
type sshContact struct {
	sid       string
	batchMode bool
	askpass   string
}

func readSSHContacts(t *testing.T, report string) []sshContact {
	t.Helper()
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("git did not run the SSH command: %v", err)
	}
	var contacts []sshContact
	for line := range strings.Lines(string(data)) {
		sid, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		batchMode, askpass, _ := strings.Cut(rest, " ")
		contacts = append(contacts, sshContact{sid: sid, batchMode: batchMode == "true", askpass: askpass})
	}
	return contacts
}

// fakeSSHOnPath puts this test binary on PATH as ssh, the command Git runs
// when no SSH command is set.
func fakeSSHOnPath(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Every Git command that can contact a remote runs SSH without a controlling
// terminal. It runs the user's core.sshCommand as set, and plain ssh with
// BatchMode, so ssh does not try passwords that a server may count.
func TestNetworkGitRunsSSHWithoutTerminal(t *testing.T) {
	const sshRemote = "ssh://git@example.invalid/repo.git"
	// elsewhere is a repo config that clones or acquires from sshRemote next
	// to cfg's git dir.
	elsewhere := func(cfg model.RepoConfig, branch string) model.RepoConfig {
		return model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: filepath.Join(filepath.Dir(cfg.GitDir), "other.git")}, RepoSpec: model.RepoSpec{RemoteURL: sshRemote, Branch: branch}}
	}
	for _, tt := range []struct {
		name   string
		filter string
		// globalSSH names the SSH command in command-scope config, as a
		// global core.sshCommand would, for a command that runs before a repo
		// config exists to hold core.sshCommand.
		globalSSH bool
		// serveOnce lets the first SSH contact succeed, for a command that
		// contacts the remote twice.
		serveOnce bool
		read      func(ctx context.Context, store *Store, cfg model.RepoConfig, commit, blob string) error
	}{
		{"blob", "blob:none", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, blob string) error {
			_, err := store.BlobToCache(ctx, cfg.RepoPaths, blob, filepath.Join(filepath.Dir(cfg.GitDir), "cache", blob))
			return err
		}},
		{"tree", "tree:0", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, commit, _ string) error {
			_, err := store.BuildTreeIndex(ctx, cfg, commit)
			return err
		}},
		{"batch blob fetch", "blob:none", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, blob string) error {
			return store.FetchMissingBlobs(ctx, cfg.RepoPaths, []string{blob})
		}},
		// A clone without trees and with no checkout has no index, so its
		// read-tree fetches HEAD's trees.
		{"index init", "tree:0", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			return store.EnsureIndexInitialized(ctx, cfg)
		}},
		{"branch transition", "tree:0", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			return store.PrepareFetchedBranch(ctx, cfg, "main")
		}},
		{"refresh fetch", "blob:none", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			cfg.FetchRef = "refs/heads/main"
			return store.FetchNonInteractive(ctx, cfg)
		}},
		{"prepare fetch", "blob:none", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			return store.FetchRefWithCredentials(ctx, cfg, "refs/heads/main")
		}},
		{"clone", "blob:none", true, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			return store.CloneBlobless(ctx, elsewhere(cfg, "refs/heads/main"))
		}},
		{"clone of an exact ref", "blob:none", true, true, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
			return store.CloneBlobless(ctx, elsewhere(cfg, "refs/tags/v1"))
		}},
		{"verified source fetch", "blob:none", true, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, commit, _ string) error {
			_, err := store.PrepareSource(ctx, elsewhere(cfg, ""), model.SourceRequirement{Ref: "refs/heads/main", RequiredCommit: commit})
			return err
		}},
	} {
		for _, sshCommand := range []bool{true, false} {
			name := "core.sshCommand"
			if !sshCommand {
				name = "plain ssh"
			}
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				gittest.IsolatePrompts(t)
				// Git would first probe an SSH command it does not know with -G.
				t.Setenv("GIT_SSH_VARIANT", "ssh")
				tmp := t.TempDir()
				gitDir, commit, blob := partialClone(t, tmp, tt.filter)
				run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", sshRemote)
				if sshCommand {
					exe, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", shellScriptQuote(exe))
					if tt.globalSSH {
						t.Setenv("GIT_CONFIG_COUNT", "1")
						t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
						t.Setenv("GIT_CONFIG_VALUE_0", shellScriptQuote(exe))
					}
				} else {
					fakeSSHOnPath(t)
				}
				if tt.serveOnce {
					src := filepath.Join(tmp, "src")
					run(t, "git", "-C", src, "tag", "v1")
					t.Setenv(sshServeOnceEnv, src)
				}
				report := filepath.Join(tmp, "ssh-session")
				t.Setenv(sshSessionReportEnv, report)

				ctx := t.Context()
				store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
				err := tt.read(ctx, store, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}, commit, blob)
				store.Close()
				if err == nil {
					t.Fatal("reached a remote through an SSH command that fails")
				}
				contacts := readSSHContacts(t, report)
				testSID, err := unix.Getsid(0)
				if err != nil {
					t.Fatal(err)
				}
				if tt.serveOnce && len(contacts) < 2 {
					t.Fatalf("git contacted the remote %d times, want a second contact after the served one", len(contacts))
				}
				for _, contact := range contacts {
					if contact.sid == strconv.Itoa(testSID) {
						t.Fatalf("SSH ran in the caller's session %d, which can have a controlling terminal", testSID)
					}
					if contact.askpass != "never" {
						t.Fatalf("SSH ran with SSH_ASKPASS_REQUIRE=%q, want never", contact.askpass)
					}
					if !sshCommand && !contact.batchMode {
						t.Fatal("plain ssh ran without BatchMode=yes")
					}
				}
			})
		}
	}
}

// git clone applies a core.sshCommand that a conditional include sets for the
// repository it creates, such as one that picks a deploy key by remote URL.
func TestCloneKeepsAnIncludedCoreSSHCommand(t *testing.T) {
	gittest.IsolatePrompts(t)
	fakeSSHOnPath(t)
	tmp := t.TempDir()
	t.Setenv(sshSessionReportEnv, filepath.Join(tmp, "plain-ssh"))
	runs := filepath.Join(tmp, "deploy-ssh-runs")
	deploySSH := filepath.Join(tmp, "deploy-ssh")
	if err := os.WriteFile(deploySSH, []byte("#!/bin/sh\necho \"$*\" >> '"+runs+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	include := filepath.Join(tmp, "deploy.inc")
	if err := os.WriteFile(include, []byte("[core]\n\tsshCommand = "+deploySSH+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(tmp, "global")
	if err := os.WriteFile(global, []byte("[includeIf \"hasconfig:remote.*.url:ssh://git@corp.invalid/**\"]\n\tpath = "+include+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	store.gitRetryDelays = nil
	defer store.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: filepath.Join(tmp, "x.git")}, RepoSpec: model.RepoSpec{RemoteURL: "ssh://git@corp.invalid/repo.git", Branch: "refs/heads/main"}}
	if err := store.CloneBlobless(t.Context(), cfg); err == nil {
		t.Fatal("cloned through an SSH command that fails")
	}
	if _, err := os.Stat(runs); err != nil {
		t.Fatalf("the clone did not run the core.sshCommand its included config sets: %v", err)
	}
}

// A Store writes its SSH wrapper again when a temp cleaner removes it, as one
// can under a long-running daemon, and removes it on Close.
func TestSSHWrapperLastsUntilClose(t *testing.T) {
	gittest.IsolatePrompts(t)
	fakeSSHOnPath(t)
	tmp := t.TempDir()
	gitDir, _, _ := partialClone(t, tmp, "blob:none")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", "ssh://git@example.invalid/repo.git")
	report := filepath.Join(tmp, "ssh-session")
	t.Setenv(sshSessionReportEnv, report)
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	store.gitRetryDelays = nil
	defer store.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: gitDir}, RepoSpec: model.RepoSpec{FetchRef: "refs/heads/main"}}
	fetch := func() {
		t.Helper()
		if err := os.Remove(report); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := store.FetchNonInteractive(t.Context(), cfg); err == nil {
			t.Fatal("fetched through an SSH command that fails")
		}
		for _, contact := range readSSHContacts(t, report) {
			if !contact.batchMode {
				t.Fatal("ssh ran without BatchMode=yes")
			}
		}
	}
	fetch()
	if err := os.RemoveAll(store.sshWrapperDir); err != nil {
		t.Fatal(err)
	}
	fetch()
	dir := store.sshWrapperDir
	store.Close()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close left the SSH wrapper's directory: %v", err)
	}
}

// A Store reuses its SSH wrapper while the wrapper and its directory are
// private to the effective uid and neither is a symlink, and only then. A
// temp cleaner can remove the directory, and another local user can then make
// one at its path and put a program there. The Store writes a new wrapper
// elsewhere, and nothing into what it found. It removes nothing it did not
// make.
func TestSSHWrapperIsReusedOnlyWhilePrivate(t *testing.T) {
	// plant writes a program named ssh in a new directory and returns the
	// directory.
	plant := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tt := range []struct {
		name string
		root bool
		// reused is whether the Store keeps the wrapper after replace.
		reused bool
		// replace changes the wrapper's directory dir and returns the
		// directory that holds what dir/ssh now reaches.
		replace func(t *testing.T, dir string) (string, error)
	}{
		{name: "unchanged", reused: true, replace: func(t *testing.T, dir string) (string, error) {
			return dir, nil
		}},
		{name: "directory symlink", replace: func(t *testing.T, dir string) (string, error) {
			planted := plant(t)
			return planted, errors.Join(os.RemoveAll(dir), os.Symlink(planted, dir))
		}},
		{name: "directory mode", replace: func(t *testing.T, dir string) (string, error) {
			return dir, os.Chmod(dir, 0o755)
		}},
		{name: "directory owner", root: true, replace: func(t *testing.T, dir string) (string, error) {
			return dir, os.Lchown(dir, 1, 1)
		}},
		{name: "wrapper symlink", replace: func(t *testing.T, dir string) (string, error) {
			planted := plant(t)
			wrapper := filepath.Join(dir, "ssh")
			return planted, errors.Join(os.Remove(wrapper), os.Symlink(filepath.Join(planted, "ssh"), wrapper))
		}},
		{name: "wrapper mode", replace: func(t *testing.T, dir string) (string, error) {
			return dir, os.Chmod(filepath.Join(dir, "ssh"), 0o755)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.root && os.Geteuid() != 0 {
				t.Skip("only root can give a directory another owner")
			}
			gittest.IsolatePrompts(t)
			fakeSSHOnPath(t)
			store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer store.Close()
			first := filepath.Dir(sshWrapper(t, store))
			watched, err := tt.replace(t, first)
			if err != nil {
				t.Fatal(err)
			}
			before := dirContents(t, watched)
			second := filepath.Dir(sshWrapper(t, store))
			switch {
			case tt.reused && second != first:
				t.Fatalf("wrote a new SSH wrapper in %s, though the one in %s was unchanged", second, first)
			case !tt.reused && second == first:
				t.Fatalf("reused the SSH wrapper in %s after a %s change", first, tt.name)
			}
			after := dirContents(t, watched)
			// The Store may remove its own replaced directory, but nothing else.
			if after == nil && (tt.reused || watched != first) {
				t.Fatalf("removed %s", watched)
			}
			if after != nil && !maps.Equal(before, after) {
				t.Fatalf("wrote into %s: %q became %q", watched, before, after)
			}
		})
	}
}

// batchModeSSH holds the Store's lock across no file operation, so a blob read
// does not wait while a fetch writes the SSH wrapper. Of two calls that write
// one at once, the first to finish wins, and the other removes its own.
func TestBlobReadProceedsWhileAnSSHWrapperIsWritten(t *testing.T) {
	gittest.IsolatePrompts(t)
	fakeSSHOnPath(t)
	gitDir, _, blob := partialClone(t, t.TempDir(), "blob:none")
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close()
	reached, release := pauseFirstSSHWrapper(t, store)
	type result struct {
		wrapper string
		err     error
	}
	fetch := make(chan result, 1)
	go func() {
		env, err := store.lazyFetchEnv()
		wrapper, _ := envValue(env, "GIT_SSH")
		fetch <- result{wrapper, err}
	}()
	paused := <-reached
	data, err := store.ReadBlob(t.Context(), model.RepoPaths{Name: "x", GitDir: gitDir}, blob, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data\n" {
		t.Fatalf("read %q", data)
	}
	release()
	got := <-fetch
	if got.err != nil {
		t.Fatal(got.err)
	}
	if want := filepath.Join(store.sshWrapperDir, "ssh"); got.wrapper != want {
		t.Fatalf("the fetch got the SSH wrapper %q, not the Store's %q", got.wrapper, want)
	}
	if _, err := os.Stat(paused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the losing call left its SSH wrapper directory: %v", err)
	}
}

// Close does not wait while a call writes the SSH wrapper. The call then
// removes its directory and returns the closed error.
func TestCloseWhileAnSSHWrapperIsWritten(t *testing.T) {
	gittest.IsolatePrompts(t)
	fakeSSHOnPath(t)
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	reached, release := pauseFirstSSHWrapper(t, store)
	fetch := make(chan error, 1)
	go func() {
		_, err := store.lazyFetchEnv()
		fetch <- err
	}()
	paused := <-reached
	store.Close()
	release()
	if err := <-fetch; !errors.Is(err, errStoreClosed) {
		t.Fatalf("lazyFetchEnv after Close returned %v, want %v", err, errStoreClosed)
	}
	if _, err := os.Stat(paused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the call left its SSH wrapper directory after Close: %v", err)
	}
}

// pauseFirstSSHWrapper pauses store's first call that writes an SSH wrapper,
// once it made the wrapper's directory. reached yields that directory when
// the call pauses, and release lets the call go on.
func pauseFirstSSHWrapper(t *testing.T, store *Store) (reached <-chan string, release func()) {
	dirs := make(chan string, 1)
	gate := make(chan struct{})
	var paused atomic.Bool
	store.testHookSSHWrapperDirMade = func(dir string) {
		if paused.CompareAndSwap(false, true) {
			dirs <- dir
			<-gate
		}
	}
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	return dirs, release
}

// sshWrapper returns the GIT_SSH that store's lazyFetchEnv sets.
func sshWrapper(t *testing.T, store *Store) string {
	t.Helper()
	env, err := store.lazyFetchEnv()
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := envValue(env, "GIT_SSH")
	if !ok {
		t.Fatalf("lazyFetchEnv set no GIT_SSH: %q", env)
	}
	return wrapper
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, key+"="); ok {
			return value, true
		}
	}
	return "", false
}

// dirContents maps each file in dir to its content, following symlinks, or
// returns nil when dir does not exist.
func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents[entry.Name()] = string(data)
	}
	return contents
}

// An SSH program the environment names runs as the user set it, without
// BatchMode added.
func TestNetworkGitKeepsTheEnvironmentsSSHProgram(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ key, value string }{
		{"GIT_SSH_COMMAND", shellScriptQuote(exe)},
		{"GIT_SSH", exe},
	} {
		t.Run(tt.key, func(t *testing.T) {
			gittest.IsolatePrompts(t)
			t.Setenv("GIT_SSH_VARIANT", "ssh")
			// Plain ssh on PATH reports BatchMode, so a BatchMode command
			// that replaced the user's program would show.
			fakeSSHOnPath(t)
			t.Setenv(tt.key, tt.value)
			tmp := t.TempDir()
			gitDir, _, _ := partialClone(t, tmp, "blob:none")
			run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", "ssh://git@example.invalid/repo.git")
			report := filepath.Join(tmp, "ssh-session")
			t.Setenv(sshSessionReportEnv, report)
			store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
			store.gitRetryDelays = nil
			if err := store.FetchNonInteractive(t.Context(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: gitDir}, RepoSpec: model.RepoSpec{FetchRef: "refs/heads/main"}}); err == nil {
				t.Fatal("fetched through an SSH command that fails")
			}
			for _, contact := range readSSHContacts(t, report) {
				if contact.batchMode {
					t.Fatalf("%s did not run: Git ran SSH with BatchMode=yes", tt.key)
				}
			}
		})
	}
}

func TestRunGitCancellationKillsDescendants(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "started")
	finished := filepath.Join(tmp, "child-finished")
	fifo := filepath.Join(tmp, "child.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Without O_NONBLOCK, opening the read end would wait for the child to open
	// the write end. Reads then block, because Go does not poll FIFOs on macOS.
	childOpen, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer childOpen.Close()
	if err := syscall.SetNonblock(int(childOpen.Fd()), false); err != nil {
		t.Fatal(err)
	}
	// The child marks git started only once its sleep runs, so the cancel cannot race the fork.
	// The child and its sleep ignore SIGTERM, so only the SIGKILL that follows it ends them.
	script := "#!/bin/sh\n(trap '' TERM; exec 3>\"$AFS_CHILD_FIFO\"; sleep 60 & : > \"$AFS_GIT_STARTED\"; wait; : > \"$AFS_CHILD_FINISHED\") &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("AFS_CHILD_FIFO", fifo)
	t.Setenv("AFS_CHILD_FINISHED", finished)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := runGit(ctx, "", "version")
		errCh <- err
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("runGit returned before git started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("runGit error = %v, want context canceled", err)
	}
	// The child and its sleep hold the only write end, so the read ends when they
	// exit, whether or not anything reaps them. A child that survives
	// cancellation exits only after its sleep, so it writes the marker.
	if _, err := io.ReadAll(childOpen); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git descendant survived cancellation")
	}
}

func TestRunGitCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		_, err := runGit(ctx, "", "version")
		return err
	})
}

func TestBlobToCacheCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer store.Close()
		oid := strings.Repeat("a", 40)
		dir := t.TempDir()
		_, err := store.BlobToCache(ctx, model.RepoPaths{ID: "x", GitDir: dir}, oid, filepath.Join(dir, "cache", oid))
		return err
	})
}

func TestStreamTreeRecordsCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		store := New(nil)
		defer store.Close()
		return store.streamTreeRecords(ctx, t.TempDir(), strings.Repeat("a", 40), func(string) {})
	})
}

func TestBatchResolveSizesCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		return New(nil).batchResolveSizes(
			ctx,
			model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}},
			[]model.BaseNode{{ObjectOID: strings.Repeat("a", 40)}},
			[]string{strings.Repeat("a", 40)},
			map[string][]int{strings.Repeat("a", 40): {0}},
		)
	})
}

func assertEscapedGitCancellationIsBounded(t *testing.T, call func(context.Context) error) {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "escaped")
	finished := filepath.Join(tmp, "escaped-finished")
	script := "#!/bin/sh\n\"$AFS_TEST_BINARY\" -test.run '^TestRunGitEscapedDescriptorHelper$' &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_TEST_BINARY", executable)
	t.Setenv("AFS_ESCAPED_HELPER", "1")
	t.Setenv("AFS_ESCAPED_MARKER", marker)
	t.Setenv("AFS_ESCAPED_FINISHED", finished)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- call(ctx)
	}()
	var helperPID int
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			helperPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-errCh:
			t.Fatalf("Git call returned before the escaped helper started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer syscall.Kill(helperPID, syscall.SIGKILL)

	// The helper holds the descriptors until it is killed or marks its minute
	// finished, so the call returns before that mark only if it did not wait
	// for the descriptors.
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("runGit error = %v, want context canceled", err)
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("Git call waited for the escaped helper to finish")
	}
}

func TestRunGitEscapedDescriptorHelper(t *testing.T) {
	if os.Getenv("AFS_ESCAPED_HELPER") != "1" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	marker := os.Getenv("AFS_ESCAPED_MARKER")
	markerTmp := marker + ".tmp"
	if err := os.WriteFile(markerTmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Rename(markerTmp, marker); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Setsid puts the helper beyond the process-group kill, and a timeout panic
	// skips the test's kill, so the sleep bounds how long an orphan lives.
	time.Sleep(time.Minute)
	if err := os.WriteFile(os.Getenv("AFS_ESCAPED_FINISHED"), nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// The daemon prepares a --prepared-gitdir with FetchRefAmbient. It must run
// the git dir's own core.sshCommand, such as one that selects a deploy key.
func TestPreparedGitDirFetchHonorsCoreSSHCommand(t *testing.T) {
	gittest.IsolatePrompts(t)
	tmp := t.TempDir()
	gitDir, _, _ := partialClone(t, tmp, "blob:none")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", "ssh://git@example.invalid/repo.git")
	run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", shellScriptQuote(exe))
	report := filepath.Join(tmp, "ssh-session")
	t.Setenv(sshSessionReportEnv, report)
	ctx := t.Context()
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	store.gitRetryDelays = nil
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: gitDir}, RepoSpec: model.RepoSpec{PreparedGitDir: true}}
	if err := store.FetchRefAmbient(ctx, cfg, "refs/heads/main"); err == nil {
		t.Fatal("fetched through an SSH command that fails")
	}
	if _, err := os.Stat(report); err != nil {
		t.Fatalf("the preparation fetch did not run the git dir's core.sshCommand: %v", err)
	}
}

// A user's core.sshCommand runs without BatchMode, so SSH can still ask to
// accept an unknown host key or for a key's passphrase. Without a controlling
// terminal and with askpass disabled, both prompts fail. DISPLAY makes the
// SSH_ASKPASS that IsolatePrompts records eligible for both prompts.
func TestNetworkGitSSHPromptsFailWithoutTerminal(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		sshd = "/usr/sbin/sshd"
	}
	skip := t.Skipf
	if os.Getenv("CI") != "" {
		// CI runs go test without -v, so a skip there would go unseen.
		skip = t.Fatalf
	}
	if _, err := os.Stat(sshd); err != nil {
		skip("no sshd to answer SSH: %v", err)
	}
	askpassRuns := gittest.IsolatePrompts(t)
	t.Setenv("DISPLAY", ":0")
	tmp := t.TempDir()
	keys := filepath.Join(tmp, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	hostKey, userKey := filepath.Join(keys, "host"), filepath.Join(keys, "user")
	run(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	run(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "a passphrase", "-f", userKey)
	// Without the public key, SSH must decrypt the private key to offer it.
	if err := os.Remove(userKey + ".pub"); err != nil {
		t.Fatal(err)
	}
	sshdConfig := filepath.Join(keys, "sshd_config")
	if err := os.WriteFile(sshdConfig, []byte("HostKey "+hostKey+"\nPidFile none\nStrictModes no\nUsePAM no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(keys, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// OpenSSH's own tests serve SSH this way, without a listening daemon.
	ssh := "ssh -F /dev/null -o IdentityAgent=none -o GlobalKnownHostsFile=/dev/null -o 'ProxyCommand=" + sshd + " -i -f " + sshdConfig + "'"
	probe := ssh + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PreferredAuthentications=none git@localhost true"
	if out, _ := exec.Command("sh", "-c", probe).CombinedOutput(); !strings.Contains(string(out), "Permission denied") {
		skip("sshd -i does not serve SSH here: %s", out)
	}
	gitDir, _, _ := partialClone(t, tmp, "blob:none")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", "ssh://git@localhost/repo.git")
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	store.gitRetryDelays = nil
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "x", GitDir: gitDir}}
	for _, tt := range []struct {
		name, ssh, want string
	}{
		{"unknown host key", ssh + " -o UserKnownHostsFile=" + knownHosts, "Host key verification failed"},
		{"key passphrase", ssh + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o PreferredAuthentications=publickey -i " + userKey, "Permission denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", tt.ssh)
			err := store.FetchRefAmbient(t.Context(), cfg, "refs/heads/main")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("fetch error = %v, want %q", err, tt.want)
			}
		})
	}
	gittest.RequireNoPrompt(t, askpassRuns)
}

// A canceled git command's error carries the caller's context error, not the
// signal the cancellation sent.
func TestCanceledRunGitErrorOmitsCancellationSignal(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "--allow-empty", "-m", "base")
	gitDir := filepath.Join(repo, ".git")
	blocked := filepath.Join(tmp, "blocked")
	hook := filepath.Join(tmp, "hook")
	script := "#!/bin/sh\nif [ ! -e \"$AFS_BLOCKED\" ]; then\n  : > \"$AFS_BLOCKED\"\n  sleep 60\nfi\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)
	t.Setenv("AFS_BLOCKED", blocked)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := runGit(ctx, gitDir, "read-tree", "-m", "HEAD", "HEAD")
		errCh <- err
	}()
	for {
		if _, err := os.Stat(blocked); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("read-tree returned before the hook ran: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if strings.Contains(err.Error(), "signal:") {
		t.Fatalf("canceled runGit error carries the cancellation's signal: %q", err.Error())
	}
}

// A blob read canceled while its lazy fetch runs auto-maintenance leaves no
// objects/maintenance.lock: git maintenance run --auto skips without a word
// while that lock exists.
func TestCanceledLazyFetchLeavesNoMaintenanceLock(t *testing.T) {
	tmp := t.TempDir()
	gitDir, _, blob := partialClone(t, tmp, "blob:none")
	// The lazy fetch adds a second pack, so gc --auto runs its pre-auto-gc hook
	// while git maintenance run --auto holds objects/maintenance.lock.
	run(t, "git", "--git-dir", gitDir, "config", "gc.autoPackLimit", "1")
	// Git 2.45 and 2.46 run a fetch's auto-maintenance in the foreground with
	// the gc task on; newer Git needs these two settings to do the same.
	run(t, "git", "--git-dir", gitDir, "config", "maintenance.autoDetach", "false")
	run(t, "git", "--git-dir", gitDir, "config", "maintenance.gc.enabled", "true")
	blocked := filepath.Join(tmp, "pre-auto-gc-blocked")
	hook := "#!/bin/sh\n: > \"$AFS_BLOCKED\"\nsleep 60\nexit 1\n"
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "hooks", "pre-auto-gc"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_BLOCKED", blocked)
	lock := filepath.Join(gitDir, "objects", "maintenance.lock")

	ctx, cancel := context.WithCancel(context.Background())
	store := New(nil)
	defer store.Close()
	errCh := make(chan error, 1)
	go func() {
		_, err := store.BlobToCache(ctx, model.RepoPaths{ID: "x", GitDir: gitDir}, blob, filepath.Join(tmp, "cache", blob))
		errCh <- err
	}()
	for {
		if _, err := os.Stat(blocked); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("BlobToCache returned before auto-maintenance ran: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("maintenance does not hold its lock while the hook runs: %v", err)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	store.Close()
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("a canceled blob read left objects/maintenance.lock behind")
	}
}
