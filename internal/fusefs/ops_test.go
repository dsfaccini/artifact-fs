package fusefs

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/dsfaccini/fuse/fuseops"
)

type fakeBatchHydrator struct {
	tasks       []model.HydrationTask
	calls       int
	path        string
	pathsByOID  map[string]string
	errorsByOID map[string]error
}

type generationSnapshot struct {
	nodes map[int64]map[string]model.BaseNode
	kids  map[int64]map[string][]model.BaseNode
}

type blockingCopyOverlay struct {
	*fakeOverlay
	beforeCopy   chan struct{}
	continueCopy chan struct{}
	backingPath  string
}

type blockingRenameHydrator struct {
	*fakeBatchHydrator
	started chan struct{}
	proceed chan struct{}
}

func (f *fakeBatchHydrator) Enqueue(task model.HydrationTask) {
	f.tasks = append(f.tasks, task)
}

func (f *fakeBatchHydrator) EnqueueBatch(tasks []model.HydrationTask) {
	f.tasks = append(f.tasks, tasks...)
}

func (f *fakeBatchHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	f.calls++
	if err := f.errorsByOID[node.ObjectOID]; err != nil {
		return "", 0, err
	}
	if f.pathsByOID != nil {
		return f.pathsByOID[node.ObjectOID], 0, nil
	}
	return f.path, 0, nil
}

func (f *fakeBatchHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := f.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (f *fakeBatchHydrator) ReadBlob(context.Context, model.RepoConfig, model.BaseNode, int64) ([]byte, error) {
	return nil, nil
}

func (f *fakeBatchHydrator) QueueDepth(model.RepoID) int { return len(f.tasks) }

func (h *blockingRenameHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	close(h.started)
	select {
	case <-h.proceed:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return h.fakeBatchHydrator.OpenHydrated(ctx, repo, node)
}

func (g *generationSnapshot) PublishGeneration(context.Context, string, string, []model.BaseNode) (int64, error) {
	return 0, nil
}

func (g *generationSnapshot) GetNode(gen int64, path string) (model.BaseNode, bool) {
	n, ok := g.nodes[gen][path]
	return n, ok
}

func (g *generationSnapshot) LookupNode(_ context.Context, gen int64, path string) (model.BaseNode, bool, error) {
	n, ok := g.GetNode(gen, path)
	return n, ok, nil
}

func (g *generationSnapshot) ListChildren(gen int64, path string) ([]model.BaseNode, error) {
	return g.kids[gen][path], nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	close(o.beforeCopy)
	<-o.continueCopy
	now := time.Now().UnixNano()
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindModify, BackingPath: o.backingPath, Mode: base.Mode, MtimeUnixNs: now, CtimeUnixNs: now, SourceOID: base.ObjectOID}
	o.entries[e.Path] = e
	return e, nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWriteFrom(ctx context.Context, repo model.RepoConfig, path string, base model.BaseNode, _ *os.File) (model.OverlayEntry, error) {
	return o.EnsureCopyOnWrite(ctx, repo, path, base)
}

func (o *blockingCopyOverlay) WriteFile(_ context.Context, path string, off int64, data []byte) (int, error) {
	f, err := os.OpenFile(o.backingPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		return 0, err
	}
	e := o.entries[model.CleanPath(path)]
	if info, err := f.Stat(); err == nil {
		e.SizeBytes = info.Size()
	}
	o.entries[model.CleanPath(path)] = e
	return len(data), nil
}

func TestPrefetchDirBatchesReaddirMetadata(t *testing.T) {
	hydrator := &fakeBatchHydrator{}
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Hydrator: hydrator}

	engine.PrefetchDir("src", []ReaddirEntry{
		{Name: "a.go", Type: "file", ObjectOID: "a", SizeState: "known", SizeBytes: 10},
		{Name: "sub", Type: "dir"},
		{Name: "overlay.txt", Type: "file"},
		{Name: "b.go", Type: "file", ObjectOID: "b", SizeState: "unknown"},
	})

	if len(hydrator.tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(hydrator.tasks))
	}
	if hydrator.tasks[0].Path != "src/a.go" || hydrator.tasks[0].ObjectOID != "a" || hydrator.tasks[0].SizeBytes != 10 {
		t.Fatalf("first task = %+v", hydrator.tasks[0])
	}
	if hydrator.tasks[1].Path != "src/b.go" || hydrator.tasks[1].ObjectOID != "b" || hydrator.tasks[1].SizeState != "unknown" {
		t.Fatalf("second task = %+v", hydrator.tasks[1])
	}
}

