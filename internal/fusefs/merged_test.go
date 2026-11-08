package fusefs

import (
	"context"
	"errors"
	iofs "io/fs"
	"maps"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
)

type fakeSnapshot struct {
	nodes map[string]model.BaseNode
	kids  map[string][]model.BaseNode
}

func (f *fakeSnapshot) LookupNode(_ context.Context, _ int64, path string) (model.BaseNode, bool, error) {
	n, ok := f.nodes[path]
	return n, ok, nil
}

func (f *fakeSnapshot) ListChildren(_ int64, path string) ([]model.BaseNode, error) {
	if v, ok := f.kids[path]; ok {
		return v, nil
	}
	return nil, nil
}

// newOverlay returns a real overlay store that holds, in order, a directory
// for each of paths that ends in "/", a delete marker for each that starts
// with "-", and an empty file for each other path.
func newOverlay(t *testing.T, paths ...string) *overlaystore.Store {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	for _, p := range paths {
		switch {
		case strings.HasSuffix(p, "/"):
			err = ov.Mkdir(ctx, strings.TrimSuffix(p, "/"), 0o755)
		case strings.HasPrefix(p, "-"):
			err = ov.Remove(ctx, p[1:])
		default:
			_, err = ov.CreateFile(ctx, p, 0o644)
		}
		if err != nil {
			t.Fatalf("overlay %s: %v", p, err)
		}
	}
	return ov
}

// writeOverlayFile creates path in ov holding content.
func writeOverlayFile(t *testing.T, ov model.OverlayStore, path, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := ov.CreateFile(ctx, path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.WriteFile(ctx, path, 0, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

// lookupOverlay returns path's overlay entry and whether the overlay holds one.
func lookupOverlay(t *testing.T, ov model.OverlayStore, path string) (model.OverlayEntry, bool) {
	t.Helper()
	e, ok, err := ov.Lookup(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return e, ok
}

func newResolver(snap model.SnapshotStore, ov model.OverlayStore) *Resolver {
	r := &Resolver{Snapshot: snap, Overlay: ov}
	r.SetGeneration(1)
	return r
}

func TestResolvePrefersWhiteout(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"a.txt": {Path: "a.txt", Type: "file", Mode: 0o644}}},
		newOverlay(t, "-a.txt"),
	)
	_, err := r.ResolvePath("a.txt")
	if err == nil {
		t.Fatal("expected not found due to whiteout")
	}
}

func TestResolveOverlayTakesPrecedence(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"f.txt": {Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "base"}}},
		newOverlay(t, "f.txt"),
	)
	n, err := r.ResolvePath("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !n.FromOverlay {
		t.Fatal("expected overlay to take precedence")
	}
}

func TestSetMtimePromotesBaseFileWithSeparateCtime(t *testing.T) {
	ov := newOverlay(t)
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	if err := engineSetMtime(context.Background(), engine, "b.txt", target); err != nil {
		t.Fatal(err)
	}
	got, ok := lookupOverlay(t, ov, "b.txt")
	if !ok {
		t.Fatal("expected base file to be promoted")
	}
	if got.MtimeUnixNs != target.UnixNano() {
		t.Fatalf("mtime = %v, want %v", time.Unix(0, got.MtimeUnixNs), target)
	}
	if got.CtimeUnixNs == target.UnixNano() || got.CtimeUnixNs == 0 {
		t.Fatalf("ctime should be non-zero and independent from caller mtime: %v", time.Unix(0, got.CtimeUnixNs))
	}
}

