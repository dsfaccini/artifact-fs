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
	"time"

	"github.com/cloudflare/artifact-fs/internal/cli"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
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
			{
				Name:  "list",
				Usage: "list the worktrees slimtree made; reads its records, git's worktree list, and the process and mount tables, never a mount",
				Flags: []ucli.Flag{
					&ucli.BoolFlag{Name: "repo", Usage: "only the worktrees of the repository that holds the current directory"},
					&ucli.BoolFlag{Name: "json", Usage: "print the list as JSON"},
				},
				// list needs no doctor checks: it changes nothing and runs
				// only git commands that read.
				Action: func(ctx context.Context, c *ucli.Command) error {
					home, err := openHome()
					if err != nil {
						return err
					}
					repo := ""
					if c.Bool("repo") {
						if repo, err = commonDir(ctx, "--repo"); err != nil {
							return err
						}
					}
					l, err := listWorktrees(ctx, home, repo, func(err error) { fmt.Fprintln(stderr, "slimtree:", err) })
					if err != nil {
						return err
					}
					return printListing(stdout, l, c.Bool("json"))
				},
			},
			{
				Name:      "new",
				Usage:     "make a worktree on a branch: the branch that exists, or a new one at --from, else at HEAD",
				ArgsUsage: "<branch>",
				Flags: []ucli.Flag{
					&ucli.StringFlag{Name: "from", Usage: "make the new branch at this commit-ish, resolved in the current directory"},
					&ucli.BoolFlag{Name: "track", Usage: "make the new branch track --from, which must name a branch"},
					&ucli.StringFlag{Name: "path", Usage: "the worktree's path (default: <base>-<the branch's last segment>, next to the base worktree)"},
				},
				Action: func(ctx context.Context, c *ucli.Command) error {
					if c.Args().Len() != 1 {
						return errors.New("slimtree new takes one branch")
					}
					b := backend(c.String("backend"))
					if err := doctor.Require(probe, goos, string(b), "new", doctor.Needs{Mount: b == artifactFS}); err != nil {
						return err
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					return newWorktree(ctx, home, newOptions{branch: c.Args().First(), from: c.String("from"), track: c.Bool("track"), path: c.String("path"), backend: b, goos: goos}, stdout, stderr)
				},
			},
			{
				Name:      "stop",
				Usage:     "stop the daemon of an artifactfs worktree and unmount it; its changes stay for resume",
				ArgsUsage: "<worktree>",
				Action: func(ctx context.Context, c *ucli.Command) error {
					if c.Args().Len() != 1 {
						return errors.New("slimtree stop takes one worktree, by name or path")
					}
					if err := doctor.Require(probe, goos, string(artifactFS), "stop", doctor.Needs{Mount: true}); err != nil {
						return err
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					return stopWorktree(ctx, home, c.Args().First(), stdout)
				},
			},
			{
				Name:      "resume",
				Usage:     "mount an artifactfs worktree again, as after stop or a reboot",
				ArgsUsage: "<worktree>",
				Flags:     []ucli.Flag{&ucli.BoolFlag{Name: "all", Usage: "resume every artifactfs worktree slimtree made"}},
				Action: func(ctx context.Context, c *ucli.Command) error {
					all := c.Bool("all")
					if all && c.Args().Len() != 0 || !all && c.Args().Len() != 1 {
						return errors.New("slimtree resume takes one worktree, by name or path, or --all")
					}
					if err := doctor.Require(probe, goos, string(artifactFS), "resume", doctor.Needs{Mount: true}); err != nil {
						return err
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					if !all {
						return resumeWorktree(ctx, home, c.Args().First(), goos, stdout, stderr)
					}
					return resumeAll(ctx, home, goos, stdout, stderr)
				},
			},
			{
				Name:      "rm",
				Usage:     "remove a worktree slimtree made, with its ignored files, as git worktree remove does; a recovery ref keeps its HEAD, an archive keeps an artifactfs worktree's files, and its branch stays",
				ArgsUsage: "<worktree>",
				Flags: []ucli.Flag{
					&ucli.BoolFlag{Name: "force", Usage: "discard the changes git status lists, or drop a worktree whose directory is gone or, for artifactfs, that no daemon serves"},
					&ucli.BoolFlag{Name: "check", Usage: "only check whether rm would refuse; changes nothing"},
					&ucli.StringSliceFlag{Name: "exclude", Usage: "leave out of an artifactfs worktree's archive the files at paths with a component that this glob matches; repeatable", Validator: checkExcludes},
				},
				Action: func(ctx context.Context, c *ucli.Command) error {
					if c.Args().Len() != 1 {
						return errors.New("slimtree rm takes one worktree, by name or path")
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					return removeWorktree(ctx, home, probe, goos, c.Args().First(), rmOptions{force: c.Bool("force"), check: c.Bool("check"), excludes: c.StringSlice("exclude")}, stdout, stderr)
				},
			},
			{
				Name:  "prune",
				Usage: "delete the archives rm left that are older than the retention rule, with their recovery refs, evict the cached blobs not read within the rule, and remove the daemon binary copies no worktree uses; skipped while another slimtree command runs",
				Action: func(ctx context.Context, c *ucli.Command) error {
					if c.Args().Len() != 0 {
						return errors.New("slimtree prune takes no arguments")
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					lock, err := home.TryLock("")
					if errors.Is(err, state.ErrLockBusy) {
						fmt.Fprintln(stderr, "slimtree: pruned nothing:", err)
						return nil
					}
					if err != nil {
						return err
					}
					defer lock.Unlock()
					p := prune(ctx, home, time.Now())
					p.write(stderr)
					if p.ruleErr != nil || len(p.problems) > 0 {
						return errPruneFailed
					}
					return nil
				},
			},
			{
				Name:      "retention",
				Usage:     "show the retention rule, or set it: " + state.RetentionUsage + " (default month)",
				ArgsUsage: "[" + state.RetentionUsage + "]",
				Action: func(_ context.Context, c *ucli.Command) error {
					if c.Args().Len() > 1 {
						return errors.New("slimtree retention takes at most one rule")
					}
					home, err := openHome()
					if err != nil {
						return err
					}
					if c.Args().Len() == 1 {
						rule, err := state.ParseRetention(c.Args().First())
						if err != nil {
							return err
						}
						if err := home.SetRetention(rule); err != nil {
							return err
						}
					}
					rule, err := home.Retention()
					if err != nil {
						return err
					}
					fmt.Fprintln(stdout, rule)
					return nil
				},
			},
		},
	}
	// A usage error goes to stderr once, as run prints it, and never puts
	// the help on stdout, where --json output goes.
	onUsageError := func(_ context.Context, _ *ucli.Command, err error, _ bool) error { return err }
	app.OnUsageError = onUsageError
	for _, c := range app.Commands {
		c.OnUsageError = onUsageError
	}
	if err := app.Run(ctx, append([]string{"slimtree"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

var errDoctorFailed = errors.New("doctor: a check failed")

var errPruneFailed = errors.New("prune: see the problems above")

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
