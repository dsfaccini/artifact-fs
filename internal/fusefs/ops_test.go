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

// fakeHydrator serves every blob from the file at path, or at pathsByOID by
// object ID, with size, and every ReadBlob with data. err fails every call,
// and errorsByOID fails the hydration of one blob. It counts the hydrations
// and the ReadBlob calls, and records each queued task.
type fakeHydrator struct {
	tasks         []model.HydrationTask
	calls         int
	readBlobCalls int
	path          string
	pathsByOID    map[string]string
	size          int64
	data          []byte
	err           error
	errorsByOID   map[string]error
}

type generationSnapshot struct {
	nodes map[int64]map[string]model.BaseNode
	kids  map[int64]map[string][]model.BaseNode
}

// blockingCopyOverlay holds each copy-on-write until continueCopy closes.
type blockingCopyOverlay struct {
	model.OverlayStore
	beforeCopy   chan struct{}
	continueCopy chan struct{}
}

type blockingRenameHydrator struct {
	*fakeHydrator
	started chan struct{}
	proceed chan struct{}
}

func (f *fakeHydrator) EnqueueBatch(tasks []model.HydrationTask) {
	f.tasks = append(f.tasks, tasks...)
}

func (f *fakeHydrator) EnsureHydrated(_ context.Context, _ model.RepoPaths, node model.BaseNode) (string, int64, error) {
	f.calls++
	if f.err != nil {
		return "", 0, f.err
	}
	if err := f.errorsByOID[node.ObjectOID]; err != nil {
		return "", 0, err
	}
	if f.pathsByOID != nil {
		return f.pathsByOID[node.ObjectOID], f.size, nil
	}
	return f.path, f.size, nil
}

func (f *fakeHydrator) OpenHydrated(ctx context.Context, repo model.RepoPaths, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := f.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (f *fakeHydrator) ReadBlob(context.Context, model.RepoPaths, model.BaseNode, int64) ([]byte, error) {
	f.readBlobCalls++
	return f.data, f.err
}

func (h *blockingRenameHydrator) OpenHydrated(ctx context.Context, repo model.RepoPaths, node model.BaseNode) (*os.File, int64, error) {
	close(h.started)
	select {
	case <-h.proceed:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return h.fakeHydrator.OpenHydrated(ctx, repo, node)
}

func (g *generationSnapshot) LookupNode(_ context.Context, gen int64, path string) (model.BaseNode, bool, error) {
	n, ok := g.nodes[gen][path]
	return n, ok, nil
}

func (g *generationSnapshot) ListChildren(gen int64, path string) ([]model.BaseNode, error) {
	return g.kids[gen][path], nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWriteFrom(ctx context.Context, repo model.RepoPaths, path string, base model.BaseNode, src *os.File) (model.OverlayEntry, error) {
	close(o.beforeCopy)
	<-o.continueCopy
	return o.OverlayStore.EnsureCopyOnWriteFrom(ctx, repo, path, base, src)
}

func TestPrefetchDirBatchesReaddirMetadata(t *testing.T) {
	hydrator := &fakeHydrator{}
	engine := &Engine{Repo: model.RepoPaths{ID: "repo"}, Hydrator: hydrator}

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
	h := &fakeHydrator{}
	engine := &Engine{Repo: model.RepoPaths{ID: "repo"}, Hydrator: h}
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
	h := &fakeHydrator{path: cachePath}
	overlay := newOverlay(t)
	engine := &Engine{
		Repo:     model.RepoPaths{ID: "repo"},
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
	h := &fakeHydrator{pathsByOID: map[string]string{"first": firstCache, "second": secondCache}}
	overlay := newOverlay(t)
	resolver := &Resolver{
		Snapshot: &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{
			1: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "first"}},
			2: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "second"}},
		}},
		Overlay: overlay,
	}
	resolver.SetGeneration(1)
	engine := &Engine{Repo: model.RepoPaths{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
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
	if err := os.WriteFile(baseCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeHydrator{path: baseCache}
	overlay := &blockingCopyOverlay{OverlayStore: newOverlay(t), beforeCopy: make(chan struct{}), continueCopy: make(chan struct{})}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob", Mode: 0o644}}}, overlay.OverlayStore)
	engine := &Engine{Repo: model.RepoPaths{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
	fs := NewArtifactFuse(model.RepoPaths{ID: "repo"}, engine)
	fh := heldHandle(fs, "file.txt")
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
		writeDone <- fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Inode: fh.inode.ID, Handle: 1, Data: []byte("new")})
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

// failingWriteOverlay writes the data and then fails, as a write that runs
// out of space does.
type failingWriteOverlay struct{ model.OverlayStore }

func (o failingWriteOverlay) WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error) {
	n, err := o.OverlayStore.WriteFile(ctx, path, off, data)
	if err != nil {
		return n, err
	}
	return n, syscall.ENOSPC
}

func TestFailedWriteThatPromotedClosesCachedBase(t *testing.T) {
	tmp := t.TempDir()
	baseCache := filepath.Join(tmp, "base")
	if err := os.WriteFile(baseCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := failingWriteOverlay{newOverlay(t)}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob", Mode: 0o644}}}, overlay.OverlayStore)
	engine := &Engine{Repo: model.RepoPaths{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: &fakeHydrator{path: baseCache}}
	fs := NewArtifactFuse(model.RepoPaths{ID: "repo"}, engine)
	fh := heldHandle(fs, "file.txt")
	defer fh.closeCachedFile()
	if got, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3)); err != nil || string(got) != "old" {
		t.Fatalf("first read = %q, %v", got, err)
	}
	err := fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Inode: fh.inode.ID, Handle: 1, Data: []byte("new")})
	if got := kernelErrno(err); got != syscall.ENOSPC {
		t.Fatalf("write errno = %v, want ENOSPC", got)
	}
	if got, err := handleRead(context.Background(), engine, fh, 0, 0, make([]byte, 3)); err != nil || string(got) != "new" {
		t.Fatalf("read after the failed write = %q, %v; want the overlay's new", got, err)
	}
}

