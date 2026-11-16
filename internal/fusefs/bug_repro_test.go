//go:build !windows

package fusefs

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/jacobsa/fuse/fuseops"
)

// opsFixture drives ArtifactFuse ops over a real overlay store and a
// generationSnapshot whose generations the test publishes.
type opsFixture struct {
	t        *testing.T
	ctx      context.Context
	ov       *overlaystore.Store
	snap     *generationSnapshot
	resolver *Resolver
	engine   *Engine
	fs       *ArtifactFuse
	blobs    map[string]string
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", Name: "repo", GitDir: filepath.Join(tmp, "git"), OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	f := &opsFixture{t: t, ctx: t.Context(), ov: ov, blobs: map[string]string{}}
	f.snap = &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{}, kids: map[int64]map[string][]model.BaseNode{}}
	f.resolver = &Resolver{Snapshot: f.snap, Overlay: ov}
	f.engine = &Engine{Repo: cfg, Resolver: f.resolver, Overlay: ov, Hydrator: &fakeBatchHydrator{pathsByOID: f.blobs}}
	f.fs = NewArtifactFuse(cfg, f.resolver, f.engine)
	return f
}

func blobOID(data string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// publish adds generation g with the given files (path to content) and dirs.
func (f *opsFixture) publish(g int64, files map[string]string, dirs ...string) {
	f.t.Helper()
	nodes := map[string]model.BaseNode{".": {Path: ".", Type: "dir", Mode: 0o40000}}
	kids := map[string][]model.BaseNode{}
	for _, d := range dirs {
		n := model.BaseNode{Path: d, Type: "dir", Mode: 0o40000, SizeState: "known"}
		nodes[d] = n
		kids[filepath.Dir(d)] = append(kids[filepath.Dir(d)], n)
	}
	for p, content := range files {
		oid := blobOID(content)
		path := filepath.Join(f.t.TempDir(), oid)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
		f.blobs[oid] = path
		n := model.BaseNode{Path: p, Type: "file", Mode: 0o100644, ObjectOID: oid, SizeState: "known", SizeBytes: int64(len(content))}
		nodes[p] = n
		kids[filepath.Dir(p)] = append(kids[filepath.Dir(p)], n)
	}
	f.snap.nodes[g] = nodes
	f.snap.kids[g] = kids
}

// switchHEAD serves generation g the way Service.onHEADChanged does.
func (f *opsFixture) switchHEAD(g int64, commitTime int64) {
	f.t.Helper()
	err := f.resolver.Transition(func() error {
		if err := f.ov.ReconcileChecked(f.ctx, func(path string) (model.BaseNode, bool, error) {
			return f.snap.LookupNode(f.ctx, g, path)
		}); err != nil {
			return err
		}
		f.resolver.SetCommitTime(commitTime)
		f.resolver.SetGeneration(g)
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *opsFixture) lookup(parent fuseops.InodeID, name string) (*fuseops.LookUpInodeOp, error) {
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	return op, f.fs.LookUpInode(f.ctx, op)
}

func (f *opsFixture) mustLookup(parent fuseops.InodeID, name string) *fuseops.LookUpInodeOp {
	f.t.Helper()
	op, err := f.lookup(parent, name)
	if err != nil {
		f.t.Fatalf("lookup %s: %v", name, err)
	}
	return op
}

func (f *opsFixture) open(inode fuseops.InodeID) fuseops.HandleID {
	f.t.Helper()
	op := &fuseops.OpenFileOp{Inode: inode}
	if err := f.fs.OpenFile(f.ctx, op); err != nil {
		f.t.Fatalf("open: %v", err)
	}
	return op.Handle
}

func (f *opsFixture) read(h fuseops.HandleID) string {
	f.t.Helper()
	op := &fuseops.ReadFileOp{Handle: h, Offset: 0, Size: 64}
	if err := f.fs.ReadFile(f.ctx, op); err != nil {
		f.t.Fatalf("read: %v", err)
	}
	var out []byte
	for _, d := range op.Data {
		out = append(out, d...)
	}
	return string(out)
}

func (f *opsFixture) write(h fuseops.HandleID, data string) {
	f.t.Helper()
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Handle: h, Data: []byte(data)}); err != nil {
		f.t.Fatalf("write: %v", err)
	}
}

// CreateFile on a name that already exists replaces the file with an empty one.
func TestCreateFileKeepsAnExistingFile(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"base.txt": "base"})
	f.switchHEAD(1, 1000)
	if err := f.engine.Create(f.ctx, "ov.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Write(f.ctx, "ov.txt", 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, content string }{{"ov.txt", "hello"}, {"base.txt", "base"}} {
		err := f.fs.CreateFile(f.ctx, &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: c.name, Mode: 0o644})
		got, readErr := f.engine.Read(f.ctx, c.name, 0, 64)
		if readErr != nil {
			t.Fatalf("read %s: %v", c.name, readErr)
		}
		if !errors.Is(err, syscall.EEXIST) || string(got) != c.content {
			t.Errorf("CreateFile on existing %s returned %v and left it holding %q; want EEXIST and %q kept", c.name, err, got, c.content)
		}
	}
}