func TestPrefetchDirCapsAndPrioritizesTasks(t *testing.T) {
	h := &fakeBatchHydrator{}
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Hydrator: h}
	entries := make([]ReaddirEntry, 0, maxPrefetchTasksPerDir+2)
	for i := range maxPrefetchTasksPerDir + 1 {
		entries = append(entries, ReaddirEntry{Name: fmt.Sprintf("image-%03d.png", i), Type: "file", ObjectOID: fmt.Sprintf("png-%03d", i)})
	}
	entries = append(entries, ReaddirEntry{Name: "README.md", Type: "file", ObjectOID: "readme"})

	engine.PrefetchDir(".", entries)

	if len(h.tasks) != maxPrefetchTasksPerDir {
		t.Fatalf("tasks = %d, want %d", len(h.tasks), maxPrefetchTasksPerDir)
	}
	foundReadme := false
	for _, task := range h.tasks {
		if task.ObjectOID == "readme" {
			foundReadme = true
			if task.Priority < hydrator.PriorityBootstrap {
				t.Fatalf("README priority = %d", task.Priority)
			}
		}
	}
	if !foundReadme {
		t.Fatal("README.md was dropped from capped prefetch")
	}
}

func TestFileHandleCachesHydratedBaseFile(t *testing.T) {
	tmp := t.TempDir()
	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{path: cachePath}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	engine := &Engine{
		Repo:     model.RepoConfig{ID: "repo"},
		Resolver: newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob"}}}, overlay),
		Overlay:  overlay,
		Hydrator: h,
	}
	fh := &FileHandle{path: "file.txt"}
	defer fh.closeCachedFile()

	first, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	second, err := handleRead(context.Background(), engine, fh, 0, 4, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "cont" || string(second) != "ent" {
		t.Fatalf("reads = %q/%q", first, second)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestFileHandleRehydratesAfterGenerationChange(t *testing.T) {
	tmp := t.TempDir()
	firstCache := filepath.Join(tmp, "first")
	secondCache := filepath.Join(tmp, "second")
	if err := os.WriteFile(firstCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondCache, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{pathsByOID: map[string]string{"first": firstCache, "second": secondCache}}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := &Resolver{
		Snapshot: &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{
			1: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "first"}},
			2: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "second"}},
		}},
		Overlay: overlay,
	}
	resolver.SetGeneration(1)
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
	fh := &FileHandle{path: "file.txt"}
	defer fh.closeCachedFile()

	first, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	resolver.SetGeneration(2)
	second, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "old" || string(second) != "new" {
		t.Fatalf("reads = %q/%q", first, second)
	}
	if h.calls != 2 {
		t.Fatalf("EnsureHydrated calls = %d, want 2", h.calls)
	}
}

func TestFileHandleInvalidatesCacheAfterOverlappingWrite(t *testing.T) {
	tmp := t.TempDir()
	baseCache := filepath.Join(tmp, "base")
	overlayBacking := filepath.Join(tmp, "overlay")
	if err := os.WriteFile(baseCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{path: baseCache}
	overlay := &blockingCopyOverlay{
		fakeOverlay:  &fakeOverlay{entries: map[string]model.OverlayEntry{}},
		beforeCopy:   make(chan struct{}),
		continueCopy: make(chan struct{}),
		backingPath:  overlayBacking,
	}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob", Mode: 0o644}}}, overlay.fakeOverlay)
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, engine)
	fh := &FileHandle{path: "file.txt"}
	fs.fileHandles[1] = fh
	defer fh.closeCachedFile()

	first, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "old" {
		t.Fatalf("first read = %q", first)
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Handle: 1, Data: []byte("new")})
	}()

	<-overlay.beforeCopy
	overlapped, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(overlapped) != "old" {
		t.Fatalf("overlapped read = %q", overlapped)
	}
	close(overlay.continueCopy)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	after, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "new" {
		t.Fatalf("read after write = %q, want new", after)
	}
}

