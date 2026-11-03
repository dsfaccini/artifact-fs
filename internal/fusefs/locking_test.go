//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/dsfaccini/fuse/fuseops"
)

// gatedHydrator serves blobs from files named by object ID in dir and counts
// every call. Once armed, its next call blocks until release is closed or the
// call's context ends.
type gatedHydrator struct {
	dir string

	mu      sync.Mutex
	calls   int
	armed   bool
	started chan struct{}
	release chan struct{}
}

func (h *gatedHydrator) arm() (started, release chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed = true
	h.started = make(chan struct{})
	h.release = make(chan struct{})
	return h.started, h.release
}

func (h *gatedHydrator) fetches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *gatedHydrator) enter(ctx context.Context) error {
	h.mu.Lock()
	h.calls++
	armed := h.armed
	h.armed = false
	started, release := h.started, h.release
	h.mu.Unlock()
	if !armed {
		return nil
	}
	close(started)
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *gatedHydrator) Enqueue(model.HydrationTask)        {}
func (h *gatedHydrator) EnqueueBatch([]model.HydrationTask) {}
func (h *gatedHydrator) QueueDepth(model.RepoID) int        { return 0 }

func (h *gatedHydrator) EnsureHydrated(ctx context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	if err := h.enter(ctx); err != nil {
		return "", 0, err
	}
	path := filepath.Join(h.dir, node.ObjectOID)
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return path, st.Size(), nil
}

func (h *gatedHydrator) OpenHydrated(ctx context.Context, _ model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	if err := h.enter(ctx); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(filepath.Join(h.dir, node.ObjectOID))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (h *gatedHydrator) ReadBlob(ctx context.Context, _ model.RepoConfig, node model.BaseNode, _ int64) ([]byte, error) {
	if err := h.enter(ctx); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(h.dir, node.ObjectOID))
}

// trackedFixture mounts nothing: it serves a snapshot whose generation 1 holds
// file.txt, link, and dir/inner.txt, through the adapter, a real overlay store,
// and a gatedHydrator. other.txt exists only in the overlay.
type trackedFixture struct {
	store    *overlaystore.Store
	snapshot *generationSnapshot
	resolver *Resolver
	engine   *Engine
	fs       *ArtifactFuse
	hydrator *gatedHydrator
}

func newTrackedFixture(t *testing.T) trackedFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blobs := filepath.Join(tmp, "blobs")
	if err := os.Mkdir(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	for oid, content := range map[string]string{"file-1": "file one", "file-2": "file two", "inner-1": "inner", "link-1": "file.txt"} {
		if err := os.WriteFile(filepath.Join(blobs, oid), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "file-1", SizeState: "unknown"}
	link := model.BaseNode{RepoID: repo.ID, Path: "link", Type: "symlink", Mode: 0o120000, ObjectOID: "link-1", SizeState: "unknown"}
	dir := model.BaseNode{RepoID: repo.ID, Path: "dir", Type: "dir", Mode: 0o40000, SizeState: "known"}
	inner := model.BaseNode{RepoID: repo.ID, Path: "dir/inner.txt", Type: "file", Mode: 0o100644, ObjectOID: "inner-1", SizeState: "known", SizeBytes: 5}
	file2 := file
	file2.ObjectOID = "file-2"
	snapshot := &generationSnapshot{
		nodes: map[int64]map[string]model.BaseNode{
			1: {".": {Path: ".", Type: "dir", Mode: 0o40000}, file.Path: file, link.Path: link, dir.Path: dir, inner.Path: inner},
			2: {".": {Path: ".", Type: "dir", Mode: 0o40000}, file.Path: file2, link.Path: link, dir.Path: dir, inner.Path: inner},
		},
		kids: map[int64]map[string][]model.BaseNode{
			1: {".": {file, link, dir}, dir.Path: {inner}},
			2: {".": {file2, link, dir}, dir.Path: {inner}},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: store}
	resolver.SetGeneration(1)
	h := &gatedHydrator{dir: blobs}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: store, Hydrator: h}
	if _, err := store.CreateFile(ctx, "other.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	return trackedFixture{store: store, snapshot: snapshot, resolver: resolver, engine: engine, fs: NewArtifactFuse(repo, engine), hydrator: h}
}

func (f trackedFixture) lookup(t *testing.T, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := f.fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("LookUpInode(%q): %v", name, err)
	}
	return op.Entry.Child
}

func (f trackedFixture) open(t *testing.T, inode fuseops.InodeID) fuseops.HandleID {
	t.Helper()
	op := &fuseops.OpenFileOp{Inode: inode}
	if err := f.fs.OpenFile(context.Background(), op); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
	})
	return op.Handle
}

