package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func (s *Service) Status(ctx context.Context, name string) (model.RepoRuntimeState, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return model.RepoRuntimeState{}, err
	}

	// If we're the running daemon, use in-memory state.
	s.mu.Lock()
	rt, ok := s.running[cfg.ID]
	if ok {
		dirty, _ := rt.overlay.DirtyCount(ctx)
		rt.state.DirtyOverlay = dirty > 0
		st := rt.state // copy under lock
		blobCacheDir := rt.cfg.BlobCacheDir
		s.mu.Unlock()
		applyHydrationStats(&st, blobCacheDir)
		applySourceStatus(&st, cfg)
		st.MountError = cfg.MountError
		return st, nil
	}
	s.mu.Unlock()
	return s.readPersistedStatus(ctx, cfg), nil
}

func (s *Service) readPersistedStatus(ctx context.Context, cfg model.RepoConfig) model.RepoRuntimeState {
	// One-shot CLI process: reconstruct state from persisted stores and
	// OS-level mount check since we don't share memory with the daemon.
	st := model.RepoRuntimeState{RepoID: cfg.ID, State: repoStateUnmounted, LastFetchResult: "never", PrepareError: cfg.PrepareError, MountError: cfg.MountError}
	if cfg.PrepareState == model.PrepareStateSyncPreparing {
		st.State = model.PrepareStatePreparing
	} else if isPendingOrFailedPrepareState(cfg.PrepareState) {
		st.State = cfg.PrepareState
	} else if isMounted(cfg.MountPath) {
		st.State = repoStateMounted
	}
	if cfg.MetaDBPath != "" {
		if snap, err := snapshot.New(ctx, cfg.MetaDBPath); err == nil {
			st.CurrentHEADOID, st.CurrentHEADRef, st.SnapshotGeneration, _ = snap.ReadState(ctx)
			snap.Close()
		}
	}
	if cfg.OverlayDBPath != "" {
		if _, statErr := os.Stat(cfg.OverlayDBPath); statErr == nil {
			if db, err := meta.OpenDB(cfg.OverlayDBPath); err == nil {
				var count int64
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind <> 'delete'`).Scan(&count); err == nil {
					st.DirtyOverlay = count > 0
				}
				db.Close()
			}
		}
	}
	// Best-effort last fetch time from FETCH_HEAD mtime.
	if fi, err := os.Stat(filepath.Join(cfg.GitDir, "FETCH_HEAD")); err == nil {
		// Verified acquisition itself writes FETCH_HEAD before its receipt.
		// Only a newer mtime is evidence of a later remote refresh.
		if cfg.RequiredCommit == "" || (!cfg.AcquiredAt.IsZero() && fi.ModTime().After(cfg.AcquiredAt)) {
			st.LastFetchAt = fi.ModTime()
			st.LastFetchResult = "ok"
		}
	}
	applyHydrationStats(&st, cfg.BlobCacheDir)
	applySourceStatus(&st, cfg)
	return st
}

func applySourceStatus(st *model.RepoRuntimeState, cfg model.RepoConfig) {
	st.SourceRef = cfg.Branch
	st.RequiredCommit = cfg.RequiredCommit
	st.RemoteRefreshDisabled = cfg.RemoteRefreshDisabled
	switch {
	case cfg.RequiredCommit == "":
		st.Acquisition = "not_required"
	case cfg.AcquiredRef == cfg.Branch && strings.EqualFold(cfg.AcquiredCommit, cfg.RequiredCommit) && !cfg.AcquiredAt.IsZero():
		st.Acquisition = "verified"
	default:
		st.Acquisition = "pending"
	}
}

func newRuntimeState(repoID model.RepoID, headOID string, headRef string, gen int64) model.RepoRuntimeState {
	return model.RepoRuntimeState{
		RepoID:             repoID,
		CurrentHEADOID:     headOID,
		CurrentHEADRef:     headRef,
		SnapshotGeneration: gen,
		LastFetchResult:    "never",
		State:              repoStateMounted,
	}
}

func setHeadState(st *model.RepoRuntimeState, oid string, ref string, gen int64) {
	st.CurrentHEADOID = oid
	st.CurrentHEADRef = ref
	st.SnapshotGeneration = gen
}

func applyAheadBehind(st *model.RepoRuntimeState, state aheadBehind) {
	st.AheadCount = state.ahead
	st.BehindCount = state.behind
	st.Diverged = state.diverged
}

func markFetchSuccess(st *model.RepoRuntimeState, at time.Time, state aheadBehind, mounted bool) {
	markFetchResult(st, at, "ok", mounted)
	applyAheadBehind(st, state)
}

func markFetchResult(st *model.RepoRuntimeState, at time.Time, result string, mounted bool) {
	st.LastFetchResult = result
	st.LastFetchAt = at
	if st.State == repoStateDegraded && result == "ok" && mounted {
		st.State = repoStateMounted
	}
}

func markFetchFailure(st *model.RepoRuntimeState, result string) {
	st.State = repoStateDegraded
	st.LastFetchResult = result
}

// applyHydrationStats reads the cache counters that the hydrator and
// evict-cache keep, without walking the cache.
func applyHydrationStats(st *model.RepoRuntimeState, cacheDir string) {
	stats, _ := blobcache.ReadStats(cacheDir)
	st.HydratedBlobCount = stats.Blobs
	st.HydratedBlobBytes = stats.Bytes
}