func TestSetMtimeRejectsRootAndBaseSymlink(t *testing.T) {
	ov := newOverlay(t)
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			".":    {Path: ".", Type: "dir", Mode: 0o755},
			"link": {Path: "link", Type: "symlink", Mode: 0o120000},
		}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	if err := engineSetMtime(context.Background(), engine, ".", target); !errors.Is(err, iofs.ErrInvalid) {
		t.Fatalf("root SetMtime err = %v, want ErrInvalid", err)
	}
	if _, ok := lookupOverlay(t, ov, "."); ok {
		t.Fatal("root SetMtime should not create an overlay entry")
	}
	if err := engineSetMtime(context.Background(), engine, "link", target); !errors.Is(err, iofs.ErrInvalid) {
		t.Fatalf("symlink SetMtime err = %v, want ErrInvalid", err)
	}
}

func TestRenameMovesBaseDirectoryTree(t *testing.T) {
	ov := newOverlay(t)
	r := newResolver(
		&fakeSnapshot{
			nodes: map[string]model.BaseNode{
				"src":          {Path: "src", Type: "dir", Mode: 0o40000},
				"src/a.txt":    {Path: "src/a.txt", Type: "file", Mode: 0o100644},
				"src/nested":   {Path: "src/nested", Type: "dir", Mode: 0o40000},
				"src/nested/b": {Path: "src/nested/b", Type: "file", Mode: 0o100644},
			},
			kids: map[string][]model.BaseNode{
				"src": {
					{Path: "src/a.txt", Type: "file", Mode: 0o100644},
					{Path: "src/nested", Type: "dir", Mode: 0o40000},
				},
				"src/nested": {
					{Path: "src/nested/b", Type: "file", Mode: 0o100644},
				},
			},
		},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"src", "src/a.txt", "src/nested", "src/nested/b"} {
		if e, ok := lookupOverlay(t, ov, path); !ok || !e.IsDeleted() {
			t.Fatalf("source %q = %+v, ok=%v; want whiteout", path, e, ok)
		}
	}
	for _, path := range []string{"dst", "dst/a.txt", "dst/nested", "dst/nested/b"} {
		if e, ok := lookupOverlay(t, ov, path); !ok || e.IsDeleted() {
			t.Fatalf("destination %q = %+v, ok=%v", path, e, ok)
		}
	}
}

func TestRenameMovesNonEmptyOverlayDirectoryTree(t *testing.T) {
	ov := newOverlay(t, "src/", "src/a.txt", "src/nested/", "src/nested/b")
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"dst", "dst/a.txt", "dst/nested", "dst/nested/b"} {
		if _, ok := lookupOverlay(t, ov, path); !ok {
			t.Fatalf("missing moved path %q", path)
		}
	}
}

func TestRenameDirectoryRejectsDescendantDestination(t *testing.T) {
	ov := newOverlay(t, "src/")
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "src/nested"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("err = %v, want EINVAL", err)
	}
}

func TestRenameDirectoryRejectsNonEmptyDestination(t *testing.T) {
	ov := newOverlay(t, "src/", "dst/", "dst/a.txt")
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("err = %v, want ENOTEMPTY", err)
	}
}

func TestRenameBaseFileRejectsBaseDirectoryDestination(t *testing.T) {
	ov := newOverlay(t)
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"a.txt": {Path: "a.txt", Type: "file", Mode: 0o644},
			"dst":   {Path: "dst", Type: "dir", Mode: 0o40000},
		}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "a.txt", "dst"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("Rename base file over base dir err = %v, want EISDIR", err)
	}
	if _, ok := lookupOverlay(t, ov, "a.txt"); ok {
		t.Fatal("invalid rename should not promote source into overlay")
	}
}

func TestRenameAllowsOverlayFileShadowingBaseDirectory(t *testing.T) {
	ov := newOverlay(t, "src")
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "dir", Mode: 0o40000}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatalf("Rename overlay file shadowing base dir: %v", err)
	}
}

