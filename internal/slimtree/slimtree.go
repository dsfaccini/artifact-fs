// Package slimtree is the slimtree CLI, which makes disk-light git worktrees
// with an APFS clone or an artifact-fs mount.
package slimtree

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/cloudflare/artifact-fs/internal/cli"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/version"
	ucli "github.com/urfave/cli/v3"
)

// Main runs slimtree with the arguments the process received, its program
// name first. The daemon that serves an artifactfs worktree runs this binary,
// so Git runs it as the FSMonitor hook: through the hook link, or with
// fsmonitor-hook from the hook script. Main hands the hook link, the hidden
// daemon and fsmonitor-hook commands, and "afs <command>" to artifact-fs's
// CLI.
func Main(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if _, _, ok := gitstore.ParseFSMonitorHookLink(args[0]); ok {
		return cli.Main(ctx, args, stdout, stderr)
	}
	if len(args) > 1 {
		switch args[1] {
		case "daemon", "fsmonitor-hook":
			return cli.Run(ctx, args[1:], stdout, stderr)
		case "afs":
			return cli.Run(ctx, args[2:], stdout, stderr)
		}
	}
	return run(ctx, runtime.GOOS, args[1:], stdout, stderr)
}

// run runs slimtree's own commands as they run on goos. The cli package sets
// the urfave version printer, which prints "slimtree <version.String()>".
func run(ctx context.Context, goos string, args []string, stdout io.Writer, stderr io.Writer) int {
	app := &ucli.Command{
		Name:      "slimtree",
		Usage:     "disk-light git worktrees from an APFS clone or an artifact-fs mount",
		Version:   version.String(),
		Writer:    stdout,
		ErrWriter: stderr,
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:      "backend",
				Value:     string(defaultBackend(goos)),
				Usage:     backendUsage(goos),
				Validator: func(b string) error { return checkBackend(goos, backend(b)) },
			},
		},
	}
	if err := app.Run(ctx, append([]string{"slimtree"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
