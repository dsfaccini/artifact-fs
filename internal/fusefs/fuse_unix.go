//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
)

// MountedFS matches tigrisfs's interface for mount lifecycle.
type MountedFS interface {
	// Backend returns the backend that mounted the filesystem: BackendFSKit or BackendKernel.
	Backend() string
	Join(ctx context.Context) error
	Unmount() error
}

// ArtifactFuse is the FUSE adapter following the tigrisfs GoofysFuse pattern:
// embed NotImplementedFileSystem + core state, thin operation wrappers. Engine
// is its only API to the merged view.
type ArtifactFuse struct {
	fuseutil.NotImplementedFileSystem
	repo           model.RepoConfig
	engine         *Engine
	gitfileContent []byte    // synthesized .git gitfile, computed once
	syntheticTime  time.Time // timestamps of synthesized entries; changing them can deadlock concurrent FSKit Git reads

	mu sync.RWMutex
	// handleOps keeps inode and handle bookkeeping consistent with the overlay:
	// Unlink and Rename hold it exclusively, and every op that creates or uses
	// inodes and handles holds it shared. Engine.Do releases it before it
	// hydrates a blob.
	handleOps    sync.RWMutex
	inodes       map[fuseops.InodeID]*InodeRef
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
}

type InodeRef struct {
	ID   fuseops.InodeID
	Path string
	Type string // file, dir, symlink
	Mode uint32
	// Gen is the snapshot generation of the base attributes last reported for
	// this inode, or 0 when the kernel's view came from the overlay. Base reads
	// serve Gen so their length matches the size the kernel holds after a HEAD
	// change, until a new reply reports the current generation.
	Gen     int64
	Refcnt  int64
	IsRoot  bool
	Overlay bool
	Stale   bool
}

type DirHandle struct {
	mu         sync.Mutex
	inode      *InodeRef
	gen        int64
	commitTime int64
	entries    []ReaddirEntry
	listed     bool
}

type FileHandle struct {
	mu              sync.Mutex
	inode           *InodeRef
	path            string
	cacheFile       *os.File
	cacheGeneration int64
	invalidateSeq   uint64
	detached        bool
}

// ReaddirEntry holds child metadata, avoiding per-child Getattr or snapshot lookups.
type ReaddirEntry struct {
	Name        string
	Type        string // file, dir, symlink
	Mode        uint32
	ObjectOID   string
	SizeState   string
	SizeBytes   int64
	FromOverlay bool
	MtimeUnixNs int64
	CtimeUnixNs int64
}

func (e ReaddirEntry) direntType() fuseutil.DirentType {
	switch e.Type {
	case "dir":
		return fuseutil.DT_Directory
	case "symlink":
		return fuseutil.DT_Link
	default:
		return fuseutil.DT_File
	}
}

func NewArtifactFuse(repo model.RepoConfig, engine *Engine) *ArtifactFuse {
	fs := &ArtifactFuse{
		repo:           repo,
		engine:         engine,
		gitfileContent: fmt.Appendf(nil, "gitdir: %s\n", repo.GitDir),
		syntheticTime:  time.Now(),
		inodes:         make(map[fuseops.InodeID]*InodeRef),
		pathToInode:    make(map[string]fuseops.InodeID),
		nextInodeID:    fuseops.RootInodeID + 1,
		dirHandles:     make(map[fuseops.HandleID]*DirHandle),
		fileHandles:    make(map[fuseops.HandleID]*FileHandle),
		nextHandleID:   1,
	}
	root := &InodeRef{ID: fuseops.RootInodeID, Path: ".", Type: "dir", Mode: 0o755, Refcnt: 1, IsRoot: true}
	fs.inodes[fuseops.RootInodeID] = root
	fs.pathToInode["."] = fuseops.RootInodeID
	return fs
}

func (fs *ArtifactFuse) allocInode(path, typ string, mode uint32, gen int64) *InodeRef {
	// Caller must hold fs.mu write lock.
	if id, ok := fs.pathToInode[path]; ok {
		if ref, ok := fs.inodes[id]; ok {
			ref.Refcnt++
			ref.Gen = gen
			return ref
		}
	}
	id := fs.nextInodeID
	fs.nextInodeID++
	ref := &InodeRef{ID: id, Path: path, Type: typ, Mode: mode, Gen: gen, Refcnt: 1}
	fs.inodes[id] = ref
	fs.pathToInode[path] = id
	return ref
}

func (fs *ArtifactFuse) requireInode(id fuseops.InodeID, missing error) (*InodeRef, error) {
	fs.mu.RLock()
	ref := fs.inodes[id]
	if ref == nil || ref.Stale {
		fs.mu.RUnlock()
		return nil, missing
	}
	snapshot := *ref
	fs.mu.RUnlock()
	return &snapshot, nil
}

