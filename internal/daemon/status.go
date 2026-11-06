package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		st.State = model.PrepareStatePreparing
	case live && rec.State != "":
		st.State = rec.State
	case isPendingOrFailedPrepareState(cfg.PrepareState):
		st.State = cfg.PrepareState
	}
	// The last failure stays visible after the daemon stops, but nothing
	// retries it.
	st.Phase, st.LastError, st.LastErrorPhase, st.LastErrorAt, st.Attempts = rec.Phase, rec.LastError, rec.LastErrorPhase, rec.LastErrorAt, rec.Attempts
	if live {
		st.NextRetryAt = rec.NextRetryAt
		st.ServedHEADOID, st.ServedHEADRef, st.ServedGeneration = rec.ServedCommit, rec.ServedRef, rec.ServedGeneration
		st.AheadCount, st.BehindCount, st.Diverged = rec.AheadCount, rec.BehindCount, rec.Diverged
	}
	if rec.LastErrorPhase == phaseMount {
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
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind <> 'delete'`).Scan(&count); err == nil {
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

func applyHydrationStats(st *model.RepoRuntimeState, cacheDir string) {
	count, bytes := blobCacheStats(cacheDir)
	st.HydratedBlobCount = count
	st.HydratedBlobBytes = bytes
}

func blobCacheStats(cacheDir string) (int64, int64) {
	if strings.TrimSpace(cacheDir) == "" {
		return 0, 0
	}
	var count int64
	var bytes int64
	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if !isObjectOIDName(d.Name()) {
			return nil
		}
		count++
		bytes += info.Size()
		return nil
	})
	return count, bytes
}

func isObjectOIDName(name string) bool {
	if len(name) != 40 && len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