func (f trackedFixture) read(t *testing.T, inode fuseops.InodeID, handle fuseops.HandleID) string {
	t.Helper()
	op := &fuseops.ReadFileOp{Inode: inode, Handle: handle, Size: 64, Dst: make([]byte, 64)}
	if err := f.fs.ReadFile(context.Background(), op); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(op.Dst[:op.BytesRead])
}

// kernelErrno is the errno the fuse library sends the kernel for an op error:
// the first syscall.Errno in its chain, or EIO when it has none.
func kernelErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EIO
}

func TestUnlinkWithoutOpenHandleFetchesNothing(t *testing.T) {
	f := newTrackedFixture(t)
	dir := f.lookup(t, fuseops.RootInodeID, "dir")
	before := f.hydrator.fetches()

	for _, op := range []*fuseops.UnlinkOp{
		{Parent: fuseops.RootInodeID, Name: "file.txt"},
		{Parent: fuseops.RootInodeID, Name: "link"},
		{Parent: dir, Name: "inner.txt"},
	} {
		if err := f.fs.Unlink(context.Background(), op); err != nil {
			t.Fatalf("Unlink(%s): %v", op.Name, err)
		}
	}

	if got := f.hydrator.fetches() - before; got != 0 {
		t.Fatalf("Unlink without an open handle fetched %d blobs, want 0", got)
	}
	for _, path := range []string{"file.txt", "link", "dir/inner.txt"} {
		entry, ok, err := f.store.Lookup(context.Background(), path)
		if err != nil || !ok || entry.Kind != model.OverlayKindDelete {
			t.Fatalf("overlay %s = (%+v, %t, %v), want delete entry", path, entry, ok, err)
		}
	}
}

func TestUnlinkKeepsOpenHandleContent(t *testing.T) {
	f := newTrackedFixture(t)
	inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
	handle := f.open(t, inode)

	if err := f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if got := f.read(t, inode, handle); got != "file one" {
		t.Fatalf("read after unlink = %q, want file one", got)
	}
	if err := f.fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Inode: inode, Handle: handle, Data: []byte("FILE")}); err != nil {
		t.Fatalf("WriteFile after unlink: %v", err)
	}
	if got := f.read(t, inode, handle); got != "FILE one" {
		t.Fatalf("read after write = %q, want FILE one", got)
	}
	if _, err := f.resolver.ResolvePath("file.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve after unlink = %v, want not exist", err)
	}
}