func (fs *ArtifactFuse) inodeGeneration(id fuseops.InodeID) int64 {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if ref := fs.inodes[id]; ref != nil {
		return ref.Gen
	}
	return 0
}

func (fs *ArtifactFuse) setInodeGeneration(id fuseops.InodeID, gen int64) {
	fs.mu.Lock()
	if ref := fs.inodes[id]; ref != nil {
		ref.Gen = gen
	}
	fs.mu.Unlock()
}

func (fs *ArtifactFuse) dropInodeLookup(id fuseops.InodeID) {
	fs.mu.Lock()
	ref, ok := fs.inodes[id]
	if ok {
		ref.Refcnt--
		if ref.Refcnt <= 0 && !ref.IsRoot {
			delete(fs.inodes, id)
			if fs.pathToInode[ref.Path] == id {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
}

func (fs *ArtifactFuse) moveInodePath(oldPath, newPath string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	type inodeMove struct {
		id      fuseops.InodeID
		oldPath string
		newPath string
	}
	var moves []inodeMove
	movingIDs := map[fuseops.InodeID]bool{}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, oldPath) {
			moves = append(moves, inodeMove{
				id:      id,
				oldPath: path,
				newPath: newPath + strings.TrimPrefix(path, oldPath),
			})
			movingIDs[id] = true
		}
	}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, newPath) && !movingIDs[id] {
			if replaced := fs.inodes[id]; replaced != nil {
				replaced.Stale = true
			}
			delete(fs.pathToInode, path)
		}
	}
	for _, move := range moves {
		delete(fs.pathToInode, move.oldPath)
	}
	for _, move := range moves {
		if ref := fs.inodes[move.id]; ref != nil {
			ref.Path = move.newPath
			ref.Gen = 0
			fs.pathToInode[move.newPath] = move.id
		}
	}
	for _, dh := range fs.dirHandles {
		if samePathOrDescendant(dh.inode.Path, oldPath) {
			dh.inode.Path = newPath + strings.TrimPrefix(dh.inode.Path, oldPath)
		}
	}
}

func samePathOrDescendant(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func (fs *ArtifactFuse) childPath(parentID fuseops.InodeID, name string) (*InodeRef, string, error) {
	parent, err := fs.requireInode(parentID, syscall.ENOENT)
	if err != nil {
		return nil, "", err
	}
	return parent, cleanChildPath(parent.Path, name), nil
}

func (fs *ArtifactFuse) dirHandle(handleID fuseops.HandleID) (*DirHandle, error) {
	fs.mu.RLock()
	dh := fs.dirHandles[handleID]
	fs.mu.RUnlock()
	if dh == nil {
		return nil, syscall.EBADF
	}
	return dh, nil
}

func (fs *ArtifactFuse) fileHandle(handleID fuseops.HandleID) (*FileHandle, error) {
	fs.mu.RLock()
	fh := fs.fileHandles[handleID]
	fs.mu.RUnlock()
	if fh == nil {
		return nil, syscall.EBADF
	}
	return fh, nil
}

func (fs *ArtifactFuse) closeCachedFilesForPath(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.closeCachedFile()
	}
}

func (fs *ArtifactFuse) hasOpenFile(path string) bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path && !fh.detached
		fh.mu.Unlock()
		if matches {
			return true
		}
	}
	return false
}

func (fs *ArtifactFuse) pinOpenFiles(path, backingPath string) error {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		if fh.cacheFile == nil || fh.cacheGeneration != -1 {
			f, openErr := os.OpenFile(backingPath, os.O_RDWR, 0)
			if openErr != nil {
				f, openErr = os.Open(backingPath)
				if openErr != nil {
					fh.mu.Unlock()
					return openErr
				}
			}
			old := fh.cacheFile
			fh.cacheFile = f
			fh.cacheGeneration = -1
			if old != nil {
				_ = old.Close()
			}
		}
		fh.mu.Unlock()
	}
	return nil
}

func (fs *ArtifactFuse) detachOpenHandles(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := samePathOrDescendant(fh.path, path)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.detached = true
		fh.mu.Unlock()
	}
}

func (fs *ArtifactFuse) moveOpenHandles(oldPath, newPath string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := samePathOrDescendant(fh.path, oldPath)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.path = newPath + strings.TrimPrefix(fh.path, oldPath)
		fh.mu.Unlock()
	}
}

