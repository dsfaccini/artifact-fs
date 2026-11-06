package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
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
	// defaultGenerationRetention keeps a generation readable for two
	// fusefs.CacheTTL after the resolver stops serving it, so attributes the
	// kernel received just before or just after the switch expire first.
	defaultGenerationRetention = 2 * fusefs.CacheTTL
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

// core is what the CLI controller and the daemon share: the state root, the
// registry, the git store and the preparation pipeline.
type core struct {
	root      string
	mountRoot string
	// prepareContext bounds one preparation attempt; nil means defaultPrepareTimeout.
	prepareContext func(context.Context) (context.Context, context.CancelFunc)
	logger         *slog.Logger
	registry       *registry.Store
	git            *gitstore.Store
	// generationRetention is how long a retired generation stays readable;
	// zero means defaultGenerationRetention.
	generationRetention time.Duration
}

// Controller carries out one CLI command. It never mounts. It reaches the
// daemon only through the registry, repo_runtime and the control socket.
type Controller struct {
	*core
	// requestTimeout bounds the wait of Unmount and Remount; zero means
	// mountRequestTimeout.
	requestTimeout time.Duration
}

// Service is the daemon: it holds the daemon lock and mounts and serves every
// registered repo.
type Service struct {
	*core
	mount mountFunc
	// unmountPath unmounts a mount the daemon did not make.
	unmountPath func(string) error
	// mountPointEntry reports whether a mount point holds a mount, and what
	// the mount table lists for it. Tests replace it to stand in for the
	// kernel's mount table.
	mountPointEntry func(string) (mountEntry, bool)
	// mountTimeout bounds the wait for one FUSE mount; zero means mountTimeout.
	mountTimeout         time.Duration
	hydrationConcurrency int
	// Test hooks, nil in production. runtimeStarted runs between starting a
	// runtime and recording it. mountReleased runs in a join goroutine after
	// it released a FUSE mount whose server exited cleanly, before it records
	// the exit. joinFinished runs as a join goroutine ends.
	runtimeStarted, mountReleased, joinFinished func()
	// headReconcileBackoff is the first retry delay after a failed HEAD
	// reconcile; zero means headReconcileMinBackoff.
	headReconcileBackoff time.Duration
	mu                   sync.Mutex
	running              map[model.RepoID]*repoRuntime
	preparing            map[model.RepoID]int64
	prepareAttempts      map[model.RepoID]int64
	prepareSeq           int64
	// leftovers holds the repos the daemon process before this one ran. A
	// FUSE mount at such a repo's mount point is that process's, and the
	// daemon unmounts it before it mounts the repo.
	leftovers map[model.RepoID]bool
	// mountRetries backs off the mounts of each repo whose mount failed.
	mountRetries map[model.RepoID]*mountRetry
	// unmounting holds each repo's unmount that has not returned.
	unmounting     map[model.RepoID]*unmountOp
	prepareWorkers sync.WaitGroup
	closing        bool
	// lock is the daemon lock on root, held until Close.
	lock *os.File
	// wake carries control socket nudges to the sync loop.
	wake chan struct{}
	// syncInterval is how often Start polls the registry; zero means
	// registrySyncInterval.
	syncInterval time.Duration
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
	c, err := newCore(ctx, root, logger)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Service{
		core:            c,
		mount:           defaultMount,
		unmountPath:     defaultUnmountPath,
		mountPointEntry: mountPointEntry,
		running:         map[model.RepoID]*repoRuntime{},
		preparing:       map[model.RepoID]int64{},
		prepareAttempts: map[model.RepoID]int64{},
		leftovers:       map[model.RepoID]bool{},
		mountRetries:    map[model.RepoID]*mountRetry{},
		unmounting:      map[model.RepoID]*unmountOp{},
		lock:            lock,
		wake:            make(chan struct{}, 1),
		records:         map[model.RepoID]*model.RepoRuntimeRecord{},
	}, nil
}

// NewController returns the controller for root. It takes no daemon lock, so
// it runs beside the daemon.
func NewController(ctx context.Context, root string, logger *slog.Logger) (*Controller, error) {
	root, err := ensureStateRoot(root)
	if err != nil {
		return nil, err
	}
	c, err := newCore(ctx, root, logger)
	if err != nil {
		return nil, err
	}
	return &Controller{core: c}, nil
}

func newCore(ctx context.Context, root string, logger *slog.Logger) (*core, error) {
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		return nil, err
	}
	c := &core{root: root, logger: logger, registry: reg, git: gitstore.New(logger)}
	c.git.SetBatchPoolSize(DefaultHydrationConcurrency)
	return c, nil
}

// Close releases the controller's registry and git processes.
func (c *Controller) Close() error {
	c.git.Close()
	return c.registry.Close()
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

// Close unmounts every repo and releases the registry, the git processes and
// the daemon lock, also when an unmount fails. It returns the first error. A
// runtime that failed to unmount keeps its workers, so Close does not wait
// for its preparation.
func (s *Service) Close() error {
	s.mu.Lock()
	s.closing = true
	cfgs := make([]model.RepoConfig, 0, len(s.running))
	for _, rt := range s.running {
		cfgs = append(cfgs, rt.cfg)
	}
	s.mu.Unlock()
	var errs []error
	for _, cfg := range cfgs {
		if err := s.unmount(cfg.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		// The next daemon on the root finds no mount of this one's to unmount.
		s.recordStopped(cfg)
	}
	if len(errs) == 0 {
		s.prepareWorkers.Wait()
	}
	s.git.Close()
	if err := s.registry.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.lock.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (s *core) fillPaths(cfg *model.RepoConfig) {
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
