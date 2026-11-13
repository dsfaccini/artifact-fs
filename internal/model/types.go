package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrBlobTooLarge = errors.New("blob too large")

type RepoID string

// SourceRequirement describes the remote revision ArtifactFS must acquire.
type SourceRequirement struct {
	Ref            string
	RequiredCommit string
	Depth          int
}

// PreparedSource records the exact source selected during acquisition.
type PreparedSource struct {
	Ref      string
	Commit   string
	Verified bool
	Acquired bool // This call installed a new remote acquisition rather than reusing its receipt.
}

// RepoConfig is a registered repo: its paths, its source, its refresh policy,
// and what the registry records about its requests and preparation. The
// daemon's runtime holds the whole config. The hydrator, the overlay and the
// FUSE layer take only its RepoPaths.
type RepoConfig struct {
	RepoPaths
	RepoSpec
	RefreshPolicy
	Enabled        bool
	PrepareState   PrepareState
	PrepareError   string
	AcquiredRef    string
	AcquiredCommit string
	AcquiredAt     time.Time
	// ConfigVersion identifies the registered config. A new registration
	// changes it, and only a new registration does, so it is the one token
	// that tells whether a config is still current.
	ConfigVersion string
	// PrepareRequest identifies the latest preparation the CLI asked for, by
	// add-repo or prepare.
	PrepareRequest string
	// MountRequest identifies the latest unmount or remount the CLI asked for.
	MountRequest string
	// WarnKeptGenerations is how many older snapshot generations a prune may
	// keep for open files before the daemon warns. Like RefreshPolicy, it is
	// not part of the repo's prepare identity.
	WarnKeptGenerations int
}

// RepoPaths names a repo and locates its state on this machine.
type RepoPaths struct {
	ID            RepoID
	Name          string
	MountRoot     string
	MountPath     string
	GitDir        string
	OverlayDir    string
	BlobCacheDir  string
	MetaDBPath    string
	OverlayDBPath string
}

// RepoSpec is the source a repo's preparation acquires: the remote, the
// refs, the required commit and history depth, and whether the git dir is
// prepared or adopted.
type RepoSpec struct {
	// RemoteURL can carry credentials. Only the CLI, the registry, the
	// daemon and gitstore read it.
	RemoteURL         string
	RemoteURLRedacted string
	Branch            string
	FetchRef          string
	RequiredCommit    string
	HistoryDepth      int
	PreparedGitDir    bool
	AdoptGitDir       bool
}

// RefreshPolicy is how the daemon refreshes a repo from its remote. It is not
// part of the repo's prepare identity.
type RefreshPolicy struct {
	RefreshInterval       time.Duration
	RemoteRefreshDisabled bool
}

// RepoRuntimeRecord is what the daemon last recorded about a repo. Only the
// daemon writes it.
type RepoRuntimeRecord struct {
	RepoID RepoID
	State  string
	// Phase is the step the repo's runtime is in or last finished.
	Phase string
	// LastError is the redacted error of the last failure that has not yet
	// succeeded on retry, and LastErrorPhase the step that failed.
	LastError      string
	LastErrorPhase string
	LastErrorAt    time.Time
	NextRetryAt    time.Time
	// Attempts counts the consecutive failures of LastErrorPhase.
	Attempts         int
	ServedGeneration int64
	ServedCommit     string
	ServedRef        string
	LastFetchAt      time.Time
	LastFetchResult  string
	AheadCount       int
	BehindCount      int
	Diverged         bool
	// MountRequest is the CLI request the daemon last carried out.
	MountRequest string
	// ClaimsLeftover reports that the daemon still claims a mount an earlier
	// daemon left at the repo's mount point, so the next daemon claims it too.
	ClaimsLeftover bool
	UpdatedAt      time.Time
	// KeptGenerations is how many generations older than the ones a prune
	// must keep the last prune kept because open files read them.
	// KeptGenerationsWarning describes them while they number at least the
	// repo's WarnKeptGenerations, and is empty otherwise.
	KeptGenerations        int
	KeptGenerationsWarning string
}

type RepoRuntimeState struct {
	RepoID             RepoID
	CurrentHEADOID     string
	CurrentHEADRef     string
	SnapshotGeneration int64
	LastFetchAt        time.Time
	LastFetchResult    string
	AheadCount         int
	BehindCount        int
	Diverged           bool
	HydratedBlobCount  int64
	HydratedBlobBytes  int64
	DirtyOverlay       bool
	State              string
	PrepareError       string
	// MountError is LastError when the mount or serve phase failed last.
	MountError            string
	SourceRef             string
	RequiredCommit        string
	Acquisition           string
	RemoteRefreshDisabled bool
	// Published is the generation the snapshot store last published. Served
	// is the one the daemon's mount serves, which lags Published after a
	// failed overlay transition. CurrentHEADOID, CurrentHEADRef and
	// SnapshotGeneration report Served while the daemon serves the repo, and
	// Published otherwise.
	PublishedHEADOID    string
	PublishedHEADRef    string
	PublishedGeneration int64
	ServedHEADOID       string
	ServedHEADRef       string
	ServedGeneration    int64
	Phase               string
	LastError           string
	LastErrorPhase      string
	LastErrorAt         time.Time
	NextRetryAt         time.Time
	Attempts            int
	// KeptGenerations and KeptGenerationsWarning are the daemon's record of
	// them, and WarnKeptGenerations the registered threshold.
	KeptGenerations        int
	WarnKeptGenerations    int
	KeptGenerationsWarning string
}

