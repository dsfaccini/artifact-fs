package overlay

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// migrations build the overlay schema.
var migrations = []meta.Migration{
	// Migration 1 creates the schema, or brings an older build's database to
	// it.
	func(ctx context.Context, conn *sql.Conn) error {
		if err := meta.SQL(overlayEntriesV1...)(ctx, conn); err != nil {
			return err
		}
		cols, err := meta.Columns(ctx, conn, "overlay_entries")
		if err != nil {
			return err
		}
		if !cols["ctime_unix_ns"] {
			if _, err := conn.ExecContext(ctx, `ALTER TABLE overlay_entries ADD COLUMN ctime_unix_ns INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		if !cols["source_mode"] {
			if _, err := conn.ExecContext(ctx, `ALTER TABLE overlay_entries ADD COLUMN source_mode INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `UPDATE overlay_entries SET source_mode=mode WHERE source_oid<>''`); err != nil {
				return err
			}
		}
		// Every write sets ctime_unix_ns, so a zero comes only from the
		// column's default.
		if _, err := conn.ExecContext(ctx, `UPDATE overlay_entries SET ctime_unix_ns=? WHERE ctime_unix_ns=0`, time.Now().UnixNano()); err != nil {
			return err
		}
		return meta.SQL(slices.Concat(changeLogV1, changeLogTriggers)...)(ctx, conn)
	},
	// Migration 2 records the paths whose writes are not yet committed, so
	// the fsmonitor hook reports them on every query until they are.
	meta.SQL(`CREATE TABLE IF NOT EXISTS overlay_pending (path TEXT PRIMARY KEY)`),
}

var overlayEntriesV1 = []string{
	`CREATE TABLE IF NOT EXISTS overlay_entries (
	  path TEXT PRIMARY KEY,
	  kind TEXT NOT NULL,
	  backing_path TEXT,
	  mode INTEGER NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  mtime_unix_ns INTEGER NOT NULL,
	  ctime_unix_ns INTEGER NOT NULL,
	  source_oid TEXT,
	  source_mode INTEGER NOT NULL DEFAULT 0,
	  target_path TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_overlay_kind ON overlay_entries(kind);`,
}

var changeLogV1 = []string{
	// overlay_changes keeps one row per path ever changed, renumbered on each
	// change. AUTOINCREMENT never reuses a sequence number, so the FSMonitor
	// hook can answer "changed since sequence N" without scanning the overlay.
	`CREATE TABLE IF NOT EXISTS overlay_changes (
	  seq INTEGER PRIMARY KEY AUTOINCREMENT,
	  path TEXT NOT NULL UNIQUE
	);`,
	// A recreated database gets a new id, so a token that names another
	// database's sequence never matches.
	`CREATE TABLE IF NOT EXISTS overlay_instance (id TEXT NOT NULL);`,
	`INSERT INTO overlay_instance(id) SELECT lower(hex(randomblob(16))) WHERE NOT EXISTS (SELECT 1 FROM overlay_instance);`,
}

// changeLogTriggers feed overlay_changes from every write to overlay_entries.
// Migration 1 and resumeChangeLog create them from this list. To change a
// trigger, change it here and append a migration that runs DROP TRIGGER IF
// EXISTS before it creates the trigger again: CREATE TRIGGER IF NOT EXISTS
// keeps an existing trigger's body.
var changeLogTriggers = []string{
	`CREATE TRIGGER IF NOT EXISTS overlay_entries_insert_change AFTER INSERT ON overlay_entries BEGIN
	  DELETE FROM overlay_changes WHERE path = NEW.path;
	  INSERT INTO overlay_changes(path) VALUES (NEW.path);
	END;`,
	`CREATE TRIGGER IF NOT EXISTS overlay_entries_update_change AFTER UPDATE ON overlay_entries BEGIN
	  DELETE FROM overlay_changes WHERE path IN (OLD.path, NEW.path);
	  INSERT INTO overlay_changes(path) SELECT OLD.path UNION SELECT NEW.path;
	END;`,
	`CREATE TRIGGER IF NOT EXISTS overlay_entries_delete_change AFTER DELETE ON overlay_entries BEGIN
	  DELETE FROM overlay_changes WHERE path = OLD.path;
	  INSERT INTO overlay_changes(path) VALUES (OLD.path);
	END;`,
}

var changeLogTriggerNames = []string{"overlay_entries_insert_change", "overlay_entries_update_change", "overlay_entries_delete_change"}

var _ model.OverlayStore = (*Store)(nil)

type Store struct {
	db       *sql.DB
	repo     model.RepoPaths
	upperDir string
	mu       sync.Mutex
	// pending holds the files written since their last commit. It changes
	// only under mu; pendingMu also guards it for readers that do not take mu.
	// A commit of pending values happens before the map forgets them, so a
	// reader that reads the map before the entries sees current values.
	pending   map[string]*pendingWrite
	pendingMu sync.Mutex
	// reconcileMu serializes reconciles without blocking writes.
	reconcileMu sync.Mutex
}

func New(ctx context.Context, cfg model.RepoPaths) (*Store, error) {
	db, err := meta.OpenDB(cfg.OverlayDBPath)
	if err != nil {
		return nil, err
	}
	if err := meta.Migrate(ctx, db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", cfg.OverlayDBPath, err)
	}
	upperDir := filepath.Join(cfg.OverlayDir, "upper")
	if err := os.MkdirAll(upperDir, 0o700); err != nil {
		db.Close()
		return nil, err
	}
	if err := commitPendingFromBackingFiles(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", cfg.OverlayDBPath, err)
	}
	return &Store{db: db, repo: cfg, upperDir: upperDir, pending: map[string]*pendingWrite{}}, nil
}