// MkDir and CreateSymlink on a name that already exists replace the file.
func TestMkDirAndCreateSymlinkKeepAnExistingFile(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"x": "file", "y": "file"})
	f.switchHEAD(1, 1000)
	mkdirErr := f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "x", Mode: 0o755})
	symlinkErr := f.fs.CreateSymlink(f.ctx, &fuseops.CreateSymlinkOp{Parent: fuseops.RootInodeID, Name: "y", Target: "z"})
	for _, c := range []struct {
		op, name string
		err      error
	}{{"MkDir", "x", mkdirErr}, {"CreateSymlink", "y", symlinkErr}} {
		n, err := f.resolver.ResolvePath(c.name)
		if err != nil {
			t.Fatalf("resolve %s: %v", c.name, err)
		}
		if typ := resolvedNodeType(n); !errors.Is(c.err, syscall.EEXIST) || typ != "file" {
			t.Errorf("%s on existing file %s returned %v and made it a %s; want EEXIST and %s kept as a file", c.op, c.name, c.err, typ, c.name)
		}
	}
}

// A directory that only an untracked file keeps after a HEAD change is listed, but its lookup fails ENOENT.
func TestLookUpFindsAListedImplicitDirectory(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"d/tracked.txt": "t"}, "d")
	f.publish(2, map[string]string{"other.txt": "o"})
	f.switchHEAD(1, 1000)
	d := f.mustLookup(fuseops.RootInodeID, "d")
	if err := f.fs.CreateFile(f.ctx, &fuseops.CreateFileOp{Parent: d.Entry.Child, Name: "untracked.txt", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	f.switchHEAD(2, 2000)
	entries, err := f.resolver.ReaddirTyped(f.ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e ReaddirEntry) bool { return e.Name == "d" }) {
		t.Fatalf("root listing %v does not show d, which holds untracked.txt", entries)
	}
	op, err := f.lookup(fuseops.RootInodeID, "d")
	if err != nil {
		t.Fatalf("the root listing shows d, but LookUpInode(d) returned %v; want d to resolve as a directory", err)
	}
	if !op.Entry.Attributes.Mode.IsDir() {
		t.Errorf("LookUpInode(d) returned mode %v; want a directory", op.Entry.Attributes.Mode)
	}
}

// After git checkout to a branch without d, an untracked file left in d cannot be reached.
func TestCheckoutKeepsAnUntrackedFileReachable(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"d/tracked.txt": "t"}, "d")
	f.publish(2, map[string]string{"other.txt": "o"})
	f.switchHEAD(1, 1000)
	d := f.mustLookup(fuseops.RootInodeID, "d").Entry.Child
	c := &fuseops.CreateFileOp{Parent: d, Name: "untracked.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	f.write(c.Handle, "untracked")
	// The ops git checkout sends when the target branch lacks d: unlink the
	// tracked file, try to remove d, then move HEAD.
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: d, Name: "tracked.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.RmDir(f.ctx, &fuseops.RmDirOp{Parent: fuseops.RootInodeID, Name: "d"}); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("rmdir d returned %v; want ENOTEMPTY", err)
	}
	f.switchHEAD(2, 2000)
	if err := f.fs.ForgetInode(f.ctx, &fuseops.ForgetInodeOp{Inode: d, N: 1}); err != nil {
		t.Fatal(err)
	}
	dir, err := f.lookup(fuseops.RootInodeID, "d")
	if err != nil {
		t.Fatalf("after git checkout's ops and a HEAD change that drops d, LookUpInode(d) returned %v; want d to resolve, so d/untracked.txt stays reachable", err)
	}
	file := f.mustLookup(dir.Entry.Child, "untracked.txt")
	if got := f.read(f.open(file.Entry.Child)); got != "untracked" {
		t.Errorf("d/untracked.txt reads %q; want %q", got, "untracked")
	}
}