// TestHydrationHoldsNoLock blocks each op that hydrates inside its hydration,
// then runs a HEAD switch, a resolve, and an Unlink, which takes the namespace
// lock exclusively. A regression leaves them waiting on the blocked op, so the
// test hangs until go test -timeout.
func TestHydrationHoldsNoLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		// op runs the FUSE op under test; prepare runs before the hydrator is armed.
		prepare func(t *testing.T, f trackedFixture) func(ctx context.Context) error
	}{
		{"LookUpInode", func(t *testing.T, f trackedFixture) func(context.Context) error {
			return func(ctx context.Context) error {
				return f.fs.LookUpInode(ctx, &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
			}
		}},
		{"GetInodeAttributes", func(t *testing.T, f trackedFixture) func(context.Context) error {
			f.snapshot.nodes[1]["file.txt"] = withKnownSize(f.snapshot.nodes[1]["file.txt"], 8)
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			f.snapshot.nodes[1]["file.txt"] = withUnknownSize(f.snapshot.nodes[1]["file.txt"])
			return func(ctx context.Context) error {
				return f.fs.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: inode})
			}
		}},
		{"OpenFile", func(t *testing.T, f trackedFixture) func(context.Context) error {
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			return func(ctx context.Context) error {
				op := &fuseops.OpenFileOp{Inode: inode}
				if err := f.fs.OpenFile(ctx, op); err != nil {
					return err
				}
				return f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
			}
		}},
		{"ReadFile", func(t *testing.T, f trackedFixture) func(context.Context) error {
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			handle := f.open(t, inode)
			f.resolver.SetGeneration(2)
			f.snapshot.nodes[2]["file.txt"] = withKnownSize(f.snapshot.nodes[2]["file.txt"], 8)
			if err := f.fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: inode}); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				return f.fs.ReadFile(ctx, &fuseops.ReadFileOp{Inode: inode, Handle: handle, Size: 8, Dst: make([]byte, 8)})
			}
		}},
		{"WriteFile", func(t *testing.T, f trackedFixture) func(context.Context) error {
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			handle := f.open(t, inode)
			return func(ctx context.Context) error {
				return f.fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: inode, Handle: handle, Data: []byte("x")})
			}
		}},
		{"SetInodeAttributes", func(t *testing.T, f trackedFixture) func(context.Context) error {
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			size := uint64(2)
			return func(ctx context.Context) error {
				return f.fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Size: &size})
			}
		}},
		{"ReadSymlink", func(t *testing.T, f trackedFixture) func(context.Context) error {
			f.snapshot.nodes[1]["link"] = withKnownSize(f.snapshot.nodes[1]["link"], 8)
			inode := f.lookup(t, fuseops.RootInodeID, "link")
			return func(ctx context.Context) error {
				return f.fs.ReadSymlink(ctx, &fuseops.ReadSymlinkOp{Inode: inode})
			}
		}},
		{"Unlink with an open handle", func(t *testing.T, f trackedFixture) func(context.Context) error {
			f.open(t, f.lookup(t, fuseops.RootInodeID, "file.txt"))
			return func(ctx context.Context) error {
				return f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
			}
		}},
		{"Rename file", func(t *testing.T, f trackedFixture) func(context.Context) error {
			return func(ctx context.Context) error {
				return f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "file.txt", NewParent: fuseops.RootInodeID, NewName: "moved.txt"})
			}
		}},
		{"Rename directory", func(t *testing.T, f trackedFixture) func(context.Context) error {
			return func(ctx context.Context) error {
				return f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "dir", NewParent: fuseops.RootInodeID, NewName: "moved"})
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackedFixture(t)
			op := tc.prepare(t, f)
			started, release := f.hydrator.arm()
			done := make(chan error, 1)
			go func() { done <- op(context.Background()) }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("op returned before hydration: %v", err)
			}

			if err := f.resolver.Transition(func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := f.resolver.ResolvePath("other.txt"); err != nil {
				t.Fatalf("ResolvePath during hydration: %v", err)
			}
			if err := f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "other.txt"}); err != nil {
				t.Fatalf("Unlink during hydration: %v", err)
			}

			close(release)
			if err := <-done; err != nil {
				t.Fatalf("op after hydration: %v", err)
			}
		})
	}
}

func withKnownSize(n model.BaseNode, size int64) model.BaseNode {
	n.SizeState, n.SizeBytes = "known", size
	return n
}

func withUnknownSize(n model.BaseNode) model.BaseNode {
	n.SizeState, n.SizeBytes = "unknown", 0
	return n
}

// faultyOverlay fails the overlay calls whose error is set.
type faultyOverlay struct {
	model.OverlayStore
	createErr, removeErr, truncateErr, writeErr, mkdirErr error
}

