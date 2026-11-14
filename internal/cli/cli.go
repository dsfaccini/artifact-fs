package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	ucli "github.com/urfave/cli/v3"
)

// Main runs artifact-fs with the arguments the process received, its program
// name first. Git runs the FSMonitor hook link with the hook protocol version
// and its last token, and the link's path names the state root and the repo.
func Main(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	root, id, ok := gitstore.ParseFSMonitorHookLink(args[0])
	if !ok {
		return Run(ctx, args[1:], stdout, stderr)
	}
	var since string
	if len(args) > 2 {
		since = args[2]
	}
	if err := daemon.FSMonitorHookByID(ctx, root, id, since, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	root := defaultRoot()
	if v := os.Getenv("ARTIFACT_FS_ROOT"); v != "" {
		root = v
	}
	// Git runs the hook on every command that reads the index, so the hook
	// skips building the CLI.
	if len(args) > 0 && args[0] == "fsmonitor-hook" {
		if err := runFSMonitorHook(ctx, root, args[1:], stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}

	app := &ucli.Command{
		Name:      "artifact-fs",
		Usage:     "Git-backed FUSE filesystem with persistent writable overlay",
		Writer:    stdout,
		ErrWriter: stderr,
	}

	app.Commands = []*ucli.Command{
		{
			Name:  "daemon",
			Usage: "start the artifact-fs daemon",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "root", Value: filepath.Join(root, "mnt"), Usage: "accepted for compatibility; changes no registered repo, which mounts where add-repo --mount-root put it"},
				&ucli.IntFlag{Name: "hydration-concurrency", Value: daemon.DefaultHydrationConcurrency, Usage: "number of concurrent blob hydration workers"},
			},
			Action: func(_ context.Context, c *ucli.Command) error {
				logger := logging.NewJSONLogger(stderr, slog.LevelInfo)
				svc, err := daemon.New(ctx, root, logger)
				if err != nil {
					return err
				}
				defer svc.Close()
				svc.SetHydrationConcurrency(c.Int("hydration-concurrency"))
				err = svc.Start(ctx)
				if err == context.Canceled {
					return nil
				}
				return err
			},
		},
		{
			Name:  "add-repo",
			Usage: "register a repository and prepare it for the daemon to mount",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.StringFlag{Name: "remote", Usage: "remote URL; required unless --adopt-gitdir or --prepared-gitdir is set"},
				&ucli.StringFlag{Name: "branch", Usage: "deprecated alias for --ref refs/heads/<branch>"},
				&ucli.StringFlag{Name: "ref", Value: "refs/heads/main", Usage: "source ref: a full ref name, a branch name, or origin/<branch>"},
				&ucli.StringFlag{Name: "require-commit", Usage: "require the source ref to resolve to this exact commit"},
				&ucli.IntFlag{Name: "depth", Usage: "history depth to transfer; 0 transfers complete history"},
				&ucli.StringFlag{Name: "refresh", Value: "30s", Usage: "remote refresh interval or never"},
				&ucli.StringFlag{Name: "mount-root", Usage: "override mount root"},
				&ucli.BoolFlag{Name: "async", Usage: "return after registration and prepare the repo in the daemon"},
				&ucli.BoolFlag{Name: "adopt-gitdir", Usage: "adopt an initialized linked worktree without fetching or resetting its index"},
				&ucli.BoolFlag{Name: "prepared-gitdir", Usage: "use an existing git dir for async preparation"},
				&ucli.StringFlag{Name: "git-dir", Usage: "explicit git dir path"},
				&ucli.StringFlag{Name: "blob-cache-dir", Usage: "directory for hydrated blobs; may be shared between repositories"},
				&ucli.StringFlag{Name: "fetch-ref", Usage: "ref to fetch and serve; defaults to --ref; cannot be combined with --require-commit"},
				&ucli.IntFlag{Name: "warn-kept-generations", Value: daemon.DefaultWarnKeptGenerations, Usage: "warn when open files keep this many older snapshot generations"},
			},
			Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
				name := strings.TrimSpace(c.String("name"))
				remote := strings.TrimSpace(c.String("remote"))
				branch := strings.TrimSpace(c.String("branch"))
				ref := strings.TrimSpace(c.String("ref"))
				if branch != "" {
					if c.IsSet("ref") {
						return fmt.Errorf("--ref and --branch cannot be used together")
					}
					// The alias drops one leading refs/heads/ and stores the rest
					// under refs/heads/, so --branch origin/X reaches a branch named
					// origin/X, which model.CanonicalRef would read as X.
					ref = "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")
				}
				requiredCommit := strings.TrimSpace(c.String("require-commit"))
				async := c.Bool("async")
				preparedGitDir := c.Bool("prepared-gitdir")
				adoptGitDir := c.Bool("adopt-gitdir")
				if adoptGitDir {
					for _, flag := range []string{"remote", "ref", "branch", "fetch-ref", "require-commit", "depth", "async", "prepared-gitdir"} {
						if c.IsSet(flag) {
							return fmt.Errorf("--adopt-gitdir cannot be combined with --%s", flag)
						}
					}
					if c.IsSet("refresh") && !strings.EqualFold(strings.TrimSpace(c.String("refresh")), "never") {
						return fmt.Errorf("--adopt-gitdir requires remote refresh to remain disabled")
					}
					ref = "HEAD"
				}
				if requiredCommit != "" && !c.IsSet("ref") && !c.IsSet("branch") {
					return fmt.Errorf("--require-commit requires an explicit --ref")
				}
				if preparedGitDir && !async {
					return fmt.Errorf("--prepared-gitdir requires --async")
				}
				if preparedGitDir && strings.TrimSpace(c.String("git-dir")) == "" {
					return fmt.Errorf("--git-dir is required with --prepared-gitdir")
				}
				if name == "" {
					return fmt.Errorf("--name is required")
				}
				if remote == "" && !preparedGitDir && !adoptGitDir {
					return fmt.Errorf("--remote is required")
				}
				refreshValue := strings.TrimSpace(c.String("refresh"))
				if adoptGitDir {
					refreshValue = "never"
				}
				d, err := daemon.ParseRefresh(refreshValue)
				if err != nil {
					return err
				}
				if c.Int("warn-kept-generations") < 1 {
					return fmt.Errorf("--warn-kept-generations must be positive")
				}
				cfg := model.RepoConfig{
					RepoPaths: model.RepoPaths{
						Name:         name,
						ID:           model.RepoID(name),
						MountRoot:    c.String("mount-root"),
						GitDir:       c.String("git-dir"),
						BlobCacheDir: c.String("blob-cache-dir"),
					},
					RepoSpec: model.RepoSpec{
						RemoteURL:      remote,
						Branch:         ref,
						RequiredCommit: requiredCommit,
						HistoryDepth:   c.Int("depth"),
						PreparedGitDir: preparedGitDir,
						AdoptGitDir:    adoptGitDir,
						FetchRef:       c.String("fetch-ref"),
					},
					RefreshPolicy: model.RefreshPolicy{
						RefreshInterval:       d,
						RemoteRefreshDisabled: strings.EqualFold(refreshValue, "never"),
					},
					Enabled:             true,
					WarnKeptGenerations: c.Int("warn-kept-generations"),
				}
				if err := svc.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{Async: async}); err != nil {
					return err
				}
				if async {
					fmt.Fprintf(stdout, "queued %s\n", cfg.Name)
					return nil
				}
				fmt.Fprintf(stdout, "added %s\n", cfg.Name)
				return nil
			}),
		},
		nameCommand("remove-repo", "remove a repository", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			if err := svc.RemoveRepo(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "removed %s\n", name)
			return nil
		}),
		nameCommand("status", "show repo status", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			st, err := svc.Status(c, name)
			if err != nil {
				return err
			}
			fmt.Fprintln(w, formatStatusLine(st))
			return nil
		}),
		nameCommand("fetch", "fetch remote updates", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			if err := svc.FetchNow(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "fetched %s\n", name)
			return nil
		}),
		nameCommand("prepare", "prepare a repository again without unmounting it", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			prepared, err := svc.Prepare(c, name)
			if err != nil {
				return err
			}
			if !prepared {
				fmt.Fprintf(w, "%s is ready; nothing to prepare\n", name)
				return nil
			}
			fmt.Fprintf(w, "preparing %s\n", name)
			return nil
		}),
		{
			Name:  "list-repos",
			Usage: "list configured repos",
			Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
				repos, err := svc.ListRepos(ctx)
				if err != nil {
					return err
				}
				for _, r := range repos {
					// Only refs/heads/ is shortened. model.BranchName would also
					// shorten an older verified source's refs/remotes/origin/X.
					displayRef := r.Branch
					if branch, ok := strings.CutPrefix(displayRef, "refs/heads/"); ok {
						displayRef = branch
					}
					fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.Name, displayRef, r.MountPath, r.RemoteURLRedacted)
				}
				return nil
			}),
		},
		nameCommand("remount", "ask the running daemon to mount a repository again", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			if err := svc.Remount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "remounted %s\n", name)
			return nil
		}),
		nameCommand("unmount", "ask the running daemon to unmount a repository until remount", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Controller, name string, w io.Writer) error {
			if err := svc.Unmount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "unmounted %s\n", name)
			return nil
		}),
		{
			Name:  "set-refresh",
			Usage: "update refresh interval",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.StringFlag{Name: "interval", Usage: "refresh interval (required)"},
			},
			Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
				name := strings.TrimSpace(c.String("name"))
				interval := strings.TrimSpace(c.String("interval"))
				if name == "" || interval == "" {
					return fmt.Errorf("--name and --interval required")
				}
				d, err := daemon.ParseRefresh(interval)
				if err != nil {
					return err
				}
				disabled := strings.EqualFold(interval, "never")
				if err := svc.SetRefresh(ctx, name, model.RefreshPolicy{RefreshInterval: d, RemoteRefreshDisabled: disabled}); err != nil {
					return err
				}
				if disabled {
					fmt.Fprintf(stdout, "updated refresh %s never\n", name)
				} else {
					fmt.Fprintf(stdout, "updated refresh %s %s\n", name, d)
				}
				return nil
			}),
		},
		{
			Name:  "set-warn-kept-generations",
			Usage: "update the number of older snapshot generations open files keep before the daemon warns",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.IntFlag{Name: "count", Required: true, Usage: "warning threshold (required)"},
			},
			Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
				name := strings.TrimSpace(c.String("name"))
				if name == "" {
					return fmt.Errorf("--name required")
				}
				if err := svc.SetWarnKeptGenerations(ctx, name, c.Int("count")); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "updated warn-kept-generations %s %d\n", name, c.Int("count"))
				return nil
			}),
		},
		{
			Name:  "evict-cache",
			Usage: "evict least recently accessed blobs until the blob cache fits a byte budget",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.Int64Flag{Name: "max-bytes", Required: true, Usage: "byte budget the blob cache must fit (required)"},
			},
			Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
				name := strings.TrimSpace(c.String("name"))
				if name == "" {
					return fmt.Errorf("--name required")
				}
				res, err := svc.EvictCache(ctx, name, c.Int64("max-bytes"))
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "evicted_blobs=%d evicted_bytes=%d hydrated_blobs=%d hydrated_bytes=%d\n",
					res.Evicted, res.EvictedBytes, res.Remaining.Blobs, res.Remaining.Bytes)
				return nil
			}),
		},
		stubCommand("gc", "garbage collect", stdout),
		stubCommand("doctor", "check repo health", stdout),
		stubCommand("prefetch", "prefetch blobs", stdout),
	}

	app.OnUsageError = showUsage
	for _, cmd := range app.Commands {
		cmd.OnUsageError = showUsage
	}

	if err := app.Run(ctx, append([]string{"artifact-fs"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// showUsage prints a usage error and the command's help on stdout, and
// returns the error, which Run prints once on stderr.
func showUsage(ctx context.Context, cmd *ucli.Command, err error, isSubcommand bool) error {
	fmt.Fprintf(cmd.Root().Writer, "Incorrect Usage: %s\n\n", err)
	if isSubcommand {
		_ = ucli.ShowCommandHelp(ctx, cmd.Root(), cmd.Name)
	} else {
		_ = ucli.ShowRootCommandHelp(cmd)
	}
	return err
}

func defaultRoot() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "artifact-fs")
		}
	}
	return "/var/lib/artifact-fs"
}