// A FORGET gives a path that still exists a new inode number at its next lookup.
func TestForgetKeepsAPathsInodeNumber(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"a.txt": "aaaa"})
	f.switchHEAD(1, 1000)
	first := f.mustLookup(fuseops.RootInodeID, "a.txt").Entry.Child
	if err := f.fs.ForgetInode(f.ctx, &fuseops.ForgetInodeOp{Inode: first, N: 1}); err != nil {
		t.Fatal(err)
	}
	if second := f.mustLookup(fuseops.RootInodeID, "a.txt").Entry.Child; second != first {
		t.Errorf("a.txt was inode %d before a FORGET and inode %d after; want the number kept while the path exists", first, second)
	}
}

// An ftruncate through an unlinked open file fails instead of truncating the file's own copy.
func TestSetattrOfAnUnlinkedOpenFileChangesItsCopy(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"f.txt": "old-content"})
	f.switchHEAD(1, 1000)
	ino := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry.Child
	h := f.open(ino)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "f.txt"}); err != nil {
		t.Fatal(err)
	}
	size := uint64(3)
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: ino, Handle: &h, Size: &size}); err != nil {
		t.Fatalf("ftruncate(3) through the descriptor of unlinked f.txt returned %v; want success on the open file's copy", err)
	}
	if got := f.read(h); got != "old" {
		t.Errorf("after ftruncate(3), the descriptor of unlinked f.txt reads %q; want %q", got, "old")
	}
}

// A create after an unlink reuses the unlinked open file's inode, so an ftruncate through the old descriptor truncates the new file.
func TestCreateAfterUnlinkOfAnOpenFileGetsANewInode(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"f.txt": "old-content"})
	f.switchHEAD(1, 1000)
	old := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry.Child
	oldHandle := f.open(old)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "f.txt"}); err != nil {
		t.Fatal(err)
	}
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "f.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	f.write(create.Handle, "brand-new-file")
	if create.Entry.Child == old {
		t.Errorf("a create of f.txt after its unlink got inode %d, which the unlinked open file still holds; want a new inode number", old)
	}
	zero := uint64(0)
	setattrErr := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: old, Handle: &oldHandle, Size: &zero})
	got, err := f.engine.Read(f.ctx, "f.txt", 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "brand-new-file" {
		t.Errorf("an ftruncate(0) (returned %v) through the unlinked file's old descriptor left the new f.txt holding %q; want %q", setattrErr, got, "brand-new-file")
	}
}

// A path whose type changes keeps its inode number, so the kernel gets a known inode with a new type.
func TestTypeChangeGivesThePathANewInode(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"x": "file"})
	f.publish(2, map[string]string{"x/inner.txt": "i"}, "x")
	f.switchHEAD(1, 1000)
	asFile := f.mustLookup(fuseops.RootInodeID, "x").Entry.Child
	f.open(asFile)
	f.switchHEAD(2, 2000)
	asDir := f.mustLookup(fuseops.RootInodeID, "x")
	if !asDir.Entry.Attributes.Mode.IsDir() {
		t.Fatalf("x has mode %v after the HEAD change; want a directory", asDir.Entry.Attributes.Mode)
	}
	if asDir.Entry.Child == asFile {
		t.Errorf("x changed from a file to a directory but kept inode %d, which an open handle holds as a file; want a new inode number for the directory", asFile)
	}
}

// pauseAtOpenHydrator pauses its pauseAt-th OpenHydrated call until resume closes.
type pauseAtOpenHydrator struct {
	*fakeBatchHydrator
	calls   atomic.Int32
	pauseAt int32
	paused  chan struct{}
	resume  chan struct{}
}

func (h *pauseAtOpenHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	if h.calls.Add(1) == h.pauseAt {
		close(h.paused)
		<-h.resume
	}
	return h.fakeBatchHydrator.OpenHydrated(ctx, repo, node)
}

// An open of a base file that overlaps a write registers the old blob, so its handle never reads the write.
func TestOpenOverlappingAWriteReadsTheWrite(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"f.txt": "old"})
	f.switchHEAD(1, 1000)
	h := &pauseAtOpenHydrator{fakeBatchHydrator: &fakeBatchHydrator{pathsByOID: f.blobs}, pauseAt: 2, paused: make(chan struct{}), resume: make(chan struct{})}
	f.engine.Hydrator = h
	ino := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry.Child
	writer := f.open(ino)
	type openResult struct {
		handle fuseops.HandleID
		err    error
	}
	opened := make(chan openResult, 1)
	go func() {
		op := &fuseops.OpenFileOp{Inode: ino}
		err := f.fs.OpenFile(f.ctx, op)
		opened <- openResult{op.Handle, err}
	}()
	<-h.paused
	f.write(writer, "new")
	close(h.resume)
	reader := <-opened
	if reader.err != nil {
		t.Fatalf("open: %v", reader.err)
	}
	if got := f.read(reader.handle); got != "new" {
		t.Errorf("a handle whose open overlapped a write of %q reads %q; want the write", "new", got)
	}
}