// read serves the overlay when it holds the path, and otherwise the base content
// of generation gen, or of the current generation when gen is 0.
func (fh *FileHandle) read(ctx context.Context, engine *Engine, b *Blobs, gen int64, off int64, dst []byte) ([]byte, error) {
	fh.mu.Lock()
	path := fh.path
	if fh.detached && fh.cacheFile != nil {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, dst)
	}
	fh.mu.Unlock()
	if gen == 0 {
		gen = engine.Generation()
	}
	fh.mu.Lock()
	if fh.cacheFile != nil && (fh.cacheGeneration == -1 || fh.cacheGeneration == gen) {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, dst)
	}
	if fh.cacheFile != nil {
		f := fh.cacheFile
		fh.cacheFile = nil
		fh.cacheGeneration = 0
		fh.invalidateSeq++
		fh.mu.Unlock()
		_ = f.Close()
		fh.mu.Lock()
	}
	seq := fh.invalidateSeq
	fh.mu.Unlock()

	f, gen, ok, err := engine.BaseCacheFile(ctx, b, path, gen)
	if err != nil {
		return nil, err
	}
	if !ok {
		return engine.Read(ctx, b, path, off, dst)
	}
	fh.mu.Lock()
	if fh.invalidateSeq != seq {
		fh.mu.Unlock()
		_ = f.Close()
		return engine.Read(ctx, b, path, off, dst)
	}
	if fh.cacheFile != nil && fh.cacheGeneration == gen {
		_ = f.Close()
		f = fh.cacheFile
	} else {
		if fh.cacheFile != nil {
			_ = fh.cacheFile.Close()
		}
		fh.cacheFile = f
		fh.cacheGeneration = gen
	}
	defer fh.mu.Unlock()
	return readFileChunkFrom(f, off, dst)
}

func (fh *FileHandle) closeCachedFile() {
	fh.mu.Lock()
	if fh.detached {
		fh.mu.Unlock()
		return
	}
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.cacheGeneration = 0
	fh.invalidateSeq++
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

func (fh *FileHandle) release() {
	fh.mu.Lock()
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// --- FUSE operations ---

func (fs *ArtifactFuse) StatFS(_ context.Context, op *fuseops.StatFSOp) error {
	const blockSize = 4096
	const totalSpace = 1 * 1024 * 1024 * 1024 * 1024 * 1024
	const totalBlocks = totalSpace / blockSize
	op.BlockSize = blockSize
	op.Blocks = totalBlocks
	op.BlocksFree = totalBlocks
	op.BlocksAvailable = totalBlocks
	op.IoSize = 1 * 1024 * 1024
	op.Inodes = 1_000_000_000
	op.InodesFree = 1_000_000_000
	return nil
}

// shared runs op through Engine.Do holding handleOps shared, and exclusive
// holding it exclusively. Both return the error the kernel receives.
func (fs *ArtifactFuse) shared(ctx context.Context, op func(context.Context, *Blobs) error) error {
	return toErrno(ctx, fs.engine.Do(ctx, fs.handleOps.RLocker(), op))
}

func (fs *ArtifactFuse) exclusive(ctx context.Context, op func(context.Context, *Blobs) error) error {
	return toErrno(ctx, fs.engine.Do(ctx, &fs.handleOps, op))
}

// toErrno returns the error the kernel receives for an op error. The fuse
// library replies with the first syscall.Errno in the error's chain, and with
// EIO when it holds none. toErrno passes an error that wraps an errno through,
// maps a canceled request to EINTR and the io/fs sentinels to their errno, and
// wraps the original error so its message survives.
func toErrno(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if isRequestCancellation(ctx, err) {
		return fmt.Errorf("%w: %w", syscall.EINTR, err)
	}
	if _, ok := errors.AsType[syscall.Errno](err); ok {
		return err
	}
	for _, m := range []struct {
		target error
		errno  syscall.Errno
	}{
		{iofs.ErrNotExist, syscall.ENOENT},
		{iofs.ErrExist, syscall.EEXIST},
		{iofs.ErrInvalid, syscall.EINVAL},
	} {
		if errors.Is(err, m.target) {
			return fmt.Errorf("%w: %w", m.errno, err)
		}
	}
	return err
}

func isRequestCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

func (fs *ArtifactFuse) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	return fs.shared(ctx, func(_ context.Context, b *Blobs) error {
		parent, err := fs.requireInode(op.Parent, syscall.ENOENT)
		if err != nil {
			return err
		}

		childPath := cleanChildPath(parent.Path, op.Name)

		// Synthesize .git gitfile in root
		if isGitfile(parent, op.Name) {
			fs.mu.Lock()
			ref := fs.allocInode(".git", "file", 0o644, fs.engine.Generation())
			fs.mu.Unlock()
			op.Entry.Child = ref.ID
			op.Entry.Attributes = fs.gitFileAttrs()
			setChildEntryExpiry(&op.Entry, time.Minute)
			return nil
		}

		attrs, err := fs.engine.Attrs(b, childPath)
		if err != nil {
			return err
		}

		fs.mu.Lock()
		ref := fs.allocInode(childPath, attrs.Type, attrs.Mode, attrs.Gen)
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		op.Entry.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		setChildEntryExpiry(&op.Entry, time.Second)
		return nil
	})
}

func (fs *ArtifactFuse) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	return fs.shared(ctx, func(_ context.Context, b *Blobs) error {
		ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
		if err != nil {
			return err
		}

		if ref.IsRoot {
			if fs.engine != nil {
				if attrs, err := fs.engine.Attrs(b, ref.Path); err == nil {
					op.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
					op.AttributesExpiration = attrExpiry(time.Second)
					return nil
				}
			}
			op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", fs.syntheticTime, fs.syntheticTime)
			op.AttributesExpiration = attrExpiry(time.Second)
			return nil
		}

		if ref.Path == ".git" {
			op.Attributes = fs.gitFileAttrs()
			op.AttributesExpiration = attrExpiry(time.Minute)
			return nil
		}

		attrs, err := fs.engine.Attrs(b, ref.Path)
		if err != nil {
			return err
		}
		fs.setInodeGeneration(op.Inode, attrs.Gen)
		op.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		op.AttributesExpiration = attrExpiry(time.Second)
		return nil
	})
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	return fs.shared(ctx, func(ctx context.Context, b *Blobs) error {
		ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
		if err != nil {
			return err
		}
		if ref.Path == ".git" {
			return syscall.EPERM
		}
		var size *int64
		if op.Size != nil {
			s := int64(*op.Size)
			size = &s
		}
		var mode *uint32
		if op.Mode != nil {
			m := uint32(op.Mode.Perm())
			mode = &m
		}
		changesContent := op.Size != nil || op.Mtime != nil
		if changesContent {
			fs.closeCachedFilesForPath(ref.Path)
		}
		attrs, err := fs.engine.SetAttributes(ctx, b, ref.Path, size, mode, op.Mtime)
		if changesContent {
			fs.closeCachedFilesForPath(ref.Path)
		}
		if err != nil {
			return err
		}
		fs.setInodeGeneration(op.Inode, attrs.Gen)
		op.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		op.AttributesExpiration = attrExpiry(time.Second)
		return nil
	})
}

