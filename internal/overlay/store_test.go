package overlay

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func testStore(t *testing.T) (*Store, model.RepoPaths) {
	t.Helper()
	dir := t.TempDir()
	cfg := model.RepoPaths{
		ID:            "test",
		Name:          "test",
		OverlayDir:    filepath.Join(dir, "overlay"),
		OverlayDBPath: filepath.Join(dir, "overlay", "meta.sqlite"),
		BlobCacheDir:  filepath.Join(dir, "cache"),
	}
	if err := os.MkdirAll(cfg.BlobCacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}

func cacheTestBlob(t *testing.T, cfg model.RepoPaths, oid string) {
	t.Helper()
	if err := os.WriteFile(cachedBlobPath(t, cfg, oid), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndGet(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	e, err := s.CreateFile(ctx, "hello.txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != model.OverlayKindCreate || e.Path != "hello.txt" {
		t.Fatalf("unexpected entry: %+v", e)
	}

	got, ok := lookupEntry(t, s, "hello.txt")
	if !ok {
		t.Fatal("expected to find hello.txt")
	}
	if got.Kind != model.OverlayKindCreate || got.BackingPath == "" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestCreateSymlinkReconcilesAfterCommit(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	target := "../README.md"

	created, err := s.CreateSymlink(ctx, "docs/readme-link", target)
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != model.OverlayKindSymlink || created.NodeType() != "symlink" {
		t.Fatalf("created entry = %+v", created)
	}
	if created.TargetPath != target || created.SizeBytes != int64(len(target)) || created.BackingPath != "" {
		t.Fatalf("created symlink metadata = %+v", created)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		if path != "docs/readme-link" {
			return model.BaseNode{}, false
		}
		return model.BaseNode{
			Path:      path,
			Type:      "symlink",
			Mode:      0o120000,
			ObjectOID: testBlobOID([]byte(target)),
		}, true
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupEntry(t, s, "docs/readme-link"); ok {
		t.Fatal("committed symlink should be removed from overlay")
	}
}

func TestSetModePreservesUncommittedModeAndReconcilesCommittedMode(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	baseData := []byte("base")
	baseOID := testBlobOID(baseData)
	if err := os.WriteFile(cachedBlobPath(t, cfg, baseOID), baseData, 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.BaseNode{Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: baseOID}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, base.Path, base); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, base.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	entry, ok := lookupEntry(t, s, base.Path)
	if !ok {
		t.Fatal("expected mode-only overlay entry")
	}
	if entry.Mode != 0o100755 {
		t.Fatalf("entry mode = %#o, want 0100755", entry.Mode)
	}
	st, err := os.Stat(entry.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("backing mode = %#o, want 0755", st.Mode().Perm())
	}

	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		return model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: baseOID}, true
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupEntry(t, s, base.Path); !ok {
		t.Fatal("uncommitted executable-bit change should remain in overlay")
	}

	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		return model.BaseNode{Path: path, Type: "file", Mode: 0o100755, ObjectOID: baseOID}, true
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupEntry(t, s, base.Path); ok {
		t.Fatal("committed executable-bit change should be removed from overlay")
	}
}

func TestReconcilePreservesUnstagedModeAfterContentCommit(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	original := []byte("original")
	originalOID := testBlobOID(original)
	if err := os.WriteFile(cachedBlobPath(t, cfg, originalOID), original, 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.BaseNode{Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: originalOID}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, base.Path, base); err != nil {
		t.Fatal(err)
	}
	committed := []byte("committed")
	if err := s.Truncate(ctx, base.Path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, base.Path, 0, committed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, base.Path, 0o755); err != nil {
		t.Fatal(err)
	}

	committedOID := testBlobOID(committed)
	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		return model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: committedOID}, true
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok := lookupEntry(t, s, base.Path)
	if !ok {
		t.Fatal("unstaged executable-bit change should remain after committing content")
	}
	if entry.Mode != 0o100755 || entry.SourceOID != committedOID || entry.SourceMode != 0o100644 {
		t.Fatalf("rebased overlay entry = %+v", entry)
	}
}

func TestReconcileDoesNotDeleteConcurrentModeChange(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	original := []byte("original")
	originalOID := testBlobOID(original)
	if err := os.WriteFile(cachedBlobPath(t, cfg, originalOID), original, 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.BaseNode{Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: originalOID}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, base.Path, base); err != nil {
		t.Fatal(err)
	}
	committed := []byte("committed")
	if err := s.Truncate(ctx, base.Path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, base.Path, 0, committed); err != nil {
		t.Fatal(err)
	}

	concurrent, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { concurrent.Close() })
	lookups := 0
	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		lookups++
		if lookups == 1 {
			if err := concurrent.SetMode(ctx, path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: testBlobOID(committed)}, true
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok := lookupEntry(t, s, base.Path)
	if !ok {
		t.Fatal("concurrent executable-bit change was deleted")
	}
	if entry.Mode != 0o100755 {
		t.Fatalf("entry mode = %#o, want 0100755", entry.Mode)
	}
}

func TestRenameBaseSymlinkReconcilesAfterCommit(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	target := "../README.md"
	oid := testBlobOID([]byte(target))
	if err := os.WriteFile(cachedBlobPath(t, cfg, oid), []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.BaseNode{Path: "docs/link", Type: "symlink", Mode: 0o120000, ObjectOID: oid}
	promoted, err := s.EnsureCopyOnWrite(ctx, cfg, base.Path, base)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Kind != model.OverlayKindSymlink || promoted.TargetPath != target || promoted.SourceOID != oid {
		t.Fatalf("promoted symlink = %+v", promoted)
	}
	if err := s.Rename(ctx, base.Path, "link", true); err != nil {
		t.Fatal(err)
	}
	if source, ok := lookupEntry(t, s, base.Path); !ok || !source.IsDeleted() {
		t.Fatalf("source entry = %+v, ok=%v, want whiteout", source, ok)
	}
	if destination, ok := lookupEntry(t, s, "link"); !ok || destination.Kind != model.OverlayKindSymlink || destination.TargetPath != target || destination.SourceOID != "" {
		t.Fatalf("destination entry = %+v, ok=%v", destination, ok)
	}

	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		if path == base.Path {
			return base, true
		}
		return model.BaseNode{}, false
	}); err != nil {
		t.Fatal(err)
	}
	if source, ok := lookupEntry(t, s, base.Path); !ok || !source.IsDeleted() {
		t.Fatalf("uncommitted rename source after reconcile = %+v, ok=%v", source, ok)
	}
	if destination, ok := lookupEntry(t, s, "link"); !ok || destination.Kind != model.OverlayKindSymlink {
		t.Fatalf("uncommitted rename destination after reconcile = %+v, ok=%v", destination, ok)
	}

	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		if path == "link" {
			return model.BaseNode{Path: path, Type: "symlink", Mode: 0o120000, ObjectOID: oid}, true
		}
		return model.BaseNode{}, false
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupEntry(t, s, base.Path); ok {
		t.Fatal("committed symlink rename source should be removed from overlay")
	}
	if _, ok := lookupEntry(t, s, "link"); ok {
		t.Fatal("committed symlink rename destination should be removed from overlay")
	}
}

func TestWriteAndRead(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "f.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	created, _ := lookupEntry(t, s, "f.txt")
	n, err := s.WriteFile(ctx, "f.txt", 0, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("wrote %d, want 5", n)
	}

	e, _ := lookupEntry(t, s, "f.txt")
	data, err := os.ReadFile(e.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q, want %q", data, "hello")
	}
	if e.CtimeUnixNs < created.CtimeUnixNs {
		t.Fatalf("ctime moved backward: got %d before %d", e.CtimeUnixNs, created.CtimeUnixNs)
	}
	if e.MtimeUnixNs != e.CtimeUnixNs {
		t.Fatalf("write should set mtime and ctime together: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
	}
}

func TestRemoveCreatesWhiteout(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "del.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "del.txt"); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "del.txt"); !ok || !e.IsDeleted() {
		t.Fatal("expected whiteout")
	}
	if _, ok := lookupEntry(t, s, "del.txt"); !ok {
		t.Fatal("expected entry (delete kind)")
	}
}

func TestRenameDBFirst(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	base := model.BaseNode{Path: "old.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "old.txt", base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "old.txt", 0, []byte("content")); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "old.txt", target); err != nil {
		t.Fatal(err)
	}
	before, ok := lookupEntry(t, s, "old.txt")
	if !ok {
		t.Fatal("expected old entry")
	}

	if err := s.Rename(ctx, "old.txt", "new.txt", true); err != nil {
		t.Fatal(err)
	}

	// Old path should have a whiteout
	if e, ok := lookupEntry(t, s, "old.txt"); !ok || !e.IsDeleted() {
		t.Fatal("expected whiteout at old path")
	} else {
		if e.MtimeUnixNs == target.UnixNano() || e.CtimeUnixNs == target.UnixNano() {
			t.Fatalf("whiteout times should not inherit user mtime: %+v", e)
		}
	}
	// New path should exist
	got, ok := lookupEntry(t, s, "new.txt")
	if !ok || got.Kind != model.OverlayKindRename {
		t.Fatalf("expected rename entry, got %+v ok=%v", got, ok)
	}
	// File content should be readable at new backing path
	data, err := os.ReadFile(got.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want %q", data, "content")
	}
	if got.SizeBytes != int64(len("content")) {
		t.Fatalf("size = %d, want %d", got.SizeBytes, len("content"))
	}
	if got.MtimeUnixNs != target.UnixNano() {
		t.Fatalf("mtime = %v, want %v", time.Unix(0, got.MtimeUnixNs), target)
	}
	if got.CtimeUnixNs == target.UnixNano() || got.CtimeUnixNs == 0 {
		t.Fatalf("ctime should advance independently from mtime: %v", time.Unix(0, got.CtimeUnixNs))
	}
	if got.CtimeUnixNs == before.CtimeUnixNs {
		t.Fatalf("rename should bump ctime: before=%v after=%v", time.Unix(0, before.CtimeUnixNs), time.Unix(0, got.CtimeUnixNs))
	}
}

// A rename onto an overlay file removes the replaced backing file, which no
// entry names afterwards.
func TestRenameOntoAnOverlayFileRemovesItsBackingFile(t *testing.T) {
	for name, rename := range map[string]func(context.Context, *Store) error{
		"Rename": func(ctx context.Context, s *Store) error {
			return s.Rename(ctx, "src.txt", "dst.txt", false)
		},
		"RenameAndMarkModifiedFromBase": func(ctx context.Context, s *Store) error {
			return s.RenameAndMarkModifiedFromBase(ctx, "src.txt", "dst.txt", "aaa", 0o644, false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := testStore(t)
			createWithContent(t, s, "src.txt", "source")
			createWithContent(t, s, "dst.txt", "replaced")
			replaced, ok := lookupEntry(t, s, "dst.txt")
			if !ok {
				t.Fatal("dst.txt missing before the rename")
			}

			if err := rename(context.Background(), s); err != nil {
				t.Fatal(err)
			}

			got, ok := lookupEntry(t, s, "dst.txt")
			if !ok {
				t.Fatal("dst.txt missing after the rename")
			}
			data, err := os.ReadFile(got.BackingPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "source" {
				t.Fatalf("dst.txt = %q, want %q", data, "source")
			}
			if _, err := os.Stat(replaced.BackingPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replaced backing file: %v, want it removed", err)
			}
		})
	}
}

func TestRenameSamePathNoop(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "same.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "same.txt", "same.txt", false); err != nil {
		t.Fatal(err)
	}
	got, ok := lookupEntry(t, s, "same.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	if got.IsDeleted() {
		t.Fatalf("same-path rename should not create whiteout: %+v", got)
	}
}

func TestRenameThenRecreateSourceUsesDistinctBacking(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateFile(ctx, "a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "a.txt", 0, []byte("renamed")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "a.txt", 0, []byte("new")); err != nil {
		t.Fatal(err)
	}
	a, _ := lookupEntry(t, s, "a.txt")
	b, _ := lookupEntry(t, s, "b.txt")
	if a.BackingPath == b.BackingPath {
		t.Fatalf("recreated source and renamed destination share backing %q", a.BackingPath)
	}
	data, err := os.ReadFile(b.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "renamed" {
		t.Fatalf("renamed content = %q, want renamed", data)
	}
}

func TestRenameSameMissingPathReturnsNotExist(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Rename(ctx, "missing.txt", "missing.txt", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestRenameCreateRemainsCreateAcrossReconcile(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "tmp.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "tmp.txt", 0, []byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "tmp.txt", "kept.txt", false); err != nil {
		t.Fatal(err)
	}

	before, ok := lookupEntry(t, s, "kept.txt")
	if !ok {
		t.Fatal("expected renamed create entry")
	}
	if before.Kind != model.OverlayKindCreate {
		t.Fatalf("kind = %q, want create", before.Kind)
	}
	if _, ok := lookupEntry(t, s, "tmp.txt"); ok {
		t.Fatal("untracked create rename should not leave a source whiteout")
	}

	baseLookup := func(string) (model.BaseNode, bool) { return model.BaseNode{}, false }
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}
	after, ok := lookupEntry(t, s, "kept.txt")
	if !ok || after.IsDeleted() {
		t.Fatalf("reconcile should keep renamed create, got %+v ok=%v", after, ok)
	}
}

func TestRenamePreservesDirectoryKindAndRepairsMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET mode=? WHERE path=?`, 0o40000, "src"); err != nil {
		t.Fatal(err)
	}
	src, _ := lookupEntry(t, s, "src")
	if err := os.Chmod(src.BackingPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "src", "dst", false); err != nil {
		t.Fatal(err)
	}

	got, ok := lookupEntry(t, s, "dst")
	if !ok {
		t.Fatal("expected renamed entry")
	}
	if got.Kind != model.OverlayKindMkdir || got.NodeType() != "dir" {
		t.Fatalf("expected renamed directory entry, got %+v", got)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", got.Mode)
	}
	st, err := os.Stat(got.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("backing mode = %#o, want 0755", st.Mode().Perm())
	}
}

func TestRenameMovesNonEmptyOverlayDirectory(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "src/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/nested/b.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "src/nested/b.txt", 0, []byte("nested")); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename(ctx, "src", "dst", false); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"src", "src/a.txt", "src/nested", "src/nested/b.txt"} {
		if _, ok := lookupEntry(t, s, path); ok {
			t.Fatalf("source entry %q still exists", path)
		}
	}
	for _, path := range []string{"dst", "dst/a.txt", "dst/nested", "dst/nested/b.txt"} {
		if e, ok := lookupEntry(t, s, path); !ok || e.IsDeleted() {
			t.Fatalf("destination entry %q = %+v, ok=%v", path, e, ok)
		}
	}
	moved, _ := lookupEntry(t, s, "dst/nested/b.txt")
	if data, err := os.ReadFile(moved.BackingPath); err != nil {
		t.Fatal(err)
	} else if string(data) != "nested" {
		t.Fatalf("moved content = %q, want nested", data)
	}
}

func TestRenameTreeWhiteoutsEveryTrackedSourcePath(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	baseFile := model.BaseNode{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, baseFile.ObjectOID)

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, baseFile.Path, baseFile); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "src/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/nested/new.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSymlink(ctx, "src/link", "../target"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET source_oid=?, source_mode=? WHERE path=?`, "bbb", 0o120000, "src/link"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "src/deleted.txt"); err != nil {
		t.Fatal(err)
	}

	basePaths := []string{"src", "src/a.txt", "src/deleted.txt", "src/link"}
	if err := s.RenameTree(ctx, "src", "dst", basePaths, nil); err != nil {
		t.Fatal(err)
	}

	for _, path := range basePaths {
		if e, ok := lookupEntry(t, s, path); !ok || !e.IsDeleted() {
			t.Fatalf("tracked source %q = %+v, ok=%v; want whiteout", path, e, ok)
		}
	}
	if e, ok := lookupEntry(t, s, "dst/a.txt"); !ok || e.Kind != model.OverlayKindRename || e.TargetPath != "src/a.txt" {
		t.Fatalf("moved tracked file = %+v, ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "dst/nested/new.txt"); !ok || e.Kind != model.OverlayKindCreate {
		t.Fatalf("moved untracked file = %+v, ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "dst/link"); !ok || e.Kind != model.OverlayKindSymlink || e.SourceOID != "" || e.TargetPath != "../target" {
		t.Fatalf("moved tracked symlink = %+v, ok=%v", e, ok)
	}
	if _, ok := lookupEntry(t, s, "dst/deleted.txt"); ok {
		t.Fatal("deleted tracked child should not be recreated at destination")
	}
}

func TestRenameTreeReplacesEmptyOverlayDestination(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "dst", 0o755); err != nil {
		t.Fatal(err)
	}
	replaced, _ := lookupEntry(t, s, "dst")

	if err := s.RenameTree(ctx, "src", "dst", nil, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(replaced.BackingPath); !os.IsNotExist(err) {
		t.Fatalf("replaced destination backing still exists: %v", err)
	}
	if e, ok := lookupEntry(t, s, "dst/a.txt"); !ok || e.IsDeleted() {
		t.Fatalf("moved child = %+v, ok=%v", e, ok)
	}
}

func TestRenameTreeRejectsNonEmptyOverlayDestinationWithoutMutation(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/source.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "dst", 0o755); err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateFile(ctx, "dst/concurrent.txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, child.Path, 0, []byte("preserve me")); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameTree(ctx, "src", "dst", nil, nil); !os.IsExist(err) {
		t.Fatalf("RenameTree error = %v, want os.ErrExist", err)
	}

	for _, path := range []string{"src", "src/source.txt", "dst", "dst/concurrent.txt"} {
		if e, ok := lookupEntry(t, s, path); !ok || e.IsDeleted() {
			t.Fatalf("entry %q = %+v, ok=%v; want preserved", path, e, ok)
		}
	}
	if data, err := os.ReadFile(child.BackingPath); err != nil {
		t.Fatal(err)
	} else if string(data) != "preserve me" {
		t.Fatalf("destination child content = %q", data)
	}
	if _, ok := lookupEntry(t, s, "dst/source.txt"); ok {
		t.Fatal("source child appeared at rejected destination")
	}
}

func TestRenameTreeAllowsDeletedDestinationDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/source.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "dst", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "dst/deleted.txt"); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameTree(ctx, "src", "dst", nil, nil); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "dst/source.txt"); !ok || e.IsDeleted() {
		t.Fatalf("moved child = %+v, ok=%v", e, ok)
	}
	if _, ok := lookupEntry(t, s, "dst/deleted.txt"); ok {
		t.Fatal("destination tombstone survived replacement")
	}
}

