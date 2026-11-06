package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const maxPrefetchTasksPerDir = 256

// Engine is the FUSE adapter's only API to the merged view: each op the
// adapter serves is one Engine method.
//
// No Engine method hydrates a blob. A method that reads base content takes a
// *Blobs, and when the blob it needs is missing it returns a *notHydratedError.
// It returns that error before it changes anything, except that a directory
// rename may already have promoted some directories and copied some files into
// the overlay, all with base content. Engine.Do then hydrates the blob with no lock held and runs the
// method again, which completes the op. The method resolves its paths afresh
// under the transition read lock, so a HEAD switch during hydration makes it ask
// for the new blob.
type Engine struct {
	Resolver *Resolver
	Repo     model.RepoConfig
	Overlay  model.OverlayStore
	Hydrator model.Hydrator

	prefetchMu sync.Mutex
	// prefetched maps a directory to the generation at which a read last
	// prefetched its files.
	prefetched map[string]int64
}

// Blobs holds the base blobs that Engine.Do hydrated for one op. It holds no
// open file: an op opens each blob when it reads it, so a directory rename over
// many files keeps one blob open at a time.
type Blobs struct {
	files map[string]hydratedBlob // hydrated blob files by object ID
	sizes map[string]int64        // blob sizes by object ID
}

// hydratedBlob is a blob file the hydrator verified, by path and identity.
type hydratedBlob struct {
	path string
	info os.FileInfo
}

// notHydratedError names the base blobs an op needs and its Blobs lacks: files
// to open, and sizes to learn.
type notHydratedError struct {
	files []model.BaseNode
	sizes []model.BaseNode
}

func (e *notHydratedError) Error() string {
	return fmt.Sprintf("%d base blobs are not hydrated", len(e.files)+len(e.sizes))
}

// open opens n's blob for the caller to close. A blob file that changed on
// disk since the hydrator verified it counts as not hydrated.
func (b *Blobs) open(n model.BaseNode) (*os.File, error) {
	missing := &notHydratedError{files: []model.BaseNode{n}}
	blob, ok := b.files[n.ObjectOID]
	if !ok {
		return nil, missing
	}
	f, err := os.Open(blob.path)
	if errors.Is(err, fs.ErrNotExist) {
		delete(b.files, n.ObjectOID)
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(st, blob.info) || st.Size() != blob.info.Size() || !st.ModTime().Equal(blob.info.ModTime()) {
		_ = f.Close()
		delete(b.files, n.ObjectOID)
		return nil, missing
	}
	return f, nil
}

// requireFiles names every blob of nodes that b lacks, so that an op copying
// several blobs hydrates all of them before it copies any.
func (b *Blobs) requireFiles(nodes []model.BaseNode) error {
	var missing []model.BaseNode
	for _, n := range nodes {
		if _, ok := b.files[n.ObjectOID]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return &notHydratedError{files: missing}
	}
	return nil
}

// size returns the size of n's blob.
func (b *Blobs) size(n model.BaseNode) (int64, error) {
	if size, ok := b.sizes[n.ObjectOID]; ok {
		return size, nil
	}
	return 0, &notHydratedError{sizes: []model.BaseNode{n}}
}

// Do runs op holding lock, or no lock when lock is nil. op's context is never
// canceled, so an op that started to change the overlay finishes. When op
// returns a *notHydratedError, Do releases lock, hydrates the blobs the error
// names under ctx, and runs op again.
func (e *Engine) Do(ctx context.Context, lock sync.Locker, op func(context.Context, *Blobs) error) error {
	b := &Blobs{}
	opCtx := context.WithoutCancel(ctx)
	for {
		if lock != nil {
			lock.Lock()
		}
		err := op(opCtx, b)
		if lock != nil {
			lock.Unlock()
		}
		var missing *notHydratedError
		if !errors.As(err, &missing) {
			return err
		}
		if err := e.hydrate(ctx, b, missing); err != nil {
			return err
		}
	}
}

func (e *Engine) hydrate(ctx context.Context, b *Blobs, missing *notHydratedError) error {
	if b.files == nil {
		b.files = map[string]hydratedBlob{}
		b.sizes = map[string]int64{}
	}
	for _, n := range missing.files {
		if _, ok := b.files[n.ObjectOID]; ok {
			continue
		}
		// OpenHydrated returns the file the hydrator verified. Its identity lets
		// Blobs.open reopen that same file once the op holds its locks.
		f, size, err := e.Hydrator.OpenHydrated(ctx, e.Repo, n)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		_ = f.Close()
		if err != nil {
			return err
		}
		b.files[n.ObjectOID] = hydratedBlob{path: f.Name(), info: info}
		b.sizes[n.ObjectOID] = size
	}
	for _, n := range missing.sizes {
		if _, ok := b.sizes[n.ObjectOID]; ok {
			continue
		}
		size, err := e.blobSize(ctx, n)
		if err != nil {
			return err
		}
		b.sizes[n.ObjectOID] = size
	}
	return nil
}

// blobSize learns the size of a blob the snapshot has no size for. A symlink
// reads its target with a bound instead of hydrating a blob of any size.
func (e *Engine) blobSize(ctx context.Context, n model.BaseNode) (int64, error) {
	if n.Type == "symlink" {
		target, err := e.Hydrator.ReadBlob(ctx, e.Repo, n, model.MaxSymlinkTargetBytes)
		return int64(len(target)), err
	}
	_, size, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, n)
	return size, err
}