func (fs *ArtifactFuse) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	fs.mu.Lock()
	ref, ok := fs.inodes[op.Inode]
	if ok {
		ref.Refcnt -= int64(op.N)
		if ref.Refcnt <= 0 && !ref.IsRoot {
			delete(fs.inodes, op.Inode)
			if fs.pathToInode[ref.Path] == op.Inode {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
		if err != nil {
			return err
		}

		dh := &DirHandle{inode: ref}
		fs.mu.Lock()
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.dirHandles[handle] = dh
		fs.mu.Unlock()
		op.Handle = handle
		return nil
	})
}

func (fs *ArtifactFuse) readDirSnapshot(ctx context.Context, ref *InodeRef) ([]ReaddirEntry, int64, int64, error) {
	entries, gen, commitTime, err := fs.engine.ReadDir(ctx, ref.Path)
	if err != nil {
		return nil, 0, 0, err
	}
	if ref.IsRoot {
		entries = append([]ReaddirEntry{{Name: ".git", Type: "file", Mode: 0o644, SizeBytes: int64(len(fs.gitfileContent)), SizeState: "known"}}, entries...)
	}
	return entries, gen, commitTime, nil
}

// listDirHandle lists dh's directory at its first read and at every read from
// offset 0, which starts an enumeration: FSKit reuses a handle across
// enumerations, and rewinddir reads from offset 0 again. Callers hold dh.mu.
func (fs *ArtifactFuse) listDirHandle(ctx context.Context, dh *DirHandle, offset fuseops.DirOffset) error {
	if offset != 0 && dh.listed {
		return nil
	}
	entries, gen, commitTime, err := fs.readDirSnapshot(ctx, dh.inode)
	if err != nil {
		return err
	}
	dh.entries, dh.gen, dh.commitTime, dh.listed = entries, gen, commitTime, true
	return nil
}

func (fs *ArtifactFuse) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		dh, err := fs.dirHandle(op.Handle)
		if err != nil {
			return err
		}
		dh.mu.Lock()
		defer dh.mu.Unlock()
		if err := fs.listDirHandle(ctx, dh, op.Offset); err != nil {
			return err
		}

		offset := int(op.Offset)
		for i := offset; i < len(dh.entries); i++ {
			e := dh.entries[i]
			dirent := fuseutil.Dirent{
				Offset: fuseops.DirOffset(i + 1),
				Inode:  fuseops.RootInodeID + 1, // placeholder; kernel re-looks-up via LookUpInode
				Name:   e.Name,
				Type:   e.direntType(),
			}
			n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
			if n == 0 {
				break
			}
			op.BytesRead += n
		}
		return nil
	})
}