func TestRenameTreeKeepsReplacedBaseDescendantsHidden(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "src/overlap", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/source.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "dst", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "dst/overlap"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "dst/gone.txt"); err != nil {
		t.Fatal(err)
	}

	destinationBasePaths := []string{
		"dst",
		"dst/gone.txt",
		"dst/overlap",
		"dst/overlap/stale.txt",
	}
	if err := s.RenameTree(ctx, "src", "dst", nil, destinationBasePaths); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"dst/gone.txt", "dst/overlap/stale.txt"} {
		if e, ok := lookupEntry(t, s, path); !ok || !e.IsDeleted() {
			t.Fatalf("replaced base path %q = %+v, ok=%v; want tombstone", path, e, ok)
		}
	}
	if e, ok := lookupEntry(t, s, "dst/overlap"); !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("incoming overlapping directory = %+v, ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "dst/source.txt"); !ok || e.IsDeleted() {
		t.Fatalf("incoming source file = %+v, ok=%v", e, ok)
	}
}

func TestRenameTreePersistsAcrossReopen(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	baseFile := model.BaseNode{Path: "src/tracked.txt", Type: "file", Mode: 0o100644, ObjectOID: "tracked"}
	cacheTestBlob(t, cfg, baseFile.ObjectOID)

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, baseFile.Path, baseFile); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "src/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateFile(ctx, "src/nested/local.txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, created.Path, 0, []byte("local")); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(ctx, "dst", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "dst/old.txt"); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameTree(
		ctx,
		"src",
		"dst",
		[]string{"src", "src/tracked.txt"},
		[]string{"dst", "dst/old.txt"},
	); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for _, path := range []string{"src", "src/tracked.txt"} {
		if entry, ok := lookupEntry(t, reopened, path); !ok || !entry.IsDeleted() {
			t.Fatalf("reopened source whiteout %q = %+v, ok=%v", path, entry, ok)
		}
	}
	for _, path := range []string{"dst", "dst/tracked.txt", "dst/nested", "dst/nested/local.txt"} {
		if entry, ok := lookupEntry(t, reopened, path); !ok || entry.IsDeleted() {
			t.Fatalf("reopened destination entry %q = %+v, ok=%v", path, entry, ok)
		}
	}
	if entry, ok := lookupEntry(t, reopened, "dst/old.txt"); !ok || !entry.IsDeleted() {
		t.Fatalf("reopened replaced descendant tombstone = %+v, ok=%v", entry, ok)
	}
	local, _ := lookupEntry(t, reopened, "dst/nested/local.txt")
	if data, err := os.ReadFile(local.BackingPath); err != nil {
		t.Fatal(err)
	} else if string(data) != "local" {
		t.Fatalf("reopened local content = %q", data)
	}
}