// openFiles is the adapter's open file handles, which Unlink and Rename keep
// readable after they remove or replace the handles' path.
type openFiles interface {
	// hasOpenFile reports whether a file handle that is not detached is open on path.
	hasOpenFile(path string) bool
	// pinOpenFiles points the open handles on path at the overlay file at backingPath.
	pinOpenFiles(path, backingPath string) error
}

// Generation returns the snapshot generation the engine serves.
func (e *Engine) Generation() int64 { return e.Resolver.Generation() }

// Attrs are a path's attributes.
type Attrs struct {
	Mode  uint32
	Size  int64
	Type  string
	Mtime time.Time
	Ctime time.Time
	// Gen is the snapshot generation the attributes came from, or 0 when they
	// came from the overlay.
	Gen int64
}

// Attrs returns the attributes of path. A base file or symlink whose size the
// snapshot does not know takes its size from b.
func (e *Engine) Attrs(b *Blobs, path string) (Attrs, error) {
	n, gen, commitTime, err := e.Resolver.ResolvePathState(path)
	if err != nil {
		return Attrs{}, err
	}
	if n.FromOverlay {
		return Attrs{
			Mode:  n.Overlay.Mode,
			Size:  n.Overlay.SizeBytes,
			Type:  n.Overlay.NodeType(),
			Mtime: time.Unix(0, n.Overlay.MtimeUnixNs),
			Ctime: time.Unix(0, n.Overlay.CtimeUnixNs),
		}, nil
	}
	size := n.Base.SizeBytes
	if (n.Base.Type == "file" || n.Base.Type == "symlink") && n.Base.SizeState != "known" && n.Base.ObjectOID != "" {
		if size, err = b.size(n.Base); err != nil {
			return Attrs{}, err
		}
	}
	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	if commitTime == 0 {
		commitTime = gen // fallback: commit time unavailable
	}
	mt := time.Unix(commitTime, 0)
	return Attrs{Mode: normalizeMode(n.Base.Mode, n.Base.Type), Size: size, Type: n.Base.Type, Mtime: mt, Ctime: mt, Gen: gen}, nil
}

// ReadDir lists path with the generation and commit time it listed.
func (e *Engine) ReadDir(ctx context.Context, path string) ([]ReaddirEntry, int64, int64, error) {
	return e.Resolver.ReaddirSnapshot(ctx, path)
}

// ensureOverlay promotes a base file to the overlay (copy-on-write from its
// blob in b). No-op if the path already has an overlay entry. The caller holds
// the transition read lock.
func (e *Engine) ensureOverlay(ctx context.Context, b *Blobs, path string) error {
	if _, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return err
	} else if ok {
		return nil
	}
	n, err := e.Resolver.resolvePath(path)
	if err != nil {
		return err
	}
	if n.Base.ObjectOID == "" {
		return e.copyOnWrite(ctx, path, n.Base, nil)
	}
	src, err := b.open(n.Base)
	if err != nil {
		return err
	}
	defer src.Close()
	return e.copyOnWrite(ctx, path, n.Base, src)
}

