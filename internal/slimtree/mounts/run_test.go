package mounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

func TestRunStartsTheCommandInItsOwnSession(t *testing.T) {
	t.Parallel()
	out, err := run(t.Context(), os.Args[0], helper, "session")
	if err != nil {
		t.Fatal(err)
	}
	var sid, pid int
	if _, err := fmt.Sscan(string(out), &sid, &pid); err != nil || sid != pid {
		t.Fatalf("helper printed %q (%v); want its session ID equal to its PID", out, err)
	}
}

// A child the command leaves behind holds its stdout and stderr. With a
// pipe, os/exec fails the command with exec.ErrWaitDelay, or without a
// WaitDelay waits for the child.
func TestRunSucceedsWhileAChildHoldsTheCommandsOutput(t *testing.T) {
	t.Parallel()
	out, err := run(t.Context(), os.Args[0], helper, "hold")
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	var token string
	if _, err := fmt.Sscan(string(out), &pid, &token); err != nil {
		t.Fatalf("helper printed %q: %v", out, err)
	}
	t.Cleanup(func() { _ = Signal(pid, token, syscall.SIGKILL) })
	if got, err := StartToken(pid); err != nil || got != token {
		t.Fatalf("the child that holds the output ended before run returned: token %q, %v; want %q", got, err, token)
	}
}

func TestRunStopsTheCommandsProcessGroupWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := gittest.ExpireOnFile(t.Context(), filepath.Join(dir, "started"))
	defer cancel()
	_, err := run(ctx, os.Args[0], helper, "hang", dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run = %v; want the deadline", err)
	}
	if _, exited := errors.AsType[*exec.ExitError](err); exited {
		t.Fatalf("run = %v; a stopped command must not read as one that exited", err)
	}
	end := filepath.Join(dir, "end")
	for {
		data, err := os.ReadFile(end)
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil || string(data) != "term" {
			t.Fatalf("sleeper in the command's process group ended with %q, %v; want term", data, err)
		}
		return
	}
}

// A command in an uninterruptible kernel wait outlives SIGKILL, and its wait
// never returns. No process can be put in one without a mount, so the hook
// holds the wait instead. Serial: it sets waitHook.
func TestRunStopsWaitingForACommandThatOutlivesItsStop(t *testing.T) {
	release := make(chan struct{})
	waitHook = func() { <-release }
	t.Cleanup(func() {
		close(release)
		waitHook = nil
	})
	dir := t.TempDir()
	ctx, cancel := gittest.ExpireOnFile(t.Context(), filepath.Join(dir, "started"))
	defer cancel()
	_, err := run(ctx, os.Args[0], helper, "hang", dir)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "had not exited "+stopGrace.String()+" after it was stopped") || !strings.Contains(err.Error(), os.Args[0]) {
		t.Fatalf("run = %v; want the deadline, naming the command that has not exited", err)
	}
}