// PrepareState is where a repo's preparation stands. The empty state is a
// repo add-repo prepared, which the daemon mounts without preparing it, or,
// with PrepareError set, whose preparation failed.
type PrepareState string

const (
	// PrepareStatePreparing is an async repo whose preparation the daemon runs.
	PrepareStatePreparing PrepareState = "preparing"
	// PrepareStateSyncPreparing is a repo the CLI prepares; the daemon waits.
	PrepareStateSyncPreparing PrepareState = "sync-preparing"
	PrepareStateReady         PrepareState = "ready"
	PrepareStateFailed        PrepareState = "failed"
)

// BaseNode represents a tracked entry from the git tree. Inode IDs are assigned
// at runtime by the FUSE layer (monotonic allocation, like tigrisfs).
type BaseNode struct {
	RepoID    RepoID
	Path      string
	Type      string // file, dir, symlink
	Mode      uint32
	ObjectOID string
	SizeState string // unknown, known
	SizeBytes int64
}

type OverlayKind string

// MaxSymlinkTargetBytes matches the largest target supported by the FUSE
// adapters and common Linux PATH_MAX behavior.
const MaxSymlinkTargetBytes = 4096

const (
	OverlayKindCreate  OverlayKind = "create"
	OverlayKindModify  OverlayKind = "modify"
	OverlayKindDelete  OverlayKind = "delete"
	OverlayKindRename  OverlayKind = "rename"
	OverlayKindMkdir   OverlayKind = "mkdir"
	OverlayKindSymlink OverlayKind = "symlink"
)

type OverlayEntry struct {
	RepoID      RepoID
	Path        string
	Kind        OverlayKind
	BackingPath string
	Mode        uint32
	SizeBytes   int64
	MtimeUnixNs int64
	CtimeUnixNs int64
	SourceOID   string
	SourceMode  uint32
	TargetPath  string
}

func (e OverlayEntry) IsDeleted() bool {
	return e.Kind == OverlayKindDelete
}

func (e OverlayEntry) NodeType() string {
	switch e.Kind {
	case OverlayKindMkdir:
		return "dir"
	case OverlayKindSymlink:
		return "symlink"
	default:
		return "file"
	}
}

type HydrationTask struct {
	RepoID     RepoID
	Path       string
	ObjectOID  string
	SizeState  string
	SizeBytes  int64
	Priority   int
	Reason     string
	EnqueuedAt time.Time
}

// CleanPath normalizes a filesystem path for use as a map key or DB lookup.
// It strips leading slashes, cleans dot-segments, and defaults empty/root to ".".
func CleanPath(path string) string {
	if path == "." || path == "/" || path == "" {
		return "."
	}
	path = filepath.Clean(path)
	if path == "/" {
		return "."
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "."
	}
	return path
}

// ValidateRepoName checks that a repo name is safe for use in filesystem paths
// and in the FUSE mount's fsname option.
func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repo name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("invalid repo name %q: must not contain path separators or '..'", name)
	}
	if strings.ContainsAny(name, ",=") {
		return fmt.Errorf("invalid repo name %q: must not contain ',' or '=', which separate FUSE mount options", name)
	}
	return nil
}

// The interfaces below list the methods production code calls through them.
// Each implementation asserts its interface with a var _ declaration.

// Registry is the repo registry: the stored configs, their prepare and mount
// requests, and the runtime records the daemon writes.
type Registry interface {
	AddRepo(ctx context.Context, cfg RepoConfig) error
	RemoveRepo(ctx context.Context, name string) error
	GetRepo(ctx context.Context, name string) (RepoConfig, error)
	ListRepos(ctx context.Context) ([]RepoConfig, error)
	UpdateRefresh(ctx context.Context, repoID RepoID, policy RefreshPolicy) error
	UpdateWarnKeptGenerations(ctx context.Context, repoID RepoID, count int) error
	BeginPrepare(ctx context.Context, cfg RepoConfig, state PrepareState, request string) error
	UpdatePrepareStateForRequest(ctx context.Context, cfg RepoConfig, state PrepareState, prepareErr string) error
	RecordAcquisition(ctx context.Context, cfg RepoConfig, source PreparedSource) error
	RequestMount(ctx context.Context, repoID RepoID, enabled bool, request string) error
	PutRuntime(ctx context.Context, rec RepoRuntimeRecord) error
	GetRuntime(ctx context.Context, repoID RepoID) (RepoRuntimeRecord, bool, error)
	DeleteRuntime(ctx context.Context, repoID RepoID) error
	ClearRuntime(ctx context.Context) ([]RepoRuntimeRecord, error)
	Close() error
}