// copyOnWrite copies base, whose blob src holds, into the overlay at path. A
// base with no blob takes a nil src.
func (e *Engine) copyOnWrite(ctx context.Context, path string, base model.BaseNode, src *os.File) error {
	if src == nil {
		_, err := e.Overlay.EnsureCopyOnWrite(ctx, e.Repo, path, base)
		return err
	}
	_, err := e.Overlay.EnsureCopyOnWriteFrom(ctx, e.Repo, path, base, src)
	return err
}

// Owner permission bits that opening or truncating a file needs.
const (
	ownerRead  uint32 = 0o400
	ownerWrite uint32 = 0o200
)

// checkOwnerAccess refuses access when mode lacks any of its owner permission
// bits. The mount serves one user, so the owner's bits decide. An overlay
// file's backing file stays owner-writable whatever its mode, so this check
// is what enforces the mode. A base file's mode from Git, 644 or 755, grants
// the owner both bits.
func checkOwnerAccess(mode, access uint32) error {
	if access&^mode != 0 {
		return syscall.EACCES
	}
	return nil
}

// OpenFile opens path's content for a new file handle: the overlay file, as
// generation -1, or else the base blob of generation gen, or of the current
// generation when gen is 0, with the generation it opened. access holds the
// owner permission bits the handle needs; a handle that may read prefetches
// the directory of a base file it opens.
func (e *Engine) OpenFile(ctx context.Context, b *Blobs, path string, gen int64, access uint32) (*os.File, int64, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return nil, 0, err
	} else if ok {
		if ov.IsDeleted() {
			return nil, 0, os.ErrNotExist
		}
		if err := checkOwnerAccess(ov.Mode, access); err != nil {
			return nil, 0, err
		}
		f, err := os.OpenFile(ov.BackingPath, os.O_RDWR, 0)
		if err != nil {
			f, err = os.Open(ov.BackingPath)
		}
		return f, -1, err
	}
	f, gen, err := e.openBase(ctx, b, path, gen)
	if err != nil {
		return nil, 0, err
	}
	if access&ownerRead != 0 {
		e.prefetchSiblings(ctx, path)
	}
	return f, gen, nil
}

// Read fills dst from path at off and returns the filled prefix of dst.
func (e *Engine) Read(ctx context.Context, b *Blobs, path string, off int64, dst []byte) ([]byte, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return nil, err
	} else if ok {
		if ov.IsDeleted() {
			return nil, os.ErrNotExist
		}
		return readFileChunk(ov.BackingPath, off, dst)
	}
	n, err := e.Resolver.resolvePath(path)
	if err != nil {
		return nil, err
	}
	f, err := b.open(n.Base)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	e.prefetchSiblings(ctx, path)
	return readFileChunkFrom(f, off, dst)
}

// BaseCacheFile opens the base content of path at generation gen, or at the
// current generation when gen is 0, for a read, and returns the generation it
// opened. The caller closes the file. It opens nothing when the overlay holds
// path.
func (e *Engine) BaseCacheFile(ctx context.Context, b *Blobs, path string, gen int64) (*os.File, int64, bool, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return nil, 0, false, err
	} else if ok {
		if ov.IsDeleted() {
			return nil, 0, false, os.ErrNotExist
		}
		return nil, 0, false, nil
	}
	f, gen, err := e.openBase(ctx, b, path, gen)
	if err != nil {
		return nil, 0, false, err
	}
	e.prefetchSiblings(ctx, path)
	return f, gen, true, nil
}

// openBase opens the base blob of path at generation gen, or at the current
// generation when gen is 0 or HEAD reconciliation pruned gen, and returns the
// generation it opened. The caller holds the transition read lock.
func (e *Engine) openBase(ctx context.Context, b *Blobs, path string, gen int64) (*os.File, int64, error) {
	current := e.Resolver.Generation()
	if gen == 0 {
		gen = current
	}
	n, ok, err := e.Resolver.Snapshot.LookupNode(ctx, gen, path)
	if err == nil && !ok && gen != current {
		// HEAD reconciliation pruned gen.
		gen = current
		n, ok, err = e.Resolver.Snapshot.LookupNode(ctx, gen, path)
	}
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	f, err := b.open(n)
	if err != nil {
		return nil, 0, err
	}
	return f, gen, nil
}