func (fs *ArtifactFuse) ReadDirPlus(ctx context.Context, op *fuseops.ReadDirPlusOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		dh, err := fs.dirHandle(op.Handle)
		if err != nil {
			return err
		}
		dh.mu.Lock()
		defer dh.mu.Unlock()
		if err := fs.listDirHandle(ctx, dh, op.Offset); err != nil {
			return err
		}

		offset := int(op.Offset)
		for i := offset; i < len(dh.entries); i++ {
			e := dh.entries[i]
			childPath := cleanChildPath(dh.inode.Path, e.Name)
			entry := fs.childEntryFromReaddir(childPath, dh.gen, dh.commitTime, e)
			dirent := fuseutil.DirentPlus{
				Dirent: fuseutil.Dirent{
					Offset: fuseops.DirOffset(i + 1),
					Inode:  entry.Child,
					Name:   e.Name,
					Type:   e.direntType(),
				},
				Entry: entry,
			}
			n := fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], dirent)
			if n == 0 {
				fs.dropInodeLookup(entry.Child)
				break
			}
			op.BytesRead += n
		}
		return nil
	})
}

func (fs *ArtifactFuse) childEntryFromReaddir(path string, gen, commitTime int64, e ReaddirEntry) fuseops.ChildInodeEntry {
	if path == ".git" {
		fs.mu.Lock()
		ref := fs.allocInode(path, "file", 0o644, gen)
		fs.mu.Unlock()
		entry := fuseops.ChildInodeEntry{Child: ref.ID, Attributes: fs.gitFileAttrs()}
		setChildEntryExpiry(&entry, time.Minute)
		return entry
	}
	mode, size, typ, mtime, ctime := readdirAttrs(e, gen, commitTime)
	refGen := gen
	if e.FromOverlay {
		refGen = 0
	}
	fs.mu.Lock()
	ref := fs.allocInode(path, typ, mode, refGen)
	fs.mu.Unlock()
	entry := fuseops.ChildInodeEntry{
		Child:      ref.ID,
		Attributes: inodeAttrs(mode, uint64(size), typ, mtime, ctime),
	}
	setChildEntryExpiry(&entry, time.Second)
	return entry
}

func readdirAttrs(e ReaddirEntry, gen, commitTime int64) (mode uint32, size int64, typ string, mtime time.Time, ctime time.Time) {
	typ = e.Type
	mode = normalizeMode(e.Mode, typ)
	size = e.SizeBytes
	if e.FromOverlay {
		return mode, size, typ, time.Unix(0, e.MtimeUnixNs), time.Unix(0, e.CtimeUnixNs)
	}
	ct := commitTime
	if ct == 0 {
		ct = gen
	}
	mt := time.Unix(ct, 0)
	return mode, size, typ, mt, mt
}

func (fs *ArtifactFuse) ReleaseDirHandle(_ context.Context, op *fuseops.ReleaseDirHandleOp) error {
	fs.mu.Lock()
	delete(fs.dirHandles, op.Handle)
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, b *Blobs) error {
		ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
		if err != nil {
			return err
		}
		fh := &FileHandle{inode: ref, path: ref.Path}
		if ref.Path != ".git" {
			// An open for writing only reads nothing, so it prefetches nothing.
			f, gen, err := fs.engine.OpenFile(ctx, b, ref.Path, ref.Gen, !op.OpenFlags.IsWriteOnly())
			if err != nil {
				return err
			}
			fh.cacheFile, fh.cacheGeneration = f, gen
		}
		fs.mu.Lock()
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.fileHandles[handle] = fh
		fs.mu.Unlock()
		op.Handle = handle
		op.KeepPageCache = false
		return nil
	})
}

func (fs *ArtifactFuse) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, b *Blobs) error {
		fh, err := fs.fileHandle(op.Handle)
		if err != nil {
			return err
		}

		fh.mu.Lock()
		path := fh.path
		fh.mu.Unlock()
		if path == ".git" {
			start := int(op.Offset)
			if start >= len(fs.gitfileContent) {
				op.BytesRead = 0
				return nil
			}
			end := min(start+int(op.Size), len(fs.gitfileContent))
			op.Data = [][]byte{fs.gitfileContent[start:end]}
			op.BytesRead = end - start
			return nil
		}

		// The library leaves Dst nil for reads larger than its request buffer.
		dst := op.Dst
		if dst == nil {
			dst = make([]byte, op.Size)
		}
		data, err := fh.read(ctx, fs.engine, b, fs.inodeGeneration(op.Inode), op.Offset, dst)
		if err != nil {
			return err
		}
		if op.Dst == nil {
			op.Data = [][]byte{data}
		}
		op.BytesRead = len(data)
		return nil
	})
}

