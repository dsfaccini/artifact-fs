package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var ErrRepoChanged = errors.New("repo config changed")

// migrations build the repos table. Add a column with a new migration, then
// list it in AddRepo, its upsert, the SELECTs and scanRepo.
var migrations = []meta.Migration{
	// Migration 1 creates reposTableV1, or brings an older build's table to
	// it.
	func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, reposTableV1); err != nil {
			return err
		}
		cols, err := meta.Columns(ctx, conn, "repos")
		if err != nil {
			return err
		}
		for _, col := range reposColumnsAddedBeforeV1 {
			if cols[col.name] {
				continue
			}
			if _, err := conn.ExecContext(ctx, `ALTER TABLE repos ADD COLUMN `+col.name+` `+col.ddl); err != nil {
				return err
			}
		}
		return nil
	},
	// Nothing reads or writes remote_url_secret_ref.
	meta.SQL(`ALTER TABLE repos DROP COLUMN remote_url_secret_ref`),
	// The daemon records what it observes in repo_runtime, which replaces
	// mount_error. mount_request carries the CLI's unmount and remount intents.
	// mount_error stays, unused: a daemon of an earlier build, still running
	// when a newer command migrates the database, keeps reading it. A later
	// migration drops it.
	meta.SQL(
		`CREATE TABLE repo_runtime (
		  repo_id TEXT PRIMARY KEY,
		  state TEXT NOT NULL DEFAULT '',
		  phase TEXT NOT NULL DEFAULT '',
		  last_error TEXT NOT NULL DEFAULT '',
		  last_error_phase TEXT NOT NULL DEFAULT '',
		  last_error_at_ns INTEGER NOT NULL DEFAULT 0,
		  next_retry_at_ns INTEGER NOT NULL DEFAULT 0,
		  attempts INTEGER NOT NULL DEFAULT 0,
		  served_generation INTEGER NOT NULL DEFAULT 0,
		  served_commit TEXT NOT NULL DEFAULT '',
		  served_ref TEXT NOT NULL DEFAULT '',
		  last_fetch_at_ns INTEGER NOT NULL DEFAULT 0,
		  last_fetch_result TEXT NOT NULL DEFAULT '',
		  ahead INTEGER NOT NULL DEFAULT 0,
		  behind INTEGER NOT NULL DEFAULT 0,
		  diverged INTEGER NOT NULL DEFAULT 0,
		  mount_request TEXT NOT NULL DEFAULT '',
		  updated_at_ns INTEGER NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE repos ADD COLUMN mount_request TEXT NOT NULL DEFAULT ''`,
	),
}

const reposTableV1 = `CREATE TABLE IF NOT EXISTS repos (
	  repo_id TEXT PRIMARY KEY,
	  name TEXT NOT NULL UNIQUE,
	  mount_root TEXT NOT NULL,
	  mount_path TEXT NOT NULL,
	  remote_url TEXT NOT NULL DEFAULT '',
	  remote_url_redacted TEXT NOT NULL,
	  remote_url_secret_ref TEXT,
	  branch TEXT NOT NULL,
	  refresh_interval_seconds INTEGER NOT NULL,
	  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
	  git_dir TEXT NOT NULL,
	  overlay_dir TEXT NOT NULL,
	  blob_cache_dir TEXT NOT NULL,
	  meta_db_path TEXT NOT NULL,
	  overlay_db_path TEXT NOT NULL,
	  enabled INTEGER NOT NULL DEFAULT 1,
	  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
	  adopt_gitdir INTEGER NOT NULL DEFAULT 0,
	  fetch_ref TEXT NOT NULL DEFAULT '',
	  prepare_state TEXT NOT NULL DEFAULT '',
	  prepare_error TEXT NOT NULL DEFAULT '',
	  mount_error TEXT NOT NULL DEFAULT '',
	  required_commit TEXT NOT NULL DEFAULT '',
	  history_depth INTEGER NOT NULL DEFAULT 0,
	  refresh_disabled INTEGER NOT NULL DEFAULT 0,
	  acquired_ref TEXT NOT NULL DEFAULT '',
	  acquired_commit TEXT NOT NULL DEFAULT '',
	  acquired_at_ns INTEGER NOT NULL DEFAULT 0,
	  config_version TEXT NOT NULL DEFAULT '',
	  created_at_ns INTEGER NOT NULL,
	  updated_at_ns INTEGER NOT NULL
	);`