func (e *Engine) Write(ctx context.Context, b *Blobs, path string, off int64, data []byte) (int, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if err := e.ensureOverlay(ctx, b, path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		// Path doesn't exist in snapshot -- create it
		if _, cErr := e.Overlay.CreateFile(ctx, path, 0o644); cErr != nil {
			return 0, cErr
		}
	}
	return e.Overlay.WriteFile(ctx, path, off, data)
}

// Flush commits the size and times of path's writes since its last flush.
func (e *Engine) Flush(ctx context.Context, path string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	return e.Overlay.FlushFile(ctx, path)
}

func (e *Engine) Sync(ctx context.Context, path string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	return e.Overlay.SyncFile(ctx, path)
}

// Create makes an empty file at path. A name that exists in the merged view
// returns fs.ErrExist and stays untouched: the kernel can send CREATE for a
// name that appeared after its lookup missed, such as after a HEAD change.
func (e *Engine) Create(ctx context.Context, path string, mode uint32) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if _, err := e.Resolver.resolvePath(path); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err := e.Overlay.CreateFile(ctx, path, mode)
	return err
}

func (e *Engine) Symlink(ctx context.Context, path string, target string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	_, err := e.Overlay.CreateSymlink(ctx, path, target)
	return err
}

// ReadSymlink returns the target of the symlink at path. It reads a base
// target with a bound, and holds no lock while it reads.
func (e *Engine) ReadSymlink(ctx context.Context, path string) (string, error) {
	n, err := e.Resolver.ResolvePath(path)
	if err != nil {
		return "", err
	}
	if n.FromOverlay && n.Overlay.Kind == model.OverlayKindSymlink {
		if len(n.Overlay.TargetPath) > model.MaxSymlinkTargetBytes {
			return "", syscall.ENAMETOOLONG
		}
		return n.Overlay.TargetPath, nil
	}
	if n.Base.ObjectOID == "" {
		return "", fs.ErrNotExist
	}
	if err := validateKnownSymlinkTargetSize(n.Base); err != nil {
		return "", err
	}
	data, err := e.Hydrator.ReadBlob(ctx, e.Repo, n.Base, model.MaxSymlinkTargetBytes)
	if errors.Is(err, model.ErrBlobTooLarge) {
		return "", fmt.Errorf("%w: %w", syscall.ENAMETOOLONG, err)
	}
	return string(data), err
}

func validateKnownSymlinkTargetSize(node model.BaseNode) error {
	if node.SizeState != "known" {
		return nil
	}
	if node.SizeBytes < 0 {
		return syscall.EIO
	}
	if node.SizeBytes > model.MaxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

// Unlink removes path. When open handles still read path, Unlink copies a
// base file into the overlay and pins the handles to that copy first; it
// fetches nothing otherwise. files may be nil when no handle can be open.
func (e *Engine) Unlink(ctx context.Context, b *Blobs, path string, files openFiles) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if _, err := e.Resolver.resolvePath(path); err != nil {
		return err
	}
	if files != nil && files.hasOpenFile(path) {
		if err := e.ensureOverlay(ctx, b, path); err != nil {
			return err
		}
		if err := e.pinOpenFiles(ctx, path, files); err != nil {
			return err
		}
	}
	return e.Overlay.Remove(ctx, path)
}

// pinOpenFiles points the open handles on path at its overlay file, if it has one.
func (e *Engine) pinOpenFiles(ctx context.Context, path string, files openFiles) error {
	if files == nil {
		return nil
	}
	ov, ok, err := e.Overlay.Lookup(ctx, path)
	if err != nil || !ok || ov.IsDeleted() || ov.BackingPath == "" {
		return err
	}
	return files.pinOpenFiles(path, ov.BackingPath)
}