func TestRenameCreateOverBaseBecomesModify(t *testing.T) {
	ov := newOverlay(t, "tmp.txt")
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"dst.txt": {Path: "dst.txt", Type: "file", Mode: 0o644, ObjectOID: "dst-oid"}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "tmp.txt", "dst.txt"); err != nil {
		t.Fatalf("Rename create over base: %v", err)
	}
	got, ok := lookupOverlay(t, ov, "dst.txt")
	if !ok {
		t.Fatal("expected destination overlay entry")
	}
	if got.Kind != model.OverlayKindModify || got.SourceOID != "dst-oid" {
		t.Fatalf("destination entry = %+v, want modify from dst-oid", got)
	}
}

func TestRenameModifyRejectsBaseDirectoryDestination(t *testing.T) {
	ov := newOverlay(t)
	if _, err := ov.EnsureCopyOnWrite(context.Background(), model.RepoPaths{}, "src.txt", model.BaseNode{Path: "src.txt", Type: "file", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"dst": {Path: "dst", Type: "dir", Mode: 0o40000}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src.txt", "dst"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("Rename modify over base dir err = %v, want EISDIR", err)
	}
}

func TestRenameAllowsPromotedBaseDirectory(t *testing.T) {
	ov := newOverlay(t, "src/")
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "dir", Mode: 0o40000}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupOverlay(t, ov, "src"); !ok || !e.IsDeleted() {
		t.Fatalf("source = %+v, ok=%v; want whiteout", e, ok)
	}
	if e, ok := lookupOverlay(t, ov, "dst"); !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("destination = %+v, ok=%v", e, ok)
	}
}

func TestRenameAllowsOverlayDirectoryReplacingBaseFileAtSource(t *testing.T) {
	ov := newOverlay(t, "src/")
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "file", Mode: 0o644}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if e, ok := lookupOverlay(t, ov, "src"); !ok || !e.IsDeleted() {
		t.Fatalf("source = %+v, ok=%v; want whiteout", e, ok)
	}
}

func TestRenameOverlayMkdirRejectsBaseFileDestination(t *testing.T) {
	ov := newOverlay(t, "tmpdir/")
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"dst.txt": {Path: "dst.txt", Type: "file", Mode: 0o644}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "tmpdir", "dst.txt"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Rename overlay mkdir over base file err = %v, want ENOTDIR", err)
	}
}

func TestRenameSameBasePathNoop(t *testing.T) {
	ov := newOverlay(t)
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"a.txt": {Path: "a.txt", Type: "file", Mode: 0o644},
			"src":   {Path: "src", Type: "dir", Mode: 0o40000},
		}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "a.txt", "a.txt"); err != nil {
		t.Fatalf("Rename same file path: %v", err)
	}
	if err := engineRename(context.Background(), engine, "src", "src"); err != nil {
		t.Fatalf("Rename same dir path: %v", err)
	}
	if entries, err := ov.ListAll(context.Background()); err != nil || len(entries) != 0 {
		t.Fatalf("same-path rename should not mutate overlay: %+v, %v", entries, err)
	}
}

func TestReaddirMergesSnapshotAndOverlay(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {
					{Path: "a.txt", Type: "file"},
					{Path: "b.txt", Type: "file"},
				},
			},
		},
		newOverlay(t, "c.txt"),
	)
	names := readdirNames(t, r, ".")
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	if !set["a.txt"] || !set["b.txt"] || !set["c.txt"] {
		t.Fatalf("expected a.txt, b.txt, c.txt, got %v", names)
	}
}

func TestReaddirSkipsOverlayEntryForListedDirectory(t *testing.T) {
	r := newResolver(&fakeSnapshot{kids: map[string][]model.BaseNode{".": {}}}, newOverlay(t, "./"))

	names := readdirNames(t, r, ".")
	for _, name := range names {
		if name == "." {
			t.Fatal("root overlay entry should not appear as a child")
		}
	}
}

