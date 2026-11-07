//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
// writes its session ID and SSH_ASKPASS_REQUIRE to the named file and fails.
const sshSessionReportEnv = "AFS_TEST_SSH_SESSION_REPORT"

func init() {
	if report := os.Getenv(sshSessionReportEnv); report != "" {
		sid, err := unix.Getsid(0)
		if err == nil {
			err = os.WriteFile(report, []byte(strconv.Itoa(sid)+" "+os.Getenv("SSH_ASKPASS_REQUIRE")), 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func TestLazyFetchUsesCoreSSHCommandWithoutTerminal(t *testing.T) {
	for _, tt := range []struct {
		name   string
		filter string
		read   func(ctx context.Context, store *Store, cfg model.RepoConfig, commit, blob string) error
	}{
		{"blob", "blob:none", func(ctx context.Context, store *Store, cfg model.RepoConfig, _, blob string) error {
			_, err := store.BlobToCache(ctx, cfg.RepoPaths, blob, filepath.Join(filepath.Dir(cfg.GitDir), "cache", blob))
			return err
		}},
		{"tree", "tree:0", func(ctx context.Context, store *Store, cfg model.RepoConfig, commit, _ string) error {
			_, err := store.BuildTreeIndex(ctx, cfg, commit)
			return err
		}},
		{"batch blob fetch", "blob:none", func(ctx context.Context, store *Store, cfg model.RepoConfig, _, blob string) error {
			return store.FetchMissingBlobs(ctx, cfg.RepoPaths, []string{blob})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gittest.IsolatePrompts(t)
			tmp := t.TempDir()
			gitDir, commit, blob := partialClone(t, tmp, tt.filter)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", "ssh://git@example.invalid/repo.git")
			run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", shellQuote(exe))
			report := filepath.Join(tmp, "ssh-session")
			t.Setenv(sshSessionReportEnv, report)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
			err = tt.read(ctx, store, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}, commit, blob)
			store.Close()
			if err == nil {
				t.Fatal("read a missing object through an SSH command that fails")
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatalf("the lazy fetch did not run core.sshCommand: %v", err)
			}
			testSID, err := unix.Getsid(0)
			if err != nil {
				t.Fatal(err)
			}
			sid, askpass, _ := strings.Cut(string(data), " ")
			if sid == strconv.Itoa(testSID) {
				t.Fatalf("core.sshCommand ran in the caller's session %d, which can have a controlling terminal", testSID)
			}
			if askpass != "never" {
				t.Fatalf("core.sshCommand ran with SSH_ASKPASS_REQUIRE=%q, want never", askpass)
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
	script := "#!/bin/sh\n(exec 3>\"$AFS_CHILD_FIFO\"; sleep 60 & : > \"$AFS_GIT_STARTED\"; wait; : > \"$AFS_CHILD_FINISHED\") &\nwait\n"
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