func (fs *ArtifactFuse) WriteFile(ctx context.Context, op *fuseops.WriteFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, b *Blobs) error {
		fh, err := fs.fileHandle(op.Handle)
		if err != nil {
			return err
		}
		fh.mu.Lock()
		if fh.path == ".git" {
			fh.mu.Unlock()
			return syscall.EPERM
		}
		if fh.detached {
			f := fh.cacheFile
			fh.mu.Unlock()
			if f == nil {
				return syscall.EIO
			}
			_, err := f.WriteAt(op.Data, op.Offset)
			return err
		}
		path := fh.path
		fh.mu.Unlock()
		fs.closeCachedFilesForPath(path)
		n, err := fs.engine.Write(ctx, b, path, op.Offset, op.Data)
		if err != nil {
			return err
		}
		if n != len(op.Data) {
			return syscall.EIO
		}
		fs.setInodeGeneration(op.Inode, 0)
		fs.closeCachedFilesForPath(path)
		return nil
	})
}

func (fs *ArtifactFuse) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		_, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if err := fs.engine.Create(ctx, childPath, uint32(op.Mode)); err != nil {
			return err
		}
		fs.mu.Lock()
		ref := fs.allocInode(childPath, "file", uint32(op.Mode), 0)
		fh := &FileHandle{inode: ref, path: childPath}
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.fileHandles[handle] = fh
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", now, now)
		setChildEntryExpiry(&op.Entry, time.Second)
		op.Handle = handle
		return nil
	})
}

func (fs *ArtifactFuse) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		_, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if len(op.Target) > model.MaxSymlinkTargetBytes {
			return syscall.ENAMETOOLONG
		}
		if err := fs.engine.Symlink(ctx, childPath, op.Target); err != nil {
			return err
		}
		fs.mu.Lock()
		ref := fs.allocInode(childPath, "symlink", 0o120000, 0)
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(0o120000, uint64(len(op.Target)), "symlink", now, now)
		setChildEntryExpiry(&op.Entry, time.Second)
		return nil
	})
}

func (fs *ArtifactFuse) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		_, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if err := fs.engine.Mkdir(ctx, childPath, uint32(op.Mode)); err != nil {
			return err
		}
		fs.mu.Lock()
		ref := fs.allocInode(childPath, "dir", uint32(op.Mode), 0)
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(uint32(op.Mode)|uint32(os.ModeDir), 4096, "dir", now, now)
		setChildEntryExpiry(&op.Entry, time.Second)
		return nil
	})
}

func (fs *ArtifactFuse) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		parent, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if isGitfile(parent, op.Name) {
			return syscall.EPERM
		}
		return fs.engine.Rmdir(ctx, childPath)
	})
}

func (fs *ArtifactFuse) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	return fs.exclusive(ctx, func(ctx context.Context, b *Blobs) error {
		parent, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if isGitfile(parent, op.Name) {
			return syscall.EPERM
		}
		if err := fs.engine.Unlink(ctx, b, childPath, fs); err != nil {
			return err
		}
		fs.detachOpenHandles(childPath)
		return nil
	})
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	return fs.exclusive(ctx, func(ctx context.Context, b *Blobs) error {
		oldParent, err := fs.requireInode(op.OldParent, syscall.ENOENT)
		if err != nil {
			return err
		}
		newParent, err := fs.requireInode(op.NewParent, syscall.ENOENT)
		if err != nil {
			return err
		}
		if isGitfile(oldParent, op.OldName) || isGitfile(newParent, op.NewName) {
			return syscall.EPERM
		}
		oldPath := cleanChildPath(oldParent.Path, op.OldName)
		newPath := cleanChildPath(newParent.Path, op.NewName)
		if err := fs.engine.Rename(ctx, b, oldPath, newPath, fs); err != nil {
			return err
		}
		if oldPath == newPath {
			return nil
		}
		fs.moveInodePath(oldPath, newPath)
		fs.detachOpenHandles(newPath)
		fs.moveOpenHandles(oldPath, newPath)
		return nil
	})
}

// isGitfile reports whether name in parent is the synthesized .git gitfile,
// which no op may remove, replace, or write.
func isGitfile(parent *InodeRef, name string) bool {
	return parent.IsRoot && name == ".git"
}

// ReadSymlink holds no lock: it changes no bookkeeping, and a base target can
// take a fetch.
func (fs *ArtifactFuse) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	target, err := fs.engine.ReadSymlink(ctx, ref.Path)
	if err != nil {
		return toErrno(ctx, err)
	}
	op.Target = target
	return nil
}