// reposColumnsAddedBeforeV1 lists the columns older builds added to repos
// after creating it, in the order they appeared.
var reposColumnsAddedBeforeV1 = []struct{ name, ddl string }{
	{"remote_url", `TEXT NOT NULL DEFAULT ''`},
	{"prepared_gitdir", `INTEGER NOT NULL DEFAULT 0`},
	{"fetch_ref", `TEXT NOT NULL DEFAULT ''`},
	{"prepare_state", `TEXT NOT NULL DEFAULT ''`},
	{"prepare_error", `TEXT NOT NULL DEFAULT ''`},
	{"refresh_interval_ns", `INTEGER NOT NULL DEFAULT 0`},
	{"required_commit", `TEXT NOT NULL DEFAULT ''`},
	{"history_depth", `INTEGER NOT NULL DEFAULT 0`},
	{"refresh_disabled", `INTEGER NOT NULL DEFAULT 0`},
	{"acquired_ref", `TEXT NOT NULL DEFAULT ''`},
	{"acquired_commit", `TEXT NOT NULL DEFAULT ''`},
	{"acquired_at_ns", `INTEGER NOT NULL DEFAULT 0`},
	{"config_version", `TEXT NOT NULL DEFAULT ''`},
	{"adopt_gitdir", `INTEGER NOT NULL DEFAULT 0`},
	{"mount_error", `TEXT NOT NULL DEFAULT ''`},
}

type Store struct {
	db *sql.DB
}

func New(ctx context.Context, dbPath string) (*Store, error) {
	db, err := meta.OpenDB(dbPath)
	if err != nil {
		return nil, err
	}
	if err := meta.Migrate(ctx, db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
	now := time.Now().UnixNano()
	var acquiredAtNS int64
	if !cfg.AcquiredAt.IsZero() {
		acquiredAtNS = cfg.AcquiredAt.UnixNano()
	}
	if cfg.ConfigVersion == "" {
		cfg.ConfigVersion = fmt.Sprintf("%d", now)
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, adopt_gitdir, fetch_ref, prepare_state, prepare_error, required_commit, history_depth, refresh_disabled, acquired_ref, acquired_commit, acquired_at_ns, config_version, created_at_ns, updated_at_ns)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(repo_id) DO UPDATE SET
	name=excluded.name,
	mount_root=excluded.mount_root,
	mount_path=excluded.mount_path,
	remote_url=excluded.remote_url,
	remote_url_redacted=excluded.remote_url_redacted,
	branch=excluded.branch,
	refresh_interval_seconds=excluded.refresh_interval_seconds,
	refresh_interval_ns=excluded.refresh_interval_ns,
	git_dir=excluded.git_dir,
	overlay_dir=excluded.overlay_dir,
	blob_cache_dir=excluded.blob_cache_dir,
	meta_db_path=excluded.meta_db_path,
	overlay_db_path=excluded.overlay_db_path,
	enabled=excluded.enabled,
	prepared_gitdir=excluded.prepared_gitdir,
	adopt_gitdir=excluded.adopt_gitdir,
	fetch_ref=excluded.fetch_ref,
	prepare_state=excluded.prepare_state,
	prepare_error=excluded.prepare_error,
	required_commit=excluded.required_commit,
	history_depth=excluded.history_depth,
	refresh_disabled=excluded.refresh_disabled,
	acquired_ref=excluded.acquired_ref,
	acquired_commit=excluded.acquired_commit,
	acquired_at_ns=excluded.acquired_at_ns,
	config_version=excluded.config_version,
	updated_at_ns=excluded.updated_at_ns
	`, string(cfg.ID), cfg.Name, cfg.MountRoot, cfg.MountPath, cfg.RemoteURL, cfg.RemoteURLRedacted, cfg.Branch, int64(cfg.RefreshInterval.Seconds()), int64(cfg.RefreshInterval), cfg.GitDir, cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath, cfg.OverlayDBPath, boolToInt(cfg.Enabled), boolToInt(cfg.PreparedGitDir), boolToInt(cfg.AdoptGitDir), cfg.FetchRef, cfg.PrepareState, cfg.PrepareError, cfg.RequiredCommit, cfg.HistoryDepth, boolToInt(cfg.RemoteRefreshDisabled), cfg.AcquiredRef, cfg.AcquiredCommit, acquiredAtNS, cfg.ConfigVersion, now, now)
	return err
}

func (s *Store) UpdateRefresh(ctx context.Context, repoID model.RepoID, interval time.Duration, disabled bool) error {
	if interval <= 0 && !disabled {
		return errors.New("refresh interval must be positive")
	}
	res, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET refresh_interval_seconds=?, refresh_interval_ns=?, refresh_disabled=?, updated_at_ns=?
	WHERE repo_id=?
	`, int64(interval.Seconds()), int64(interval), boolToInt(disabled), time.Now().UnixNano(), string(repoID))
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("repo not found")
	}
	return nil
}