func TestRenameTreeReconcilesAcrossBaseChange(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	baseFile := model.BaseNode{Path: "src/tracked.txt", Type: "file", Mode: 0o100644, ObjectOID: "old"}
	cacheTestBlob(t, cfg, baseFile.ObjectOID)

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, baseFile.Path, baseFile); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameTree(ctx, "src", "dst", []string{"src", baseFile.Path}, nil); err != nil {
		t.Fatal(err)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "src":
			return model.BaseNode{Path: "src", Type: "dir", Mode: 0o40000}, true
		case "src/tracked.txt":
			return model.BaseNode{Path: path, Type: "file", Mode: 0o100755, ObjectOID: "new"}, true
		case "dst":
			return model.BaseNode{Path: "dst", Type: "dir", Mode: 0o40000}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}
	if entry, ok := lookupEntry(t, s, "dst"); ok {
		t.Fatalf("destination directory represented by new base should reconcile, got %+v", entry)
	}
	if entry, ok := lookupEntry(t, s, "dst/tracked.txt"); !ok ||
		entry.Kind != model.OverlayKindRename ||
		entry.TargetPath != "src/tracked.txt" ||
		entry.SourceOID != "new" ||
		entry.SourceMode != 0o100755 {
		t.Fatalf("reconciled tree rename entry = %+v, ok=%v", entry, ok)
	}
	if entry, ok := lookupEntry(t, s, "src/tracked.txt"); !ok || !entry.IsDeleted() {
		t.Fatalf("reconciled source whiteout = %+v, ok=%v", entry, ok)
	}
}

