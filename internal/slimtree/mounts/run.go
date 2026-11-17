package mounts

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
)

const (
	// commandTimeout bounds each external command. lsof can stall on a hung
	// volume anywhere, and an unmount on a hung mount.
	commandTimeout = 30 * time.Second
	// stopGrace is how long run waits for a command once its context ended.
	// A command in an uninterruptible kernel wait, such as an unmount(2) of a
	// hung mount, outlives SIGKILL, and exec.Cmd.Wait waits for it forever.
	stopGrace = 5 * time.Second
)

// waitHook, when set, runs once the command's wait returned, before run sees
// the result.
var waitHook func()

// run runs name with args to completion and returns its stdout. It starts
// the command in its own session, which has no controlling terminal, so no
// prompt can stop it. When ctx ends or commandTimeout passes, the command's
// process group gets SIGTERM, then SIGKILL, and run returns at most
// stopGrace later, whether or not the command exited. It runs the command
// through gitstore.RunWithFiles, so a child the command leaves holding its
// output cannot fail or stall it.
//
// A command that exits non-zero returns its stdout with an error that wraps
// its *exec.ExitError. A command that ctx or the bound stopped returns an
// error that wraps the cause and no *exec.ExitError.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, commandTimeout, fmt.Errorf("%s did not finish within %s", name, commandTimeout))
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	gitstore.ConfigureCancelableCommand(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdout bytes.Buffer
	stderr := gitstore.NewBoundedStderr()
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	command := strings.Join(append([]string{name}, args...), " ")
	// The goroutine owns stdout and stderr until it sends: a command that
	// outlives stopGrace leaves them to it.
	done := make(chan error, 1)
	hook := waitHook
	go func() {
		err := gitstore.RunWithFiles(cmd, "", nil, nil)
		if hook != nil {
			hook()
		}
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		select {
		case err = <-done:
		case <-time.After(stopGrace):
			return nil, fmt.Errorf("%s: %w; it had not exited %s after it was stopped", command, context.Cause(ctx), stopGrace)
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", command, context.Cause(ctx))
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