// runFSMonitorHook runs the internal fsmonitor-hook command that the hook
// script execs: --name, then the arguments Git passes, the hook protocol
// version and Git's last token.
func runFSMonitorHook(ctx context.Context, root string, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fsmonitor-hook", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "repo name (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name required")
	}
	return daemon.FSMonitorHook(ctx, root, strings.TrimSpace(*name), flags.Arg(1), stdout)
}

// withController creates a daemon.Controller for the duration of the action.
func withController(ctx context.Context, root string, stderr io.Writer, fn func(*ucli.Command, *daemon.Controller) error) ucli.ActionFunc {
	return func(_ context.Context, c *ucli.Command) error {
		logger := logging.NewJSONLogger(stderr, slog.LevelInfo)
		svc, err := daemon.NewController(ctx, root, logger)
		if err != nil {
			return err
		}
		defer svc.Close()
		return fn(c, svc)
	}
}

// nameCommand builds a subcommand that takes a --name flag and runs fn.
func nameCommand(name, usage string, ctx context.Context, root string, stderr io.Writer, stdout io.Writer, fn func(context.Context, *daemon.Controller, string, io.Writer) error) *ucli.Command {
	return &ucli.Command{
		Name:  name,
		Usage: usage,
		Flags: []ucli.Flag{&ucli.StringFlag{Name: "name", Usage: "repo name (required)"}},
		Action: withController(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Controller) error {
			n := strings.TrimSpace(c.String("name"))
			if n == "" {
				return fmt.Errorf("--name required")
			}
			return fn(ctx, svc, n, stdout)
		}),
	}
}