func TestRenameOverlayDirectoryIgnoresDeletedDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "a_b", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "a_b/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "a_b/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "axb/secret.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a_b", "dst", false); err != nil {
		t.Fatal(err)
	}
	got, ok := lookupEntry(t, s, "dst")
	if !ok || got.Kind != model.OverlayKindMkdir {
		t.Fatalf("expected renamed directory, got %+v ok=%v", got, ok)
	}
	if e, ok := lookupEntry(t, s, "a_b/a.txt"); ok {
		t.Fatalf("deleted descendant tombstone should be removed after directory rename, got %+v", e)
	}
	if e, ok := lookupEntry(t, s, "axb/secret.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("unrelated wildcard-like tombstone should remain, got %+v ok=%v", e, ok)
	}
}

func TestRenameOverlayDirectoryCleansUTF8DeletedDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	dir := "\u00e9"
	child := dir + "/a.txt"

	if err := s.Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, child, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, dir, "dst", false); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, child); ok {
		t.Fatalf("UTF-8 deleted descendant tombstone should be removed after directory rename, got %+v", e)
	}
}

func TestRenameOverlayDirectoryDoesNotDependOnDestinationBackingPath(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "src/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "src", "dst", false); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, ok := lookupEntry(t, s, "src/a.txt"); ok {
		t.Fatal("deleted descendant tombstone should be removed")
	}
	if e, ok := lookupEntry(t, s, "dst"); !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("destination entry = %+v, ok=%v", e, ok)
	}
}

func TestRenameChainPreservesOriginalSourceForReconcile(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "b.txt", "c.txt", false); err != nil {
		t.Fatal(err)
	}

	before, ok := lookupEntry(t, s, "c.txt")
	if !ok {
		t.Fatal("expected final rename entry")
	}
	if before.TargetPath != "a.txt" {
		t.Fatalf("target path = %q, want original source a.txt", before.TargetPath)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		if path == "a.txt" {
			return base, true
		}
		return model.BaseNode{}, false
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}

	after, ok := lookupEntry(t, s, "c.txt")
	if !ok || after.IsDeleted() {
		t.Fatalf("reconcile should keep chained rename, got %+v ok=%v", after, ok)
	}
	if after.TargetPath != "a.txt" {
		t.Fatalf("target path after reconcile = %q, want a.txt", after.TargetPath)
	}
}

func TestReconcilePreservesDivergentRenameAcrossBaseChange(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt", true); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("expected source whiteout before reconcile, got %+v ok=%v", e, ok)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		if path == "a.txt" {
			return model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "bbb"}, true
		}
		return model.BaseNode{}, false
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "b.txt"); !ok || e.Kind != model.OverlayKindRename || e.SourceOID != "bbb" {
		t.Fatalf("divergent rename should be preserved against new base, got %+v ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("source whiteout should remain with divergent rename, got %+v ok=%v", e, ok)
	}
}

func TestReconcileKeepsRenameWhenIdenticalSourceStillExists(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	payload := []byte("same")
	oid := testBlobOID(payload)
	if err := os.WriteFile(cachedBlobPath(t, cfg, oid), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: oid}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt", true); err != nil {
		t.Fatal(err)
	}
	lookup := func(path string) (model.BaseNode, bool) {
		if path == "a.txt" || path == "b.txt" {
			return model.BaseNode{Path: path, Type: "file", ObjectOID: oid}, true
		}
		return model.BaseNode{}, false
	}
	if err := reconcile(ctx, s, lookup); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "b.txt"); !ok || e.Kind != model.OverlayKindRename {
		t.Fatalf("rename should remain pending, got %+v ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("source whiteout should remain pending, got %+v ok=%v", e, ok)
	}
}

func TestReconcilePreservesDivergentChainedRenameAndRemovesIntermediateWhiteout(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "b.txt", "c.txt", false); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "b.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("expected intermediate whiteout before reconcile, got %+v ok=%v", e, ok)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "a.txt":
			return model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "bbb"}, true
		case "b.txt":
			return model.BaseNode{Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "new-b"}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("source whiteout should remain, got %+v ok=%v", e, ok)
	}
	if e, ok := lookupEntry(t, s, "b.txt"); ok {
		t.Fatalf("intermediate whiteout should be removed, got %+v", e)
	}
	if e, ok := lookupEntry(t, s, "c.txt"); !ok || e.Kind != model.OverlayKindRename || e.SourceOID != "bbb" {
		t.Fatalf("final rename should remain rebased, got %+v ok=%v", e, ok)
	}
}

func TestEnsureCopyOnWritePreservesSize(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	const oid = "abc123"
	if err := os.WriteFile(cachedBlobPath(t, cfg, oid), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	e, err := s.EnsureCopyOnWrite(ctx, cfg, "tracked.txt", model.BaseNode{
		RepoID:    cfg.ID,
		Path:      "tracked.txt",
		Type:      "file",
		Mode:      0o644,
		ObjectOID: oid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.SizeBytes != int64(len("payload")) {
		t.Fatalf("size = %d, want %d", e.SizeBytes, len("payload"))
	}

	got, ok := lookupEntry(t, s, "tracked.txt")
	if !ok {
		t.Fatal("expected tracked.txt entry")
	}
	if got.SizeBytes != int64(len("payload")) {
		t.Fatalf("stored size = %d, want %d", got.SizeBytes, len("payload"))
	}
}

func TestMkdir(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "subdir", 0o755); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupEntry(t, s, "subdir")
	if !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("expected mkdir entry, got %+v", e)
	}
}

func TestMkdirNormalizesGitTreeMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupEntry(t, s, "src")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", e.Mode)
	}
}

func TestMkdirPreservesExplicitZeroMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "private", 0); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupEntry(t, s, "private")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0 {
		t.Fatalf("mode = %#o, want 0", e.Mode)
	}
}

func TestSetMtimeRepairsGitTreeDirectoryMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET mode=? WHERE path=?`, 0o40000, "src"); err != nil {
		t.Fatal(err)
	}
	src, _ := lookupEntry(t, s, "src")
	if err := os.Chmod(src.BackingPath, 0); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "src", target); err != nil {
		t.Fatal(err)
	}

	e, ok := lookupEntry(t, s, "src")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", e.Mode)
	}
	st, err := os.Stat(e.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("backing mode = %#o, want 0755", st.Mode().Perm())
	}
}

func TestRenameRepairsDirectoryModeBeforeCommitting(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := lookupEntry(t, s, "src")
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET mode=? WHERE path=?`, 0o40000, "src"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src.BackingPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "src", "dst", false); err == nil {
		t.Fatal("expected backing mode repair failure")
	}
	if _, ok := lookupEntry(t, s, "src"); !ok {
		t.Fatal("source entry was removed despite rename failure")
	}
	if _, ok := lookupEntry(t, s, "dst"); ok {
		t.Fatal("destination entry was committed despite rename failure")
	}
}