// failingWriteOverlay promotes a path into backingPath, and its WriteFile
// writes the data and then fails, as a write that runs out of space does.
type failingWriteOverlay struct {
	*fakeOverlay
	backingPath string
}

func (o *failingWriteOverlay) EnsureCopyOnWriteFrom(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode, _ *os.File) (model.OverlayEntry, error) {
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindModify, BackingPath: o.backingPath, Mode: base.Mode, SourceOID: base.ObjectOID}
	o.entries[e.Path] = e
	return e, nil
}

func (o *failingWriteOverlay) WriteFile(_ context.Context, path string, off int64, data []byte) (int, error) {
	f, err := os.OpenFile(o.backingPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.WriteAt(data, off)
	if err != nil {
		return n, err
	}
	e := o.entries[model.CleanPath(path)]
	e.SizeBytes = off + int64(n)
	o.entries[e.Path] = e
	return n, syscall.ENOSPC
}

func TestFailedWriteThatPromotedClosesCachedBase(t *testing.T) {
	tmp := t.TempDir()
	baseCache := filepath.Join(tmp, "base")
	if err := os.WriteFile(baseCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := &failingWriteOverlay{fakeOverlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}}, backingPath: filepath.Join(tmp, "overlay")}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob", Mode: 0o644}}}, overlay.fakeOverlay)
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: &fakeBatchHydrator{path: baseCache}}
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, engine)
	fh := &FileHandle{path: "file.txt"}
	fs.fileHandles[1] = fh
	defer fh.closeCachedFile()
	if got, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3)); err != nil || string(got) != "old" {
		t.Fatalf("first read = %q, %v", got, err)
	}
	err := fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Handle: 1, Data: []byte("new")})
	if got := kernelErrno(err); got != syscall.ENOSPC {
		t.Fatalf("write errno = %v, want ENOSPC", got)
	}
	if got, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3)); err != nil || string(got) != "new" {
		t.Fatalf("read after the failed write = %q, %v; want the overlay's new", got, err)
	}
}