// GitStore runs git for the daemon's preparations and refreshes, and fetches
// and reads blobs for the hydrator.
type GitStore interface {
	CloneBlobless(ctx context.Context, cfg RepoConfig) error
	PrepareSource(ctx context.Context, cfg RepoConfig, requirement SourceRequirement) (PreparedSource, error)
	ValidateAmbientRemote(repo RepoConfig) error
	ConfigureRemoteAmbient(ctx context.Context, repo RepoConfig) error
	ConfigureRemoteWithCredentials(ctx context.Context, repo RepoConfig) error
	FetchNonInteractive(ctx context.Context, repo RepoConfig) error
	FetchRefAmbient(ctx context.Context, repo RepoConfig, ref string) error
	FetchRefWithCredentials(ctx context.Context, repo RepoConfig, ref string) error
	PrepareFetchedBranch(ctx context.Context, repo RepoConfig, ref string) error
	ValidatePreparedGitDir(ctx context.Context, repo RepoConfig) error
	ConfigureAdoptedWorktree(ctx context.Context, repo RepoConfig) error
	WarnAdoptedRemoteCredentials(ctx context.Context, repo RepoConfig)
	ReleaseAdoptedWorktree(ctx context.Context, repo RepoConfig, stateRoot string) error
	EnsureIndexInitialized(ctx context.Context, repo RepoConfig) error
	ConfigureStatusOptimization(ctx context.Context, repo RepoConfig, stateRoot string) error
	ConfigureFSMonitorHook(ctx context.Context, repo RepoConfig, stateRoot string) error
	BootstrapCleanIndexForFSMonitor(ctx context.Context, repo RepoConfig, snapshotOID string, validate func() (bool, error)) (bool, error)
	ResolveHEAD(ctx context.Context, repo RepoConfig) (oid string, ref string, err error)
	CommitTimestamp(ctx context.Context, repo RepoConfig, oid string) (int64, error)
	ComputeAheadBehind(ctx context.Context, repo RepoConfig) (ahead int, behind int, diverged bool, err error)
	BuildTreeIndex(ctx context.Context, repo RepoConfig, headOID string) ([]BaseNode, error)
	BlobToCache(ctx context.Context, repo RepoPaths, objectOID string, dstPath string) (size int64, err error)
	FetchMissingBlobs(ctx context.Context, repo RepoPaths, objectOIDs []string) error
	ReadBlob(ctx context.Context, repo RepoPaths, objectOID string, maxBytes int64) ([]byte, error)
	VerifyBlob(ctx context.Context, repo RepoPaths, objectOID string, cachePath string) (ok bool, err error)
	SetBatchPoolSize(n int)
	CloseBatchPool(gitDir string)
	Close()
}

type SnapshotStore interface {
	LookupNode(ctx context.Context, generation int64, path string) (BaseNode, bool, error)
	ListChildren(generation int64, parentPath string) ([]BaseNode, error)
}

type OverlayStore interface {
	Lookup(ctx context.Context, path string) (OverlayEntry, bool, error)
	EnsureCopyOnWrite(ctx context.Context, repo RepoPaths, path string, base BaseNode) (OverlayEntry, error)
	EnsureCopyOnWriteFrom(ctx context.Context, repo RepoPaths, path string, base BaseNode, src *os.File) (OverlayEntry, error)
	CreateFile(ctx context.Context, path string, mode uint32) (OverlayEntry, error)
	CreateSymlink(ctx context.Context, path string, target string) (OverlayEntry, error)
	WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error)
	FlushFile(ctx context.Context, path string) error
	SyncFile(ctx context.Context, path string) error
	Truncate(ctx context.Context, path string, size int64) error
	// GrantOwnerAccess lets path's backing file open for writing when an
	// earlier version left it read-only.
	GrantOwnerAccess(ctx context.Context, path string) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string, sourceTracked bool) error
	RenameTree(ctx context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error
	RenameAndMarkModifiedFromBase(ctx context.Context, oldPath, newPath string, sourceOID string, sourceMode uint32, sourceTracked bool) error
	Mkdir(ctx context.Context, path string, mode uint32) error
	SetMode(ctx context.Context, path string, mode uint32) error
	SetMtime(ctx context.Context, path string, t time.Time) error
	ReconcileChecked(ctx context.Context, baseLookup func(path string) (BaseNode, bool, error), lock sync.Locker, switchBase func()) error
	ListByPrefix(ctx context.Context, prefix string) ([]OverlayEntry, error)
	// HasEntryBelow reports whether an entry of any kind lies below path.
	HasEntryBelow(ctx context.Context, path string) (bool, error)
	// CountEntries returns the number of entries of any kind, counting at
	// most limit.
	CountEntries(ctx context.Context, limit int) (int, error)
}

type Hydrator interface {
	EnqueueBatch(tasks []HydrationTask)
	EnsureHydrated(ctx context.Context, repo RepoPaths, node BaseNode) (cachePath string, size int64, err error)
	OpenHydrated(ctx context.Context, repo RepoPaths, node BaseNode) (*os.File, int64, error)
	ReadBlob(ctx context.Context, repo RepoPaths, node BaseNode, maxBytes int64) ([]byte, error)
}