func TestRenameBaseDirectoryHydratesAndMovesContents(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoPaths{
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
		Hydrator: &fakeHydrator{path: cachePath},
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
			cfg := model.RepoPaths{
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
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeHydrator{path: cachePath}}

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
			if err := reconcileOverlay(ctx, ov, func(path string) (model.BaseNode, bool) {
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
			if err := reconcileOverlay(ctx, ov, func(path string) (model.BaseNode, bool) {
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
			cfg := model.RepoPaths{
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
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeHydrator{path: cachePath}}

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
			if err := reconcileOverlay(ctx, ov, func(path string) (model.BaseNode, bool) {
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
			cfg := model.RepoPaths{
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
			engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: &fakeHydrator{path: cachePath}}

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
			if err := reconcileOverlay(ctx, ov, func(path string) (model.BaseNode, bool) {
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
	cfg := model.RepoPaths{
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
	hydrator := &fakeHydrator{
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
	cfg := model.RepoPaths{
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
		Hydrator: &fakeHydrator{},
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
	cfg := model.RepoPaths{
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
		fakeHydrator: &fakeHydrator{path: cachePath},
		started:      make(chan struct{}),
		proceed:      make(chan struct{}),
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

// heldHandle registers handle 1 on path, as an open through the kernel
// leaves it: the kernel holds the path's inode, and the path maps to it.
func heldHandle(fs *ArtifactFuse, path string) *FileHandle {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fh := &FileHandle{inode: fs.allocInode(path, Attrs{Type: "file", Mode: 0o644}), path: path}
	fs.fileHandles[1] = fh
	return fh
}

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

func engineSetMtime(ctx context.Context, e *Engine, path, typ string, t time.Time) error {
	return e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		_, err := e.SetAttributes(ctx, b, path, typ, nil, 0, nil, &t, nil)
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

// reconcileOverlay reconciles ov with the base lookup reads, with no readers
// to exclude.
func reconcileOverlay(ctx context.Context, ov model.OverlayStore, lookup func(string) (model.BaseNode, bool)) error {
	return ov.ReconcileChecked(ctx, 0, func(path string) (model.BaseNode, bool, error) {
		n, ok := lookup(path)
		return n, ok, nil
	}, model.SwitchedPaths{}, nil, nil)
}