func TestTruncateUpdatesSizeAndTimes(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "trunc.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "trunc.txt", 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "trunc.txt", target); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "trunc.txt", 2); err != nil {
		t.Fatal(err)
	}

	e, ok := lookupEntry(t, s, "trunc.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.SizeBytes != 2 {
		t.Fatalf("size = %d, want 2", e.SizeBytes)
	}
	if e.MtimeUnixNs == target.UnixNano() || e.CtimeUnixNs == target.UnixNano() {
		t.Fatalf("truncate should replace user mtime with mutation time: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
	}
	if e.MtimeUnixNs != e.CtimeUnixNs {
		t.Fatalf("truncate should set mtime and ctime together: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
	}
}

func TestListByPrefix(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	for _, p := range []string{"src/a.go", "src/b.go", "srclib/c.go", "readme.md"} {
		if _, err := s.CreateFile(ctx, p, 0o644); err != nil {
			t.Fatalf("CreateFile(%q): %v", p, err)
		}
	}

	entries, err := s.ListByPrefix(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(got, want) {
		t.Fatalf("ListByPrefix(src) = %q, want %q", got, want)
	}
}

func TestListByPrefixMatchesPathPrefixExactly(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	for _, p := range []string{"src_a/mine.txt", "src_a/deep/nested.txt", "srcXa/secret.txt", "SRC_A/upper.txt", "src_a.txt", "src_a0/sibling.txt", "src_ab/sibling.txt"} {
		if _, err := s.CreateFile(ctx, p, 0o644); err != nil {
			t.Fatalf("CreateFile(%q): %v", p, err)
		}
	}

	entries, err := s.ListByPrefix(ctx, "src_a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}
	if want := []string{"src_a/deep/nested.txt", "src_a/mine.txt"}; !slices.Equal(got, want) {
		t.Fatalf("ListByPrefix(src_a) = %q, want %q", got, want)
	}

	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+listByPrefixQuery, "src_a/", "src_a0")
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
	if len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH overlay_entries") || !strings.Contains(plan[0], "(path>? AND path<?)") {
		t.Fatalf("ListByPrefix plan = %q, want one range search of the path key", plan)
	}
}

func TestReconcileAfterCommit(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	// Simulate: user modified foo.txt (source_oid="aaa") then committed.
	// After commit, the base has foo.txt with a new OID ("bbb").
	base := model.BaseNode{RepoID: cfg.ID, Path: "foo.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "foo.txt", base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "foo.txt", 0, []byte("modified")); err != nil {
		t.Fatal(err)
	}

	// Also create a new file that was committed.
	if _, err := s.CreateFile(ctx, "new.txt", 0o644); err != nil {
		t.Fatal(err)
	}

	// And a whiteout for a file that was deleted in the commit.
	if err := s.Remove(ctx, "gone.txt"); err != nil {
		t.Fatal(err)
	}
	for path, kind := range map[string]model.OverlayKind{"foo.txt": model.OverlayKindModify, "new.txt": model.OverlayKindCreate, "gone.txt": model.OverlayKindDelete} {
		if e, ok := lookupEntry(t, s, path); !ok || e.Kind != kind {
			t.Fatalf("%s before reconcile = %+v, %v; want a %s entry", path, e, ok, kind)
		}
	}

	// Reconcile against a base where:
	// - foo.txt exists with different OID (was committed)
	// - new.txt exists (was committed)
	// - gone.txt doesn't exist (was removed)
	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "foo.txt":
			return model.BaseNode{Path: "foo.txt", ObjectOID: testBlobOID([]byte("modified"))}, true
		case "new.txt":
			return model.BaseNode{Path: "new.txt", ObjectOID: testBlobOID(nil)}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}

	// All three entries should be removed.
	if _, ok := lookupEntry(t, s, "foo.txt"); ok {
		t.Fatal("foo.txt should be removed (base OID changed)")
	}
	if _, ok := lookupEntry(t, s, "new.txt"); ok {
		t.Fatal("new.txt should be removed (now in base)")
	}
	if _, ok := lookupEntry(t, s, "gone.txt"); ok {
		t.Fatal("gone.txt whiteout should be removed (not in base)")
	}
}