// commitPendingFromBackingFiles commits the size and mtime of each path a
// process left with uncommitted writes, from its backing file.
func commitPendingFromBackingFiles(ctx context.Context, db *sql.DB) error {
	// Read outside the write transaction: SQLite fails a read transaction's
	// upgrade to a write at once, without the busy handler.
	rows, err := db.QueryContext(ctx, `SELECT e.path, e.backing_path FROM overlay_pending p JOIN overlay_entries e USING(path)`)
	if err != nil {
		return err
	}
	type pendingRow struct{ path, backingPath string }
	var pending []pendingRow
	for rows.Next() {
		var r pendingRow
		var backing sql.NullString
		if err := rows.Scan(&r.path, &backing); err != nil {
			rows.Close()
			return err
		}
		r.backingPath = backing.String
		pending = append(pending, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range pending {
		if st, err := os.Stat(r.backingPath); err == nil {
			t := st.ModTime().UnixNano()
			if _, err := tx.ExecContext(ctx, `UPDATE overlay_entries SET size_bytes=?, mtime_unix_ns=?, ctime_unix_ns=? WHERE path=? AND backing_path=?`, st.Size(), t, t, r.path, r.backingPath); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_pending WHERE path=?`, r.path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	s.mu.Lock()
	flushErr := s.flushAllLocked(context.Background())
	s.mu.Unlock()
	return errors.Join(flushErr, s.db.Close())
}

// overlayCols is the column list for overlay_entries queries. Keep in sync with
// the Scan call in queryEntries and the single-row scan in Lookup.
const overlayCols = `path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path`

func (s *Store) Lookup(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	path = model.CleanPath(path)
	s.pendingMu.Lock()
	p, pending := s.pending[path]
	var values pendingWrite
	if pending {
		values = *p
	}
	s.pendingMu.Unlock()
	e, ok, err := s.lookupCommitted(ctx, path)
	if ok && pending {
		values.apply(&e)
	}
	return e, ok, err
}

// pendingValues copies the pending writes for a read that applies them to
// entries it selects afterwards.
func (s *Store) pendingValues() map[string]pendingWrite {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	values := make(map[string]pendingWrite, len(s.pending))
	for path, p := range s.pending {
		values[path] = *p
	}
	return values
}

func (s *Store) lookupCommitted(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+overlayCols+` FROM overlay_entries WHERE path=?`, path)
	var e model.OverlayEntry
	if err := row.Scan(&e.Path, &e.Kind, &e.BackingPath, &e.Mode, &e.SizeBytes, &e.MtimeUnixNs, &e.CtimeUnixNs, &e.SourceOID, &e.SourceMode, &e.TargetPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.OverlayEntry{}, false, nil
		}
		return model.OverlayEntry{}, false, err
	}
	e.RepoID = s.repo.ID
	return e, true, nil
}

func (s *Store) EnsureCopyOnWrite(ctx context.Context, repo model.RepoPaths, path string, base model.BaseNode) (model.OverlayEntry, error) {
	var src *os.File
	if base.ObjectOID != "" {
		var err error
		src, err = os.Open(blobcache.Path(repo.BlobCacheDir, base.ObjectOID))
		if err != nil {
			return model.OverlayEntry{}, err
		}
		defer src.Close()
	}
	return s.EnsureCopyOnWriteFrom(ctx, repo, path, base, src)
}

func (s *Store) EnsureCopyOnWriteFrom(ctx context.Context, _ model.RepoPaths, path string, base model.BaseNode, src *os.File) (model.OverlayEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return model.OverlayEntry{}, err
	}
	if e, ok, err := s.Lookup(ctx, path); err != nil {
		return model.OverlayEntry{}, err
	} else if ok && !e.IsDeleted() {
		return e, nil
	}
	if base.Type == "symlink" {
		if src == nil {
			return model.OverlayEntry{}, errors.New("symlink blob is not hydrated")
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return model.OverlayEntry{}, err
		}
		target, err := io.ReadAll(io.LimitReader(src, model.MaxSymlinkTargetBytes+1))
		if err != nil {
			return model.OverlayEntry{}, err
		}
		if len(target) > model.MaxSymlinkTargetBytes {
			return model.OverlayEntry{}, fmt.Errorf("symlink target exceeds %d bytes", model.MaxSymlinkTargetBytes)
		}
		now := time.Now().UnixNano()
		e := model.OverlayEntry{
			RepoID:      s.repo.ID,
			Path:        model.CleanPath(path),
			Kind:        model.OverlayKindSymlink,
			Mode:        base.Mode,
			SizeBytes:   int64(len(target)),
			MtimeUnixNs: now,
			CtimeUnixNs: now,
			SourceOID:   base.ObjectOID,
			SourceMode:  base.Mode,
			TargetPath:  string(target),
		}
		if err := s.upsertEntry(ctx, e); err != nil {
			return model.OverlayEntry{}, err
		}
		return e, nil
	}
	tmp, err := os.CreateTemp(s.upperDir, ".artifact-fs-entry-*")
	if err != nil {
		return model.OverlayEntry{}, err
	}
	backing := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	if src != nil {
		if err := copyFileFrom(src, backing, backingFileMode(base.Mode)); err != nil {
			os.Remove(backing)
			return model.OverlayEntry{}, fmt.Errorf("copy-on-write %s: %w", path, err)
		}
	}
	if err := os.Chmod(backing, backingFileMode(base.Mode)); err != nil {
		os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	st, err := os.Stat(backing)
	if err != nil {
		return model.OverlayEntry{}, err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{
		RepoID:      s.repo.ID,
		Path:        model.CleanPath(path),
		Kind:        model.OverlayKindModify,
		BackingPath: backing,
		Mode:        base.Mode,
		SizeBytes:   st.Size(),
		MtimeUnixNs: now,
		CtimeUnixNs: now,
		SourceOID:   base.ObjectOID,
		SourceMode:  base.Mode,
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	return e, nil
}

func (s *Store) CreateFile(ctx context.Context, path string, mode uint32) (model.OverlayEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return model.OverlayEntry{}, err
	}
	f, err := os.CreateTemp(s.upperDir, ".artifact-fs-entry-*")
	if err != nil {
		return model.OverlayEntry{}, err
	}
	backing := f.Name()
	if err := f.Chmod(backingFileMode(mode)); err != nil {
		f.Close()
		os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: model.CleanPath(path), Kind: model.OverlayKindCreate, BackingPath: backing, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	// The create logs the path and marks it pending, so the writes that
	// usually follow commit nothing until the flush.
	if err := s.commitPending(ctx, e); err != nil {
		f.Close()
		os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	s.pendingMu.Lock()
	s.pending[e.Path] = &pendingWrite{f: f, backingPath: backing, timeUnixNs: now}
	s.pendingMu.Unlock()
	return e, nil
}

func (s *Store) CreateSymlink(ctx context.Context, path string, target string) (model.OverlayEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return model.OverlayEntry{}, err
	}
	if len(target) > model.MaxSymlinkTargetBytes {
		return model.OverlayEntry{}, fmt.Errorf("symlink target exceeds %d bytes", model.MaxSymlinkTargetBytes)
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{
		RepoID:      s.repo.ID,
		Path:        model.CleanPath(path),
		Kind:        model.OverlayKindSymlink,
		Mode:        0o120000,
		SizeBytes:   int64(len(target)),
		MtimeUnixNs: now,
		CtimeUnixNs: now,
		TargetPath:  target,
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		return model.OverlayEntry{}, err
	}
	return e, nil
}

// pendingWrite is a file whose content reached its backing file while its
// size and times are newer than its committed entry.
type pendingWrite struct {
	f           *os.File
	backingPath string
	size        int64
	timeUnixNs  int64
	// written says a write changed size or times after the last commit.
	written bool
}

// apply sets e's size and times to p's when e still names p's backing file.
// Other fields stay as committed, so a concurrent mode change or rebase holds.
func (p pendingWrite) apply(e *model.OverlayEntry) {
	if e.BackingPath == p.backingPath {
		e.SizeBytes = p.size
		e.MtimeUnixNs = p.timeUnixNs
		e.CtimeUnixNs = p.timeUnixNs
	}
}

// WriteFile writes through the open backing file. The first write since the
// path's last flush commits the entry and marks it pending; later writes
// change only the in-memory size and times, which Lookup reports at once.
// FlushFile, SyncFile and every other mutation commit them.
func (s *Store) WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	s.pendingMu.Lock()
	p := s.pending[path]
	s.pendingMu.Unlock()
	if p != nil {
		n, err := p.f.WriteAt(data, off)
		if n > 0 {
			s.pendingMu.Lock()
			p.size = max(p.size, off+int64(n))
			p.timeUnixNs = time.Now().UnixNano()
			p.written = true
			s.pendingMu.Unlock()
		}
		return n, err
	}
	e, ok, err := s.lookupCommitted(ctx, path)
	if err != nil {
		return 0, err
	}
	if !ok || e.IsDeleted() {
		return 0, os.ErrNotExist
	}
	var f *os.File
	err = retryWithOwnerAccess(e.BackingPath, e.Mode, func() error {
		var openErr error
		f, openErr = os.OpenFile(e.BackingPath, os.O_WRONLY|os.O_CREATE, backingFileMode(e.Mode))
		return openErr
	})
	if err != nil {
		return 0, err
	}
	n, err := f.WriteAt(data, off)
	if n == 0 {
		f.Close()
		return n, err
	}
	p = &pendingWrite{f: f, backingPath: e.BackingPath, size: max(e.SizeBytes, off+int64(n)), timeUnixNs: time.Now().UnixNano()}
	p.apply(&e)
	if e.Kind != model.OverlayKindCreate {
		e.Kind = model.OverlayKindModify
	}
	if upErr := s.commitPending(ctx, e); upErr != nil {
		f.Close()
		return n, upErr
	}
	s.pendingMu.Lock()
	s.pending[path] = p
	s.pendingMu.Unlock()
	return n, err
}

// commitPending commits e and marks its path pending, so the fsmonitor hook
// reports the path until a flush commits its later writes.
func (s *Store) commitPending(ctx context.Context, e model.OverlayEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertEntryOn(ctx, tx, e); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO overlay_pending(path) VALUES (?)`, e.Path); err != nil {
		return err
	}
	return tx.Commit()
}

// FlushFile commits the size and times of path's writes since its last flush.
func (s *Store) FlushFile(ctx context.Context, path string) error {
	path = model.CleanPath(path)
	// Every close(2) flushes, so a path with nothing to commit skips mu.
	s.pendingMu.Lock()
	_, ok := s.pending[path]
	s.pendingMu.Unlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked(ctx, []string{path})
}

func (s *Store) flushAllLocked(ctx context.Context) error {
	s.pendingMu.Lock()
	paths := slices.Collect(maps.Keys(s.pending))
	s.pendingMu.Unlock()
	return s.flushLocked(ctx, paths)
}

// flushLocked commits the pending size and times of paths in one
// transaction, then forgets them.
func (s *Store) flushLocked(ctx context.Context, paths []string) error {
	values := s.pendingValues()
	var flushed []string
	for _, path := range paths {
		if _, ok := values[path]; ok {
			flushed = append(flushed, path)
		}
	}
	if len(flushed) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range flushed {
		p := values[path]
		if !p.written {
			// The entry already holds the size and times; an update would log
			// a change that did not happen.
			if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_pending WHERE path=?`, path); err != nil {
				return err
			}
			continue
		}
		if err := commitSizeAndTimes(ctx, tx, path, p.backingPath, p.size, p.timeUnixNs, p.timeUnixNs); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var closeErrs []error
	s.pendingMu.Lock()
	for _, path := range flushed {
		closeErrs = append(closeErrs, s.pending[path].f.Close())
		delete(s.pending, path)
	}
	s.pendingMu.Unlock()
	return errors.Join(closeErrs...)
}

func (s *Store) SyncFile(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	e, ok, err := s.Lookup(ctx, path)
	if err != nil {
		return err
	}
	if !ok || e.IsDeleted() || e.BackingPath == "" {
		return os.ErrNotExist
	}
	f, err := os.OpenFile(e.BackingPath, os.O_RDWR, 0)
	if err != nil {
		f, err = os.Open(e.BackingPath)
		if err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.commitSizeAndTimesDurable(ctx, e); err != nil {
		return err
	}
	s.pendingMu.Lock()
	p := s.pending[path]
	delete(s.pending, path)
	s.pendingMu.Unlock()
	if p != nil {
		if err := p.f.Close(); err != nil {
			return err
		}
	}
	dir, err := os.Open(s.upperDir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}

// GrantOwnerAccess lets path's backing file open for writing, as a write
// through the overlay does: when an earlier version left it read-only, it
// grants the owner read and write. A file that opens for writing stays as it
// is.
func (s *Store) GrantOwnerAccess(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok, err := s.lookupCommitted(ctx, model.CleanPath(path))
	if err != nil || !ok || e.IsDeleted() || e.BackingPath == "" {
		return err
	}
	return retryWithOwnerAccess(e.BackingPath, e.Mode, func() error {
		f, err := os.OpenFile(e.BackingPath, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return f.Close()
	})
}

func (s *Store) Truncate(ctx context.Context, path string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	e, ok, err := s.Lookup(ctx, path)
	if err != nil {
		return err
	}
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	if err := retryWithOwnerAccess(e.BackingPath, e.Mode, func() error { return os.Truncate(e.BackingPath, size) }); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	e.SizeBytes = size
	e.MtimeUnixNs = now
	e.CtimeUnixNs = now
	if e.Kind != model.OverlayKindCreate {
		e.Kind = model.OverlayKindModify
	}
	return s.upsertEntry(ctx, e)
}

func (s *Store) Remove(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	path = model.CleanPath(path)
	existing, _, err := s.Lookup(ctx, path)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: path, Kind: model.OverlayKindDelete, Mode: 0, MtimeUnixNs: now, CtimeUnixNs: now}
	if err := s.upsertEntry(ctx, e); err != nil {
		return err
	}
	if existing.BackingPath != "" {
		_ = os.Remove(existing.BackingPath)
	}
	return nil
}

// Rename moves the overlay entry at oldPath to newPath. sourceTracked reports
// whether the snapshot has oldPath. A tracked file or symlink source leaves a
// delete marker so the snapshot entry stays hidden.
func (s *Store) Rename(ctx context.Context, oldPath, newPath string, sourceTracked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e, ok, err := s.Lookup(ctx, oldPath)
	if err != nil {
		return err
	}
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	if oldPath == newPath {
		return nil
	}
	if e.Kind == model.OverlayKindMkdir {
		return s.renameTreeLocked(ctx, oldPath, newPath, nil, nil)
	}
	newBacking := e.BackingPath
	newKind := model.OverlayKindRename
	newTargetPath := oldPath
	whiteoutTargetPath := oldPath
	newSourceOID := e.SourceOID
	newSourceMode := e.SourceMode
	writeSourceWhiteout := true
	if e.Kind == model.OverlayKindRename && e.TargetPath != "" {
		newTargetPath = e.TargetPath
		// ReconcileChecked drops a whiteout that names another path, so only an
		// untracked intermediate path of a rename chain may name the original.
		if !sourceTracked {
			whiteoutTargetPath = e.TargetPath
		}
	}
	mode := e.Mode
	switch e.Kind {
	case model.OverlayKindCreate:
		newKind = e.Kind
		newTargetPath = ""
		writeSourceWhiteout = sourceTracked
	case model.OverlayKindSymlink:
		newKind = e.Kind
		newTargetPath = e.TargetPath
		writeSourceWhiteout = sourceTracked || e.SourceOID != ""
		newSourceOID = ""
		newSourceMode = 0
	}
	var replaced model.OverlayEntry
	if dst, exists, err := s.Lookup(ctx, newPath); err != nil {
		return err
	} else if exists {
		replaced = dst
	}
	// Backing files have stable identities independent of their visible path, so
	// the database transaction is the complete rename operation.
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=?`, oldPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=?`, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES(?,?,?,?,?,?,?,?,?,?)`, newPath, newKind, newBacking, mode, e.SizeBytes, e.MtimeUnixNs, now, newSourceOID, newSourceMode, newTargetPath); err != nil {
		return err
	}
	if writeSourceWhiteout {
		if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET kind='delete',mode=0,source_oid='',source_mode=0,mtime_unix_ns=excluded.mtime_unix_ns,ctime_unix_ns=excluded.ctime_unix_ns,target_path=excluded.target_path`, oldPath, model.OverlayKindDelete, "", 0, 0, now, now, "", 0, whiteoutTargetPath); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if replaced.BackingPath != "" && replaced.BackingPath != newBacking {
		_ = os.RemoveAll(replaced.BackingPath)
	}
	return nil
}

func (s *Store) RenameTree(ctx context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	return s.renameTreeLocked(ctx, model.CleanPath(oldPath), model.CleanPath(newPath), sourceBasePaths, destinationBasePaths)
}

func (s *Store) renameTreeLocked(ctx context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error {
	if oldPath == newPath {
		e, ok, err := s.Lookup(ctx, oldPath)
		if err != nil {
			return err
		}
		if !ok || e.IsDeleted() {
			return os.ErrNotExist
		}
		return nil
	}
	if oldPath == "." || newPath == "." || strings.HasPrefix(newPath, oldPath+"/") {
		return iofs.ErrInvalid
	}

	sourceEntries, err := s.listTreeEntries(ctx, oldPath)
	if err != nil {
		return err
	}
	var root model.OverlayEntry
	rootFound := false
	for _, e := range sourceEntries {
		if e.Path == oldPath && !e.IsDeleted() {
			root = e
			rootFound = true
			break
		}
	}
	if !rootFound {
		return os.ErrNotExist
	}
	if root.Kind != model.OverlayKindMkdir {
		return iofs.ErrInvalid
	}

	destinationEntries, err := s.listTreeEntries(ctx, newPath)
	if err != nil {
		return err
	}
	for _, e := range destinationEntries {
		if e.IsDeleted() {
			continue
		}
		if e.Path == newPath && e.Kind == model.OverlayKindMkdir {
			continue
		}
		if e.Path == newPath {
			return iofs.ErrInvalid
		}
		return os.ErrExist
	}
	tracked := make(map[string]bool, len(sourceBasePaths))
	for _, path := range sourceBasePaths {
		path = model.CleanPath(path)
		if path == oldPath || strings.HasPrefix(path, oldPath+"/") {
			tracked[path] = true
		}
	}
	replacedBasePaths := make(map[string]bool, len(destinationBasePaths))
	for _, path := range destinationBasePaths {
		path = model.CleanPath(path)
		if strings.HasPrefix(path, newPath+"/") {
			replacedBasePaths[path] = true
		}
	}

	now := time.Now().UnixNano()
	moved := make([]model.OverlayEntry, 0, len(sourceEntries))
	movedBackings := make(map[string]bool, len(sourceEntries))
	movedPaths := make(map[string]bool, len(sourceEntries))
	for _, e := range sourceEntries {
		if e.IsDeleted() {
			continue
		}
		sourcePath := e.Path
		e.Path = newPath + strings.TrimPrefix(e.Path, oldPath)
		e.CtimeUnixNs = now
		if tracked[sourcePath] {
			switch e.Kind {
			case model.OverlayKindModify:
				e.Kind = model.OverlayKindRename
				if e.TargetPath == "" {
					e.TargetPath = sourcePath
				}
			case model.OverlayKindSymlink:
				e.SourceOID = ""
				e.SourceMode = 0
			}
		}
		if e.Kind == model.OverlayKindMkdir {
			if mode, normalized := normalizeGitDirMode(e.Mode); normalized {
				e.Mode = mode
				if e.BackingPath != "" {
					if err := os.Chmod(e.BackingPath, os.FileMode(mode)); err != nil {
						return err
					}
				}
			}
		}
		if e.BackingPath != "" {
			movedBackings[e.BackingPath] = true
		}
		movedPaths[e.Path] = true
		moved = append(moved, e)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := suspendChangeLog(ctx, tx); err != nil {
		return err
	}
	deleteTree := func(path string) error {
		prefix := path + "/"
		_, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=? OR substr(path, 1, length(?))=?`, path, prefix, prefix)
		return err
	}
	if err := deleteTree(newPath); err != nil {
		return err
	}
	if err := deleteTree(oldPath); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, e := range moved {
		if _, err := insert.ExecContext(ctx, e.Path, e.Kind, e.BackingPath, e.Mode, e.SizeBytes, e.MtimeUnixNs, e.CtimeUnixNs, e.SourceOID, e.SourceMode, e.TargetPath); err != nil {
			return err
		}
	}
	for path := range tracked {
		if _, err := insert.ExecContext(ctx, path, model.OverlayKindDelete, "", 0, 0, now, now, "", 0, path); err != nil {
			return err
		}
	}
	for path := range replacedBasePaths {
		if movedPaths[path] {
			continue
		}
		if _, err := insert.ExecContext(ctx, path, model.OverlayKindDelete, "", 0, 0, now, now, "", 0, path); err != nil {
			return err
		}
	}
	if err := resumeChangeLog(ctx, tx, treeRenameChanges(oldPath, newPath, sourceEntries, destinationEntries, moved, tracked, replacedBasePaths)...); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	for _, e := range destinationEntries {
		if e.BackingPath != "" && !movedBackings[e.BackingPath] {
			_ = os.RemoveAll(e.BackingPath)
		}
	}
	return nil
}

// treeRenameChanges returns the paths that tell Git what a tree rename
// changed. Git reads a changed path as a change to the listing of its
// directory, and a path with no index entry of its own, such as a directory,
// as a change to every index entry below it. Both roots and one removed or
// written path per directory therefore stand for every entry.
func treeRenameChanges(oldPath, newPath string, removed, destination, moved []model.OverlayEntry, whiteouts, replaced map[string]bool) []string {
	perDir := map[string]string{}
	add := func(path string) {
		dir := path[:max(strings.LastIndexByte(path, '/'), 0)]
		if logged, ok := perDir[dir]; !ok || path < logged {
			perDir[dir] = path
		}
	}
	for _, entries := range [][]model.OverlayEntry{removed, destination, moved} {
		for _, e := range entries {
			add(e.Path)
		}
	}
	for _, paths := range []map[string]bool{whiteouts, replaced} {
		for path := range paths {
			add(path)
		}
	}
	changes := []string{oldPath, newPath}
	for _, path := range perDir {
		changes = append(changes, path)
	}
	return changes
}

func (s *Store) listTreeEntries(ctx context.Context, path string) ([]model.OverlayEntry, error) {
	prefix := path + "/"
	return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries WHERE path=? OR substr(path, 1, length(?))=? ORDER BY path`, path, prefix, prefix)
}

func (s *Store) RenameAndMarkModifiedFromBase(ctx context.Context, oldPath, newPath string, sourceOID string, sourceMode uint32, sourceTracked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e, ok, err := s.Lookup(ctx, oldPath)
	if err != nil {
		return err
	}
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	newKind := model.OverlayKindModify
	if e.Kind == model.OverlayKindSymlink {
		newKind = model.OverlayKindSymlink
	}
	if oldPath == newPath {
		targetPath := ""
		if newKind == model.OverlayKindSymlink {
			targetPath = e.TargetPath
		}
		_, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET kind=?, source_oid=?, source_mode=?, target_path=? WHERE path=?`, newKind, sourceOID, sourceMode, targetPath, oldPath)
		return err
	}
	newBacking := e.BackingPath
	var replaced model.OverlayEntry
	if dst, exists, err := s.Lookup(ctx, newPath); err != nil {
		return err
	} else if exists {
		replaced = dst
	}
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=?`, oldPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=?`, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES(?,?,?,?,?,?,?,?,?,?)`, newPath, newKind, newBacking, e.Mode, e.SizeBytes, e.MtimeUnixNs, now, sourceOID, sourceMode, e.TargetPath); err != nil {
		return err
	}
	if sourceTracked || (e.Kind == model.OverlayKindSymlink && e.SourceOID != "") {
		if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES(?,?,?,?,?,?,?,?,?,?)`, oldPath, model.OverlayKindDelete, "", 0, 0, now, now, "", 0, ""); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if replaced.BackingPath != "" && replaced.BackingPath != newBacking {
		_ = os.RemoveAll(replaced.BackingPath)
	}
	return nil
}

