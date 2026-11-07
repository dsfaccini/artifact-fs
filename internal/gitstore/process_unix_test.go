//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

// sshSessionReportEnv makes this test binary act as an SSH command that
// appends a line with its session ID and SSH_ASKPASS_REQUIRE to the named
// file and fails.
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
				_, err = fmt.Fprintf(f, "%d %s\n", sid, os.Getenv("SSH_ASKPASS_REQUIRE"))
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

func TestLazyFetchUsesCoreSSHCommandWithoutTerminal(t *testing.T) {
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
		{"refresh fetch", "blob:none", false, false, func(ctx context.Context, store *Store, cfg model.RepoConfig, _, _ string) error {
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
		t.Run(tt.name, func(t *testing.T) {
			gittest.IsolatePrompts(t)
			// Git would first probe an SSH command it does not know with -G.
			t.Setenv("GIT_SSH_VARIANT", "ssh")
			tmp := t.TempDir()
			gitDir, commit, blob := partialClone(t, tmp, tt.filter)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", sshRemote)
			run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", shellScriptQuote(exe))
			if tt.globalSSH {
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
				t.Setenv("GIT_CONFIG_VALUE_0", shellScriptQuote(exe))
			}
			if tt.serveOnce {
				src := filepath.Join(tmp, "src")
				run(t, "git", "-C", src, "tag", "v1")
				t.Setenv(sshServeOnceEnv, src)
			}
			report := filepath.Join(tmp, "ssh-session")
			t.Setenv(sshSessionReportEnv, report)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
			err = tt.read(ctx, store, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}, commit, blob)
			store.Close()
			if err == nil {
				t.Fatal("reached a remote through an SSH command that fails")
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatalf("git did not run the SSH command: %v", err)
			}
			testSID, err := unix.Getsid(0)
			if err != nil {
				t.Fatal(err)
			}
			contacts := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if tt.serveOnce && len(contacts) < 2 {
				t.Fatalf("git contacted the remote %d times, want a second contact after the served one", len(contacts))
			}
			for _, contact := range contacts {
				sid, askpass, _ := strings.Cut(contact, " ")
				if sid == strconv.Itoa(testSID) {
					t.Fatalf("core.sshCommand ran in the caller's session %d, which can have a controlling terminal", testSID)
				}
				if askpass != "never" {
					t.Fatalf("core.sshCommand ran with SSH_ASKPASS_REQUIRE=%q, want never", askpass)
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
		return streamTreeRecords(ctx, t.TempDir(), strings.Repeat("a", 40), func(string) {})
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

// Git runs SSH without BatchMode, so SSH can still ask to accept an unknown
// host key or for a key's passphrase. Without a controlling terminal and with
// askpass disabled, both prompts fail. DISPLAY makes the SSH_ASKPASS that
// IsolatePrompts records eligible for both prompts.
func TestNetworkGitSSHPromptsFailWithoutTerminal(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		sshd = "/usr/sbin/sshd"
	}
	if _, err := os.Stat(sshd); err != nil {
		t.Skipf("no sshd to answer SSH: %v", err)
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
		t.Skipf("sshd -i does not serve SSH here: %s", out)
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