func TestRenameBaseDirectoryHydratesAndMovesContents(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("tracked content"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"},
		},
		kids: map[string][]model.BaseNode{
			"src": {{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"}},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: &fakeBatchHydrator{path: cachePath},
	}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	got, err := engineRead(context.Background(), engine, "dst/a.txt", 0, make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tracked content" {
		t.Fatalf("moved content = %q", got)
	}
	if _, err := resolver.ResolvePath("src/a.txt"); !os.IsNotExist(err) {
		t.Fatalf("source child resolve err = %v, want not exist", err)
	}
}

func TestRenameRecreatedTrackedEntryHidesSource(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sourceType         string
		sourceMode         uint32
		trackedDestination bool
	}{
		{"file/absent destination", "file", 0o100644, false},
		{"file/tracked destination", "file", 0o100644, true},
		{"symlink/absent destination", "symlink", 0o120000, false},
		{"symlink/tracked destination", "symlink", 0o120000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			cfg := model.RepoConfig{
				ID:            "repo",
				OverlayDir:    filepath.Join(tmp, "overlay"),
				OverlayDBPath: filepath.Join(tmp, "overlay.db"),
			}
			ov, err := overlaystore.New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ov.Close() })

			const content = "original tracked content"
			cachePath := filepath.Join(tmp, "blob")
			if err := os.WriteFile(cachePath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			nodes := map[string]model.BaseNode{
				"README.md": {Path: "README.md", Type: tc.sourceType, Mode: tc.sourceMode, ObjectOID: "source-oid"},
			}
			if tc.trackedDestination {
				nodes["destination.txt"] = model.BaseNode{Path: "destination.txt", Type: "file", Mode: 0o100644, ObjectOID: "destination-oid"}
			}
			children := make([]model.BaseNode, 0, len(nodes))
			for _, node := range nodes {
				children = append(children, node)
			}
			snapshot := &fakeSnapshot{nodes: nodes, kids: map[string][]model.BaseNode{".": children}}
			resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
			resolver.SetGeneration(1)
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeBatchHydrator{path: cachePath}}

			if tc.sourceType == "symlink" {
				if err := engineUnlink(ctx, engine, "README.md"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := engineRename(ctx, engine, "README.md", "first.md"); err != nil {
					t.Fatal(err)
				}
				if err := engineUnlink(ctx, engine, "first.md"); err != nil {
					t.Fatal(err)
				}
			}
			if err := engine.Create(ctx, "README.md", 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := engineWrite(ctx, engine, "README.md", 0, []byte(content)); err != nil {
				t.Fatal(err)
			}

			created, ok, err := ov.Lookup(ctx, "README.md")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || created.Kind != model.OverlayKindCreate || created.SourceOID != "" {
				t.Fatalf("recreated source entry = %+v, exists = %t; want create without source oid", created, ok)
			}
			if _, ok, err := snapshot.LookupNode(ctx, resolver.Generation(), "README.md"); err != nil || !ok {
				t.Fatalf("snapshot source lookup = (%t, %v), want tracked source", ok, err)
			}

			destination := "second.md"
			if tc.trackedDestination {
				destination = "destination.txt"
			}
			if err := engineRename(ctx, engine, "README.md", destination); err != nil {
				t.Fatal(err)
			}

			if _, err := resolver.ResolvePath("README.md"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source resolve error = %v, want not exist", err)
			}
			if err := ov.Reconcile(ctx, func(path string) (model.BaseNode, bool) {
				node, ok := snapshot.nodes[path]
				return node, ok
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.ResolvePath("README.md"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source resolve error after reconciliation = %v, want not exist", err)
			}
			entries, err := resolver.ReaddirTyped(ctx, ".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name == "README.md" {
					t.Fatal("recreated source remains in root directory listing")
				}
			}
			got, err := engineRead(ctx, engine, destination, 0, make([]byte, len(content)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Fatalf("destination content = %q, want %q", got, content)
			}

			sourceEntry, ok, err := ov.Lookup(ctx, "README.md")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || sourceEntry.Kind != model.OverlayKindDelete {
				t.Fatalf("source overlay entry = %+v, exists = %t; want delete", sourceEntry, ok)
			}
			destinationEntry, ok, err := ov.Lookup(ctx, destination)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || destinationEntry.Kind != model.OverlayKindRename || destinationEntry.SourceOID != "source-oid" {
				t.Fatalf("destination overlay entry = %+v, exists = %t; want rename from original source", destinationEntry, ok)
			}
			if destinationEntry.NodeType() != "file" || destinationEntry.Mode != 0o644 || destinationEntry.SourceMode != tc.sourceMode {
				t.Fatalf("destination entry = %+v; want regular replacement with original source metadata", destinationEntry)
			}

			blob := fmt.Sprintf("blob %d\x00%s", len(content), content)
			committed := model.BaseNode{
				Path: destination, Type: "file", Mode: 0o100644,
				ObjectOID: fmt.Sprintf("%x", sha1.Sum([]byte(blob))),
			}
			snapshot.nodes = map[string]model.BaseNode{destination: committed}
			snapshot.kids = map[string][]model.BaseNode{".": {committed}}
			resolver.SetGeneration(2)
			if err := ov.Reconcile(ctx, func(path string) (model.BaseNode, bool) {
				node, ok := snapshot.nodes[path]
				return node, ok
			}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"README.md", destination} {
				if entry, ok, err := ov.Lookup(ctx, path); err != nil || ok {
					t.Fatalf("overlay after commit at %s = (%+v, %t, %v); want no pending entry", path, entry, ok, err)
				}
			}
		})
	}
}

func TestRenameReplacedTrackedEntryHidesSource(t *testing.T) {
	modes := map[string]uint32{"file": 0o100644, "symlink": 0o120000, "dir": 0o040000}
	for _, tc := range []struct {
		name               string
		sourceType         string
		replacementType    string
		trackedDestination bool
	}{
		{"file replaced by symlink/absent destination", "file", "symlink", false},
		{"file replaced by symlink/tracked destination", "file", "symlink", true},
		{"symlink replaced by symlink/absent destination", "symlink", "symlink", false},
		{"symlink replaced by symlink/tracked destination", "symlink", "symlink", true},
		{"dir replaced by file/absent destination", "dir", "file", false},
		{"dir replaced by file/tracked destination", "dir", "file", true},
		{"dir replaced by symlink/absent destination", "dir", "symlink", false},
		{"dir replaced by symlink/tracked destination", "dir", "symlink", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			cfg := model.RepoConfig{
				ID:            "repo",
				OverlayDir:    filepath.Join(tmp, "overlay"),
				OverlayDBPath: filepath.Join(tmp, "overlay.db"),
			}
			ov, err := overlaystore.New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ov.Close() })

			cachePath := filepath.Join(tmp, "blob")
			if err := os.WriteFile(cachePath, []byte("tracked content"), 0o644); err != nil {
				t.Fatal(err)
			}
			source := model.BaseNode{Path: "entry", Type: tc.sourceType, Mode: modes[tc.sourceType], ObjectOID: "source-oid"}
			nodes := map[string]model.BaseNode{source.Path: source}
			kids := map[string][]model.BaseNode{".": {source}}
			if tc.sourceType == "dir" {
				child := model.BaseNode{Path: "entry/child.txt", Type: "file", Mode: 0o100644, ObjectOID: "child-oid"}
				nodes[child.Path] = child
				kids[source.Path] = []model.BaseNode{child}
			}
			destination := "moved"
			if tc.trackedDestination {
				destination = "destination.txt"
				tracked := model.BaseNode{Path: destination, Type: "file", Mode: 0o100644, ObjectOID: "destination-oid"}
				nodes[destination] = tracked
				kids["."] = append(kids["."], tracked)
			}
			snapshot := &fakeSnapshot{nodes: nodes, kids: kids}
			resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
			resolver.SetGeneration(1)
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeBatchHydrator{path: cachePath}}

			if tc.sourceType == "dir" {
				if err := engineUnlink(ctx, engine, "entry/child.txt"); err != nil {
					t.Fatal(err)
				}
				if err := engine.Rmdir(ctx, "entry"); err != nil {
					t.Fatal(err)
				}
			} else if err := engineUnlink(ctx, engine, "entry"); err != nil {
				t.Fatal(err)
			}
			if tc.replacementType == "symlink" {
				if err := engine.Symlink(ctx, "entry", "elsewhere"); err != nil {
					t.Fatal(err)
				}
			} else if err := engine.Create(ctx, "entry", 0o644); err != nil {
				t.Fatal(err)
			}
			if err := engineRename(ctx, engine, "entry", destination); err != nil {
				t.Fatal(err)
			}

			moved, err := resolver.ResolvePath(destination)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolvedNodeType(moved); got != tc.replacementType {
				t.Fatalf("destination type = %q, want %q", got, tc.replacementType)
			}
			if n, err := resolver.ResolvePath("entry"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source after rename = (%+v, %v), want not exist", n, err)
			}
			if err := ov.Reconcile(ctx, func(path string) (model.BaseNode, bool) {
				node, ok := snapshot.nodes[path]
				return node, ok
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := resolver.ResolvePath("entry"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source after reconciliation = (%+v, %v), want not exist", n, err)
			}
			entries, err := resolver.ReaddirTyped(ctx, ".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name == "entry" {
					t.Fatal("tracked source reappeared in root directory listing")
				}
			}
		})
	}
}