func (fs *ArtifactFuse) FlushFile(_ context.Context, _ *fuseops.FlushFileOp) error {
	return nil
}

func (fs *ArtifactFuse) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		fh, err := fs.fileHandle(op.Handle)
		if err != nil {
			return err
		}
		fh.mu.Lock()
		if fh.detached {
			f := fh.cacheFile
			fh.mu.Unlock()
			if f == nil {
				return syscall.EIO
			}
			return f.Sync()
		}
		path := fh.path
		if fh.cacheFile != nil && fh.cacheGeneration >= 0 {
			fh.mu.Unlock()
			return nil
		}
		fh.mu.Unlock()
		return fs.engine.Sync(ctx, path)
	})
}

func (fs *ArtifactFuse) ReleaseFileHandle(ctx context.Context, op *fuseops.ReleaseFileHandleOp) error {
	return fs.shared(ctx, func(context.Context, *Blobs) error {
		fs.mu.Lock()
		fh := fs.fileHandles[op.Handle]
		delete(fs.fileHandles, op.Handle)
		fs.mu.Unlock()
		if fh != nil {
			fh.release()
		}
		return nil
	})
}

func (fs *ArtifactFuse) GetXattr(_ context.Context, _ *fuseops.GetXattrOp) error {
	return syscall.ENOSYS
}
func (fs *ArtifactFuse) ListXattr(_ context.Context, _ *fuseops.ListXattrOp) error {
	return syscall.ENOSYS
}
func (fs *ArtifactFuse) SetXattr(_ context.Context, _ *fuseops.SetXattrOp) error {
	return setXattrErrno
}
func (fs *ArtifactFuse) RemoveXattr(_ context.Context, _ *fuseops.RemoveXattrOp) error {
	return syscall.ENOSYS
}

// --- Mount lifecycle ---

type mountedFSWrapper struct {
	*fuse.MountedFileSystem
	mountPoint string
	backend    string
}

func (m *mountedFSWrapper) Unmount() error {
	return TryUnmount(m.mountPoint)
}

func (m *mountedFSWrapper) Backend() string { return m.backend }

func MountRepo(repo model.RepoConfig, engine *Engine, logger *slog.Logger) (MountedFS, error) {
	return MountRepoWithGate(repo, engine, nil, logger)
}

// fuseMount is fuse.Mount. Package tests replace it so they cannot mount.
var fuseMount = fuse.Mount

// fuseErrorLog writes each line the fuse library logs to logger, with the
// repo's remote redacted. A bug the library answered with EIO goes out as an
// error. A short FSKit read the library answered with EIO goes out as a
// warning. Any other op error, which the library logs for expected errnos such
// as EEXIST too, goes out at Debug. Anything else, such as a failed reply,
// goes out as a warning.
type fuseErrorLog struct {
	logger *slog.Logger
	repo   model.RepoConfig
}

// fuseBugLine matches the op error lines the fuse library writes for a
// recovered panic, a method that exited through runtime.Goexit, and a
// BytesRead outside its buffer. Every other op error's message is quoted, so
// it never matches the panic form.
var fuseBugLine = regexp.MustCompile(`^Op 0x[0-9a-f]+ \S+\] -> \w+: (panic: |"fuseutil: the method exited without returning: |"\S+\.BytesRead is -?\d+, outside its \d+-byte buffer: )`)

// fuseShortReadLine matches the op error line the fuse library writes when, on
// FSKit, a read's reply is shorter than the size last reported for the file,
// which it answers with EIO.
var fuseShortReadLine = regexp.MustCompile(`^Op 0x[0-9a-f]+ \*fuseops\.ReadFileOp\] -> Error: "read of inode \d+ at offset \d+ returned \d+ bytes, want \d+ for size \d+: `)

func (l fuseErrorLog) Write(p []byte) (int, error) {
	msg := strings.TrimSuffix(string(p), "\n")
	level := slog.LevelWarn
	switch {
	case fuseBugLine.MatchString(msg):
		level = slog.LevelError
	case fuseShortReadLine.MatchString(msg):
		level = slog.LevelWarn
	case strings.HasPrefix(msg, "Op "):
		level = slog.LevelDebug
	}
	l.logger.Log(context.Background(), level, "fuse error", "repo", l.repo.Name, "error", auth.RedactLogString(msg, l.repo.RemoteURL))
	return len(p), nil
}

const backendEnv = "ARTIFACT_FS_FUSE_BACKEND"

// Values of ARTIFACT_FS_FUSE_BACKEND. Only darwin acts on them.
const (
	BackendAuto   = "auto"
	BackendFSKit  = "fskit"
	BackendKernel = "kernel"
)