// Rename moves oldPath to newPath. Moving a base file copies it into the
// overlay. A destination it replaces is copied only when open handles still
// read it, so that they keep its content. files may be nil when no handle can
// be open.
func (e *Engine) Rename(ctx context.Context, b *Blobs, oldPath, newPath string, files openFiles) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	source, err := e.Resolver.resolvePath(oldPath)
	if err != nil {
		return err
	}
	if oldPath == newPath {
		return nil
	}
	sourceType := resolvedNodeType(source)
	destination, err := e.Resolver.resolvePath(newPath)
	destinationExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if sourceType == "dir" {
		if strings.HasPrefix(newPath, oldPath+"/") {
			return syscall.EINVAL
		}
		if destinationExists && resolvedNodeType(destination) != "dir" {
			return syscall.ENOTDIR
		}
		return e.renameDirectory(ctx, b, oldPath, newPath)
	}
	if destinationExists && resolvedNodeType(destination) == "dir" {
		return syscall.EISDIR
	}
	gen := e.Resolver.Generation()
	dst, dstOK, err := e.Resolver.Snapshot.LookupNode(ctx, gen, newPath)
	if err != nil {
		return err
	}
	if dstOK && dst.Type == "dir" {
		return fs.ErrInvalid
	}

	copies := map[string]ResolvedNode{}
	if !source.FromOverlay {
		copies[oldPath] = source
	}
	if destinationExists && !destination.FromOverlay && files != nil && files.hasOpenFile(newPath) {
		copies[newPath] = destination
	}
	var blobs []model.BaseNode
	for _, n := range copies {
		if n.Base.ObjectOID != "" {
			blobs = append(blobs, n.Base)
		}
	}
	// Open both blobs before copying either, so a missing or replaced blob
	// leaves no copy-on-write source behind.
	if err := b.requireFiles(blobs); err != nil {
		return err
	}
	srcs := map[string]*os.File{}
	defer func() {
		for _, f := range srcs {
			_ = f.Close()
		}
	}()
	for path, n := range copies {
		if n.Base.ObjectOID == "" {
			continue
		}
		f, err := b.open(n.Base)
		if err != nil {
			return err
		}
		srcs[path] = f
	}
	for _, path := range []string{oldPath, newPath} {
		if n, ok := copies[path]; ok {
			if err := e.copyOnWrite(ctx, path, n.Base, srcs[path]); err != nil {
				return err
			}
		}
		if err := e.pinOpenFiles(ctx, path, files); err != nil {
			return err
		}
	}
	if source, err = e.Resolver.resolvePath(oldPath); err != nil {
		return err
	}

	ov := source.Overlay
	src, srcOK, err := e.Resolver.Snapshot.LookupNode(ctx, gen, oldPath)
	if err != nil {
		return err
	}
	if ov.Kind == model.OverlayKindCreate && srcOK && (src.Type == "file" || src.Type == "symlink") {
		if err := e.Overlay.RenameAndMarkModifiedFromBase(ctx, oldPath, oldPath, src.ObjectOID, src.Mode, true); err != nil {
			return err
		}
		return e.Overlay.Rename(ctx, oldPath, newPath, true)
	}
	if dstOK && (ov.Kind == model.OverlayKindCreate || ov.Kind == model.OverlayKindSymlink) {
		return e.Overlay.RenameAndMarkModifiedFromBase(ctx, oldPath, newPath, dst.ObjectOID, dst.Mode, srcOK)
	}
	return e.Overlay.Rename(ctx, oldPath, newPath, srcOK)
}

func resolvedNodeType(n ResolvedNode) string {
	if n.FromOverlay {
		return n.Overlay.NodeType()
	}
	return n.Base.Type
}

