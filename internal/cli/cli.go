package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	ucli "github.com/urfave/cli/v3"
)

func Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	root := defaultRoot()
	if v := os.Getenv("ARTIFACT_FS_ROOT"); v != "" {
		root = v
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
				&ucli.StringFlag{Name: "root", Value: filepath.Join(root, "mnt"), Usage: "mount root directory"},
				&ucli.IntFlag{Name: "hydration-concurrency", Value: daemon.DefaultHydrationConcurrency, Usage: "number of concurrent blob hydration workers"},
			},
			Action: func(_ context.Context, c *ucli.Command) error {
				logger := logging.NewJSONLogger(stderr, slog.LevelInfo)
				svc, err := daemon.New(ctx, root, logger)
				if err != nil {
					return err
				}
				defer svc.Close()
				svc.SetMountRoot(c.String("root"))
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
			Usage: "add and mount a repository",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.StringFlag{Name: "remote", Usage: "remote URL (required)"},
				&ucli.StringFlag{Name: "branch", Usage: "deprecated alias for --ref refs/heads/<branch>"},
				&ucli.StringFlag{Name: "ref", Value: "refs/heads/main", Usage: "canonical source ref"},
				&ucli.StringFlag{Name: "require-commit", Usage: "require the source ref to resolve to this exact commit"},
				&ucli.IntFlag{Name: "depth", Usage: "history depth to transfer; 0 transfers complete history"},
				&ucli.StringFlag{Name: "refresh", Value: "30s", Usage: "remote refresh interval or never"},
				&ucli.StringFlag{Name: "mount-root", Usage: "override mount root"},
				&ucli.BoolFlag{Name: "async", Usage: "return after registration and prepare the repo in the daemon"},
				&ucli.BoolFlag{Name: "adopt-gitdir", Usage: "adopt an initialized linked worktree without fetching or resetting its index"},
				&ucli.BoolFlag{Name: "prepared-gitdir", Usage: "use an existing git dir for async preparation"},
				&ucli.StringFlag{Name: "git-dir", Usage: "explicit git dir path"},
				&ucli.StringFlag{Name: "blob-cache-dir", Usage: "directory for hydrated blobs; may be shared between repositories"},
				&ucli.StringFlag{Name: "fetch-ref", Usage: "ref to fetch during async preparation"},
			},
			Action: withService(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Service) error {
				name := strings.TrimSpace(c.String("name"))
				remote := strings.TrimSpace(c.String("remote"))
				branch := strings.TrimSpace(c.String("branch"))
				ref := strings.TrimSpace(c.String("ref"))
				if branch != "" {
					if c.IsSet("ref") {
						return fmt.Errorf("--ref and --branch cannot be used together")
					}
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
				cfg := model.RepoConfig{
					Name:                  name,
					ID:                    model.RepoID(name),
					RemoteURL:             remote,
					Branch:                ref,
					RefreshInterval:       d,
					RequiredCommit:        requiredCommit,
					HistoryDepth:          c.Int("depth"),
					RemoteRefreshDisabled: strings.EqualFold(refreshValue, "never"),
					MountRoot:             c.String("mount-root"),
					GitDir:                c.String("git-dir"),
					BlobCacheDir:          c.String("blob-cache-dir"),
					PreparedGitDir:        preparedGitDir,
					AdoptGitDir:           adoptGitDir,
					FetchRef:              c.String("fetch-ref"),
					Enabled:               true,
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
		nameCommand("remove-repo", "remove a repository", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.RemoveRepo(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "removed %s\n", name)
			return nil
		}),
		nameCommand("status", "show repo status", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			st, err := svc.Status(c, name)
			if err != nil {
				return err
			}
			fmt.Fprintln(w, formatStatusLine(st))
			return nil
		}),
		nameCommand("fetch", "fetch remote updates", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.FetchNow(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "fetched %s\n", name)
			return nil
		}),
		nameCommand("prepare", "retry async repository preparation", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.Prepare(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "preparing %s\n", name)
			return nil
		}),
		{
			Name:  "list-repos",
			Usage: "list configured repos",
			Action: withService(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Service) error {
				repos, err := svc.ListRepos(ctx)
				if err != nil {
					return err
				}
				for _, r := range repos {
					displayRef := r.Branch
					if branch, ok := strings.CutPrefix(displayRef, "refs/heads/"); ok {
						displayRef = branch
					}
					fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.Name, displayRef, r.MountPath, r.RemoteURLRedacted)
				}
				return nil
			}),
		},
		nameCommand("remount", "ask the running daemon to mount a repository again", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.Remount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "remounted %s\n", name)
			return nil
		}),
		nameCommand("unmount", "ask the running daemon to unmount a repository until remount", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.Unmount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "unmounted %s\n", name)
			return nil
		}),
		{
			Name:   "fsmonitor-hook",
			Usage:  "internal fsmonitor hook",
			Hidden: true,
			Flags:  []ucli.Flag{&ucli.StringFlag{Name: "name", Usage: "repo name (required)"}},
			Action: withService(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Service) error {
				name := strings.TrimSpace(c.String("name"))
				if name == "" {
					return fmt.Errorf("--name required")
				}
				// Git passes the hook protocol version, then its last token.
				return svc.FSMonitorHook(ctx, name, c.Args().Get(1), stdout)
			}),
		},
		{
			Name:  "set-refresh",
			Usage: "update refresh interval",
			Flags: []ucli.Flag{
				&ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				&ucli.StringFlag{Name: "interval", Usage: "refresh interval (required)"},
			},
			Action: withService(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Service) error {
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
				if err := svc.SetRefresh(ctx, name, d, disabled); err != nil {
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
		stubCommand("evict-cache", "evict blob cache", stdout),
		stubCommand("gc", "garbage collect", stdout),
		stubCommand("doctor", "check repo health", stdout),
		stubCommand("prefetch", "prefetch blobs", stdout),
	}

	if err := app.Run(ctx, append([]string{"artifact-fs"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func defaultRoot() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "artifact-fs")
		}
	}
	return "/var/lib/artifact-fs"
}

// withService creates a daemon.Service for the duration of the action.
func withService(ctx context.Context, root string, stderr io.Writer, fn func(*ucli.Command, *daemon.Service) error) ucli.ActionFunc {
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
func nameCommand(name, usage string, ctx context.Context, root string, stderr io.Writer, stdout io.Writer, fn func(context.Context, *daemon.Service, string, io.Writer) error) *ucli.Command {
	return &ucli.Command{
		Name:  name,
		Usage: usage,
		Flags: []ucli.Flag{&ucli.StringFlag{Name: "name", Usage: "repo name (required)"}},
		Action: withService(ctx, root, stderr, func(c *ucli.Command, svc *daemon.Service) error {
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
	return fmt.Sprintf("repo=%s state=%s head=%s ref=%s source_ref=%s required_commit=%s acquisition=%s base_commit=%s served_commit=%s served_generation=%d published_commit=%s published_generation=%d remote_refresh=%s ahead=%d behind=%d diverged=%t last_fetch=%s result=%s prepare_error=%s mount_error=%s last_error=%s last_error_phase=%s last_error_at=%s next_retry=%s attempts=%d hydrated_blobs=%d hydrated_bytes=%d overlay_dirty=%t",
		st.RepoID, st.State, st.CurrentHEADOID, st.CurrentHEADRef, st.SourceRef, requiredCommit, acquisition, st.CurrentHEADOID,
		formatNone(st.ServedHEADOID), st.ServedGeneration, formatNone(st.PublishedHEADOID), st.PublishedGeneration, remoteRefresh,
		st.AheadCount, st.BehindCount, st.Diverged,
		formatLastFetchAt(st.LastFetchAt), formatLastFetchResult(st.LastFetchResult),
		formatStatusError(st.PrepareError), formatStatusError(st.MountError),
		formatStatusError(st.LastError), formatNone(st.LastErrorPhase), formatStatusTime(st.LastErrorAt), formatStatusTime(st.NextRetryAt), st.Attempts,
		st.HydratedBlobCount, st.HydratedBlobBytes, st.DirtyOverlay)
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
	return at.Format(time.RFC3339)
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
