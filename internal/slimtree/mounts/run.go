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

// commandTimeout bounds each external command. lsof can stall on a hung
// volume anywhere, and an unmount on a hung mount.
const commandTimeout = 30 * time.Second

// run runs name with args to completion and returns its stdout. It bounds
// the command at commandTimeout, and starts it in its own session, which has
// no controlling terminal, so no prompt can stop it. When ctx ends or the
// bound passes, the command's process group gets SIGTERM, then SIGKILL. It
// runs the command through gitstore.RunWithFiles, so a child the command
// leaves holding its output cannot fail or stall it.
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
	err := gitstore.RunWithFiles(cmd, "", nil, nil)
	command := strings.Join(append([]string{name}, args...), " ")
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", command, context.Cause(ctx))
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