func formatStatusLine(st model.RepoRuntimeState) string {
	requiredCommit := st.RequiredCommit
	if requiredCommit == "" {
		requiredCommit = "none"
	}
	acquisition := st.Acquisition
	if acquisition == "" {
		acquisition = "not_required"
	}
	remoteRefresh := "enabled"
	if st.RemoteRefreshDisabled {
		remoteRefresh = "disabled"
	}
	return fmt.Sprintf("repo=%s state=%s head=%s ref=%s source_ref=%s required_commit=%s acquisition=%s base_commit=%s served_commit=%s served_generation=%d published_commit=%s published_generation=%d phase=%s remote_refresh=%s ahead=%d behind=%d diverged=%t last_fetch=%s result=%s prepare_error=%s mount_error=%s last_error=%s last_error_phase=%s last_error_at=%s next_retry=%s attempts=%d hydrated_blobs=%d hydrated_bytes=%d overlay_dirty=%t kept_generations=%d warn_kept_generations=%d kept_generations_warning=%s",
		st.RepoID, st.State, st.CurrentHEADOID, st.CurrentHEADRef, st.SourceRef, requiredCommit, acquisition, st.CurrentHEADOID,
		formatNone(st.ServedHEADOID), st.ServedGeneration, formatNone(st.PublishedHEADOID), st.PublishedGeneration, formatNone(st.Phase), remoteRefresh,
		st.AheadCount, st.BehindCount, st.Diverged,
		formatLastFetchAt(st.LastFetchAt), formatLastFetchResult(st.LastFetchResult),
		formatStatusError(st.PrepareError), formatStatusError(st.MountError),
		formatStatusError(st.LastError), formatNone(st.LastErrorPhase), formatStatusTime(st.LastErrorAt), formatStatusTime(st.NextRetryAt), st.Attempts,
		st.HydratedBlobCount, st.HydratedBlobBytes, st.DirtyOverlay,
		st.KeptGenerations, st.WarnKeptGenerations, formatStatusText(st.KeptGenerationsWarning))
}

func formatNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

func formatStatusTime(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.UTC().Format(time.RFC3339)
}

func formatLastFetchAt(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.UTC().Format(time.RFC3339)
}

func formatLastFetchResult(result string) string {
	if strings.TrimSpace(result) == "" {
		return "never"
	}
	return result
}

func formatStatusError(err string) string {
	if strings.TrimSpace(err) == "" {
		return "none"
	}
	return strings.Join(strings.Fields(auth.RedactString(err)), "_")
}

// formatStatusText prints text as one double-quoted, Go-escaped value, so a
// command inside it keeps its spaces and can be pasted.
func formatStatusText(text string) string {
	if strings.TrimSpace(text) == "" {
		return "none"
	}
	return strconv.Quote(auth.RedactString(text))
}

func stubCommand(name, usage string, stdout io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  name,
		Usage: usage,
		Flags: []ucli.Flag{&ucli.StringFlag{Name: "name", Usage: "repo name"}},
		Action: func(_ context.Context, c *ucli.Command) error {
			fmt.Fprintf(stdout, "%s not implemented yet\n", name)
			return nil
		},
	}
}