func TestRenameChainHidesReplacedTrackedIntermediate(t *testing.T) {
	for _, tc := range []struct {
		name               string
		unlinkIntermediate bool
	}{
		{"unlinked intermediate", true},
		{"replaced intermediate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			cfg := model.RepoConfig{
				ID:            "repo",
				OverlayDir:    filepath.Join(tmp, "overlay"),
				OverlayDBPath: filepath.Join(tmp, "overlay.db"),
			}
			ov, err := overlaystore.New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ov.Close() })

			const content = "first tracked content"
			cachePath := filepath.Join(tmp, "blob")
			if err := os.WriteFile(cachePath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			first := model.BaseNode{Path: "first.txt", Type: "file", Mode: 0o100644, ObjectOID: "first-oid"}
			intermediate := model.BaseNode{Path: "intermediate.txt", Type: "file", Mode: 0o100644, ObjectOID: "intermediate-oid"}
			snapshot := &fakeSnapshot{
				nodes: map[string]model.BaseNode{first.Path: first, intermediate.Path: intermediate},
				kids:  map[string][]model.BaseNode{".": {first, intermediate}},
			}
			resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
			resolver.SetGeneration(1)
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeBatchHydrator{path: cachePath}}

			if tc.unlinkIntermediate {
				if err := engineUnlink(ctx, engine, intermediate.Path); err != nil {
					t.Fatal(err)
				}
			}
			if err := engineRename(ctx, engine, first.Path, intermediate.Path); err != nil {
				t.Fatal(err)
			}
			if err := engineRename(ctx, engine, intermediate.Path, "final.txt"); err != nil {
				t.Fatal(err)
			}
			if err := ov.Reconcile(ctx, func(path string) (model.BaseNode, bool) {
				node, ok := snapshot.nodes[path]
				return node, ok
			}); err != nil {
				t.Fatal(err)
			}

			for _, path := range []string{first.Path, intermediate.Path} {
				if n, err := resolver.ResolvePath(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s after reconciliation = (%+v, %v), want not exist", path, n, err)
				}
			}
			entries, err := resolver.ReaddirTyped(ctx, ".")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name != "final.txt" {
				t.Fatalf("root entries = %+v, want only final.txt", entries)
			}
			got, err := engineRead(ctx, engine, "final.txt", 0, make([]byte, len(content)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Fatalf("final content = %q, want %q", got, content)
			}
		})
	}
}