func (e *Engine) renameDirectory(ctx context.Context, b *Blobs, oldPath, newPath string) error {
	if oldPath == "." || newPath == "." || strings.HasPrefix(newPath, oldPath+"/") {
		return fs.ErrInvalid
	}
	if destination, err := e.Resolver.resolvePath(newPath); err == nil {
		if resolvedNodeType(destination) != "dir" {
			return fs.ErrInvalid
		}
		children, err := e.Resolver.readdirTypedAt(ctx, newPath, e.Resolver.Generation())
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return syscall.ENOTEMPTY
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	destinationBasePaths, err := e.snapshotTreePaths(ctx, newPath)
	if err != nil {
		return err
	}
	sourceBasePaths, err := e.snapshotTreePaths(ctx, oldPath)
	if err != nil {
		return err
	}
	visible, err := e.mergedTree(ctx, oldPath)
	if err != nil {
		return err
	}
	var blobs []model.BaseNode
	for _, n := range visible {
		if !n.node.FromOverlay && n.node.Base.Type != "dir" && n.node.Base.ObjectOID != "" {
			blobs = append(blobs, n.node.Base)
		}
	}
	if err := b.requireFiles(blobs); err != nil {
		return err
	}
	// Each blob opens only while it is copied, so a large tree holds one
	// descriptor at a time. A blob replaced since hydration stops the rename
	// after the directories it promoted and the files it copied before that
	// blob. Those entries hold base content, and the rerun completes the rename.
	for _, n := range visible {
		if n.node.FromOverlay {
			continue
		}
		if n.node.Base.Type == "dir" {
			if err := e.Overlay.Mkdir(ctx, n.path, n.node.Base.Mode); err != nil {
				return err
			}
			continue
		}
		if err := e.ensureOverlay(ctx, b, n.path); err != nil {
			return err
		}
	}
	return e.Overlay.RenameTree(ctx, oldPath, newPath, sourceBasePaths, destinationBasePaths)
}

type resolvedTreeNode struct {
	path string
	node ResolvedNode
}

func (e *Engine) mergedTree(ctx context.Context, root string) ([]resolvedTreeNode, error) {
	var out []resolvedTreeNode
	var walk func(string) error
	walk = func(path string) error {
		n, err := e.Resolver.resolvePath(path)
		if err != nil {
			return err
		}
		out = append(out, resolvedTreeNode{path: path, node: n})
		if resolvedNodeType(n) != "dir" {
			return nil
		}
		children, err := e.Resolver.readdirTypedAt(ctx, path, e.Resolver.Generation())
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := walk(model.CleanPath(filepath.Join(path, child.Name))); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) snapshotTreePaths(ctx context.Context, root string) ([]string, error) {
	gen := e.Resolver.Generation()
	n, ok, err := e.Resolver.Snapshot.LookupNode(ctx, gen, root)
	if err != nil || !ok {
		return nil, err
	}
	var out []string
	var walk func(model.BaseNode) error
	walk = func(n model.BaseNode) error {
		out = append(out, n.Path)
		if n.Type != "dir" {
			return nil
		}
		children, err := e.Resolver.Snapshot.ListChildren(gen, n.Path)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(n); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) Mkdir(ctx context.Context, path string, mode uint32) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	return e.Overlay.Mkdir(ctx, path, mode)
}

func (e *Engine) Rmdir(ctx context.Context, path string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	// Only allow rmdir if the merged directory is empty
	children, err := e.Resolver.readdirTypedAt(ctx, path, e.Resolver.Generation())
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return syscall.ENOTEMPTY
	}
	return e.Overlay.Remove(ctx, path)
}

// errAttrUnsupported rejects a mode or mtime change on the root or on a base
// entry that is neither a file nor a directory.
var errAttrUnsupported = fmt.Errorf("%w: %w", syscall.ENOTSUP, fs.ErrInvalid)

// SetAttributes applies each of size, mode, and mtime that is not nil to path,
// then returns its attributes. Changing a base file copies it into the
// overlay first.
// SetAttributes changes path's size, mode and mtime. sizeAccess holds the
// owner permission bits a size change needs: ownerWrite for a truncate
// without a handle, and none through a handle, whose open was checked.
func (e *Engine) SetAttributes(ctx context.Context, b *Blobs, path string, size *int64, sizeAccess uint32, mode *uint32, mtime *time.Time) (Attrs, error) {
	if err := func() error {
		e.Resolver.transition.RLock()
		defer e.Resolver.transition.RUnlock()
		if size != nil {
			if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
				return err
			} else if ok && !ov.IsDeleted() {
				if err := checkOwnerAccess(ov.Mode, sizeAccess); err != nil {
					return err
				}
			}
			if err := e.ensureOverlay(ctx, b, path); err != nil {
				return err
			}
			if err := e.Overlay.Truncate(ctx, path, *size); err != nil {
				return err
			}
		}
		if mode != nil {
			if err := e.promoteForAttr(ctx, b, path); err != nil {
				return err
			}
			if err := e.Overlay.SetMode(ctx, path, *mode); err != nil {
				return err
			}
		}
		if mtime != nil {
			if err := e.promoteForAttr(ctx, b, path); err != nil {
				return err
			}
			if err := e.Overlay.SetMtime(ctx, path, *mtime); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return Attrs{}, err
	}
	return e.Attrs(b, path)
}

// promoteForAttr promotes a base file or directory before an attribute change,
// so that the change never overwrites base snapshot attrs. The caller holds
// the transition read lock.
func (e *Engine) promoteForAttr(ctx context.Context, b *Blobs, path string) error {
	path = model.CleanPath(path)
	if path == "." {
		return errAttrUnsupported
	}
	if _, ok, err := e.Overlay.Lookup(ctx, path); err != nil || ok {
		return err
	}
	n, err := e.Resolver.resolvePath(path)
	if err != nil {
		return err
	}
	switch n.Base.Type {
	case "dir":
		return e.Overlay.Mkdir(ctx, path, n.Base.Mode)
	case "file":
		return e.ensureOverlay(ctx, b, path)
	default:
		return errAttrUnsupported
	}
}

// prefetchSiblings prefetches the files of path's directory the first time a
// base file in it is read at the current generation. A read through a handle
// still at an older generation counts at the current one, so reads at two
// generations do not prefetch the directory again and again. Listing a
// directory prefetches nothing, so directory walks stay lazy. It runs after the
// read's own blob is hydrated, so that read never waits in a speculative batch.
// The caller holds the transition read lock. Enqueueing only queues and wakes
// workers, so it hydrates nothing under that lock.
func (e *Engine) prefetchSiblings(ctx context.Context, path string) {
	dir := model.CleanPath(filepath.Dir(path))
	gen := e.Resolver.Generation()
	e.prefetchMu.Lock()
	if prefetchedGen, ok := e.prefetched[dir]; ok && prefetchedGen == gen {
		e.prefetchMu.Unlock()
		return
	}
	if e.prefetched == nil {
		e.prefetched = map[string]int64{}
	}
	e.prefetched[dir] = gen
	e.prefetchMu.Unlock()
	entries, err := e.Resolver.readdirTypedAt(ctx, dir, gen)
	if err != nil {
		e.prefetchMu.Lock()
		delete(e.prefetched, dir)
		e.prefetchMu.Unlock()
		return
	}
	e.PrefetchDir(dir, entries)
}

// PrefetchDir enqueues the base file children of a directory for speculative
// hydration. Enqueueing only queues, so the caller does not wait for a fetch.
func (e *Engine) PrefetchDir(dirPath string, entries []ReaddirEntry) {
	tasks := make([]model.HydrationTask, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "file" || entry.ObjectOID == "" {
			continue
		}
		childPath := model.CleanPath(filepath.Join(dirPath, entry.Name))
		pri := hydrator.ClassifyPriority(childPath)
		tasks = append(tasks, model.HydrationTask{
			RepoID:     e.Repo.ID,
			Path:       childPath,
			ObjectOID:  entry.ObjectOID,
			SizeState:  entry.SizeState,
			SizeBytes:  entry.SizeBytes,
			Priority:   pri,
			Reason:     "prefetch",
			EnqueuedAt: time.Now(),
		})
	}
	if len(tasks) > maxPrefetchTasksPerDir {
		sort.SliceStable(tasks, func(i, j int) bool {
			if tasks[i].Priority == tasks[j].Priority {
				return tasks[i].Path < tasks[j].Path
			}
			return tasks[i].Priority > tasks[j].Priority
		})
		tasks = tasks[:maxPrefetchTasksPerDir]
	}
	if len(tasks) == 0 {
		return
	}
	e.Hydrator.EnqueueBatch(tasks)
}

func readFileChunk(path string, off int64, dst []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readFileChunkFrom(f, off, dst)
}

func readFileChunkFrom(f *os.File, off int64, dst []byte) ([]byte, error) {
	n, err := f.ReadAt(dst, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return dst[:n], nil
}
