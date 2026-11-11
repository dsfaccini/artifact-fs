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
	"github.com/cloudflare/artifact-fs/internal/gitstore"
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
	if err := s.normalizeRepoConfig(&cfg, opts); err != nil {
		return err
	}
	cfg.ConfigVersion = rand.Text()
	cfg.PrepareRequest = rand.Text()
	cloneURL := cfg.RemoteURL
	if opts.Async {
		cfg.PrepareState = model.PrepareStatePreparing
	} else {
		cfg.PrepareState = model.PrepareStateSyncPreparing
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			cfg.RemoteURL = ""
		}
	}
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
	return s.prepareSync(ctx, cfg)
}

// normalizeRepoConfig validates cfg and fills in what add-repo leaves to
// defaults, once, before the registry stores it: full refs, the fetch ref,
// and absolute paths. Every reader trusts the stored config.
func (s *core) normalizeRepoConfig(cfg *model.RepoConfig, opts AddRepoOptions) error {
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if err := gitstore.ValidateRemoteNotOption(cfg.RemoteURL); err != nil {
		return err
	}
	if cfg.AdoptGitDir {
		if opts.Async {
			return errors.New("--adopt-gitdir cannot be combined with --async")
		}
		if strings.TrimSpace(cfg.GitDir) == "" {
			return errors.New("--adopt-gitdir requires --git-dir")
		}
		if cfg.PreparedGitDir || cfg.RemoteURL != "" || cfg.RequiredCommit != "" || cfg.HistoryDepth != 0 || cfg.FetchRef != "" || (cfg.Branch != "" && cfg.Branch != "HEAD") {
			return errors.New("--adopt-gitdir cannot fetch, select a source ref, or prepare a remote git dir")
		}
		cfg.Branch = "HEAD"
		cfg.RemoteRefreshDisabled = true
	} else if err := normalizeSource(cfg, opts); err != nil {
		return err
	}
	if cfg.ID == "" {
		cfg.ID = model.RepoID(cfg.Name)
	}
	cfg.RemoteURLRedacted = auth.RedactRemoteURL(cfg.RemoteURL)
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	if cfg.WarnKeptGenerations < 0 {
		return errors.New("--warn-kept-generations must be positive")
	}
	cfg.WarnKeptGenerations = cmp.Or(cfg.WarnKeptGenerations, DefaultWarnKeptGenerations)
	s.fillPaths(cfg)
	for _, path := range []*string{&cfg.MountRoot, &cfg.MountPath, &cfg.GitDir, &cfg.OverlayDir, &cfg.BlobCacheDir, &cfg.MetaDBPath, &cfg.OverlayDBPath} {
		abs, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		*path = abs
	}
	cfg.PrepareError = ""
	return nil
}

// normalizeSource validates the source of a repo add-repo clones or fetches,
// and stores its refs in full: the source ref defaults to refs/heads/main,
// and the fetch ref to the source ref.
func normalizeSource(cfg *model.RepoConfig, opts AddRepoOptions) error {
	explicitGitDir := strings.TrimSpace(cfg.GitDir) != ""
	if cfg.PreparedGitDir && !opts.Async {
		return errors.New("--prepared-gitdir requires --async")
	}
	if cfg.PreparedGitDir && cfg.RequiredCommit != "" {
		return errors.New("--prepared-gitdir is not supported with --require-commit")
	}
	if opts.Async {
		if strings.TrimSpace(cfg.RemoteURL) == "" && !cfg.PreparedGitDir {
			return errors.New("--remote is required unless --prepared-gitdir is set")
		}
		if cfg.PreparedGitDir && !explicitGitDir {
			return errors.New("--git-dir is required with --prepared-gitdir")
		}
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			return errors.New("async repositories must use ambient credentials; remove credentials from --remote")
		}
	}
	// A verified source serves the commit its source ref must resolve to,
	// so a fetch ref would change nothing.
	if strings.TrimSpace(cfg.RequiredCommit) != "" && strings.TrimSpace(cfg.FetchRef) != "" {
		return errors.New("--fetch-ref cannot be combined with --require-commit")
	}
	cfg.Branch = model.CanonicalRef(cmp.Or(strings.TrimSpace(cfg.Branch), "refs/heads/main"))
	cfg.FetchRef = model.CanonicalRef(cmp.Or(strings.TrimSpace(cfg.FetchRef), cfg.Branch))
	if cfg.HistoryDepth < 0 {
		return errors.New("--depth must not be negative")
	}
	cfg.RequiredCommit = strings.ToLower(strings.TrimSpace(cfg.RequiredCommit))
	if cfg.RequiredCommit == "" {
		return nil
	}
	if len(cfg.RequiredCommit) != 40 && len(cfg.RequiredCommit) != 64 {
		return errors.New("--require-commit must be a full 40- or 64-character commit OID")
	}
	for _, character := range cfg.RequiredCommit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("--require-commit must be hexadecimal")
		}
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
			if err := s.git.ReleaseAdoptedWorktree(ctx, latest); err != nil {
				return err
			}
		}
		return s.registry.RemoveRepo(ctx, name)
	})
}

