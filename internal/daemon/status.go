package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// Status reports name from what the daemon recorded in repo_runtime and from
// the stores. A record counts only while a daemon holds the daemon lock.
func (s *Controller) Status(ctx context.Context, name string) (model.RepoRuntimeState, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return model.RepoRuntimeState{}, err
	}
	rec, recorded, err := s.registry.GetRuntime(ctx, cfg.ID)
	if err != nil {
		return model.RepoRuntimeState{}, err
	}
	live, err := daemonRunning(s.root)
	if err != nil {
		return model.RepoRuntimeState{}, err
	}
	return persistedStatus(ctx, cfg, rec, recorded && live), nil
}

// persistedStatus builds cfg's status. live says whether rec comes from the
// running daemon.
func persistedStatus(ctx context.Context, cfg model.RepoConfig, rec model.RepoRuntimeRecord, live bool) model.RepoRuntimeState {
	st := model.RepoRuntimeState{RepoID: cfg.ID, State: repoStateUnmounted, LastFetchResult: "never", PrepareError: cfg.PrepareError}
	switch {
	case cfg.PrepareState == model.PrepareStateSyncPreparing:
		st.State = string(model.PrepareStatePreparing)
	case live && rec.State != "":
		st.State = rec.State
	case isPendingOrFailedPrepareState(cfg.PrepareState):
		st.State = string(cfg.PrepareState)
	}
	// The last failure stays visible after the daemon stops, but nothing
	// retries it.
	st.LastError, st.LastErrorPhase, st.LastErrorAt, st.Attempts = rec.LastError, rec.LastErrorPhase, rec.LastErrorAt, rec.Attempts
	if live {
		st.Phase = rec.Phase
		st.NextRetryAt = rec.NextRetryAt
		st.ServedHEADOID, st.ServedHEADRef, st.ServedGeneration = rec.ServedCommit, rec.ServedRef, rec.ServedGeneration
		st.AheadCount, st.BehindCount, st.Diverged = rec.AheadCount, rec.BehindCount, rec.Diverged
	}
	if rec.LastErrorPhase == phaseMount || rec.LastErrorPhase == phaseServe {
		st.MountError = rec.LastError
	}
	if !rec.LastFetchAt.IsZero() || rec.LastFetchResult != "" {
		st.LastFetchAt, st.LastFetchResult = rec.LastFetchAt, rec.LastFetchResult
	}
	if cfg.MetaDBPath != "" {
		if snap, err := snapshot.New(ctx, cfg.MetaDBPath); err == nil {
			st.PublishedHEADOID, st.PublishedHEADRef, st.PublishedGeneration, _ = snap.ReadState(ctx)
			snap.Close()
		}
	}
	st.CurrentHEADOID, st.CurrentHEADRef, st.SnapshotGeneration = st.PublishedHEADOID, st.PublishedHEADRef, st.PublishedGeneration
	if st.ServedGeneration > 0 {
		st.CurrentHEADOID, st.CurrentHEADRef, st.SnapshotGeneration = st.ServedHEADOID, st.ServedHEADRef, st.ServedGeneration
	}
	if cfg.OverlayDBPath != "" {
		if _, statErr := os.Stat(cfg.OverlayDBPath); statErr == nil {
			if db, err := meta.OpenDB(cfg.OverlayDBPath); err == nil {
				var count int64
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries`).Scan(&count); err == nil {
					st.DirtyOverlay = count > 0
				}
				db.Close()
			}
		}
	}
	// A CLI fetch writes FETCH_HEAD and no record, so a FETCH_HEAD newer than
	// the daemon's last fetch is a later successful fetch.
	lastFetch := st.LastFetchAt
	if rec.LastErrorPhase == phaseFetch && rec.LastErrorAt.After(lastFetch) {
		lastFetch = rec.LastErrorAt
	}
	if fi, err := os.Stat(filepath.Join(cfg.GitDir, "FETCH_HEAD")); err == nil && fi.ModTime().After(lastFetch) {
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

// applyHydrationStats reads the cache counters that the hydrator and
// evict-cache keep, without walking the cache.
func applyHydrationStats(st *model.RepoRuntimeState, cacheDir string) {
	stats, _ := blobcache.ReadStats(cacheDir)
	st.HydratedBlobCount = stats.Blobs
	st.HydratedBlobBytes = stats.Bytes
}
