package daemon

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func (s *Service) bootstrapCleanStatusIndex(ctx context.Context, cfg model.RepoConfig, headOID string, gen int64, snap *snapshot.Store, ov *overlay.Store) error {
	storedOID, _, storedGen, err := snap.ReadState(ctx)
	if err != nil {
		return err
	}
	if storedOID != headOID || storedGen != gen {
		return nil
	}
	entries, err := ov.ListAll(ctx)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	if err := s.git.ConfigureFSMonitorHook(ctx, cfg, s.root); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("git status clean baseline setup skipped", "repo", cfg.Name, "error", err)
		return nil
	}
	bootstrapped, err := s.git.BootstrapCleanIndexForFSMonitor(ctx, cfg, headOID, func() (bool, error) {
		currentOID, _, currentGen, err := snap.ReadState(ctx)
		if err != nil {
			return false, err
		}
		if currentOID != headOID || currentGen != gen {
			return false, nil
		}
		currentEntries, err := ov.ListAll(ctx)
		if err != nil {
			return false, err
		}
		return len(currentEntries) == 0, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("git status clean baseline bootstrap skipped", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
		return nil
	}
	if bootstrapped {
		s.logger.Debug("git status clean baseline bootstrapped", "repo", cfg.Name)
	}
	return nil
}

func (s *Service) configureStatusOptimization(ctx context.Context, cfg model.RepoConfig) {
	if err := s.git.ConfigureStatusOptimization(ctx, cfg, s.root); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("git status optimization setup failed", "repo", cfg.Name, "error", err)
	}
}

// configureCurrentStatusOptimization writes the status optimization config
// of rt's mount under the repo's prepare lock, while cfg's config version is
// the registry's. It runs on the sync loop, so it never waits for the lock,
// which a preparation can hold for up to its timeout. While another step
// holds the lock, it marks the write pending for retryMount.
func (s *Service) configureCurrentStatusOptimization(rt *repoRuntime, cfg model.RepoConfig) {
	ran, err := s.tryRepoPrepareLock(cfg.Name, func() error {
		latest, err := s.registry.GetRepo(rt.ctx, cfg.Name)
		if err != nil || latest.ConfigVersion != cfg.ConfigVersion {
			return err
		}
		s.configureStatusOptimization(rt.ctx, cfg)
		return nil
	})
	busy := !ran && err == nil
	s.mu.Lock()
	wasPending := rt.statusPending
	rt.statusPending = busy
	s.mu.Unlock()
	switch {
	case busy && !wasPending:
		s.logger.Info("git status optimization setup deferred while another step holds the repo's prepare lock", "repo", cfg.Name)
	case err == nil || rt.ctx.Err() != nil:
	case ran:
		// A remove-repo that held the lock leaves nothing to write.
		s.logger.Info("git status optimization setup skipped", "repo", cfg.Name, "error", err)
	default:
		s.logger.Warn("git status optimization setup failed", "repo", cfg.Name, "error", err)
	}
}

// FSMonitorHook answers Git's fsmonitor hook protocol version 2 from the
// overlay, which records changes made through the mount. Its token carries the
// overlay database's id and change sequence, so Git receives only the paths
// changed since its last query. It trusts only tokens issued for this
// registration and this database while the worktree is mounted, and only for
// a git that works in the registration's git dir. Otherwise native edits may
// have bypassed the overlay, or the caller is another worktree, and "/" makes
// Git rescan the worktree.
//
// Git runs the hook on every command that reads the index, so it opens no
// Controller: it reads the registry with registry.ReadRepo and builds no git
// store. It checks the state root as every command does.
func FSMonitorHook(ctx context.Context, root, name, since string, w io.Writer) error {
	root, err := ensureStateRoot(root)
	if err != nil {
		return err
	}
	cfg, err := registry.ReadRepo(ctx, registryPath(root), name)
	if err != nil {
		return err
	}
	return answerFSMonitorHook(ctx, cfg, since, w)
}

// FSMonitorHookByID answers as FSMonitorHook does for the repo registered as
// id, which Git runs through the hook link gitstore.FSMonitorHookLink returns.
func FSMonitorHookByID(ctx context.Context, root string, id model.RepoID, since string, w io.Writer) error {
	root, err := ensureStateRoot(root)
	if err != nil {
		return err
	}
	cfg, err := registry.ReadRepoByID(ctx, registryPath(root), id)
	if err != nil {
		return err
	}
	return answerFSMonitorHook(ctx, cfg, since, w)
}

// answerFSMonitorHook writes FSMonitorHook's answer for cfg.
func answerFSMonitorHook(ctx context.Context, cfg model.RepoConfig, since string, w io.Writer) error {
	prefix := fmt.Sprintf("artifact-fs:%s:%s:", cfg.ID, cfg.ConfigVersion)
	rest, trusted := strings.CutPrefix(since, prefix)
	sinceInstance, seqText, ok := strings.Cut(rest, ":")
	sinceSeq, err := strconv.ParseInt(seqText, 10, 64)
	// The mount table check sends no request to the mount, which stays gated
	// while the daemon runs Git during preparation.
	mounted := isMounted(cfg.MountPath)
	own := callerUsesGitDir(cfg.GitDir)
	trusted = trusted && ok && err == nil && sinceSeq >= 0 && mounted && own
	if !trusted {
		sinceSeq = math.MaxInt64
	}
	changes, logged, err := overlay.ReadChangesSince(ctx, cfg.RepoPaths, sinceSeq)
	if err != nil {
		return err
	}
	paths := []string{"/"}
	if logged && trusted && sinceInstance == changes.Instance && sinceSeq <= changes.Seq {
		paths = changes.Paths
	}
	// Git run while unmounted caches the empty mountpoint's listing, which the
	// mount's later answers would not correct. The bootstrap marks entries from
	// the snapshot instead, so its token may carry into the mount.
	token := prefix + "rescan"
	if own && logged && (mounted || os.Getenv(gitstore.FSMonitorBootstrapEnv) != "") {
		token = fmt.Sprintf("%s%s:%d", prefix, changes.Instance, changes.Seq)
	}
	if err := writeHookField(w, token); err != nil {
		return err
	}
	for _, p := range paths {
		if err := writeHookField(w, p); err != nil {
			return err
		}
	}
	return nil
}

// callerUsesGitDir reports whether the git that runs the hook works in
// gitDir. Every worktree reads the repo's config, and git worktree add copies
// the config.worktree of the worktree it runs in, so another worktree's git
// can run the hook. Git exports GIT_DIR for a linked worktree, for --git-dir
// and for a gitfile, such as the mount's .git, and leaves it unset in a main
// worktree it found by its .git directory. A main worktree runs the hook only
// as the registered worktree, so an unset GIT_DIR counts as gitDir. The check
// stats only git dirs, never the mount.
func callerUsesGitDir(gitDir string) bool {
	caller := os.Getenv("GIT_DIR")
	if caller == "" {
		return true
	}
	a, err := os.Stat(caller)
	if err != nil {
		return false
	}
	b, err := os.Stat(gitDir)
	return err == nil && os.SameFile(a, b)
}

func writeHookField(w io.Writer, value string) error {
	n, err := io.WriteString(w, value)
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	n, err = w.Write([]byte{0})
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}