func (s *Controller) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	return s.registry.ListRepos(ctx)
}

func (s *Controller) SetRefresh(ctx context.Context, name string, policy model.RefreshPolicy) error {
	if policy.RefreshInterval <= 0 && !policy.RemoteRefreshDisabled {
		return errors.New("refresh interval must be positive")
	}
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if cfg.AdoptGitDir && !policy.RemoteRefreshDisabled {
		return errors.New("remote refresh cannot be enabled for an adopted worktree")
	}
	// The daemon applies the new policy at its next registry sync.
	return s.registry.UpdateRefresh(ctx, cfg.ID, policy)
}

// SetWarnKeptGenerations changes name's kept-generations warning threshold.
// The daemon applies it from the first prune after its next registry sync.
func (s *Controller) SetWarnKeptGenerations(ctx context.Context, name string, count int) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	return s.registry.UpdateWarnKeptGenerations(ctx, cfg.ID, count)
}

func (s *Service) updateRuntimeWarnKeptGenerations(rt *repoRuntime, count int) {
	s.mu.Lock()
	rt.cfg.WarnKeptGenerations = count
	s.mu.Unlock()
}

func (s *Service) updateRuntimeRefresh(rt *repoRuntime, policy model.RefreshPolicy) {
	if rt == nil {
		return
	}
	s.mu.Lock()
	if rt.cfg.RefreshPolicy == policy {
		s.mu.Unlock()
		return
	}
	rt.cfg.RefreshPolicy = policy
	interval := policy.RefreshInterval
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
	return s.git.FetchNonInteractive(ctx, cfg)
}

// Prepare asks for a new preparation of name. A sync repo prepares in the
// CLI; the daemon prepares an async one. Either way the daemon keeps serving
// the repo's mount, and only an async repo's gate holds FUSE ops meanwhile. An
// async repo that is ready and follows its ref has nothing to prepare, and
// Prepare reports false for it.
func (s *Controller) Prepare(ctx context.Context, name string) (bool, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return false, err
	}
	async := isAsyncRepo(cfg)
	if async && cfg.PrepareState == model.PrepareStateReady && cfg.RequiredCommit == "" {
		return false, nil
	}
	request := rand.Text()
	state := model.PrepareStateSyncPreparing
	if async {
		state = model.PrepareStatePreparing
	}
	if err := s.registry.BeginPrepare(ctx, cfg, state, request); err != nil {
		return false, err
	}
	if async {
		// The daemon prepares the repo again at its next registry sync.
		return true, nil
	}
	cfg.PrepareRequest, cfg.PrepareState, cfg.PrepareError = request, state, ""
	return true, s.prepareSync(ctx, cfg)
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
	return blobcache.Evict(ctx, cfg.BlobCacheDir, maxBytes)
}
