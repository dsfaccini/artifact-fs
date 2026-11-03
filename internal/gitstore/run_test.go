package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunGitClassifiesBeforeCredentialRedaction(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'HTTP 500 transport failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runGitWithEnv(context.Background(), "", []string{"ARTIFACT_FS_GIT_PASSWORD=0"}, "version")
	_, _, retryable := gitOperationCauses(err)
	if !retryable {
		t.Fatalf("redacted command error was not retryable: %v", err)
	}
	if strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("command error leaked credential value: %v", err)
	}
}

func TestRunGitCancellationKillsDescendants(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "started")
	childPID := filepath.Join(tmp, "child.pid")
	script := "#!/bin/sh\nsleep 60 &\necho $! > \"$AFS_CHILD_PID\"\n: > \"$AFS_GIT_STARTED\"\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("AFS_CHILD_PID", childPID)

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
	data, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = child.Kill()
		}
	})
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("runGit error = %v, want context canceled", err)
	}
	// The child sleeps far longer than this wait, so it is gone only if cancellation killed it.
	for deadline := time.Now().Add(10 * time.Second); child.Signal(syscall.Signal(0)) == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("git descendant %d survived cancellation", pid)
		}
	}
}

func TestKillCommandProcessGroupReportsExitedProcess(t *testing.T) {
	cmd := exec.Command("git", "version")
	configureCommandProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killCommandProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill exited process error = %v, want os.ErrProcessDone", err)
	}
}