func (o *faultyOverlay) CreateFile(ctx context.Context, path string, mode uint32) (model.OverlayEntry, error) {
	if o.createErr != nil {
		return model.OverlayEntry{}, o.createErr
	}
	return o.OverlayStore.CreateFile(ctx, path, mode)
}

func (o *faultyOverlay) Remove(ctx context.Context, path string) error {
	if o.removeErr != nil {
		return o.removeErr
	}
	return o.OverlayStore.Remove(ctx, path)
}

func (o *faultyOverlay) Truncate(ctx context.Context, path string, size int64) error {
	if o.truncateErr != nil {
		return o.truncateErr
	}
	return o.OverlayStore.Truncate(ctx, path, size)
}

func (o *faultyOverlay) WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error) {
	if o.writeErr != nil {
		return 0, o.writeErr
	}
	return o.OverlayStore.WriteFile(ctx, path, off, data)
}

func (o *faultyOverlay) Mkdir(ctx context.Context, path string, mode uint32) error {
	if o.mkdirErr != nil {
		return o.mkdirErr
	}
	return o.OverlayStore.Mkdir(ctx, path, mode)
}

func TestOpErrorsReachKernelAsTheirErrno(t *testing.T) {
	pathErr := func(errno syscall.Errno) error {
		return &os.PathError{Op: "open", Path: "/overlay/upper/.artifact-fs-entry-1", Err: errno}
	}
	for _, tc := range []struct {
		name    string
		overlay faultyOverlay
		op      func(t *testing.T, f trackedFixture) error
		want    syscall.Errno
	}{
		{"create", faultyOverlay{createErr: pathErr(syscall.ENOSPC)}, func(t *testing.T, f trackedFixture) error {
			return f.fs.CreateFile(context.Background(), &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "new.txt", Mode: 0o644})
		}, syscall.ENOSPC},
		{"remove", faultyOverlay{removeErr: pathErr(syscall.EROFS)}, func(t *testing.T, f trackedFixture) error {
			return f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "other.txt"})
		}, syscall.EROFS},
		{"remove without errno", faultyOverlay{removeErr: errors.New("database is locked")}, func(t *testing.T, f trackedFixture) error {
			return f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "other.txt"})
		}, syscall.EIO},
		{"unlink missing", faultyOverlay{}, func(t *testing.T, f trackedFixture) error {
			return f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "missing.txt"})
		}, syscall.ENOENT},
		{"mkdir", faultyOverlay{mkdirErr: pathErr(syscall.EDQUOT)}, func(t *testing.T, f trackedFixture) error {
			return f.fs.MkDir(context.Background(), &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "new", Mode: 0o755})
		}, syscall.EDQUOT},
		{"truncate", faultyOverlay{truncateErr: pathErr(syscall.EFBIG)}, func(t *testing.T, f trackedFixture) error {
			size := uint64(1 << 40)
			inode := f.lookup(t, fuseops.RootInodeID, "other.txt")
			return f.fs.SetInodeAttributes(context.Background(), &fuseops.SetInodeAttributesOp{Inode: inode, Size: &size})
		}, syscall.EFBIG},
		{"write", faultyOverlay{writeErr: pathErr(syscall.ENOSPC)}, func(t *testing.T, f trackedFixture) error {
			inode := f.lookup(t, fuseops.RootInodeID, "other.txt")
			handle := f.open(t, inode)
			return f.fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Inode: inode, Handle: handle, Data: []byte("x")})
		}, syscall.ENOSPC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackedFixture(t)
			overlay := tc.overlay
			overlay.OverlayStore = f.store
			f.resolver.Overlay = &overlay
			f.engine.Overlay = &overlay
			if got := kernelErrno(tc.op(t, f)); got != tc.want {
				t.Fatalf("kernel errno = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanceledHydrationReturnsEINTR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f trackedFixture) func(ctx context.Context) error
	}{
		{"LookUpInode", func(t *testing.T, f trackedFixture) func(context.Context) error {
			return func(ctx context.Context) error {
				return f.fs.LookUpInode(ctx, &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
			}
		}},
		{"WriteFile", func(t *testing.T, f trackedFixture) func(context.Context) error {
			inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
			handle := f.open(t, inode)
			return func(ctx context.Context) error {
				return f.fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: inode, Handle: handle, Data: []byte("x")})
			}
		}},
		{"Unlink with an open handle", func(t *testing.T, f trackedFixture) func(context.Context) error {
			f.open(t, f.lookup(t, fuseops.RootInodeID, "file.txt"))
			return func(ctx context.Context) error {
				return f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackedFixture(t)
			op := tc.prepare(t, f)
			started, _ := f.hydrator.arm()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- op(ctx) }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("op returned before hydration: %v", err)
			}
			cancel()
			if got := kernelErrno(<-done); got != syscall.EINTR {
				t.Fatalf("kernel errno = %v, want EINTR", got)
			}
			if n, err := f.resolver.ResolvePath("file.txt"); err != nil || n.FromOverlay {
				t.Fatalf("file.txt after interrupted op = (%+v, %v), want base entry", n, err)
			}
		})
	}
}

