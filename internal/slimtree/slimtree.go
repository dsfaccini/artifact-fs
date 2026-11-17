// Package slimtree is the slimtree CLI, which makes disk-light git worktrees
// with an APFS clone or an artifact-fs mount.
package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/cloudflare/artifact-fs/internal/cli"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
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
	return run(ctx, runtime.GOOS, doctor.System(), args[1:], stdout, stderr)
}

// run runs slimtree's own commands as they run on goos, reading the machine
// through probe. The cli package sets the urfave version printer, which
// prints "slimtree <version.String()>".
func run(ctx context.Context, goos string, probe doctor.Probe, args []string, stdout io.Writer, stderr io.Writer) int {
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
		// urfave runs Before only for a command's action, never for --help
		// or --version. doctor prints the full report, and help is help.
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			if sub := c.Command(c.Args().First()); sub == nil || sub.Name != "doctor" && sub.Name != "help" {
				firstRun(stderr, probe, goos, c.String("backend"))
			}
			return ctx, nil
		},
		Commands: []*ucli.Command{
			{
				Name:  "doctor",
				Usage: "check what the backend needs in the current directory; installs nothing",
				Flags: []ucli.Flag{&ucli.BoolFlag{Name: "json", Usage: "print the report as JSON"}},
				Action: func(_ context.Context, c *ucli.Command) error {
					dir, err := os.Getwd()
					if err != nil {
						return err
					}
					return printDoctor(stdout, doctor.Run(probe, goos, c.String("backend"), dir), c.Bool("json"))
				},
			},
		},
	}
	if err := app.Run(ctx, append([]string{"slimtree"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

var errDoctorFailed = errors.New("doctor: a check failed")

func printDoctor(w io.Writer, r doctor.Report, asJSON bool) error {
	if asJSON {
		if err := r.WriteJSON(w); err != nil {
			return err
		}
	} else {
		writeChecks(w, r)
	}
	if !r.OK {
		return errDoctorFailed
	}
	return nil
}

// writeChecks writes r's checks, their fixes and its reminders as text.
func writeChecks(w io.Writer, r doctor.Report) {
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-4s  %-13s  %s\n", c.Status, c.ID, c.Detail)
		if c.Status != doctor.StatusOK && c.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", c.Fix)
		}
	}
	for _, m := range r.Reminders {
		fmt.Fprintf(w, "reminder: %s\n", m)
	}
}