func TestRenameBaseDirectoryHydratesBeforeMutatingOverlay(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	firstCache := filepath.Join(tmp, "first")
	secondCache := filepath.Join(tmp, "second")
	if err := os.WriteFile(firstCache, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondCache, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "first"},
			"src/b.txt": {Path: "src/b.txt", Type: "file", Mode: 0o100644, ObjectOID: "second"},
		},
		kids: map[string][]model.BaseNode{
			"src": {
				{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "first"},
				{Path: "src/b.txt", Type: "file", Mode: 0o100644, ObjectOID: "second"},
			},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	hydrationFailure := errors.New("hydrate second blob")
	hydrator := &fakeBatchHydrator{
		pathsByOID:  map[string]string{"first": firstCache, "second": secondCache},
		errorsByOID: map[string]error{"second": hydrationFailure},
	}
	engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: hydrator}

	if err := engineRename(context.Background(), engine, "src", "dst"); !errors.Is(err, hydrationFailure) {
		t.Fatalf("Rename error = %v, want hydration failure", err)
	}
	entries, err := ov.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed rename left partial overlay entries: %+v", entries)
	}
	for _, path := range []string{"src", "src/a.txt", "src/b.txt"} {
		if _, err := resolver.ResolvePath(path); err != nil {
			t.Fatalf("source path %q after failed rename: %v", path, err)
		}
	}
	if _, err := resolver.ResolvePath("dst"); !os.IsNotExist(err) {
		t.Fatalf("destination after failed rename error = %v, want not exist", err)
	}

	delete(hydrator.errorsByOID, "second")
	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatalf("retry Rename: %v", err)
	}
	for path, want := range map[string]string{"dst/a.txt": "first", "dst/b.txt": "second"} {
		got, err := engineRead(context.Background(), engine, path, 0, make([]byte, 64))
		if err != nil {
			t.Fatalf("read %q after retry: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("read %q after retry = %q, want %q", path, got, want)
		}
	}
}