// After a reconcile drops a file's overlay copy, a handle opened on that copy keeps reading it, while a newer handle reads the blob the mount serves.
func TestHandlesOfOneInodeReadOneContentAfterAReconcile(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"f.txt": "aaaa"})
	f.publish(2, map[string]string{"f.txt": "bbbb"}) // the commit of the write
	f.publish(3, map[string]string{"f.txt": "cccc"}) // a later HEAD change
	f.switchHEAD(1, 1000)
	ino := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry.Child
	f.write(f.open(ino), "bbbb")
	older := f.open(ino)
	f.switchHEAD(2, 2000)
	if n, err := f.resolver.ResolvePath("f.txt"); err != nil || n.FromOverlay {
		t.Fatalf("resolve f.txt after the commit's reconcile: from overlay %v, error %v; want the overlay entry dropped", n.FromOverlay, err)
	}
	f.switchHEAD(3, 3000)
	newer := f.open(ino)
	if a, b := f.read(older), f.read(newer); a != b {
		t.Errorf("two handles of f.txt read different content after a reconcile dropped its overlay copy: the older reads %q, the newer %q; want both to read the same content", a, b)
	}
}

// A switch between two commits made in one second gives a changed file of the same size the same stat data.
func TestSameSecondCommitChangesTheStatDataOfAChangedFile(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"f.txt": "v1-a"})
	f.publish(2, map[string]string{"f.txt": "v2-b"})
	f.switchHEAD(1, 5000)
	a := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry
	f.switchHEAD(2, 5000)
	b := f.mustLookup(fuseops.RootInodeID, "f.txt").Entry
	if a.Child == b.Child && a.Attributes.Size == b.Attributes.Size && a.Attributes.Mtime.Equal(b.Attributes.Mtime) && a.Attributes.Ctime.Equal(b.Attributes.Ctime) {
		t.Errorf("f.txt changed content between two commits made in one second but kept inode %d, size %d, mtime %v and ctime %v; want stat data that differs, so git's stat check sees the change", b.Child, b.Attributes.Size, b.Attributes.Mtime.UTC(), b.Attributes.Ctime.UTC())
	}
}

// Question: should a write through a file held open across a HEAD change recreate the path the change removed, or change the file another blob now holds there?
func TestShouldAWriteThroughAFileOpenAcrossAHEADChangeReachItsPath(t *testing.T) {
	f := newOpsFixture(t)
	f.publish(1, map[string]string{"gone.txt": "keep-me", "same.txt": "blob-A"})
	f.publish(2, map[string]string{"same.txt": "blob-B"})
	f.switchHEAD(1, 1000)
	gone := f.open(f.mustLookup(fuseops.RootInodeID, "gone.txt").Entry.Child)
	same := f.open(f.mustLookup(fuseops.RootInodeID, "same.txt").Entry.Child)
	if got := f.read(gone); got != "keep-me" {
		t.Fatalf("gone.txt reads %q; want %q", got, "keep-me")
	}
	if got := f.read(same); got != "blob-A" {
		t.Fatalf("same.txt reads %q; want %q", got, "blob-A")
	}
	f.switchHEAD(2, 2000)
	goneErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Handle: gone, Offset: 5, Data: []byte("XY")})
	got, err := f.engine.Read(f.ctx, "gone.txt", 0, 64)
	switch {
	case err == nil:
		t.Errorf("a write at offset 5 (returned %v) through a descriptor opened before a HEAD change removed gone.txt recreated gone.txt holding %q; want gone.txt to stay removed", goneErr, got)
	case !errors.Is(err, iofs.ErrNotExist):
		t.Fatalf("read gone.txt: %v", err)
	}
	sameErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Handle: same, Offset: 0, Data: []byte("XY")})
	got, err = f.engine.Read(f.ctx, "same.txt", 0, 64)
	if err != nil {
		t.Fatalf("read same.txt: %v", err)
	}
	if string(got) != "blob-B" {
		t.Errorf("a write (returned %v) through a descriptor opened on same.txt at blob-A changed same.txt, which now holds blob-B, to %q; want blob-B's file left holding %q", sameErr, got, "blob-B")
	}
}
