package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

type AddRepoOptions struct {
	Async bool
}

func (s *Controller) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
	return s.AddRepoWithOptions(ctx, cfg, AddRepoOptions{})
}

func (s *Controller) AddRepoWithOptions(ctx context.Context, cfg model.RepoConfig, opts AddRepoOptions) error {
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if cfg.AdoptGitDir && opts.Async {
		return errors.New("--adopt-gitdir cannot be combined with --async")
	}
	if err := normalizeSourceConfig(&cfg); err != nil {
		return err
	}
	if cfg.ID == "" {
		cfg.ID = model.RepoID(cfg.Name)
	}
	cfg.RemoteURLRedacted = auth.RedactRemoteURL(cfg.RemoteURL)
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	explicitGitDir := strings.TrimSpace(cfg.GitDir) != ""
	s.fillPaths(&cfg)
	if cfg.AdoptGitDir {
		var err error
		cfg.GitDir, err = filepath.Abs(cfg.GitDir)
		if err != nil {
			return err
		}
		cfg.MountPath, err = filepath.Abs(cfg.MountPath)
		if err != nil {
			return err
		}
	}
	if !cfg.AdoptGitDir && strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = defaultFetchRef(cfg.Branch)
	}
	cfg.ConfigVersion = rand.Text()
	cloneURL := cfg.RemoteURL
	if cfg.PreparedGitDir && !opts.Async {
		return fmt.Errorf("--prepared-gitdir requires --async")
	}
	if cfg.PreparedGitDir && cfg.RequiredCommit != "" {
		return fmt.Errorf("--prepared-gitdir is not supported with --require-commit")
	}
	if opts.Async {
		if strings.TrimSpace(cfg.RemoteURL) == "" && !cfg.PreparedGitDir {
			return fmt.Errorf("--remote is required unless --prepared-gitdir is set")
		}
		if cfg.PreparedGitDir && !explicitGitDir {
			return fmt.Errorf("--git-dir is required with --prepared-gitdir")
		}
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			return fmt.Errorf("async repositories must use ambient credentials; remove credentials from --remote")
		}
		cfg.PrepareState = model.PrepareStatePreparing
		cfg.PrepareError = ""
	} else {
		cfg.PrepareState = model.PrepareStateSyncPreparing
		cfg.PrepareError = ""
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			cfg.RemoteURL = ""
		}
	}
	registeredCfg := cfg
	if err := s.withRepoConfigLock(ctx, cfg.Name, func() error {
		return s.registry.AddRepo(ctx, cfg)
	}); err != nil {
		return err
	}
	if opts.Async {
		return nil
	}
	// Clone and build snapshot so the repo is ready to mount, but don't start
	// the FUSE server -- that's the daemon's job.
	cfg.RemoteURL = cloneURL
	prepareCtx, cancel := s.withPrepareTimeout(ctx)
	err := s.prepareRepo(prepareCtx, cfg)
	prepareContextErr := prepareCtx.Err()
	cancel()
	if err == nil {
		return s.persistSyncPrepareSuccess(ctx, registeredCfg)
	}
	if prepareContextErr != nil && !errors.Is(err, prepareContextErr) {
		err = errors.Join(err, prepareContextErr)
	}
	return s.persistSyncPrepareFailure(ctx, cfg, registeredCfg, err)
}

func (s *Controller) persistSyncPrepareFailure(ctx context.Context, cfg model.RepoConfig, registeredCfg model.RepoConfig, prepareErr error) error {
	stateErr := prepareErr
	if errors.Is(prepareErr, context.DeadlineExceeded) {
		stateErr = errors.New("prepare timed out")
	} else if errors.Is(prepareErr, context.Canceled) {
		stateErr = errors.New("prepare canceled")
	}
	stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer stateCancel()
	if stateWriteErr := s.registry.UpdatePrepareStateForConfig(stateCtx, registeredCfg, "", auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)); stateWriteErr != nil {
		if errors.Is(stateWriteErr, registry.ErrRepoChanged) {
			return prepareErr
		}
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeSync, "phase", preparationFailurePhase(prepareErr), "target_state", "", "error", auth.RedactLogString(stateWriteErr.Error()))
		return errors.Join(prepareErr, fmt.Errorf("persist failed prepare state: %w", stateWriteErr))
	}
	return prepareErr
}