func TestReaddirWhiteoutRemovesEntry(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {{Path: "keep.txt", Type: "file"}, {Path: "del.txt", Type: "file"}},
			},
		},
		newOverlay(t, "-del.txt"),
	)
	names := readdirNames(t, r, ".")
	for _, n := range names {
		if n == "del.txt" {
			t.Fatal("del.txt should be removed by whiteout")
		}
	}
	found := false
	for _, n := range names {
		if n == "keep.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("keep.txt should remain")
	}
}

func TestReaddirTypedReturnsTypes(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {
					{Path: "dir", Type: "dir"},
					{Path: "file.txt", Type: "file", ObjectOID: "abc123", SizeState: "known", SizeBytes: 42},
					{Path: "link", Type: "symlink"},
				},
			},
		},
		newOverlay(t),
	)
	entries, err := r.ReaddirTyped(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, e := range entries {
		types[e.Name] = e.Type
	}
	if types["dir"] != "dir" || types["file.txt"] != "file" || types["link"] != "symlink" {
		t.Fatalf("wrong types: %v", types)
	}
	for _, e := range entries {
		if e.Name == "file.txt" {
			if e.ObjectOID != "abc123" || e.SizeState != "known" || e.SizeBytes != 42 {
				t.Fatalf("file metadata = %+v", e)
			}
			return
		}
	}
	t.Fatal("file.txt entry not found")
}

// lookupCountingOverlay counts the overlay lookups an op makes.
type lookupCountingOverlay struct {
	model.OverlayStore
	lookups int
}

func (o *lookupCountingOverlay) Lookup(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	o.lookups++
	return o.OverlayStore.Lookup(ctx, path)
}

func TestReaddirTypedDerivesOverlayChildrenFromOneListing(t *testing.T) {
	ctx := context.Background()
	ov := &lookupCountingOverlay{OverlayStore: newOverlay(t, "-src/gone.txt", "src/made/", "src/made/inner.txt", "src/new.txt", "src/implicit/deep/x.txt", "src/basedir/added.txt")}
	r := newResolver(&fakeSnapshot{kids: map[string][]model.BaseNode{"src": {
		{Path: "src/kept.txt", Type: "file"},
		{Path: "src/gone.txt", Type: "file"},
		{Path: "src/basedir", Type: "dir"},
	}}}, ov)

	entries, err := r.ReaddirTyped(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	type kind struct {
		typ         string
		fromOverlay bool
	}
	got := map[string]kind{}
	for _, e := range entries {
		got[e.Name] = kind{e.Type, e.FromOverlay}
	}
	want := map[string]kind{
		"kept.txt": {"file", false},
		"basedir":  {"dir", false},
		"made":     {"dir", true},
		"new.txt":  {"file", true},
		"implicit": {"dir", false},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("ReaddirTyped(src) = %v, want %v", got, want)
	}
	if ov.lookups != 0 {
		t.Fatalf("ReaddirTyped made %d overlay Lookup calls, want 0", ov.lookups)
	}
}

func TestChildName(t *testing.T) {
	tests := []struct {
		parent, entry string
		wantName      string
		wantOK        bool
	}{
		{".", "foo", "foo", true},
		{".", "foo/bar", "foo", true},
		{"src", "src/main.go", "main.go", true},
		{"src", "srclib/foo.go", "", false}, // prefix collision
		{"src", "src", "", false},           // exact match, not a child
		{"pkg/sub", "pkg/sub/a.txt", "a.txt", true},
		{".", "", "", false},
	}
	for _, tt := range tests {
		name, ok := childName(tt.parent, tt.entry)
		if ok != tt.wantOK || name != tt.wantName {
			t.Errorf("childName(%q, %q) = (%q, %v), want (%q, %v)",
				tt.parent, tt.entry, name, ok, tt.wantName, tt.wantOK)
		}
	}
}

// readdirNames lists path through ReaddirSnapshot, as the FUSE adapter does,
// and returns the entry names.
func readdirNames(t *testing.T, r *Resolver, path string) []string {
	t.Helper()
	entries, _, _, err := r.ReaddirSnapshot(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}