func (s *Store) Mkdir(ctx context.Context, path string, mode uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	path = model.CleanPath(path)
	mode, _ = normalizeGitDirMode(mode)
	backing, err := os.MkdirTemp(s.upperDir, ".artifact-fs-dir-*")
	if err != nil {
		return err
	}
	if err := os.Chmod(backing, os.FileMode(mode)); err != nil {
		os.Remove(backing)
		return err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: path, Kind: model.OverlayKindMkdir, BackingPath: backing, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	if err := s.upsertEntry(ctx, e); err != nil {
		os.Remove(backing)
		return err
	}
	return nil
}

func normalizeGitDirMode(mode uint32) (uint32, bool) {
	if mode&0o170000 == 0o40000 && mode&0o777 == 0 {
		return 0o755, true
	}
	return mode, false
}

func (s *Store) SetMtime(ctx context.Context, path string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	path = model.CleanPath(path)
	if e, ok, err := s.Lookup(ctx, path); err != nil {
		return err
	} else if ok && e.Kind == model.OverlayKindMkdir {
		if mode, normalized := normalizeGitDirMode(e.Mode); normalized {
			if err := os.Chmod(e.BackingPath, os.FileMode(mode)); err != nil {
				return err
			}
			_, err := s.db.ExecContext(ctx,
				`UPDATE overlay_entries SET mode=?, mtime_unix_ns=?, ctime_unix_ns=? WHERE path=?`,
				mode, t.UnixNano(), time.Now().UnixNano(), path)
			return err
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE overlay_entries SET mtime_unix_ns=?, ctime_unix_ns=? WHERE path=?`,
		t.UnixNano(), time.Now().UnixNano(), path)
	return err
}

func (s *Store) SetMode(ctx context.Context, path string, mode uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return err
	}
	path = model.CleanPath(path)
	e, ok, err := s.Lookup(ctx, path)
	if err != nil {
		return err
	}
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	e.Mode = e.Mode&^0o777 | mode&0o777
	if e.BackingPath != "" {
		perm := backingFileMode(e.Mode)
		if e.Kind == model.OverlayKindMkdir {
			perm = os.FileMode(e.Mode)
		}
		if err := os.Chmod(e.BackingPath, perm); err != nil {
			return err
		}
	}
	e.CtimeUnixNs = time.Now().UnixNano()
	return s.upsertEntry(ctx, e)
}

// ReconcileChecked prunes overlay entries that are stale relative to the new
// base snapshot. Called after a generation change (commit, branch switch, fetch).
//
// Source OID and mode identify the base version an overlay entry was derived
// from. When either changes, matching overlay content was committed and can be
// removed; divergent content is kept and rebased onto the new source metadata.
// Creates, symlinks, deletes, and directories are removed once the new base
// independently represents their pending operation.
//
// It runs in two phases, so that readers keep working while it compares the
// overlay with the new base. The plan phase reads every entry, looks up its base and hashes its content
// with no lock that a reader or an overlay write takes. The apply phase holds
// lock, which excludes the readers of the overlay and the base. It takes the
// database write lock, classifies again every entry the change log shows a
// write changed since the plan read it, and applies the result. Then it calls
// switchBase to switch the readers to the base baseLookup reads, and releases
// lock. Either may be nil.
func (s *Store) ReconcileChecked(ctx context.Context, baseLookup func(path string) (model.BaseNode, bool, error), lock sync.Locker, switchBase func()) error {
	// A reconcile's own changes reach the change log only in part, so one
	// reconcile must not plan while another applies.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	r := &reconciler{lookup: baseLookup, bases: map[string]reconcileBase{}, matches: map[model.OverlayEntry]bool{}}
	// A flush after the sequence read would log the flushed paths as written
	// during the reconcile.
	s.mu.Lock()
	err := s.flushAllLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// Read the sequence before the entries: a write between the two reads
	// is then classified again in the apply phase.
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM overlay_changes`).Scan(&seq); err != nil {
		return fmt.Errorf("reconcile change log: %w", err)
	}
	entries, err := s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries ORDER BY path`)
	if err != nil {
		return fmt.Errorf("reconcile list: %w", err)
	}
	plan, err := r.classify(entries, false)
	if err != nil {
		return err
	}
	if lock != nil {
		lock.Lock()
	}
	removed, err := s.applyReconcile(ctx, r, seq, entries, plan)
	if err == nil && switchBase != nil {
		switchBase()
	}
	if lock != nil {
		lock.Unlock()
	}
	// Delete backing files only after the transaction commits. If we deleted
	// before commit and the transaction rolled back, DB rows would reference
	// non-existent files. No committed row names them, so readers need not
	// wait for the deletes. Reverse order so children are removed before
	// parents (os.Remove fails on non-empty directories).
	for _, v := range slices.Backward(removed) {
		if v.BackingPath != "" {
			_ = os.Remove(v.BackingPath)
		}
	}
	return err
}

// applyReconcile applies plan and returns the entries it removed.
func (s *Store) applyReconcile(ctx context.Context, r *reconciler, seq int64, entries []model.OverlayEntry, plan reconcilePlan) ([]model.OverlayEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushAllLocked(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Removing an entry the new base matches, rebasing an entry, and removing
	// a delete marker for a path the new base lacks leave the merged view as
	// it was. Only a removed delete marker over a base path is a change for Git.
	// Suspending the log takes the write lock first, so no write lands between
	// reading the log and applying the result.
	if err := suspendChangeLog(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, changesSinceQuery, seq)
	if err != nil {
		return nil, err
	}
	changed, err := scanPaths(rows)
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 || plan.incomplete {
		if entries, err = s.rereadChanged(ctx, tx, entries, changed); err != nil {
			return nil, err
		}
		if plan, err = r.classify(entries, true); err != nil {
			return nil, err
		}
	}
	if len(plan.remove) == 0 && len(plan.update) == 0 {
		return nil, nil
	}
	var revealed []string
	// Guard the mutations so that a change the log does not show, such as
	// another Store's reconcile, is not lost.
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM overlay_entries WHERE path=? AND kind=? AND source_oid=? AND source_mode=? AND mode=? AND mtime_unix_ns=? AND ctime_unix_ns=?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	deleted := make([]model.OverlayEntry, 0, len(plan.remove))
	for _, e := range plan.remove {
		res, err := stmt.ExecContext(ctx, e.Path, e.Kind, e.SourceOID, e.SourceMode, e.Mode, e.MtimeUnixNs, e.CtimeUnixNs)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			deleted = append(deleted, e)
			if e.Kind == model.OverlayKindDelete && plan.revealsBase[e.Path] {
				revealed = append(revealed, e.Path)
			}
		}
	}
	updateStmt, err := tx.PrepareContext(ctx, `UPDATE overlay_entries SET kind=?, source_oid=?, source_mode=?, target_path=? WHERE path=? AND kind=? AND source_oid=? AND source_mode=? AND mode=? AND mtime_unix_ns=? AND ctime_unix_ns=?`)
	if err != nil {
		return nil, err
	}
	defer updateStmt.Close()
	for _, update := range plan.update {
		if _, err := updateStmt.ExecContext(ctx, update.after.Kind, update.after.SourceOID, update.after.SourceMode, update.after.TargetPath, update.before.Path, update.before.Kind, update.before.SourceOID, update.before.SourceMode, update.before.Mode, update.before.MtimeUnixNs, update.before.CtimeUnixNs); err != nil {
			return nil, err
		}
	}
	if err := resumeChangeLog(ctx, tx, revealed...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

// rereadChanged replaces the entries at each changed path and at every path
// in [path, path+"0"), which holds the paths below it: a tree rename logs only
// both roots and one path per directory.
func (s *Store) rereadChanged(ctx context.Context, tx *sql.Tx, entries []model.OverlayEntry, changed []string) ([]model.OverlayEntry, error) {
	byPath := make(map[string]model.OverlayEntry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	comparePath := func(e model.OverlayEntry, path string) int { return strings.Compare(e.Path, path) }
	for _, path := range changed {
		end := path + "0"
		lo, _ := slices.BinarySearchFunc(entries, path, comparePath)
		hi, _ := slices.BinarySearchFunc(entries, end, comparePath)
		for _, e := range entries[lo:hi] {
			delete(byPath, e.Path)
		}
		pending := s.pendingValues()
		rows, err := tx.QueryContext(ctx, `SELECT `+overlayCols+` FROM overlay_entries WHERE path >= ? AND path < ?`, path, end)
		if err != nil {
			return nil, err
		}
		current, err := s.scanEntries(rows, pending)
		if err != nil {
			return nil, err
		}
		for _, e := range current {
			byPath[e.Path] = e
		}
	}
	return slices.SortedFunc(maps.Values(byPath), func(a, b model.OverlayEntry) int { return strings.Compare(a.Path, b.Path) }), nil
}

type reconcileUpdate struct {
	before model.OverlayEntry
	after  model.OverlayEntry
}

// reconcilePlan is what a reconcile changes in the overlay.
type reconcilePlan struct {
	remove []model.OverlayEntry
	update []reconcileUpdate
	// revealsBase holds the delete markers in remove whose path the new base has.
	revealsBase map[string]bool
	// incomplete is set when an entry's backing file vanished before the plan
	// hashed it, so the plan left that entry out.
	incomplete bool
}

type reconcileBase struct {
	node   model.BaseNode
	exists bool
}

// reconciler classifies overlay entries against one base. It caches each base
// lookup by path, and each hash by the entry it read, so classifying again
// under the caller's lock looks up and hashes only what changed.
type reconciler struct {
	lookup  func(path string) (model.BaseNode, bool, error)
	bases   map[string]reconcileBase
	matches map[model.OverlayEntry]bool
}

func (r *reconciler) base(path string) (model.BaseNode, bool, error) {
	if b, ok := r.bases[path]; ok {
		return b.node, b.exists, nil
	}
	n, ok, err := r.lookup(path)
	if err != nil {
		return model.BaseNode{}, false, err
	}
	r.bases[path] = reconcileBase{node: n, exists: ok}
	return n, ok, nil
}

// matchesBase reports whether e's content is base's. Unless final, known is
// false when e's backing file is gone: a write removes a backing file only
// after it changed the entry, so the apply phase classifies the new entry.
func (r *reconciler) matchesBase(e model.OverlayEntry, base model.BaseNode, final bool) (matches, known bool, err error) {
	if m, ok := r.matches[e]; ok {
		return m, true, nil
	}
	m, err := overlayEntryMatchesBase(e, base)
	if err != nil {
		if !final && errors.Is(err, iofs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("reconcile hash %s: %w", e.Path, err)
	}
	r.matches[e] = m
	return m, true, nil
}

// classify plans the reconcile of entries, which are sorted by path.
func (r *reconciler) classify(entries []model.OverlayEntry, final bool) (reconcilePlan, error) {
	p := reconcilePlan{revealsBase: map[string]bool{}}
	staleRenameSources := map[string]bool{}
	var deletes []model.OverlayEntry
	for _, e := range entries {
		base, baseExists, err := r.base(e.Path)
		if err != nil {
			return reconcilePlan{}, err
		}
		switch e.Kind {
		case model.OverlayKindModify:
			if !baseExists {
				before := e
				e.Kind = model.OverlayKindCreate
				e.SourceOID = ""
				e.SourceMode = 0
				p.update = append(p.update, reconcileUpdate{before: before, after: e})
			} else if e.SourceOID != base.ObjectOID || e.SourceMode != base.Mode {
				matches, known, err := r.matchesBase(e, base, final)
				if err != nil {
					return reconcilePlan{}, err
				}
				if !known {
					p.incomplete = true
				} else if matches {
					p.remove = append(p.remove, e)
				} else {
					before := e
					e.SourceOID = base.ObjectOID
					e.SourceMode = base.Mode
					p.update = append(p.update, reconcileUpdate{before: before, after: e})
				}
			}
		case model.OverlayKindRename:
			sourcePath := e.TargetPath
			if sourcePath == "" {
				sourcePath = e.Path
			}
			sourceBase, sourceExists, err := r.base(sourcePath)
			if err != nil {
				return reconcilePlan{}, err
			}
			if baseExists && !sourceExists {
				matches, known, err := r.matchesBase(e, base, final)
				if err != nil {
					return reconcilePlan{}, err
				}
				if !known {
					p.incomplete = true
					continue
				}
				if matches {
					p.remove = append(p.remove, e)
					staleRenameSources[e.TargetPath] = true
					continue
				}
			}
			if sourceExists {
				if e.SourceOID != sourceBase.ObjectOID || e.SourceMode != sourceBase.Mode {
					before := e
					e.SourceOID = sourceBase.ObjectOID
					e.SourceMode = sourceBase.Mode
					p.update = append(p.update, reconcileUpdate{before: before, after: e})
				}
			} else {
				before := e
				if baseExists {
					e.Kind = model.OverlayKindModify
					e.SourceOID = base.ObjectOID
					e.SourceMode = base.Mode
				} else {
					e.Kind = model.OverlayKindCreate
					e.SourceOID = ""
					e.SourceMode = 0
				}
				e.TargetPath = ""
				p.update = append(p.update, reconcileUpdate{before: before, after: e})
				staleRenameSources[sourcePath] = true
			}
		case model.OverlayKindDelete:
			// A delete marker depends on every rename's source.
			deletes = append(deletes, e)
		case model.OverlayKindCreate:
			if baseExists {
				matches, known, err := r.matchesBase(e, base, final)
				if err != nil {
					return reconcilePlan{}, err
				}
				if !known {
					p.incomplete = true
				} else if matches {
					p.remove = append(p.remove, e)
				} else {
					before := e
					e.Kind = model.OverlayKindModify
					e.SourceOID = base.ObjectOID
					e.SourceMode = base.Mode
					p.update = append(p.update, reconcileUpdate{before: before, after: e})
				}
			}
		case model.OverlayKindMkdir:
			if baseExists && base.Type == "dir" {
				p.remove = append(p.remove, e)
			}
		case model.OverlayKindSymlink:
			if baseExists && base.Type == "symlink" && (e.SourceOID != base.ObjectOID || e.SourceMode != base.Mode) {
				matches, known, err := r.matchesBase(e, base, final)
				if err != nil {
					return reconcilePlan{}, err
				}
				if !known {
					p.incomplete = true
				} else if matches {
					p.remove = append(p.remove, e)
				} else {
					before := e
					e.SourceOID = base.ObjectOID
					e.SourceMode = base.Mode
					p.update = append(p.update, reconcileUpdate{before: before, after: e})
				}
			}
		}
	}
	for _, e := range deletes {
		_, baseExists, err := r.base(e.Path)
		if err != nil {
			return reconcilePlan{}, err
		}
		if (e.TargetPath != "" && e.Path != e.TargetPath) || staleRenameSources[e.Path] || staleRenameSources[e.TargetPath] || !baseExists {
			p.remove = append(p.remove, e)
			p.revealsBase[e.Path] = baseExists
		}
	}
	return p, nil
}

func overlayEntryMatchesBase(e model.OverlayEntry, base model.BaseNode) (bool, error) {
	if base.Mode != 0 && e.Mode&0o777 != base.Mode&0o777 {
		return false, nil
	}
	if e.Kind == model.OverlayKindSymlink {
		return model.ReaderMatchesBlobOID(strings.NewReader(e.TargetPath), int64(len(e.TargetPath)), base.ObjectOID)
	}
	return model.FileMatchesBlobOID(e.BackingPath, base.ObjectOID)
}

func (s *Store) DirtyCount(ctx context.Context) (int64, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind <> ?`, model.OverlayKindDelete)
	var c int64
	err := row.Scan(&c)
	return c, err
}

// suspendChangeLog drops the triggers for the rest of tx, so a bulk operation
// pays no per-row trigger cost. SQLite runs a trigger's program for each row
// even when a WHEN clause skips its body. Other connections keep seeing the
// triggers, because the drop commits only together with resumeChangeLog.
func suspendChangeLog(ctx context.Context, tx *sql.Tx) error {
	for _, name := range changeLogTriggerNames {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name); err != nil {
			return err
		}
	}
	return nil
}