func TestRenameBaseDirectoryDoesNotResurrectReplacedDestinationDescendants(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":                   {Path: "src", Type: "dir", Mode: 0o40000},
			"src/overlap":           {Path: "src/overlap", Type: "dir", Mode: 0o40000},
			"dst":                   {Path: "dst", Type: "dir", Mode: 0o40000},
			"dst/gone.txt":          {Path: "dst/gone.txt", Type: "file", Mode: 0o100644, ObjectOID: "gone"},
			"dst/overlap":           {Path: "dst/overlap", Type: "dir", Mode: 0o40000},
			"dst/overlap/stale.txt": {Path: "dst/overlap/stale.txt", Type: "file", Mode: 0o100644, ObjectOID: "stale"},
		},
		kids: map[string][]model.BaseNode{
			"src": {
				{Path: "src/overlap", Type: "dir", Mode: 0o40000},
			},
			"dst": {
				{Path: "dst/gone.txt", Type: "file", Mode: 0o100644, ObjectOID: "gone"},
				{Path: "dst/overlap", Type: "dir", Mode: 0o40000},
			},
			"dst/overlap": {
				{Path: "dst/overlap/stale.txt", Type: "file", Mode: 0o100644, ObjectOID: "stale"},
			},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: &fakeBatchHydrator{},
	}
	if err := ov.Remove(context.Background(), "dst"); err != nil {
		t.Fatal(err)
	}

	if err := engineRename(context.Background(), engine, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolvePath("dst/overlap"); err != nil {
		t.Fatalf("resolve incoming overlapping directory: %v", err)
	}
	for _, path := range []string{"dst/gone.txt", "dst/overlap/stale.txt"} {
		if _, err := resolver.ResolvePath(path); !os.IsNotExist(err) {
			t.Fatalf("replaced destination path %q resolve error = %v, want not exist", path, err)
		}
	}
}

func TestRenameDirectoryRejectsDestinationChildCreatedDuringHydration(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("tracked content"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"},
		},
		kids: map[string][]model.BaseNode{
			"src": {{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"}},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	hydrator := &blockingRenameHydrator{
		fakeBatchHydrator: &fakeBatchHydrator{path: cachePath},
		started:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: hydrator,
	}
	if err := engine.Mkdir(context.Background(), "dst", 0o755); err != nil {
		t.Fatal(err)
	}

	renameDone := make(chan error, 1)
	go func() {
		renameDone <- engineRename(context.Background(), engine, "src", "dst")
	}()
	select {
	case <-hydrator.started:
	case err := <-renameDone:
		t.Fatalf("rename returned before hydration: %v", err)
	}

	if err := engine.Mkdir(context.Background(), "dst/concurrent", 0o755); err != nil {
		t.Fatalf("concurrent destination mkdir: %v", err)
	}
	close(hydrator.proceed)
	if err := <-renameDone; !os.IsExist(err) {
		t.Fatalf("Rename error = %v, want os.ErrExist", err)
	}

	for _, path := range []string{"src", "src/a.txt", "dst", "dst/concurrent"} {
		if _, err := resolver.ResolvePath(path); err != nil {
			t.Fatalf("resolve preserved path %q: %v", path, err)
		}
	}
	if _, err := resolver.ResolvePath("dst/a.txt"); !os.IsNotExist(err) {
		t.Fatalf("destination source child resolve error = %v, want not exist", err)
	}
}

// The helpers below run an Engine method through Engine.Do, as the adapter
// does, without the adapter's namespace lock or open handles.

func engineRename(ctx context.Context, e *Engine, oldPath, newPath string) error {
	return e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		return e.Rename(ctx, b, oldPath, newPath, nil)
	})
}

func engineUnlink(ctx context.Context, e *Engine, path string) error {
	return e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		return e.Unlink(ctx, b, path, nil)
	})
}

func engineWrite(ctx context.Context, e *Engine, path string, off int64, data []byte) (n int, err error) {
	err = e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		n, err = e.Write(ctx, b, path, off, data)
		return err
	})
	return n, err
}

func engineRead(ctx context.Context, e *Engine, path string, off int64, dst []byte) (data []byte, err error) {
	err = e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		data, err = e.Read(ctx, b, path, off, dst)
		return err
	})
	return data, err
}

func engineSetMtime(ctx context.Context, e *Engine, path string, t time.Time) error {
	return e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		_, err := e.SetAttributes(ctx, b, path, nil, nil, &t)
		return err
	})
}

func handleRead(ctx context.Context, e *Engine, fh *FileHandle, gen, off int64, dst []byte) (data []byte, err error) {
	err = e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		data, err = fh.read(ctx, e, b, gen, off, dst)
		return err
	})
	return data, err
}
