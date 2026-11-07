package fusefs

import (
	"context"
	"errors"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeOverlay satisfies model.OverlayStore for testing.
type fakeOverlay struct {
	entries map[string]model.OverlayEntry
	list    []model.OverlayEntry
	writes  int
	data    []byte
}

func (f *fakeOverlay) Lookup(_ context.Context, path string) (model.OverlayEntry, bool, error) {
	e, ok := f.entries[path]
	return e, ok, nil
}
func (f *fakeOverlay) ListByPrefix(_ context.Context, prefix string) ([]model.OverlayEntry, error) {
	if f.list != nil {
		return f.list, nil
	}
	prefix = model.CleanPath(prefix)
	var out []model.OverlayEntry
	for path, e := range f.entries {
		if prefix == "." || strings.HasPrefix(path, prefix+"/") {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	if f.entries == nil {
		f.entries = map[string]model.OverlayEntry{}
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindModify, Mode: base.Mode, MtimeUnixNs: now, CtimeUnixNs: now, SourceOID: base.ObjectOID, SourceMode: base.Mode}
	f.entries[e.Path] = e
	return e, nil
}
func (f *fakeOverlay) EnsureCopyOnWriteFrom(ctx context.Context, repo model.RepoConfig, path string, base model.BaseNode, _ *os.File) (model.OverlayEntry, error) {
	return f.EnsureCopyOnWrite(ctx, repo, path, base)
}
func (f *fakeOverlay) CreateFile(_ context.Context, _ string, _ uint32) (model.OverlayEntry, error) {
	return model.OverlayEntry{}, nil
}
func (f *fakeOverlay) CreateSymlink(_ context.Context, path string, target string) (model.OverlayEntry, error) {
	if f.entries == nil {
		f.entries = map[string]model.OverlayEntry{}
	}
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindSymlink, Mode: 0o120000, SizeBytes: int64(len(target)), TargetPath: target}
	f.entries[e.Path] = e
	return e, nil
}
func (f *fakeOverlay) WriteFile(_ context.Context, _ string, _ int64, data []byte) (int, error) {
	f.writes++
	f.data = append([]byte(nil), data...)
	return len(data), nil
}
func (f *fakeOverlay) FlushFile(_ context.Context, _ string) error         { return nil }
func (f *fakeOverlay) SyncFile(_ context.Context, _ string) error          { return nil }
func (f *fakeOverlay) Truncate(_ context.Context, _ string, _ int64) error { return nil }
func (f *fakeOverlay) Remove(_ context.Context, _ string) error            { return nil }
func (f *fakeOverlay) Rename(_ context.Context, oldPath, newPath string, _ bool) error {
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e := f.entries[oldPath]
	delete(f.entries, oldPath)
	e.Path = newPath
	f.entries[newPath] = e
	return nil
}
func (f *fakeOverlay) RenameAndMarkModifiedFromBase(_ context.Context, oldPath, newPath string, sourceOID string, sourceMode uint32, sourceTracked bool) error {
	if err := f.Rename(context.Background(), oldPath, newPath, sourceTracked); err != nil {
		return err
	}
	e := f.entries[model.CleanPath(newPath)]
	if e.Kind != model.OverlayKindSymlink {
		e.Kind = model.OverlayKindModify
	}
	e.SourceOID = sourceOID
	e.SourceMode = sourceMode
	f.entries[model.CleanPath(newPath)] = e
	return nil
}
func (f *fakeOverlay) RenameTree(_ context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error {
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	moved := map[string]model.OverlayEntry{}
	for path, e := range f.entries {
		if path != oldPath && !strings.HasPrefix(path, oldPath+"/") {
			continue
		}
		delete(f.entries, path)
		if e.IsDeleted() {
			continue
		}
		e.Path = newPath + strings.TrimPrefix(path, oldPath)
		moved[e.Path] = e
	}
	for path := range f.entries {
		if path == newPath || strings.HasPrefix(path, newPath+"/") {
			delete(f.entries, path)
		}
	}
	maps.Copy(f.entries, moved)
	for _, path := range sourceBasePaths {
		f.entries[path] = model.OverlayEntry{Path: path, Kind: model.OverlayKindDelete}
	}
	for _, path := range destinationBasePaths {
		if _, overwritten := moved[path]; path != newPath && !overwritten {
			f.entries[path] = model.OverlayEntry{Path: path, Kind: model.OverlayKindDelete}
		}
	}
	return nil
}
func (f *fakeOverlay) Mkdir(_ context.Context, path string, mode uint32) error {
	if f.entries == nil {
		f.entries = map[string]model.OverlayEntry{}
	}
	now := time.Now().UnixNano()
	f.entries[model.CleanPath(path)] = model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindMkdir, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	return nil
}
func (f *fakeOverlay) SetMtime(_ context.Context, path string, t time.Time) error {
	e := f.entries[model.CleanPath(path)]
	e.MtimeUnixNs = t.UnixNano()
	e.CtimeUnixNs = time.Now().UnixNano()
	f.entries[model.CleanPath(path)] = e
	return nil
}
func (f *fakeOverlay) SetMode(_ context.Context, path string, mode uint32) error {
	e := f.entries[model.CleanPath(path)]
	e.Mode = e.Mode&^0o777 | mode&0o777
	f.entries[model.CleanPath(path)] = e
	return nil
}
func (f *fakeOverlay) ReconcileChecked(_ context.Context, _ func(string) (model.BaseNode, bool, error), lock sync.Locker, switchBase func()) error {
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	if switchBase != nil {
		switchBase()
	}
	return nil
}

func newResolver(snap *fakeSnapshot, ov *fakeOverlay) *Resolver {
	r := &Resolver{Snapshot: snap, Overlay: ov}
	r.SetGeneration(1)
	return r
}

func TestResolvePrefersWhiteout(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"a.txt": {Path: "a.txt", Type: "file", Mode: 0o644}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{"a.txt": {Path: "a.txt", Kind: model.OverlayKindDelete}}},
	)
	_, err := r.ResolvePath("a.txt")
	if err == nil {
		t.Fatal("expected not found due to whiteout")
	}
}

func TestResolveOverlayTakesPrecedence(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"f.txt": {Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "base"}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{"f.txt": {Path: "f.txt", Kind: model.OverlayKindModify, Mode: 0o644}}},
	)
	n, err := r.ResolvePath("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !n.FromOverlay {
		t.Fatal("expected overlay to take precedence")
	}
}