func TestHEADSwitchDuringHydrationUsesNewBlob(t *testing.T) {
	f := newTrackedFixture(t)
	inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
	handle := f.open(t, inode)
	started, release := f.hydrator.arm()
	done := make(chan error, 1)
	go func() {
		done <- f.fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Inode: inode, Handle: handle, Data: []byte("F")})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("WriteFile returned before hydration: %v", err)
	}
	if err := f.resolver.Transition(func() error {
		f.resolver.SetGeneration(2)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entry, ok, err := f.store.Lookup(context.Background(), "file.txt")
	if err != nil || !ok {
		t.Fatalf("overlay file.txt = (%+v, %t, %v)", entry, ok, err)
	}
	if entry.SourceOID != "file-2" {
		t.Fatalf("overlay source = %q, want the blob of the new HEAD", entry.SourceOID)
	}
	got, err := os.ReadFile(entry.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "File two" {
		t.Fatalf("overlay content = %q, want the write over the new HEAD's blob", got)
	}
}

func TestRenameCopiesReplacedDestinationOnlyForOpenHandles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		open        bool
		wantFetches int
	}{
		{"no open handle", false, 1},
		{"open handle", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackedFixture(t)
			dir := f.lookup(t, fuseops.RootInodeID, "dir")
			var handle fuseops.HandleID
			if tc.open {
				f.snapshot.nodes[1]["file.txt"] = withKnownSize(f.snapshot.nodes[1]["file.txt"], 8)
				inode := f.lookup(t, fuseops.RootInodeID, "file.txt")
				handle = f.fs.nextHandleID
				f.fs.fileHandles[handle] = &FileHandle{inode: &InodeRef{ID: inode, Path: "file.txt"}, path: "file.txt"}
			}
			before := f.hydrator.fetches()

			if err := f.fs.Rename(context.Background(), &fuseops.RenameOp{OldParent: dir, OldName: "inner.txt", NewParent: fuseops.RootInodeID, NewName: "file.txt"}); err != nil {
				t.Fatalf("Rename: %v", err)
			}

			if got := f.hydrator.fetches() - before; got != tc.wantFetches {
				t.Fatalf("Rename fetched %d blobs, want %d", got, tc.wantFetches)
			}
			if got, err := engineRead(context.Background(), f.engine, "file.txt", 0, make([]byte, 16)); err != nil || string(got) != "inner" {
				t.Fatalf("file.txt after rename = (%q, %v), want inner", got, err)
			}
			if tc.open {
				fh := f.fs.fileHandles[handle]
				if got, err := handleRead(context.Background(), f.engine, fh, 0, 0, make([]byte, 16)); err != nil || string(got) != "file one" {
					t.Fatalf("replaced handle reads (%q, %v), want file one", got, err)
				}
			}
		})
	}
}