func (s *Store) UpdatePrepareState(ctx context.Context, repoID model.RepoID, state string, prepareErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE repos
		SET prepare_state=?, prepare_error=?, updated_at_ns=?
		WHERE repo_id=?
	`, state, prepareErr, time.Now().UnixNano(), string(repoID))
	return err
}

// RequestMount records the CLI's unmount (enabled false) or remount (enabled
// true) intent under request, which the daemon echoes in repo_runtime once it
// carried it out.
func (s *Store) RequestMount(ctx context.Context, repoID model.RepoID, enabled bool, request string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE repos SET enabled=?, mount_request=?, updated_at_ns=? WHERE repo_id=?`, boolToInt(enabled), request, time.Now().UnixNano(), string(repoID))
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("repo not found")
	}
	return nil
}

// PutRuntime replaces the daemon's record of rec.RepoID.
func (s *Store) PutRuntime(ctx context.Context, rec model.RepoRuntimeRecord) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT OR REPLACE INTO repo_runtime (repo_id, state, phase, last_error, last_error_phase, last_error_at_ns, next_retry_at_ns, attempts, served_generation, served_commit, served_ref, last_fetch_at_ns, last_fetch_result, ahead, behind, diverged, mount_request, updated_at_ns)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, string(rec.RepoID), rec.State, rec.Phase, rec.LastError, rec.LastErrorPhase, unixNano(rec.LastErrorAt), unixNano(rec.NextRetryAt), rec.Attempts, rec.ServedGeneration, rec.ServedCommit, rec.ServedRef, unixNano(rec.LastFetchAt), rec.LastFetchResult, rec.AheadCount, rec.BehindCount, boolToInt(rec.Diverged), rec.MountRequest, time.Now().UnixNano())
	return err
}

// GetRuntime returns the daemon's record of repoID, and false when it has none.
func (s *Store) GetRuntime(ctx context.Context, repoID model.RepoID) (model.RepoRuntimeRecord, bool, error) {
	var rec model.RepoRuntimeRecord
	var lastErrorAt, nextRetryAt, lastFetchAt, updatedAt int64
	var diverged int
	err := s.db.QueryRowContext(ctx, `SELECT repo_id, state, phase, last_error, last_error_phase, last_error_at_ns, next_retry_at_ns, attempts, served_generation, served_commit, served_ref, last_fetch_at_ns, last_fetch_result, ahead, behind, diverged, mount_request, updated_at_ns FROM repo_runtime WHERE repo_id=?`, string(repoID)).
		Scan(&rec.RepoID, &rec.State, &rec.Phase, &rec.LastError, &rec.LastErrorPhase, &lastErrorAt, &nextRetryAt, &rec.Attempts, &rec.ServedGeneration, &rec.ServedCommit, &rec.ServedRef, &lastFetchAt, &rec.LastFetchResult, &rec.AheadCount, &rec.BehindCount, &diverged, &rec.MountRequest, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RepoRuntimeRecord{}, false, nil
	}
	if err != nil {
		return model.RepoRuntimeRecord{}, false, err
	}
	rec.LastErrorAt = fromUnixNano(lastErrorAt)
	rec.NextRetryAt = fromUnixNano(nextRetryAt)
	rec.LastFetchAt = fromUnixNano(lastFetchAt)
	rec.UpdatedAt = fromUnixNano(updatedAt)
	rec.Diverged = diverged == 1
	return rec, true, nil
}