func testBlobOID(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestReconcileKeepsValidEntries(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	// Modify foo.txt from base OID "aaa" -- but base hasn't changed.
	base := model.BaseNode{RepoID: cfg.ID, Path: "foo.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "foo.txt", base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "foo.txt", 0, []byte("local change")); err != nil {
		t.Fatal(err)
	}

	// Create a file that doesn't exist in base.
	if _, err := s.CreateFile(ctx, "local-only.txt", 0o644); err != nil {
		t.Fatal(err)
	}

	// Whiteout a file that still exists in base.
	if err := s.Remove(ctx, "hidden.txt"); err != nil {
		t.Fatal(err)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "foo.txt":
			// Same OID as source_oid -- base unchanged, keep overlay.
			return model.BaseNode{Path: "foo.txt", ObjectOID: "aaa"}, true
		case "hidden.txt":
			return model.BaseNode{Path: "hidden.txt", ObjectOID: "ddd"}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := reconcile(ctx, s, baseLookup); err != nil {
		t.Fatal(err)
	}

	// All three entries should be kept.
	if _, ok := lookupEntry(t, s, "foo.txt"); !ok {
		t.Fatal("foo.txt should be kept (base OID unchanged)")
	}
	if _, ok := lookupEntry(t, s, "local-only.txt"); !ok {
		t.Fatal("local-only.txt should be kept (not in base)")
	}
	if e, ok := lookupEntry(t, s, "hidden.txt"); !ok || !e.IsDeleted() {
		t.Fatal("hidden.txt whiteout should be kept (still in base)")
	}
}

func TestSetMtime(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "m.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "m.txt", target); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupEntry(t, s, "m.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	got := time.Unix(0, e.MtimeUnixNs)
	if !got.Equal(target) {
		t.Fatalf("mtime = %v, want %v", got, target)
	}
	if e.CtimeUnixNs == target.UnixNano() {
		t.Fatalf("ctime should not be caller-controlled: ctime=%v target=%v", time.Unix(0, e.CtimeUnixNs), target)
	}
	if e.CtimeUnixNs == 0 {
		t.Fatal("ctime should be non-zero")
	}
}

func changesSince(t *testing.T, cfg model.RepoPaths, since int64) Changes {
	t.Helper()
	changes, ok, err := ReadChangesSince(context.Background(), cfg, since)
	if err != nil || !ok {
		t.Fatalf("ReadChangesSince = %v, %v", ok, err)
	}
	return changes
}

func TestRenameTreeLogsBothRootsAndOnePathPerDirectory(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	for _, dir := range []string{"src", "src/sub"} {
		if err := s.Mkdir(ctx, dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"src/a.txt", "src/b.txt", "src/sub/c.txt", "src/sub/d.txt"} {
		if _, err := s.CreateFile(ctx, path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := changesSince(t, cfg, 0).Seq
	if err := s.Rename(ctx, "src", "dst", false); err != nil {
		t.Fatal(err)
	}
	// Each path names a listing Git must reread: src/a.txt drops the cached
	// listing of src, as a row for every moved entry would.
	want := []string{"dst", "dst/a.txt", "dst/sub/c.txt", "src", "src/a.txt", "src/sub/c.txt"}
	if got := changesSince(t, cfg, before).Paths; !slices.Equal(got, want) {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
}

// Linux allows a name that is not valid UTF-8, and Git reports it byte for byte.
func TestRenameTreeLogsNonUTF8PathsVerbatim(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	for _, dir := range []string{"src\xff", "src\xff/sub\xfe"} {
		if err := s.Mkdir(ctx, dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateFile(ctx, "src\xff/sub\xfe/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	before := changesSince(t, cfg, 0).Seq
	if err := s.Rename(ctx, "src\xff", "dst\xfd", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"dst\xfd", "dst\xfd/sub\xfe", "dst\xfd/sub\xfe/a.txt", "src\xff", "src\xff/sub\xfe", "src\xff/sub\xfe/a.txt"}
	if got := changesSince(t, cfg, before).Paths; !slices.Equal(got, want) {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
}

func TestReconcileLogsOnlyDeleteMarkersOverBasePaths(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	cacheTestBlob(t, cfg, base.ObjectOID)
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	// The chain leaves a delete marker at the untracked b.txt that names a.txt.
	if err := s.Rename(ctx, "a.txt", "b.txt", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "b.txt", "c.txt", false); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := s.Remove(ctx, fmt.Sprintf("gone/%d.txt", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := changesSince(t, cfg, 0).Seq
	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		switch path {
		case "a.txt":
			return model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "bbb"}, true
		case "b.txt":
			return model.BaseNode{Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "new-b"}, true
		}
		return model.BaseNode{}, false
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupEntry(t, s, "gone/0.txt"); ok {
		t.Fatal("delete marker for a path the base lacks survived reconcile")
	}
	// Dropping the gone/ markers and rebasing c.txt leave the merged view as
	// it was. Dropping b.txt's marker shows the new base file.
	if got := changesSince(t, cfg, before).Paths; !slices.Equal(got, []string{"b.txt"}) {
		t.Fatalf("changed paths after reconcile = %q, want [b.txt]", got)
	}
}

func TestChangesSinceQuerySearchesTheSequenceRange(t *testing.T) {
	s, _ := testStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+changesSinceQuery, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || !strings.HasPrefix(plan[0], "SEARCH overlay_changes USING INTEGER PRIMARY KEY") {
		t.Fatalf("query plan = %q, want a search of the seq range", plan)
	}
}

func TestReadChangesSinceDistrustsALogMissingATrigger(t *testing.T) {
	s, cfg := testStore(t)
	changesSince(t, cfg, 0)
	if _, err := s.db.Exec(`DROP TRIGGER overlay_entries_delete_change`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadChangesSince(context.Background(), cfg, 0); err != nil || ok {
		t.Fatalf("ReadChangesSince without the delete trigger = %v, %v; want false, nil", ok, err)
	}
}

// The FSMonitor hook reads the change log while the daemon writes the
// overlay, so its read never waits for the write lock.
func TestReadChangesSinceReadsWhileAWriterHoldsTheLock(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	if err := s.Mkdir(ctx, "dir", 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)
	if got, want := changesSince(t, cfg, 0).Paths, []string{"dir"}; !slices.Equal(got, want) {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
}

// Writes that land while a reconcile looks up the base must end as if they
// had landed before it: none lost, none left classified against the old base.
func TestReconcileClassifiesWritesThatLandDuringItsLookups(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	original := []byte("original")
	originalOID := testBlobOID(original)
	if err := os.WriteFile(cachedBlobPath(t, cfg, originalOID), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "committed.txt", model.BaseNode{Path: "committed.txt", Type: "file", Mode: 0o100644, ObjectOID: originalOID}); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "committed.txt", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "committed.txt", 0, []byte("committed")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"src", "src/sub"} {
		if err := s.Mkdir(ctx, dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"src/sub/b.txt", "src/sub/c.txt"} {
		if _, err := s.CreateFile(ctx, path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WriteFile(ctx, path, 0, []byte(filepath.Base(path))); err != nil {
			t.Fatal(err)
		}
	}
	file := func(path string, content string) model.BaseNode {
		return model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: testBlobOID([]byte(content))}
	}
	base := map[string]model.BaseNode{
		"committed.txt": file("committed.txt", "committed"),
		"new.txt":       file("new.txt", "theirs"),
		"dst":           {Path: "dst", Type: "dir", Mode: 0o40000},
		"dst/sub":       {Path: "dst/sub", Type: "dir", Mode: 0o40000},
		"dst/sub/c.txt": file("dst/sub/c.txt", "c.txt"),
	}

	before := changesSince(t, cfg, 0).Seq
	var logged []string
	var once sync.Once
	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		once.Do(func() {
			if _, err := s.WriteFile(ctx, "committed.txt", int64(len("committed")), []byte(" and more")); err != nil {
				t.Error(err)
			}
			if _, err := s.CreateFile(ctx, "new.txt", 0o644); err != nil {
				t.Error(err)
			}
			if _, err := s.WriteFile(ctx, "new.txt", 0, []byte("mine")); err != nil {
				t.Error(err)
			}
			// A tree rename logs only both roots and one path per
			// directory, so the log never names dst/sub/c.txt.
			if err := s.Rename(ctx, "src", "dst", false); err != nil {
				t.Error(err)
			}
			logged = changesSince(t, cfg, before).Paths
		})
		n, ok := base[path]
		return n, ok
	}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(logged, "dst/sub/c.txt") {
		t.Fatalf("the tree rename logged dst/sub/c.txt, so the test proves nothing about unlogged paths: %q", logged)
	}

	if e, ok := lookupEntry(t, s, "committed.txt"); !ok || e.Kind != model.OverlayKindModify || e.SourceOID != base["committed.txt"].ObjectOID {
		t.Errorf("committed.txt after reconcile = %+v, %t; want a modify of the new base", e, ok)
	} else if got, err := os.ReadFile(e.BackingPath); err != nil || string(got) != "committed and more" {
		t.Errorf("committed.txt content = %q, %v; want the write that landed during reconcile", got, err)
	}
	if e, ok := lookupEntry(t, s, "new.txt"); !ok || e.Kind != model.OverlayKindModify || e.SourceOID != base["new.txt"].ObjectOID {
		t.Errorf("new.txt after reconcile = %+v, %t; want a modify of the new base", e, ok)
	}
	if e, ok := lookupEntry(t, s, "dst/sub/c.txt"); ok {
		t.Errorf("dst/sub/c.txt after reconcile = %+v; want it gone, as the new base matches it", e)
	}
	if e, ok := lookupEntry(t, s, "dst/sub/b.txt"); !ok || e.Kind != model.OverlayKindCreate {
		t.Errorf("dst/sub/b.txt after reconcile = %+v, %t; want the create kept", e, ok)
	}
	if got := changesSince(t, cfg, before).Paths; !slices.Equal(got, logged) {
		t.Errorf("changed paths after reconcile = %q, want only the writes' %q", got, logged)
	}
}

func createWithContent(t *testing.T, s *Store, path, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateFile(ctx, path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, path, 0, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func baseFile(path, content string) model.BaseNode {
	return model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: testBlobOID([]byte(content))}
}

// Another Store's reconcile changes a row without logging it, so only the
// DELETE guard keeps this reconcile from deleting the row it rebased.
func TestReconcileGuardKeepsAnotherStoresUnloggedRebase(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "x.txt", "mine")
	other, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	theirs := baseFile("x.txt", "theirs")
	var once sync.Once
	if err := reconcile(ctx, s, func(string) (model.BaseNode, bool) {
		once.Do(func() {
			if err := reconcile(ctx, other, func(string) (model.BaseNode, bool) { return theirs, true }); err != nil {
				t.Error(err)
			}
		})
		return baseFile("x.txt", "mine"), true
	}); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "x.txt"); !ok || e.SourceOID != theirs.ObjectOID {
		t.Fatalf("x.txt = %+v, %t; want the other reconcile's rebase kept", e, ok)
	}
}

// A write that removes an entry's backing file while the plan hashes it must
// not fail the reconcile: the apply phase classifies the entry the write left.
func TestReconcileClassifiesAnEntryWhoseBackingFileVanishedDuringItsLookups(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "x.txt", "mine")
	before := changesSince(t, cfg, 0).Seq
	var once sync.Once
	if err := reconcile(ctx, s, func(path string) (model.BaseNode, bool) {
		once.Do(func() {
			if err := s.Remove(ctx, "x.txt"); err != nil {
				t.Error(err)
			}
		})
		return baseFile("x.txt", "theirs"), true
	}); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "x.txt"); !ok || e.Kind != model.OverlayKindDelete {
		t.Fatalf("x.txt = %+v, %t; want the delete marker", e, ok)
	}
	if got := changesSince(t, cfg, before).Paths; !slices.Equal(got, []string{"x.txt"}) {
		t.Fatalf("changed paths = %q, want only the write's [x.txt]", got)
	}
}

// Another Store's reconcile rebases a row without logging it while this
// reconcile plans to rebase the same row onto another base. Only the UPDATE
// guard keeps the other store's rebase.
func TestReconcileUpdateGuardKeepsAnotherStoresUnloggedRebase(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "x.txt", "mine")
	other, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	theirs := baseFile("x.txt", "theirs")
	var once sync.Once
	if err := reconcile(ctx, s, func(string) (model.BaseNode, bool) {
		once.Do(func() {
			if err := reconcile(ctx, other, func(string) (model.BaseNode, bool) { return theirs, true }); err != nil {
				t.Error(err)
			}
		})
		return baseFile("x.txt", "ours"), true
	}); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupEntry(t, s, "x.txt"); !ok || e.SourceOID != theirs.ObjectOID {
		t.Fatalf("x.txt = %+v, %t; want the other reconcile's rebase kept", e, ok)
	}
}

// A backing file that vanished with no logged write fails the reconcile, as
// it did when the whole reconcile ran under the lock, and leaves the entry.
func TestReconcileFailsOnABackingFileMissingWithNoWrite(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "x.txt", "mine")
	before, _ := lookupEntry(t, s, "x.txt")
	if err := os.Remove(before.BackingPath); err != nil {
		t.Fatal(err)
	}
	err := reconcile(ctx, s, func(string) (model.BaseNode, bool) { return baseFile("x.txt", "theirs"), true })
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Reconcile = %v, want the missing backing file's ErrNotExist", err)
	}
	if after, ok := lookupEntry(t, s, "x.txt"); !ok || after != before {
		t.Fatalf("x.txt after the failed reconcile = %+v, %t; want it unchanged: %+v", after, ok, before)
	}
}

func TestLookupShowsUnflushedWriteSize(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "f.txt", "one")
	before, _ := lookupEntry(t, s, "f.txt")
	if _, err := s.WriteFile(ctx, "f.txt", 3, []byte("two")); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupEntry(t, s, "f.txt")
	if !ok || e.SizeBytes != 6 || e.MtimeUnixNs <= before.MtimeUnixNs {
		t.Fatalf("f.txt = %+v, %t; want size 6 and an mtime after %d", e, ok, before.MtimeUnixNs)
	}
}

// SyncFile commits through the live store, not through the next open.
func TestSyncFileCommitsUnflushedWrites(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "f.txt", "one")
	if _, err := s.WriteFile(ctx, "f.txt", 3, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFile(ctx, "f.txt"); err != nil {
		t.Fatal(err)
	}
	if e, _ := lookupEntry(t, s, "f.txt"); e.SizeBytes != 6 {
		t.Fatalf("live f.txt after SyncFile = %d, want 6", e.SizeBytes)
	} else if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "onetwo" {
		t.Fatalf("backing file = %q, %v; want onetwo", data, err)
	}
	db, err := sql.Open("sqlite", cfg.OverlayDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var size int64
	var pending int
	if err := db.QueryRow(`SELECT size_bytes, (SELECT COUNT(*) FROM overlay_pending) FROM overlay_entries WHERE path='f.txt'`).Scan(&size, &pending); err != nil {
		t.Fatal(err)
	}
	if size != 6 || pending != 0 {
		t.Fatalf("committed size %d, pending rows %d; want 6 and 0", size, pending)
	}
}

func TestHookReportsEveryWriteAfterAQueryUntilFlush(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	write := func(off int64, data string) {
		t.Helper()
		if _, err := s.WriteFile(ctx, "f.txt", off, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	createWithContent(t, s, "f.txt", "one")
	if err := s.FlushFile(ctx, "f.txt"); err != nil {
		t.Fatal(err)
	}
	for i, data := range []string{"two", "six"} {
		seq := changesSince(t, cfg, 0).Seq
		write(int64(3*(i+1)), data)
		if got := changesSince(t, cfg, seq).Paths; !slices.Equal(got, []string{"f.txt"}) {
			t.Fatalf("changes after write %d following a query = %q, want f.txt", i+1, got)
		}
	}
	seq := changesSince(t, cfg, 0).Seq
	if err := s.FlushFile(ctx, "f.txt"); err != nil {
		t.Fatal(err)
	}
	flushed := changesSince(t, cfg, seq)
	if !slices.Equal(flushed.Paths, []string{"f.txt"}) {
		t.Fatalf("changes after the flush = %q, want f.txt", flushed.Paths)
	}
	if got := changesSince(t, cfg, flushed.Seq).Paths; len(got) != 0 {
		t.Fatalf("changes after a query following the flush = %q, want none", got)
	}
}

func TestNewCommitsWritesAProcessLeftUncommitted(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "f.txt", "one")
	if _, err := s.WriteFile(ctx, "f.txt", 3, []byte("two")); err != nil {
		t.Fatal(err)
	}
	// A crashed process never flushes; a second store stands in for its restart.
	restarted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if e, ok := lookupEntry(t, restarted, "f.txt"); !ok || e.SizeBytes != 6 {
		t.Fatalf("f.txt after restart = %+v, %t; want size 6", e, ok)
	}
	if got := changesSince(t, cfg, changesSince(t, cfg, 0).Seq).Paths; len(got) != 0 {
		t.Fatalf("pending paths after restart = %q, want none", got)
	}
}

// setHook sets *hook to fn for the rest of the test. fn clears it on its first
// call, so a read or flush that fn runs does not reach it again.
func setHook(t *testing.T, hook *func(), fn func()) {
	t.Helper()
	*hook = func() {
		*hook = nil
		fn()
	}
	t.Cleanup(func() { *hook = nil })
}

// A read must report the size of every write that completed before it began,
// however a flush interleaves with its two reads.
func TestReadAcrossAFlushShowsCompletedWrites(t *testing.T) {
	ctx := context.Background()
	reads := map[string]func(*Store) (int64, error){
		"Lookup": func(s *Store) (int64, error) {
			e, _, err := s.Lookup(ctx, "d/f.txt")
			return e.SizeBytes, err
		},
		"ListByPrefix": func(s *Store) (int64, error) {
			entries, err := s.ListByPrefix(ctx, "d")
			if err != nil || len(entries) != 1 {
				return 0, fmt.Errorf("ListByPrefix(d) = %+v, %v", entries, err)
			}
			return entries[0].SizeBytes, nil
		},
	}
	flushes := map[string]func(*Store) error{
		"FlushFile":       func(s *Store) error { return s.FlushFile(ctx, "d/f.txt") },
		"SyncFile":        func(s *Store) error { return s.SyncFile(ctx, "d/f.txt") },
		"unrelated Mkdir": func(s *Store) error { return s.Mkdir(ctx, "other", 0o755) },
	}
	for readName, read := range reads {
		for flushName, flush := range flushes {
			// The committed size is 2 and the pending size 3.
			setup := func(t *testing.T) *Store {
				s, _ := testStore(t)
				createWithContent(t, s, "d/f.txt", "x")
				if err := s.FlushFile(ctx, "d/f.txt"); err != nil {
					t.Fatal(err)
				}
				for off := int64(1); off < 3; off++ {
					if _, err := s.WriteFile(ctx, "d/f.txt", off, []byte("y")); err != nil {
						t.Fatal(err)
					}
				}
				return s
			}
			check := func(t *testing.T, size int64, err error) {
				t.Helper()
				if err != nil || size != 3 {
					t.Errorf("%s = %d, %v; want 3, the size of the completed writes", readName, size, err)
				}
			}
			t.Run(fmt.Sprintf("%s spans %s", readName, flushName), func(t *testing.T) {
				s := setup(t)
				ran := false
				setHook(t, &testHookBetweenReads, func() {
					ran = true
					if err := flush(s); err != nil {
						t.Error(err)
					}
				})
				size, err := read(s)
				if !ran {
					t.Fatalf("%s never reached the step between its two reads", readName)
				}
				check(t, size, err)
			})
			t.Run(fmt.Sprintf("%s inside %s", readName, flushName), func(t *testing.T) {
				s := setup(t)
				ran := false
				setHook(t, &testHookBetweenCommitAndForget, func() {
					ran = true
					size, err := read(s)
					check(t, size, err)
				})
				if err := flush(s); err != nil {
					t.Fatal(err)
				}
				if !ran {
					t.Fatalf("%s never reached the step between its commit and its forget", flushName)
				}
			})
		}
	}
}

// Reads copy the pending writes under pendingMu, because writes and flushes
// change them from another goroutine. The race detector reports an unlocked
// read. Most writes take the path that touches only pendingMu: every query
// locks the database/sql pool's mutex, which orders the writer's earlier
// accesses before the reader's later ones and hides them from the detector.
func TestReadsCopyPendingWritesUnderTheLock(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "d/f.txt", "x")
	done := make(chan struct{})
	started := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for off := int64(1); ; off += 100 {
			for i := range int64(100) {
				if _, err := s.WriteFile(ctx, "d/f.txt", off+i, []byte("y")); err != nil {
					t.Error(err)
					return
				}
			}
			if err := s.FlushFile(ctx, "d/f.txt"); err != nil {
				t.Error(err)
				return
			}
			if off == 1 {
				close(started)
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	defer func() {
		close(done)
		<-stopped
	}()
	select {
	case <-started:
	case <-stopped:
		t.FailNow()
	}
	for range 20 {
		if _, ok, err := s.Lookup(ctx, "d/f.txt"); err != nil || !ok {
			t.Fatalf("Lookup(d/f.txt) = %v, %v; want the file", ok, err)
		}
		if entries, err := s.ListByPrefix(ctx, "d"); err != nil || len(entries) != 1 {
			t.Fatalf("ListByPrefix(d) = %+v, %v; want d/f.txt", entries, err)
		}
	}
}

func TestListByPrefixShowsUnflushedWriteSize(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	createWithContent(t, s, "d/f.txt", "one")
	if err := s.FlushFile(ctx, "d/f.txt"); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"two", "six"} {
		e, _ := lookupEntry(t, s, "d/f.txt")
		if _, err := s.WriteFile(ctx, "d/f.txt", e.SizeBytes, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ListByPrefix(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SizeBytes != 9 {
		t.Fatalf("ListByPrefix(d) = %+v, want d/f.txt with size 9", entries)
	}
}

// open(2) with O_CREAT and a read-only mode returns a writable descriptor,
// and a chmod leaves an open descriptor writable. Their writes must succeed
// however other mutations interleave, while the entry keeps the mode.
func TestReadOnlyModeFileAcceptsWrites(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	write := func(step string, off int64, data string) {
		t.Helper()
		if _, err := s.WriteFile(ctx, "ro.txt", off, []byte(data)); err != nil {
			t.Fatalf("write %s: %v", step, err)
		}
	}
	ownerWritable := func(step string) {
		t.Helper()
		e, _ := lookupEntry(t, s, "ro.txt")
		st, err := os.Stat(e.BackingPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm&0o600 != 0o600 {
			t.Fatalf("backing file %s has mode %o; want owner read and write", step, perm)
		}
	}
	if _, err := s.CreateFile(ctx, "ro.txt", 0o444); err != nil {
		t.Fatal(err)
	}
	ownerWritable("after the create")
	write("after the create", 0, "one")
	if err := s.Mkdir(ctx, "d", 0o755); err != nil {
		t.Fatal(err)
	}
	write("after an unrelated mkdir", 3, "two")
	if err := s.SetMode(ctx, "ro.txt", 0o444); err != nil {
		t.Fatal(err)
	}
	ownerWritable("after a chmod")
	write("after a chmod", 6, "six")
	e, _ := lookupEntry(t, s, "ro.txt")
	if err := s.FlushFile(ctx, "ro.txt"); err != nil {
		t.Fatal(err)
	}
	// An earlier version left the backing file read-only.
	if err := os.Chmod(e.BackingPath, 0o444); err != nil {
		t.Fatal(err)
	}
	write("to a read-only backing file", 9, "ten")
	if err := os.Chmod(e.BackingPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "ro.txt", 6); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	e, _ = lookupEntry(t, s, "ro.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "onetwo" || e.Mode&0o777 != 0o444 {
		t.Fatalf("ro.txt = %q, %v, mode %o; want onetwo and mode 444", data, err, e.Mode&0o777)
	}
}

// cachedBlobPath returns oid's blob cache path, with its shard created.
func cachedBlobPath(t *testing.T, cfg model.RepoPaths, oid string) string {
	t.Helper()
	path := blobcache.Path(cfg.BlobCacheDir, oid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// lookupEntry looks path up in s and fails t on a database error.
func lookupEntry(t *testing.T, s *Store, path string) (model.OverlayEntry, bool) {
	t.Helper()
	e, ok, err := s.Lookup(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return e, ok
}

// One log of a switch writes at most maxSwitchedPaths paths. Past that, it
// gives the overlay a new instance, so Git rescans and the transition lock
// stays short. The cap counts only the paths no overlay entry covers, the
// paths the log writes.
func TestReconcileLogsAtMostMaxSwitchedPaths(t *testing.T) {
	for _, tt := range []struct {
		name             string
		changed, covered int
		renews           bool
	}{
		{"at the cap", maxSwitchedPaths, 0, false},
		{"past the cap", maxSwitchedPaths + 1, 0, true},
		{"past the cap until the overlay covers one", maxSwitchedPaths + 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s, cfg := testStore(t)
			paths := make([]string, tt.changed)
			for i := range paths {
				paths[i] = fmt.Sprintf("f%06d", i)
			}
			for _, path := range paths[:tt.covered] {
				if _, err := s.CreateFile(ctx, path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := changesSince(t, cfg, 0)
			noBase := func(string) (model.BaseNode, bool, error) { return model.BaseNode{}, false, nil }
			if err := s.ReconcileChecked(ctx, 1, noBase, model.SwitchedPaths{Paths: paths}, nil, nil); err != nil {
				t.Fatal(err)
			}
			after := changesSince(t, cfg, before.Seq)
			if renewed := after.Instance != before.Instance; renewed != tt.renews {
				t.Errorf("instance renewed = %v, want %v", renewed, tt.renews)
			}
			want := paths[tt.covered:]
			if tt.renews {
				want = nil
			}
			if !slices.Equal(after.Paths, want) {
				t.Errorf("logged %d paths, want %d, %s through %s", len(after.Paths), len(want), paths[tt.covered], paths[tt.changed-1])
			}
		})
	}
}

// A reconcile records the generation it reconciled with, also when it changes
// and logs nothing, and MarkServed records the generation it is given.
func TestReconcileAndMarkServedRecordTheServedGeneration(t *testing.T) {
	ctx := t.Context()
	s, _ := testStore(t)
	served := func() int64 {
		t.Helper()
		gen, err := s.ServedGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return gen
	}
	if got := served(); got != 0 {
		t.Fatalf("served generation of a new overlay = %d, want 0", got)
	}
	noBase := func(string) (model.BaseNode, bool, error) { return model.BaseNode{}, false, nil }
	if err := s.ReconcileChecked(ctx, 3, noBase, model.SwitchedPaths{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := served(); got != 3 {
		t.Errorf("served generation after a reconcile with 3 = %d, want 3", got)
	}
	if err := s.MarkServed(ctx, 5, model.SwitchedPaths{}); err != nil {
		t.Fatal(err)
	}
	if got := served(); got != 5 {
		t.Errorf("served generation after MarkServed(5) = %d, want 5", got)
	}
}

// reconcile reconciles s with the base lookup reads, with no readers to
// exclude.
func reconcile(ctx context.Context, s *Store, lookup func(string) (model.BaseNode, bool)) error {
	return s.ReconcileChecked(ctx, 0, func(path string) (model.BaseNode, bool, error) {
		n, ok := lookup(path)
		return n, ok, nil
	}, model.SwitchedPaths{}, nil, nil)
}