// resumeChangeLog recreates the triggers and logs paths in place of the
// changes made while they were dropped.
func resumeChangeLog(ctx context.Context, tx *sql.Tx, paths ...string) error {
	for _, stmt := range changeLogTriggers {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// REPLACE moves a path that is already logged to a new sequence number.
	// Each path is bound as is: a name need not be valid UTF-8, which a JSON
	// list would replace.
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO overlay_changes(path) VALUES (?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, path := range paths {
		if _, err := stmt.ExecContext(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// changesSinceQuery sorts with a unary + so SQLite searches the seq range and
// sorts only the delta, instead of walking the whole path index in order.
const changesSinceQuery = `SELECT path FROM overlay_changes WHERE seq > ? ORDER BY +path`

// hookChangesQuery also returns every path with uncommitted writes, whose
// later writes log no change until a flush.
const hookChangesQuery = `SELECT path FROM overlay_changes WHERE seq > ? UNION SELECT path FROM overlay_pending ORDER BY 1`

// Changes is one consistent read of an overlay database's change log.
type Changes struct {
	// Instance identifies the database.
	Instance string
	// Seq is the latest change sequence.
	Seq int64
	// Paths changed after the sequence the caller asked about, sorted.
	Paths []string
}

// ReadChangesSince reads the change log of an existing overlay database
// without the migrations that New runs, so it never takes the write lock. ok is false when the database, its change log or a trigger that
// feeds the log is missing.
func ReadChangesSince(ctx context.Context, cfg model.RepoPaths, since int64) (changes Changes, ok bool, err error) {
	if _, err := os.Stat(cfg.OverlayDBPath); errors.Is(err, os.ErrNotExist) {
		return Changes{}, false, nil
	} else if err != nil {
		return Changes{}, false, err
	}
	db, err := meta.OpenDB(cfg.OverlayDBPath)
	if err != nil {
		return Changes{}, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Changes{}, false, err
	}
	defer tx.Rollback()
	// Without every trigger, the log misses writes.
	var objects int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE (type = 'table' AND name IN ('overlay_changes', 'overlay_instance', 'overlay_pending')) OR (type = 'trigger' AND name IN (?, ?, ?))`, changeLogTriggerNames[0], changeLogTriggerNames[1], changeLogTriggerNames[2]).Scan(&objects); err != nil {
		return Changes{}, false, err
	}
	if objects != 3+len(changeLogTriggerNames) {
		return Changes{}, false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM overlay_instance`).Scan(&changes.Instance); err != nil {
		return Changes{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM overlay_changes`).Scan(&changes.Seq); err != nil {
		return Changes{}, false, err
	}
	rows, err := tx.QueryContext(ctx, hookChangesQuery, since)
	if err != nil {
		return Changes{}, false, err
	}
	if changes.Paths, err = scanPaths(rows); err != nil {
		return Changes{}, false, err
	}
	return changes, true, nil
}

func scanPaths(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (s *Store) ListAll(ctx context.Context) ([]model.OverlayEntry, error) {
	return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries ORDER BY path`)
}

// listByPrefixQuery selects paths in [prefix/, prefix0): '0' follows '/', so
// the range holds exactly the paths below prefix and searches the primary key.
const listByPrefixQuery = `SELECT ` + overlayCols + ` FROM overlay_entries WHERE path >= ? AND path < ? ORDER BY path`

// ListByPrefix returns every overlay entry below the given directory path.
func (s *Store) ListByPrefix(ctx context.Context, prefix string) ([]model.OverlayEntry, error) {
	prefix = model.CleanPath(prefix)
	if prefix == "." {
		return s.ListAll(ctx)
	}
	return s.queryEntries(ctx, listByPrefixQuery, prefix+"/", prefix+"0")
}

// HasEntryBelow reports whether an entry of any kind lies below path. It
// reads at most one row of the path index.
func (s *Store) HasEntryBelow(ctx context.Context, path string) (bool, error) {
	path = model.CleanPath(path)
	query, args := `SELECT 1 FROM overlay_entries LIMIT 1`, []any{}
	if path != "." {
		query, args = `SELECT 1 FROM overlay_entries WHERE path >= ? AND path < ? LIMIT 1`, []any{path + "/", path + "0"}
	}
	var one int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// queryEntries runs an arbitrary overlay query and scans the results.
func (s *Store) queryEntries(ctx context.Context, query string, args ...any) ([]model.OverlayEntry, error) {
	pending := s.pendingValues()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return s.scanEntries(rows, pending)
}

// scanEntries applies pending, which the caller read before the query.
func (s *Store) scanEntries(rows *sql.Rows, pending map[string]pendingWrite) ([]model.OverlayEntry, error) {
	defer rows.Close()
	var out []model.OverlayEntry
	for rows.Next() {
		var e model.OverlayEntry
		if err := rows.Scan(&e.Path, &e.Kind, &e.BackingPath, &e.Mode, &e.SizeBytes, &e.MtimeUnixNs, &e.CtimeUnixNs, &e.SourceOID, &e.SourceMode, &e.TargetPath); err != nil {
			return nil, err
		}
		e.RepoID = s.repo.ID
		if p, ok := pending[e.Path]; ok {
			p.apply(&e)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) upsertEntry(ctx context.Context, e model.OverlayEntry) error {
	return upsertEntryOn(ctx, s.db, e)
}

// commitSizeAndTimesDurable commits e's size and times at synchronous=FULL,
// which syncs the WAL and with it every earlier commit. OpenDB's
// synchronous=NORMAL syncs the WAL only at checkpoints. On macOS fsync()
// leaves writes in the drive's cache; fullfsync makes SQLite sync with
// F_FULLFSYNC, as os.File.Sync does. SQLite ignores fullfsync elsewhere.
func (s *Store) commitSizeAndTimesDurable(ctx context.Context, e model.OverlayEntry) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = execEach(ctx, conn, `PRAGMA synchronous=FULL`, `PRAGMA fullfsync=ON`)
	if err == nil {
		err = commitSizeAndTimesOn(ctx, conn, e)
	}
	if resetErr := execEach(context.Background(), conn, `PRAGMA fullfsync=OFF`, `PRAGMA synchronous=NORMAL`); resetErr != nil {
		// A pooled connection left at FULL would sync every later commit.
		conn.Raw(func(any) error { return driver.ErrBadConn })
		err = errors.Join(err, resetErr)
	}
	return err
}

func commitSizeAndTimesOn(ctx context.Context, conn *sql.Conn, e model.OverlayEntry) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := commitSizeAndTimes(ctx, tx, e.Path, e.BackingPath, e.SizeBytes, e.MtimeUnixNs, e.CtimeUnixNs); err != nil {
		return err
	}
	return tx.Commit()
}

func execEach(ctx context.Context, conn *sql.Conn, stmts ...string) error {
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// commitSizeAndTimes commits path's size and times while its entry still
// names backingPath, and clears its pending marker. It leaves the entry's
// other fields as another writer committed them.
func commitSizeAndTimes(ctx context.Context, tx *sql.Tx, path, backingPath string, size, mtimeUnixNs, ctimeUnixNs int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE overlay_entries SET size_bytes=?, mtime_unix_ns=?, ctime_unix_ns=? WHERE path=? AND backing_path=?`, size, mtimeUnixNs, ctimeUnixNs, path, backingPath); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM overlay_pending WHERE path=?`, path)
	return err
}

func upsertEntryOn(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, e model.OverlayEntry) error {
	if e.Path == "" {
		return errors.New("empty path")
	}
	_, err := db.ExecContext(ctx, `
	INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path)
	VALUES(?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(path) DO UPDATE SET
	kind=excluded.kind,
	backing_path=excluded.backing_path,
	mode=excluded.mode,
	size_bytes=excluded.size_bytes,
	mtime_unix_ns=excluded.mtime_unix_ns,
	ctime_unix_ns=excluded.ctime_unix_ns,
	source_oid=excluded.source_oid,
	source_mode=excluded.source_mode,
	target_path=excluded.target_path`, e.Path, e.Kind, e.BackingPath, e.Mode, e.SizeBytes, e.MtimeUnixNs, e.CtimeUnixNs, e.SourceOID, e.SourceMode, e.TargetPath)
	return err
}

// copyFileContents copies src into dst, truncating dst first. Returns
// os.ErrNotExist if src doesn't exist so the caller can decide whether that's
// fatal. Other errors (permissions, I/O) are returned as-is.
func copyFileContents(src string, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return copyFileFrom(f, dst, mode)
}

// backingFileMode is the permission of a file's backing file. The entry holds
// the mode the mount reports; the backing file stays readable and writable by
// its owner, so the store can reopen it for a descriptor that may write, such
// as one open(2) returned with O_CREAT and a read-only mode.
func backingFileMode(mode uint32) os.FileMode {
	return os.FileMode(mode).Perm() | 0o600
}

// retryWithOwnerAccess runs op on the backing file at path. When the file
// refuses op for permission, as one an earlier version left read-only does,
// it grants the owner read and write and runs op again.
func retryWithOwnerAccess(path string, mode uint32, op func() error) error {
	err := op()
	if !errors.Is(err, iofs.ErrPermission) {
		return err
	}
	if chmodErr := os.Chmod(path, backingFileMode(mode)); chmodErr != nil {
		return errors.Join(err, chmodErr)
	}
	return op()
}

func copyFileFrom(src *os.File, dst string, mode os.FileMode) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tf, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(tf, src); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	return tf.Close()
}

func (s *Store) String() string {
	return fmt.Sprintf("overlay[%s]", s.repo.Name)
}
