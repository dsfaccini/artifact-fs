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

func TestBlobToCacheLazyFetchUsesCoreSSHCommandWithoutTerminal(t *testing.T) {
	// Keep the machine's SSH and Git settings out of the lazy fetch.
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH", "SSH_ASKPASS_REQUIRE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	run(t, "git", "init", "--initial-branch", "main", src)
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", src, "add", "file.txt")
	run(t, "git", "-C", src, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", src, "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(tmp, "clone")
	run(t, "git", "clone", "--filter=blob:none", "--no-checkout", "file://"+src, clone)
	blobOID := strings.TrimSpace(runOutput(t, "git", "-C", src, "rev-parse", "HEAD:file.txt"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", clone, "remote", "set-url", "origin", "ssh://git@example.invalid/repo.git")
	run(t, "git", "-C", clone, "config", "core.sshCommand", shellQuote(exe))
	report := filepath.Join(tmp, "ssh-session")
	t.Setenv(sshSessionReportEnv, report)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(clone, ".git")}
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := store.BlobToCache(ctx, cfg, blobOID, filepath.Join(tmp, "cache", blobOID)); err == nil {
		t.Fatal("BlobToCache fetched a blob through an SSH command that fails")
	}
	store.Close()
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
}

func TestRunGitCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		_, err := runGit(ctx, "", "version")
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
			model.RepoConfig{ID: "repo", GitDir: t.TempDir()},
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
	script := "#!/bin/sh\n\"$AFS_TEST_BINARY\" -test.run '^TestRunGitEscapedDescriptorHelper$' &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_TEST_BINARY", executable)
	t.Setenv("AFS_ESCAPED_HELPER", "1")
	t.Setenv("AFS_ESCAPED_MARKER", marker)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- call(ctx)
	}()
	deadline := time.Now().Add(time.Second)
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
		if time.Now().After(deadline) {
			t.Fatal("escaped helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if helper, err := os.FindProcess(helperPID); err == nil {
		defer helper.Kill()
	}

	started := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runGit error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runGit remained blocked by escaped descriptor holder")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Git cancellation took %v, want bounded descriptor wait", elapsed)
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
	time.Sleep(10 * time.Second)
}
