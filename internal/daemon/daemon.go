package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

const DefaultHydrationConcurrency = 4

const (
	defaultPrepareTimeout    = 30 * time.Minute
	headReconcileMinBackoff  = time.Second
	headReconcileMaxBackoff  = 30 * time.Second
	prepareStateWriteTimeout = 5 * time.Second
	repoConfigLockRetry      = 25 * time.Millisecond
	sizeUpdateFlushInterval  = 100 * time.Millisecond
)

const (
	repoStateMounted   = "mounted"
	repoStateUnmounted = "unmounted"
	repoStateDegraded  = "degraded"
)

const (
	prepareModeSync  = "sync"
	prepareModeAsync = "async"

	prepareSourceFreshClone     = "fresh_clone"
	prepareSourceExistingClone  = "existing_clone"
	prepareSourcePreparedGitDir = "prepared_git_dir"
	prepareSourceVerified       = "verified_source"

	preparePhaseUnknown           = "unknown"
	preparePhaseValidate          = "validate"
	preparePhaseInitializeStorage = "initialize_storage"
	preparePhaseClone             = "clone"
	preparePhaseConfigureRemote   = "configure_remote"
	preparePhaseFetch             = "fetch"
	preparePhaseUpdateBranch      = "update_branch"
	preparePhaseResolveHEAD       = "resolve_head"
	preparePhaseOpenSnapshot      = "open_snapshot"
	preparePhaseBuildTree         = "build_tree"
	preparePhasePublishSnapshot   = "publish_snapshot"
	preparePhasePersistReady      = "persist_ready"
	preparePhasePersistPreparing  = "persist_preparing"
	preparePhaseActivateRuntime   = "activate_runtime"
	preparePhaseCleanupSnapshot   = "cleanup_snapshot"
	preparePhaseComplete          = "complete"
	snapshotPhaseBuild            = "build"
	snapshotPhasePublish          = "publish"

	prepareLogStateStarted   = "started"
	prepareLogStateCompleted = "completed"
	prepareLogStateFailed    = "failed"
	prepareLogStateCanceled  = "canceled"

	logRepoPreparationStarted    = "repo preparation started"
	logRepoPreparationCompleted  = "repo preparation completed"
	logRepoPreparationFailed     = "repo preparation failed"
	logRepoPreparationCanceled   = "repo preparation canceled"
	logRepoPrepareStatusWriteErr = "repo prepare status write failed"
	logRepoConfigChanged         = "repo config changed, remounting"
)

type Service struct {
	root                 string
	mountRoot            string
	mount                mountFunc
	hydrationConcurrency int
	// prepareContext bounds one preparation attempt; nil means defaultPrepareTimeout.
	prepareContext func(context.Context) (context.Context, context.CancelFunc)
	// headReconcileBackoff is the first retry delay after a failed HEAD
	// reconcile; zero means headReconcileMinBackoff.
	headReconcileBackoff time.Duration
	logger               *slog.Logger
	registry             *registry.Store
	git                  *gitstore.Store
	mu                   sync.Mutex
	running              map[model.RepoID]*repoRuntime
	preparing            map[model.RepoID]int64
	prepareAttempts      map[model.RepoID]int64
	prepareSeq           int64
	mountFailures        map[model.RepoID]*mountFailure
	prepareWorkers       sync.WaitGroup
	closing              bool
	// lock is the daemon lock on root, held until Close.
	lock *os.File
	// wake carries control socket nudges to the sync loop.
	wake chan struct{}
	// records holds what recordRuntime last wrote for each repo. recordMu
	// serializes the writes, and is taken before mu.
	recordMu sync.Mutex
	records  map[model.RepoID]*model.RepoRuntimeRecord
}

// New returns the daemon for root. It takes the daemon lock, so a second
// daemon on root fails here.
func New(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	root, err := ensureStateRoot(root)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDaemonLock(root)
	if err != nil {
		return nil, err
	}
	svc, err := newService(ctx, root, logger)
	if err != nil {
		lock.Close()
		return nil, err
	}
	svc.lock = lock
	return svc, nil
}

// NewController returns the controller one CLI command runs on. It takes no
// daemon lock and reaches the daemon only through the registry and the
// control socket.
// TODO(daemon-status): split into its own type.
func NewController(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	root, err := ensureStateRoot(root)
	if err != nil {
		return nil, err
	}
	return newService(ctx, root, logger)
}

func newService(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		return nil, err
	}
	svc := &Service{
		root:            root,
		mount:           defaultMount,
		logger:          logger,
		registry:        reg,
		git:             gitstore.New(logger),
		running:         map[model.RepoID]*repoRuntime{},
		preparing:       map[model.RepoID]int64{},
		prepareAttempts: map[model.RepoID]int64{},
		mountFailures:   map[model.RepoID]*mountFailure{},
		wake:            make(chan struct{}, 1),
		records:         map[model.RepoID]*model.RepoRuntimeRecord{},
	}
	svc.git.SetBatchPoolSize(DefaultHydrationConcurrency)
	return svc, nil
}

func (s *Service) SetMountRoot(root string) {
	if strings.TrimSpace(root) != "" {
		s.mountRoot = root
	}
}

func (s *Service) SetHydrationConcurrency(n int) {
	if n > 0 {
		s.hydrationConcurrency = n
		s.git.SetBatchPoolSize(n)
	}
}

func (s *Service) hydrationWorkers() int {
	if s.hydrationConcurrency > 0 {
		return s.hydrationConcurrency
	}
	return DefaultHydrationConcurrency
}

func (s *Service) Close() error {
	s.mu.Lock()
	s.closing = true
	ids := make([]model.RepoID, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := s.unmount(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	s.prepareWorkers.Wait()
	s.git.Close()
	err := s.registry.Close()
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}

func (s *Service) fillPaths(cfg *model.RepoConfig) {
	if cfg.MountRoot == "" {
		if s.mountRoot != "" {
			cfg.MountRoot = s.mountRoot
		} else {
			cfg.MountRoot = filepath.Join(s.root, "mnt")
		}
	}
	if cfg.MountPath == "" {
		cfg.MountPath = filepath.Join(cfg.MountRoot, cfg.Name)
	}
	if cfg.GitDir == "" {
		cfg.GitDir = filepath.Join(s.root, "repos", string(cfg.ID), "git")
	}
	if cfg.OverlayDir == "" {
		cfg.OverlayDir = filepath.Join(s.root, "overlays", string(cfg.ID))
	}
	if cfg.BlobCacheDir == "" {
		cfg.BlobCacheDir = filepath.Join(s.root, "cache", "blobs", string(cfg.ID))
	}
	if cfg.MetaDBPath == "" {
		cfg.MetaDBPath = filepath.Join(s.root, "meta", string(cfg.ID)+".sqlite")
	}
	if cfg.OverlayDBPath == "" {
		cfg.OverlayDBPath = filepath.Join(cfg.OverlayDir, "meta.sqlite")
	}
}
