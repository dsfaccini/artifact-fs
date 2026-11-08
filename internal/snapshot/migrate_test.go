package snapshot

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
	repoStateTable = `CREATE TABLE IF NOT EXISTS repo_state (
	  key TEXT PRIMARY KEY,
	  value TEXT NOT NULL
	);`
	baseNodesTable = `CREATE TABLE IF NOT EXISTS base_nodes (
	  generation INTEGER NOT NULL,
	  path TEXT NOT NULL,
	  parent_path TEXT NOT NULL DEFAULT '',
	  type TEXT NOT NULL,
	  mode INTEGER NOT NULL,
	  object_oid TEXT NOT NULL,
	  size_state TEXT NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  PRIMARY KEY (generation, path)
	);`
	parentIndex  = `CREATE INDEX IF NOT EXISTS idx_base_nodes_gen_parent ON base_nodes(generation, parent_path);`
	oidIndex     = `CREATE INDEX IF NOT EXISTS idx_base_nodes_gen_oid ON base_nodes(generation, object_oid);`
	snapshotRows = `INSERT INTO repo_state(key, value) VALUES ('current_generation', '1'), ('head_oid', 'head1'), ('head_ref', 'main');
	INSERT INTO base_nodes(generation, path, parent_path, type, mode, object_oid, size_state, size_bytes) VALUES (1, 'README.md', '.', 'file', 420, 'abc', 'known', 42);`
)

// legacySnapshots are databases that earlier builds created, named for the
// commit that created them or for the user_version a migrated one records.
// Keep each fixture as it is, and add one at the new user_version with each
// migration. The fixture at the current user_version fails when an edit to a
// shipped migration changes the schema a new database gets.
var legacySnapshots = map[string][]string{
	// The first schema: learned_path_stats and blob_cache_index, and no
	// idx_base_nodes_gen_oid.
	"01dd4de": {repoStateTable, baseNodesTable, parentIndex,
		`CREATE TABLE IF NOT EXISTS learned_path_stats (
		  path TEXT PRIMARY KEY,
		  access_count INTEGER NOT NULL DEFAULT 0,
		  last_access_ns INTEGER NOT NULL DEFAULT 0,
		  last_hydrated_ns INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS blob_cache_index (
		  object_oid TEXT PRIMARY KEY,
		  cache_path TEXT NOT NULL,
		  size_bytes INTEGER NOT NULL DEFAULT 0,
		  last_access_ns INTEGER NOT NULL DEFAULT 0
		);`,
		`INSERT INTO learned_path_stats(path, access_count) VALUES ('README.md', 3)`,
		snapshotRows,
	},
	// No idx_base_nodes_gen_oid.
	"43ec712": {repoStateTable, baseNodesTable, parentIndex, snapshotRows},
	// main before migrations.
	"25971f7": {repoStateTable, baseNodesTable, parentIndex, oidIndex, snapshotRows},
	// This schema, after migration 1.
	"user_version 1": {repoStateTable, baseNodesTable, parentIndex, oidIndex, snapshotRows, `PRAGMA user_version = 1`},
}

func openSnapshot(path string) func() (io.Closer, error) {
	return func() (io.Closer, error) { return New(context.Background(), path) }
}

func TestNewMigratesEarlierSchemas(t *testing.T) {
	want := metatest.Schema(t, createSnapshot(t))
	for name, stmts := range legacySnapshots {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "snap.sqlite")
			metatest.Exec(t, path, stmts...)
			s, err := New(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got := metatest.Schema(t, path); !slices.Equal(got, want) {
				t.Fatalf("schema = %q, want %q", got, want)
			}
			if got := metatest.UserVersion(t, path); got != len(migrations) {
				t.Fatalf("user_version = %d, want %d", got, len(migrations))
			}
			headOID, headRef, gen, err := s.ReadState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if headOID != "head1" || headRef != "main" || gen != 1 {
				t.Fatalf("state = %q, %q, %d; want head1, main, 1", headOID, headRef, gen)
			}
			node, ok, err := s.LookupNode(ctx, 1, "README.md")
			if err != nil {
				t.Fatal(err)
			}
			want := model.BaseNode{Path: "README.md", Type: "file", Mode: 420, ObjectOID: "abc", SizeState: "known", SizeBytes: 42}
			if !ok || node != want {
				t.Fatalf("README.md = %+v, %v; want %+v", node, ok, want)
			}
		})
	}
}

func TestNewWaitsOutAnotherConnectionsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	metatest.Exec(t, path, legacySnapshots["01dd4de"]...)
	metatest.OpenWhileLocked(t, path, openSnapshot(path))
}

func TestConcurrentFirstOpensBuildTheSchema(t *testing.T) {
	want := metatest.Schema(t, createSnapshot(t))
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	metatest.OpenConcurrently(t, 16, openSnapshot(path))
	if got := metatest.Schema(t, path); !slices.Equal(got, want) {
		t.Fatalf("schema = %q, want %q", got, want)
	}
	if got := metatest.UserVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version = %d, want %d", got, len(migrations))
	}
}

// createSnapshot creates a snapshot database at a new path and returns the
// path.
func createSnapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	s, err := New(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return path
}

func TestNewRefusesANewerSchema(t *testing.T) {
	path := createSnapshot(t)
	metatest.Exec(t, path, `PRAGMA user_version = 99`)
	s, err := New(context.Background(), path)
	if err == nil {
		s.Close()
		t.Fatal("New on a newer schema succeeded")
	}
	metatest.RequireNewerSchemaError(t, err, path)
}