func (s *Controller) persistSyncPrepareSuccess(ctx context.Context, cfg model.RepoConfig) error {
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer cancel()
	if err := s.registry.UpdatePrepareStateForConfig(stateCtx, cfg, "", ""); err != nil {
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeSync, "phase", preparePhaseComplete, "target_state", "", "error", auth.RedactLogString(err.Error()))
		return err
	}
	return nil
}

func (s *Controller) RemoveRepo(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	return s.withRepoConfigLock(ctx, cfg.Name, func() error {
		latest, err := s.registry.GetRepo(ctx, name)
		if err != nil {
			return err
		}
		if latest.ConfigVersion != cfg.ConfigVersion {
			return registry.ErrRepoChanged
		}
		if latest.AdoptGitDir {
			if err := s.git.RemoveAdoptedFSMonitorHook(ctx, latest); err != nil {
				return err
			}
		}
		return s.registry.RemoveRepo(ctx, name)
	})
}

func (s *Controller) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	return s.registry.ListRepos(ctx)
}

func (s *Controller) SetRefresh(ctx context.Context, name string, interval time.Duration, disabled bool) error {
	if interval <= 0 && !disabled {
		return errors.New("refresh interval must be positive")
	}
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if cfg.AdoptGitDir && !disabled {
		return errors.New("remote refresh cannot be enabled for an adopted worktree")
	}
	cfg.RefreshInterval = interval
	cfg.RemoteRefreshDisabled = disabled
	// The daemon applies the new interval at its next registry sync.
	return s.registry.UpdateRefresh(ctx, cfg.ID, interval, disabled)
}

func (s *Service) updateRuntimeRefresh(rt *repoRuntime, interval time.Duration, disabled bool) {
	if rt == nil {
		return
	}
	s.mu.Lock()
	if rt.cfg.RefreshInterval == interval && rt.cfg.RemoteRefreshDisabled == disabled {
		s.mu.Unlock()
		return
	}
	rt.cfg.RefreshInterval = interval
	rt.cfg.RemoteRefreshDisabled = disabled
	refresh := rt.refresh
	s.mu.Unlock()
	if refresh != nil && interval > 0 {
		select {
		case refresh <- interval:
		default:
			select {
			case <-refresh:
			default:
			}
			select {
			case refresh <- interval:
			default:
			}
		}
	}
}

func (s *Controller) FetchNow(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if cfg.PrepareState == model.PrepareStatePreparing || cfg.PrepareState == model.PrepareStateSyncPreparing {
		return fusefs.ErrRepoNotReady
	}
	if cfg.PrepareState == model.PrepareStateFailed {
		return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
	}
	if cfg.RemoteRefreshDisabled {
		return errors.New("remote refresh is disabled for this repository")
	}
	// Status reads the fetch from FETCH_HEAD. Ahead and behind come from the
	// daemon's next refresh.
	return s.git.Fetch(ctx, cfg)
}

func (s *Controller) Prepare(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	async := isAsyncRepo(cfg)
	if async && cfg.PrepareState == model.PrepareStateReady && cfg.RequiredCommit == "" {
		return nil
	}
	nextVersion := rand.Text()
	nextState := model.PrepareStateSyncPreparing
	if async {
		nextState = model.PrepareStatePreparing
	}
	if err := s.registry.BeginPrepare(ctx, cfg, nextState, nextVersion); err != nil {
		return err
	}
	cfg.ConfigVersion = nextVersion
	cfg.PrepareState = nextState
	cfg.PrepareError = ""
	if !async {
		prepareCtx, cancel := s.withPrepareTimeout(ctx)
		err := s.prepareRepo(prepareCtx, cfg)
		prepareContextErr := prepareCtx.Err()
		cancel()
		if err != nil {
			if prepareContextErr != nil && !errors.Is(err, prepareContextErr) {
				err = errors.Join(err, prepareContextErr)
			}
			return s.persistSyncPrepareFailure(ctx, cfg, cfg, err)
		}
		return s.persistSyncPrepareSuccess(ctx, cfg)
	}
	// The new config version makes the daemon prepare the repo again at its
	// next registry sync.
	return nil
}