// MountBackend returns the backend ARTIFACT_FS_FUSE_BACKEND selects. Empty selects BackendAuto.
func MountBackend() (string, error) {
	switch backend := os.Getenv(backendEnv); backend {
	case "":
		return BackendAuto, nil
	case BackendAuto, BackendFSKit, BackendKernel:
		return backend, nil
	default:
		return "", fmt.Errorf("unsupported %s=%q; use auto, fskit, or kernel", backendEnv, backend)
	}
}

// mountWithBackend mounts through the transports backend allows and returns the
// backend that mounted. mount(true) mounts through FSKit, mount(false) through the
// kernel transport. BackendAuto falls back to the kernel transport only when the
// FSKit error matches fuse.ErrFSKitUnavailable, which means FSKit cannot serve any
// mount here, and kextLoaded reports a loaded macFUSE kernel extension, because
// mounting through an unloaded one prompts macOS to load it. Any other FSKit
// failure may leave a mount behind, so a retry could mount twice.
func mountWithBackend(backend string, mount func(fskit bool) (*fuse.MountedFileSystem, error), kextLoaded func() bool) (*fuse.MountedFileSystem, string, error) {
	switch backend {
	case BackendKernel:
		mfs, err := mount(false)
		return mfs, BackendKernel, err
	case BackendFSKit:
		mfs, err := mount(true)
		return mfs, BackendFSKit, err
	}
	mfs, fskitErr := mount(true)
	if fskitErr == nil || !errors.Is(fskitErr, fuse.ErrFSKitUnavailable) {
		return mfs, BackendFSKit, fskitErr
	}
	if !kextLoaded() {
		return nil, BackendFSKit, fmt.Errorf("%w; no kernel transport fallback because the macFUSE kernel extension is not loaded", fskitErr)
	}
	mfs, err := mount(false)
	if err != nil {
		return nil, BackendKernel, fmt.Errorf("%w; kernel transport fallback: %w", fskitErr, err)
	}
	return mfs, BackendKernel, nil
}

func MountRepoWithGate(repo model.RepoConfig, engine *Engine, gate *ReadyGate, logger *slog.Logger) (MountedFS, error) {
	backend, err := MountBackend()
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}
	fsint := NewArtifactFuse(repo, engine)

	mountCfg := fuse.MountConfig{
		FSName:                  "artifact-fs:" + repo.Name,
		Subtype:                 "artifact-fs",
		DisableWritebackCaching: true,
		ErrorLogger:             log.New(fuseErrorLog{logger: logger, repo: repo}, "", 0),
	}
	// READDIRPLUS would cache unknown blob sizes as zero before lookup can hydrate them.
	platformMountConfig(&mountCfg)
	mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
		cfg := mountCfg
		if fskit {
			cfg.Backend = fuse.BackendFSKit
		}
		// A Server must not serve a second mount once a failed one has
		// answered INIT, so each attempt gets its own.
		server := fuseutil.NewFileSystemServer(NewGatedFileSystem(fsint, gate))
		return fuseMount(repo.MountPath, server, &cfg)
	}

	mfs, mounted, err := platformMount(backend, mount)
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}

	return &mountedFSWrapper{MountedFileSystem: mfs, mountPoint: repo.MountPath, backend: mounted}, nil
}

func TryUnmount(mountPoint string) error {
	var err error
	for range 20 {
		err = fuse.Unmount(mountPoint)
		if err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func inodeAttrs(mode uint32, size uint64, typ string, mtime time.Time, ctime time.Time) fuseops.InodeAttributes {
	m := os.FileMode(mode & 0o777)
	switch typ {
	case "dir":
		m |= os.ModeDir
		if size == 0 {
			size = 4096
		}
	case "symlink":
		m |= os.ModeSymlink
	}
	return fuseops.InodeAttributes{
		Size:  size,
		Nlink: 1,
		Mode:  m,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: mtime,
		Mtime: mtime,
		Ctime: ctime,
	}
}

func cleanChildPath(parentPath string, name string) string {
	return model.CleanPath(filepath.Join(parentPath, name))
}

func attrExpiry(ttl time.Duration) time.Time {
	return time.Now().Add(ttl)
}

func setChildEntryExpiry(entry *fuseops.ChildInodeEntry, ttl time.Duration) {
	expiresAt := attrExpiry(ttl)
	entry.AttributesExpiration = expiresAt
	entry.EntryExpiration = expiresAt
}

func (fs *ArtifactFuse) gitFileAttrs() fuseops.InodeAttributes {
	return fuseops.InodeAttributes{
		Size:  uint64(len(fs.gitfileContent)),
		Mode:  0o644,
		Nlink: 1,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: fs.syntheticTime,
		Mtime: fs.syntheticTime,
		Ctime: fs.syntheticTime,
	}
}