// DeleteRuntime removes the daemon's record of repoID.
func (s *Store) DeleteRuntime(ctx context.Context, repoID model.RepoID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repo_runtime WHERE repo_id=?`, string(repoID))
	return err
}

// ClearRuntime removes every daemon record. A starting daemon serves nothing
// yet, so a record a stopped daemon left behind is stale.
func (s *Store) ClearRuntime(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repo_runtime`)
	return err
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (s *Store) BeginPrepare(ctx context.Context, cfg model.RepoConfig, state string, configVersion string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE repos
		SET prepare_state=?, prepare_error='', config_version=?, updated_at_ns=?
		WHERE repo_id=? AND config_version=?
	`, state, configVersion, time.Now().UnixNano(), string(cfg.ID), cfg.ConfigVersion)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrRepoChanged
	}
	return nil
}

func (s *Store) UpdatePrepareStateForConfig(ctx context.Context, cfg model.RepoConfig, state string, prepareErr string) error {
	res, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET prepare_state=?, prepare_error=?, updated_at_ns=?
	WHERE repo_id=?
	  AND branch=?
	  AND remote_url=?
	  AND prepared_gitdir=?
	  AND adopt_gitdir=?
	  AND fetch_ref=?
	  AND git_dir=?
	  AND overlay_dir=?
	  AND blob_cache_dir=?
	  AND meta_db_path=?
	  AND overlay_db_path=?
	  AND mount_path=?
	  AND required_commit=?
	  AND history_depth=?
	  AND refresh_disabled=?
	  AND config_version=?
	`, state, prepareErr, time.Now().UnixNano(), string(cfg.ID), cfg.Branch, cfg.RemoteURL, boolToInt(cfg.PreparedGitDir), boolToInt(cfg.AdoptGitDir), cfg.FetchRef, cfg.GitDir, cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath, cfg.OverlayDBPath, cfg.MountPath, cfg.RequiredCommit, cfg.HistoryDepth, boolToInt(cfg.RemoteRefreshDisabled), cfg.ConfigVersion)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrRepoChanged
	}
	return nil
}

func (s *Store) RecordAcquisition(ctx context.Context, cfg model.RepoConfig, source model.PreparedSource) error {
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET acquired_ref=?, acquired_commit=?, acquired_at_ns=?, updated_at_ns=?
	WHERE repo_id=?
	  AND branch=?
	  AND remote_url_redacted=?
	  AND git_dir=?
	  AND required_commit=?
	  AND history_depth=?
	  AND refresh_disabled=?
	  AND config_version=?
	`, source.Ref, source.Commit, now, now, string(cfg.ID), cfg.Branch, cfg.RemoteURLRedacted, cfg.GitDir, cfg.RequiredCommit, cfg.HistoryDepth, boolToInt(cfg.RemoteRefreshDisabled), cfg.ConfigVersion)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrRepoChanged
	}
	return nil
}

func (s *Store) RemoveRepo(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE name=?`, name)
	return err
}

func (s *Store) GetRepo(ctx context.Context, name string) (model.RepoConfig, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, adopt_gitdir, fetch_ref, prepare_state, prepare_error, required_commit, history_depth, refresh_disabled, acquired_ref, acquired_commit, acquired_at_ns, config_version, mount_request FROM repos WHERE name=?`, name)
	return scanRepo(row)
}

func (s *Store) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, adopt_gitdir, fetch_ref, prepare_state, prepare_error, required_commit, history_depth, refresh_disabled, acquired_ref, acquired_commit, acquired_at_ns, config_version, mount_request FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.RepoConfig, 0)
	for rows.Next() {
		cfg, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRepo(s scanner) (model.RepoConfig, error) {
	var cfg model.RepoConfig
	var refresh int64
	var refreshNS int64
	var enabled int
	var preparedGitDir int
	var adoptGitDir int
	var refreshDisabled int
	var acquiredAtNS int64
	if err := s.Scan(&cfg.ID, &cfg.Name, &cfg.MountRoot, &cfg.MountPath, &cfg.RemoteURL, &cfg.RemoteURLRedacted, &cfg.Branch, &refresh, &refreshNS, &cfg.GitDir, &cfg.OverlayDir, &cfg.BlobCacheDir, &cfg.MetaDBPath, &cfg.OverlayDBPath, &enabled, &preparedGitDir, &adoptGitDir, &cfg.FetchRef, &cfg.PrepareState, &cfg.PrepareError, &cfg.RequiredCommit, &cfg.HistoryDepth, &refreshDisabled, &cfg.AcquiredRef, &cfg.AcquiredCommit, &acquiredAtNS, &cfg.ConfigVersion, &cfg.MountRequest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return cfg, fmt.Errorf("repo not found")
		}
		return cfg, err
	}
	if refreshNS > 0 {
		cfg.RefreshInterval = time.Duration(refreshNS)
	} else if refresh > 0 {
		cfg.RefreshInterval = time.Duration(refresh) * time.Second
	} else {
		cfg.RefreshInterval = 30 * time.Second
	}
	cfg.Enabled = enabled == 1
	cfg.PreparedGitDir = preparedGitDir == 1
	cfg.AdoptGitDir = adoptGitDir == 1
	cfg.RemoteRefreshDisabled = refreshDisabled == 1
	if acquiredAtNS > 0 {
		cfg.AcquiredAt = time.Unix(0, acquiredAtNS)
	}
	return cfg, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