func TestGetattrReturnsMtime(t *testing.T) {
	mtime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	ctime := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC).UnixNano()
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{".": {}}},
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{"x.txt": {Path: "x.txt", Kind: model.OverlayKindCreate, Mode: 0o644, SizeBytes: 10, MtimeUnixNs: mtime, CtimeUnixNs: ctime}},
		},
	)
	_, _, _, mt, ct, err := r.Getattr("x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if mt.UnixNano() != mtime {
		t.Fatalf("mtime = %v, want %v", mt, time.Unix(0, mtime))
	}
	if ct.UnixNano() != ctime {
		t.Fatalf("ctime = %v, want %v", ct, time.Unix(0, ctime))
	}
}

func TestGetattrBaseFileUsesCommitTime(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	// Set a realistic commit timestamp.
	commitTS := int64(1700000000) // 2023-11-14
	r.SetCommitTime(commitTS)

	_, _, _, mt, ct, err := r.Getattr("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Unix(commitTS, 0)
	if !mt.Equal(expected) {
		t.Fatalf("mtime = %v, want %v", mt, expected)
	}
	if !ct.Equal(expected) {
		t.Fatalf("ctime = %v, want %v", ct, expected)
	}
}

func TestGetattrBaseFileFallsBackToGeneration(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	// Don't set commit time -- should fall back to generation.
	_, _, _, mt, ct, err := r.Getattr("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Unix(1, 0) // generation = 1
	if !mt.Equal(expected) {
		t.Fatalf("mtime = %v, want %v", mt, expected)
	}
	if !ct.Equal(expected) {
		t.Fatalf("ctime = %v, want %v", ct, expected)
	}
}

func TestSetMtimePromotesBaseFileWithSeparateCtime(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	if err := engineSetMtime(context.Background(), engine, "b.txt", target); err != nil {
		t.Fatal(err)
	}
	got, ok := ov.entries["b.txt"]
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
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
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
	if _, ok := ov.entries["."]; ok {
		t.Fatal("root SetMtime should not create an overlay entry")
	}
	if err := engineSetMtime(context.Background(), engine, "link", target); !errors.Is(err, iofs.ErrInvalid) {
		t.Fatalf("symlink SetMtime err = %v, want ErrInvalid", err)
	}
}

func TestRenameMovesBaseDirectoryTree(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
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
		if e, ok := ov.entries[path]; !ok || !e.IsDeleted() {
			t.Fatalf("source %q = %+v, ok=%v; want whiteout", path, e, ok)
		}
	}
	for _, path := range []string{"dst", "dst/a.txt", "dst/nested", "dst/nested/b"} {
		if e, ok := ov.entries[path]; !ok || e.IsDeleted() {
			t.Fatalf("destination %q = %+v, ok=%v", path, e, ok)
		}
	}
}

func TestRenameMovesNonEmptyOverlayDirectoryTree(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src":          {Path: "src", Kind: model.OverlayKindMkdir, Mode: 0o755},
		"src/a.txt":    {Path: "src/a.txt", Kind: model.OverlayKindCreate, Mode: 0o644},
		"src/nested":   {Path: "src/nested", Kind: model.OverlayKindMkdir, Mode: 0o755},
		"src/nested/b": {Path: "src/nested/b", Kind: model.OverlayKindCreate, Mode: 0o644},
	}}
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"dst", "dst/a.txt", "dst/nested", "dst/nested/b"} {
		if _, ok := ov.entries[path]; !ok {
			t.Fatalf("missing moved path %q", path)
		}
	}
}

