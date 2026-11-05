package overlay

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/metatest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const (
	overlayEntriesTable = `CREATE TABLE IF NOT EXISTS overlay_entries (
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
	);`
	overlayKindIndex = `CREATE INDEX IF NOT EXISTS idx_overlay_kind ON overlay_entries(kind);`
	overlayRow       = `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, source_mode, target_path) VALUES ('a.txt', 'modify', '/upper/a.txt', 33188, 5, 7, 9, 'oid', 33188, '')`
)

var legacyRow = model.OverlayEntry{RepoID: "test", Path: "a.txt", Kind: model.OverlayKindModify, BackingPath: "/upper/a.txt", Mode: 33188, SizeBytes: 5, MtimeUnixNs: 7, CtimeUnixNs: 9, SourceOID: "oid", SourceMode: 33188}

// overlayWithChangeLog is main's schema at 25971f7, with overlayRow logged.
var overlayWithChangeLog = []string{overlayEntriesTable, overlayKindIndex,
	`CREATE TABLE IF NOT EXISTS overlay_changes (
		  seq INTEGER PRIMARY KEY AUTOINCREMENT,
		  path TEXT NOT NULL UNIQUE
		);`,
	`CREATE TABLE IF NOT EXISTS overlay_instance (id TEXT NOT NULL);`,
	`INSERT INTO overlay_instance(id) VALUES ('instance')`,
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
	overlayRow,
}

// legacyOverlays are databases that earlier builds created, named for the
// commit that created them or for the user_version a migrated one records.
// Keep each fixture as it is, and add one at the new user_version with each
// migration. The fixture at the current user_version fails when an edit to a
// shipped migration changes the schema a new database gets.
var legacyOverlays = map[string][]string{
	// The first schema: no ctime_unix_ns, source_mode or change log.
	"01dd4de": {
		`CREATE TABLE IF NOT EXISTS overlay_entries (
		  path TEXT PRIMARY KEY,
		  kind TEXT NOT NULL,
		  backing_path TEXT,
		  mode INTEGER NOT NULL,
		  size_bytes INTEGER NOT NULL DEFAULT 0,
		  mtime_unix_ns INTEGER NOT NULL,
		  source_oid TEXT,
		  target_path TEXT
		);`,
		overlayKindIndex,
		`INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, source_oid, target_path) VALUES ('a.txt', 'modify', '/upper/a.txt', 33188, 5, 7, 'oid', '')`,
	},
	// No change log.
	"26ccac8": {overlayEntriesTable, overlayKindIndex, overlayRow},
	// main before migrations.
	"25971f7": overlayWithChangeLog,
	// This schema, after migration 1.
	"user_version 1": append(slices.Clone(overlayWithChangeLog), `PRAGMA user_version = 1`),
}

func testConfig(t *testing.T) model.RepoConfig {
	t.Helper()
	dir := t.TempDir()
	return model.RepoConfig{ID: "test", OverlayDir: filepath.Join(dir, "overlay"), OverlayDBPath: filepath.Join(dir, "overlay", "meta.sqlite")}
}

func openOverlay(cfg model.RepoConfig) func() (io.Closer, error) {
	return func() (io.Closer, error) { return New(context.Background(), cfg) }
}

func TestNewMigratesEarlierSchemas(t *testing.T) {
	_, fresh := testStore(t)
	current := metatest.Schema(t, fresh.OverlayDBPath)
	for name, stmts := range legacyOverlays {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			metatest.Exec(t, cfg.OverlayDBPath, stmts...)
			s, err := New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := current
			if name == "01dd4de" {
				// ALTER TABLE ADD COLUMN needs a default for a NOT NULL
				// column. Every write sets ctime_unix_ns.
				want = slices.Clone(current)
				i := slices.Index(want, "table overlay_entries column ctime_unix_ns INTEGER notnull=1 default=NULL pk=0")
				want[i] = "table overlay_entries column ctime_unix_ns INTEGER notnull=1 default=0 pk=0"
			}
			if got := metatest.Schema(t, cfg.OverlayDBPath); !slices.Equal(got, want) {
				t.Fatalf("schema = %q, want %q", got, want)
			}
			if got := metatest.UserVersion(t, cfg.OverlayDBPath); got != len(migrations) {
				t.Fatalf("user_version = %d, want %d", got, len(migrations))
			}
			got, ok, err := s.Lookup(ctx, "a.txt")
			if err != nil || !ok {
				t.Fatalf("Lookup = %v, %v", ok, err)
			}
			wantRow := legacyRow
			if name == "01dd4de" {
				// The migration fills the columns the first schema lacks.
				if got.CtimeUnixNs == 0 {
					t.Fatal("ctime of an entry from before ctime_unix_ns = 0, want backfilled")
				}
				wantRow.CtimeUnixNs = got.CtimeUnixNs
			}
			if got != wantRow {
				t.Fatalf("a.txt = %+v, want %+v", got, wantRow)
			}
			changes := changesSince(t, cfg, 0)
			if (name == "25971f7" || name == "user_version 1") && (changes.Instance != "instance" || !slices.Equal(changes.Paths, []string{"a.txt"})) {
				t.Fatalf("change log = %+v, want instance's log of a.txt", changes)
			}
		})
	}
}

// A migrated database needs no migration on a later open, which therefore
// changes no entry and logs no change.
func TestNewLeavesAMigratedOverlayUnchanged(t *testing.T) {
	ctx := context.Background()
	s, cfg := testStore(t)
	if _, err := s.CreateFile(ctx, "a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET ctime_unix_ns=0 WHERE path='a.txt'`); err != nil {
		t.Fatal(err)
	}
	before := changesSince(t, cfg, 0)
	s.Close()

	reopened, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, _, err := reopened.Lookup(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.CtimeUnixNs != 0 {
		t.Fatalf("ctime after reopening = %d, want the 0 the database held", got.CtimeUnixNs)
	}
	if after := changesSince(t, cfg, 0); after.Seq != before.Seq {
		t.Fatalf("change sequence after reopening = %d, want %d", after.Seq, before.Seq)
	}
}

func TestNewWaitsOutAnotherConnectionsWriteLock(t *testing.T) {
	cfg := testConfig(t)
	metatest.Exec(t, cfg.OverlayDBPath, legacyOverlays["26ccac8"]...)
	metatest.OpenWhileLocked(t, cfg.OverlayDBPath, openOverlay(cfg))
}

func TestConcurrentFirstOpensApplyEachMigrationOnce(t *testing.T) {
	_, fresh := testStore(t)
	want := metatest.Schema(t, fresh.OverlayDBPath)
	for range 20 {
		cfg := testConfig(t)
		metatest.OpenConcurrently(t, 16, openOverlay(cfg))
		if got := metatest.Schema(t, cfg.OverlayDBPath); !slices.Equal(got, want) {
			t.Fatalf("schema = %q, want %q", got, want)
		}
		if got := metatest.UserVersion(t, cfg.OverlayDBPath); got != len(migrations) {
			t.Fatalf("user_version = %d, want %d", got, len(migrations))
		}
	}
}

func TestNewRefusesANewerSchema(t *testing.T) {
	_, cfg := testStore(t)
	metatest.Exec(t, cfg.OverlayDBPath, `PRAGMA user_version = 99`)
	s, err := New(context.Background(), cfg)
	if err == nil {
		s.Close()
		t.Fatal("New on a newer schema succeeded")
	}
	metatest.RequireNewerSchemaError(t, err, cfg.OverlayDBPath)
}
