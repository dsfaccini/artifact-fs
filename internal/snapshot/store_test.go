package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(context.Background(), filepath.Join(dir, "snap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPublishAndLookupNode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "abc123", SizeState: "known", SizeBytes: 42},
		{RepoID: "r", Path: "src", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "src/main.go", Type: "file", Mode: 0o644, ObjectOID: "def456", SizeState: "known", SizeBytes: 100},
	}
	gen, err := s.PublishGeneration(ctx, "head1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected gen 1, got %d", gen)
	}

	n, ok := lookupNode(t, s, gen, "README.md")
	if !ok {
		t.Fatal("expected README.md")
	}
	if n.SizeBytes != 42 || n.ObjectOID != "abc123" {
		t.Fatalf("wrong node: %+v", n)
	}

	_, ok = lookupNode(t, s, gen, "nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

// The CLI's prepare and the daemon's HEAD reconcile publish in one database.
// SQLite fails a read transaction's upgrade to a write at once with
// SQLITE_BUSY, without the busy handler, while another connection holds the
// write lock. So a publish holds the write lock from its read of the current
// generation, and another writer waits for it.
func TestPublishHoldsTheWriteLockFromItsGenerationRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	s, err := New(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The other writer fails at once instead of waiting for the lock.
	other, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(0)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var lockErr error
	testHookAfterGenerationRead = func() {
		if _, lockErr = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); lockErr == nil {
			lockErr = errors.New("the other writer took the write lock")
		}
	}
	defer func() { testHookAfterGenerationRead = nil }()

	gen, err := s.PublishGeneration(ctx, "head1", "refs/heads/main", []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}})
	// The other writer holds the lock only when it took it inside the publish.
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	if err != nil {
		t.Fatalf("publish with another writer between its read and its write: %v", err)
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](lockErr); !ok || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("other writer inside the publish: %v; want SQLITE_BUSY", lockErr)
	}
	if gen != 1 {
		t.Fatalf("published generation %d, want 1", gen)
	}
}

func TestListChildren(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "a1", SizeState: "known", SizeBytes: 1},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "b1", SizeState: "known", SizeBytes: 2},
		{RepoID: "r", Path: "sub", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "sub/c.txt", Type: "file", Mode: 0o644, ObjectOID: "c1", SizeState: "known", SizeBytes: 3},
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	// Root children
	children, err := s.ListChildren(gen, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range children {
		names[c.Path] = true
	}
	if !names["a.txt"] || !names["b.txt"] || !names["sub"] {
		t.Fatalf("expected a.txt, b.txt, sub in root children, got %v", names)
	}
	if names["sub/c.txt"] {
		t.Fatal("sub/c.txt should not be a root child")
	}

	// Sub children
	children, err = s.ListChildren(gen, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Path != "sub/c.txt" {
		t.Fatalf("expected [sub/c.txt], got %v", children)
	}
}

func TestListChildrenSearchesParentIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen, err := s.PublishGeneration(ctx, "h1", "main", []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "sub", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "sub/c.txt", Type: "file", Mode: 0o644, ObjectOID: "c1", SizeState: "known", SizeBytes: 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+listChildrenQuery, gen, "sub")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0], "idx_base_nodes_gen_parent (generation=? AND parent_path=?)") {
		t.Fatalf("ListChildren plan = %q, want one search of idx_base_nodes_gen_parent", plan)
	}
}

func TestGenerationCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "x", SizeState: "known"},
	}
	for i, head := range []string{"h1", "h2", "h3"} {
		gen, err := s.PublishGeneration(ctx, head, "main", nodes)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(i + 1); gen != want {
			t.Fatalf("generation of %s = %d, want %d", head, gen, want)
		}
	}

	if err := s.PruneGenerations(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for gen, want := range map[int64]bool{1: false, 2: true, 3: true} {
		if _, ok := lookupNode(t, s, gen, "f.txt"); ok != want {
			t.Fatalf("generation %d holds f.txt = %v, want %v after pruning below 2", gen, ok, want)
		}
	}
}

// The estimate of generations that hold equal trees splits the used pages
// evenly between them. Pages a prune freed are not used.
func TestEstimateGenerationBytesSharesTheUsedPages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	nodes := make([]model.BaseNode, 2000)
	for i := range nodes {
		nodes[i] = model.BaseNode{Path: fmt.Sprintf("dir/file-%04d.go", i), Type: "file", Mode: 0o644, ObjectOID: strings.Repeat("ab", 20), SizeState: "known"}
	}
	for range 3 {
		if _, err := s.PublishGeneration(ctx, "h", "main", nodes); err != nil {
			t.Fatal(err)
		}
	}
	estimate := func(gens ...int64) int64 {
		t.Helper()
		bytes, err := s.EstimateGenerationBytes(ctx, gens)
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	all, one := estimate(1, 2, 3), estimate(2)
	if all <= 0 || one*3 > all || one*3 < all-2 {
		t.Fatalf("estimate of generation 2 = %d and of all three = %d, want a third of a positive total", one, all)
	}
	if err := s.PruneGenerations(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if left := estimate(2, 3); left >= all || left <= 0 {
		t.Fatalf("estimate of the two generations a prune left = %d, want less than the %d of all three", left, all)
	}
}

func TestCurrentGeneration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	gen, err := s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 0 {
		t.Fatalf("expected 0 for empty store, got %d", gen)
	}

	nodes := []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
	if _, err := s.PublishGeneration(ctx, "h1", "main", nodes); err != nil {
		t.Fatal(err)
	}

	gen, err = s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected 1, got %d", gen)
	}
}

func TestUpdateSizeUsesGenerationObjectOIDIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "shared", SizeState: "unknown"},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "other", SizeState: "unknown"},
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	var id, parent, notUsed int
	var detail string
	err = s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN UPDATE base_nodes SET size_bytes=?, size_state='known' WHERE generation=? AND object_oid=?`, 42, gen, "shared").Scan(&id, &parent, &notUsed, &detail)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "idx_base_nodes_gen_oid") {
		t.Fatalf("UpdateSizes plan = %q, want idx_base_nodes_gen_oid", detail)
	}

	if err := s.UpdateSizes(ctx, gen, map[string]int64{"shared": 42, "other": 7}); err != nil {
		t.Fatal(err)
	}
	n, ok := lookupNode(t, s, gen, "a.txt")
	if !ok {
		t.Fatal("a.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 42 {
		t.Fatalf("a.txt size = %s/%d, want known/42", n.SizeState, n.SizeBytes)
	}
	n, ok = lookupNode(t, s, gen, "b.txt")
	if !ok {
		t.Fatal("b.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 7 {
		t.Fatalf("b.txt size = %s/%d, want known/7", n.SizeState, n.SizeBytes)
	}
}

// lookupNode looks path up in generation and fails t on a database error.
func lookupNode(t *testing.T, s *Store, generation int64, path string) (model.BaseNode, bool) {
	t.Helper()
	n, ok, err := s.LookupNode(context.Background(), generation, path)
	if err != nil {
		t.Fatal(err)
	}
	return n, ok
}