// mountRequestTimeout bounds how long Unmount and Remount wait for the
// daemon. Unmounting can take a minute: the daemon retries a busy unmount for
// unmountTimeout, then drains the FUSE server for up to drainTimeout.
const mountRequestTimeout = 2 * time.Minute

// ErrNoDaemon is the error of a request that needs the running daemon.
var ErrNoDaemon = errors.New("no artifact-fs daemon is running on this state root")

// Unmount asks the running daemon to unmount name and keep it unmounted until
// Remount, and waits for the outcome.
func (s *Controller) Unmount(ctx context.Context, name string) error {
	return s.requestMount(ctx, name, false)
}

// Remount asks the running daemon to unmount name if it serves it and mount
// it again, and waits for the outcome.
func (s *Controller) Remount(ctx context.Context, name string) error {
	return s.requestMount(ctx, name, true)
}

// requestMount records the intent in the registry, nudges the daemon over its
// control socket, and waits until repo_runtime shows the daemon carried out
// the request. The intent stays in the registry when the wait ends early, so
// the daemon still carries it out unless a later request replaces it.
func (s *Controller) requestMount(ctx context.Context, name string, mount bool) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if live, err := daemonRunning(s.root); err != nil {
		return err
	} else if !live {
		return fmt.Errorf("%w: start one with artifact-fs daemon", ErrNoDaemon)
	}
	if mount && cfg.PrepareState == model.PrepareStateSyncPreparing {
		return fmt.Errorf("repo %s is still preparing", name)
	}
	request := rand.Text()
	requested := time.Now()
	if err := s.registry.RequestMount(ctx, cfg.ID, mount, request); err != nil {
		return err
	}
	nudgeDaemon(s.root)
	op := "unmount"
	if mount {
		op = "remount"
	}
	pending := fmt.Sprintf("the request stays pending until the daemon carries it out; run status --name %s", name)
	timeout := time.NewTimer(cmp.Or(s.requestTimeout, mountRequestTimeout))
	defer timeout.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		latest, err := s.registry.GetRepo(ctx, name)
		if err != nil {
			return err
		}
		if latest.MountRequest != request {
			return fmt.Errorf("%s %s: a later unmount or remount request superseded this one", op, name)
		}
		rec, ok, err := s.registry.GetRuntime(ctx, cfg.ID)
		if err != nil {
			return err
		}
		if ok && rec.MountRequest == request {
			failed := !rec.LastErrorAt.Before(requested) && (rec.LastErrorPhase == phaseUnmount || mount && rec.LastErrorPhase == phaseMount)
			if failed {
				return fmt.Errorf("%s %s failed in phase %s: %s; the daemon retries it until it succeeds or a later request replaces it", op, name, rec.LastErrorPhase, rec.LastError)
			}
			if !mount && rec.State == repoStateUnmounted || mount && rec.State != repoStateUnmounted {
				return nil
			}
		}
		if live, err := daemonRunning(s.root); err != nil {
			return err
		} else if !live {
			return fmt.Errorf("%s %s: the daemon stopped before it carried out the request; %s", op, name, pending)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %s: %w; %s", op, name, ctx.Err(), pending)
		case <-timeout.C:
			return fmt.Errorf("%s %s: the daemon did not carry out the request within %s; %s", op, name, cmp.Or(s.requestTimeout, mountRequestTimeout), pending)
		case <-ticker.C:
		}
	}
}

// EvictCache evicts the least recently accessed blobs from name's blob cache
// until it holds at most maxBytes. A cache directory that repos share is
// evicted as a whole.
func (s *Controller) EvictCache(ctx context.Context, name string, maxBytes int64) (blobcache.EvictResult, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return blobcache.EvictResult{}, err
	}
	s.fillPaths(&cfg)
	return blobcache.Evict(ctx, cfg.BlobCacheDir, maxBytes)
}