func TestRenameDirectoryRejectsDescendantDestination(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src": {Path: "src", Kind: model.OverlayKindMkdir, Mode: 0o755},
	}}
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "src/nested"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("err = %v, want EINVAL", err)
	}
}

func TestRenameDirectoryRejectsNonEmptyDestination(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src":       {Path: "src", Kind: model.OverlayKindMkdir, Mode: 0o755},
		"dst":       {Path: "dst", Kind: model.OverlayKindMkdir, Mode: 0o755},
		"dst/a.txt": {Path: "dst/a.txt", Kind: model.OverlayKindCreate, Mode: 0o644},
	}}
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("err = %v, want ENOTEMPTY", err)
	}
}

func TestRenameBaseFileRejectsBaseDirectoryDestination(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
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
	if _, ok := ov.entries["a.txt"]; ok {
		t.Fatal("invalid rename should not promote source into overlay")
	}
}

func TestRenameAllowsOverlayFileShadowingBaseDirectory(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src": {Path: "src", Kind: model.OverlayKindCreate, Mode: 0o644},
	}}
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
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"tmp.txt": {Path: "tmp.txt", Kind: model.OverlayKindCreate, Mode: 0o644},
	}}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"dst.txt": {Path: "dst.txt", Type: "file", Mode: 0o644, ObjectOID: "dst-oid"}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "tmp.txt", "dst.txt"); err != nil {
		t.Fatalf("Rename create over base: %v", err)
	}
	got, ok := ov.entries["dst.txt"]
	if !ok {
		t.Fatal("expected destination overlay entry")
	}
	if got.Kind != model.OverlayKindModify || got.SourceOID != "dst-oid" {
		t.Fatalf("destination entry = %+v, want modify from dst-oid", got)
	}
}

func TestRenameModifyRejectsBaseDirectoryDestination(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src.txt": {Path: "src.txt", Kind: model.OverlayKindModify, Mode: 0o644, SourceOID: "src-oid"},
	}}
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
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src": {Path: "src", Kind: model.OverlayKindMkdir, Mode: 0o755},
	}}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "dir", Mode: 0o40000}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if e, ok := ov.entries["src"]; !ok || !e.IsDeleted() {
		t.Fatalf("source = %+v, ok=%v; want whiteout", e, ok)
	}
	if e, ok := ov.entries["dst"]; !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("destination = %+v, ok=%v", e, ok)
	}
}

func TestRenameAllowsOverlayDirectoryReplacingBaseFileAtSource(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"src": {Path: "src", Kind: model.OverlayKindMkdir, Mode: 0o755},
	}}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "file", Mode: 0o644}}},
		ov,
	)
	engine := &Engine{Resolver: r, Overlay: ov}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if e, ok := ov.entries["src"]; !ok || !e.IsDeleted() {
		t.Fatalf("source = %+v, ok=%v; want whiteout", e, ok)
	}
}

func TestRenameOverlayMkdirRejectsBaseFileDestination(t *testing.T) {
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"tmpdir": {Path: "tmpdir", Kind: model.OverlayKindMkdir, Mode: 0o755},
	}}
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
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
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
	if len(ov.entries) != 0 {
		t.Fatalf("same-path rename should not mutate overlay: %+v", ov.entries)
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
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{},
			list:    []model.OverlayEntry{{Path: "c.txt", Kind: model.OverlayKindCreate}},
		},
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
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{".": {}}},
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{".": {Path: ".", Kind: model.OverlayKindMkdir}},
			list:    []model.OverlayEntry{{Path: ".", Kind: model.OverlayKindMkdir}},
		},
	)

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
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{"del.txt": {Path: "del.txt", Kind: model.OverlayKindDelete}},
			list:    []model.OverlayEntry{{Path: "del.txt", Kind: model.OverlayKindDelete}},
		},
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
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
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

type countingLookupOverlay struct {
	model.OverlayStore
	lookups int
}

func (o *countingLookupOverlay) Lookup(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	o.lookups++
	return o.OverlayStore.Lookup(ctx, path)
}

func TestReaddirTypedDerivesOverlayChildrenFromOneListing(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := overlaystore.New(ctx, model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Remove(ctx, "src/gone.txt"); err != nil {
		t.Fatal(err)
	}
	if err := store.Mkdir(ctx, "src/made", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/made/inner.txt", "src/new.txt", "src/implicit/deep/x.txt", "src/basedir/added.txt"} {
		if _, err := store.CreateFile(ctx, p, 0o644); err != nil {
			t.Fatalf("CreateFile(%q): %v", p, err)
		}
	}
	ov := &countingLookupOverlay{OverlayStore: store}
	r := &Resolver{
		Snapshot: &fakeSnapshot{kids: map[string][]model.BaseNode{"src": {
			{Path: "src/kept.txt", Type: "file"},
			{Path: "src/gone.txt", Type: "file"},
			{Path: "src/basedir", Type: "dir"},
		}}},
		Overlay: ov,
	}
	r.SetGeneration(1)

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
