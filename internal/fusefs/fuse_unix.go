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
	// Unmount retries a busy mount until ctx ends, and forces the unmount
	// once Join has returned. ctx cannot interrupt an unmount(2) that hangs.
	Unmount(ctx context.Context) error
	// InvalidateChanged tells the kernel to drop what it caches for the mount
	// and the merged view no longer matches: the attributes and pages of an
	// inode whose type, permission bits or blob changed, the entry of a name
	// that vanished or changed type, and a negative entry whose name now
	// exists. It also retires the inode number of each path that vanished.
	// Call it after a generation switch, holding no lock an op takes. FSKit
	// takes no invalidations, so it only retires numbers there.
	InvalidateChanged() error
}

// CacheTTL is how long the kernel may cache the entries, negative entries and
// attributes that the mount reports, except those of the synthesized .git
// file, which never change.
const CacheTTL = time.Second

// negativeRecordTTL is how long a negative entry stays recorded. The kernel
// starts an entry's TTL when the reply reaches it, a little after it is sent.
const negativeRecordTTL = 2 * CacheTTL

// kernelNotifier sends cache invalidations to the kernel. *fuse.Notifier is one.
type kernelNotifier interface {
	InvalidateInode(inode fuseops.InodeID, offset, length int64) error
	InvalidateEntry(parent fuseops.InodeID, name string) error
}

// kernelEntry is a name in a directory inode, as the kernel caches it.
type kernelEntry struct {
	parent fuseops.InodeID
	name   string
}

// AbandonedMount is the error of a mount whose context ended before the mount
// completed. The mount ends in the background, and its server may get ops
// until it has, so Wait for it before mounting at the same path again.
type AbandonedMount interface {
	error
	Wait(ctx context.Context) error
}

var _ AbandonedMount = (*fuse.AbandonedMountError)(nil)

// ArtifactFuse is the FUSE adapter following the tigrisfs GoofysFuse pattern:
// embed NotImplementedFileSystem + core state, thin operation wrappers. Engine
// is its only API to the merged view.
type ArtifactFuse struct {
	fuseutil.NotImplementedFileSystem
	engine         *Engine
	gitfileContent []byte    // synthesized .git gitfile, computed once
	syntheticTime  time.Time // timestamps of synthesized entries; changing them can deadlock concurrent FSKit Git reads

	mu sync.RWMutex
	// handleOps keeps inode and handle bookkeeping consistent with the overlay:
	// Unlink, RmDir and Rename hold it exclusively, and every op that creates
	// or uses inodes and handles holds it shared. The switch pass's re-check,
	// unmapStillMissing, holds it shared too. Engine.Do releases it before it
	// hydrates a blob.
	handleOps sync.RWMutex
	// inodes holds each inode the kernel holds.
	inodes map[fuseops.InodeID]*InodeRef
	// pathToInode maps each existing path the kernel has looked up to its
	// inode number. A mapping outlives the kernel's FORGET, so a path keeps
	// its number while it exists. A mapping whose inode is not in inodes is
	// dormant. An inode the kernel still holds after its path stopped
	// existing is unmapped.
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
	// negatives holds each negative entry the kernel may still cache, with
	// the time its record lapses.
	negatives       map[kernelEntry]time.Time
	negativeSweepAt int
	// renameSeq is odd while a Rename op runs, and grows by two with each one.
	// A rename commits the overlay before it moves the mappings, so a switch
	// pass that a rename overlapped checks its vanished paths again.
	renameSeq uint64
	// writeSeq counts successful writes through file handles that are not
	// detached. Such a write recreates its path, so the switch pass leaves
	// mapped the path of an inode written since the pass read the table.
	writeSeq uint64

	// notifier is nil when nothing should reach the kernel. fs.mu guards it:
	// the mount publishes it after ops may have started.
	notifier kernelNotifier
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
	Gen int64
	// Blob is the Attrs.Blob last reported for this inode. Type, Mode and Blob
	// are the identity the kernel holds.
	Blob string
	// PageBlob is the base blob that the inode's last open served, while the
	// kernel's cached pages can hold only that blob. It is empty otherwise.
	PageBlob string
	// Prior is the identity a base reply replaced while no switch pass had
	// invalidated the inode since the kernel received Prior. The kernel's pages
	// can still hold Prior's blob, and the macFUSE kernel extension ignores a
	// lookup's attributes for an inode it holds, so the pass checks Prior too.
	// Its Type is empty when no reply replaced an identity.
	Prior Identity
	// Invalidated says the switch pass invalidated the inode and no reply has
	// reached the kernel since, so the kernel holds nothing of an older identity.
	Invalidated bool
	// WrittenAt is the writeSeq of the last write through this inode.
	WrittenAt uint64
	Refcnt    int64
	IsRoot    bool
	Overlay   bool
	Stale     bool
	// CreateOpenPending says a CreateFile made the inode and no OpenFile or
	// write has reached it since. The next OpenFile skips the permission
	// check: on an FSKit mount the creator's descriptor can arrive as a
	// separate open after the create, and a create grants its creator access
	// whatever the mode.
	CreateOpenPending bool
}

// report records attrs as what the kernel last received for ref. Reads then
// serve attrs.Gen, so the kernel's pages may mix two blobs when the blob changes.
//
// A reply computed before a generation switch can be recorded after a reply
// computed after it. The switch's InvalidateInode makes the kernel discard
// the older reply, so report keeps the newer generation.
//
// A base reply between a switch and its pass can change the identity before
// the pass compares it, so report keeps the identity it replaced in Prior.
func (ref *InodeRef) report(attrs Attrs) {
	if attrs.Gen != 0 && ref.Gen > attrs.Gen {
		return
	}
	if attrs.Gen != 0 && ref.Type != "" && !ref.Invalidated && ref.Prior.Type == "" && attrs.identity() != ref.identity() {
		ref.Prior = ref.identity()
	}
	ref.Invalidated = false
	if attrs.Blob != ref.Blob {
		ref.PageBlob = ""
	}
	ref.Type, ref.Mode, ref.Gen, ref.Blob = attrs.Type, attrs.Mode, attrs.Gen, attrs.Blob
}

func (ref *InodeRef) identity() Identity {
	return Identity{Type: ref.Type, Perm: ref.Mode & 0o777, Blob: ref.Blob}
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
	// wrote says the handle created or wrote its file, so its flush commits
	// the path's pending writes. A reader's close leaves them coalescing.
	wrote bool
	// writable says the handle's access mode allows writes.
	writable bool
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

func NewArtifactFuse(repo model.RepoPaths, engine *Engine) *ArtifactFuse {
	fs := &ArtifactFuse{
		engine:         engine,
		gitfileContent: fmt.Appendf(nil, "gitdir: %s\n", repo.GitDir),
		syntheticTime:  time.Now(),
		inodes:         make(map[fuseops.InodeID]*InodeRef),
		pathToInode:    make(map[string]fuseops.InodeID),
		nextInodeID:    fuseops.RootInodeID + 1,
		dirHandles:     make(map[fuseops.HandleID]*DirHandle),
		fileHandles:    make(map[fuseops.HandleID]*FileHandle),
		nextHandleID:   1,
		negatives:      make(map[kernelEntry]time.Time),
	}
	root := &InodeRef{ID: fuseops.RootInodeID, Path: ".", Type: "dir", Mode: 0o755, Refcnt: 1, IsRoot: true}
	fs.inodes[fuseops.RootInodeID] = root
	fs.pathToInode["."] = fuseops.RootInodeID
	return fs
}

// allocInode returns the inode of path with one more lookup, and records attrs
// as what the kernel last received for it. A path with a dormant mapping gets
// its number back. Caller must hold fs.mu write lock.
func (fs *ArtifactFuse) allocInode(path string, attrs Attrs) *InodeRef {
	id, ok := fs.pathToInode[path]
	if ref := fs.inodes[id]; ok && ref != nil {
		ref.Refcnt++
		ref.report(attrs)
		return ref
	}
	if !ok {
		id = fs.nextInodeID
		fs.nextInodeID++
	}
	ref := &InodeRef{ID: id, Path: path, Refcnt: 1}
	ref.report(attrs)
	fs.inodes[id] = ref
	fs.pathToInode[path] = id
	return ref
}

// allocEntry returns the inode of the name in parent at path, for an entry
// reply that carries attrs. The reply replaces any negative entry the kernel
// holds for the name. Caller must hold fs.mu write lock.
func (fs *ArtifactFuse) allocEntry(parent fuseops.InodeID, name, path string, attrs Attrs) *InodeRef {
	delete(fs.negatives, kernelEntry{parent, name})
	return fs.allocInode(path, attrs)
}

// createEntry returns a new inode for the name in parent at path, which an op
// just created. A created path never takes the number of the path that
// existed there before, even while the kernel still holds that inode. Caller
// must hold fs.mu write lock.
func (fs *ArtifactFuse) createEntry(parent fuseops.InodeID, name, path string, attrs Attrs) *InodeRef {
	delete(fs.pathToInode, path)
	return fs.allocEntry(parent, name, path, attrs)
}

// unmapPath retires the inode number of path, which stopped existing. An
// inode the kernel still holds keeps its number until its own FORGET.
func (fs *ArtifactFuse) unmapPath(path string) {
	fs.mu.Lock()
	delete(fs.pathToInode, path)
	fs.mu.Unlock()
}

// pathMapping is a path and the inode number it maps to.
type pathMapping struct {
	id   fuseops.InodeID
	path string
}

// unmapLocked unmaps each of ms whose path still maps to its number, unless
// its inode was written after writeSeq: an op may have mapped the path again,
// or a write recreated it, since the pass read the table. Caller must hold
// fs.mu write lock.
func (fs *ArtifactFuse) unmapLocked(ms []pathMapping, writeSeq uint64) {
	for _, m := range ms {
		if ref := fs.inodes[m.id]; ref != nil && ref.WrittenAt > writeSeq {
			continue
		}
		if fs.pathToInode[m.path] == m.id {
			delete(fs.pathToInode, m.path)
		}
	}
}

// unmapStillMissing checks the paths of vanished again and unmaps each one
// still missing. It holds handleOps meanwhile, which keeps every Rename op
// out, so a switch pass that a rename overlapped still makes progress.
func (fs *ArtifactFuse) unmapStillMissing(vanished []pathMapping, writeSeq uint64) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	paths := make([]string, len(vanished))
	for i, m := range vanished {
		paths[i] = m.path
	}
	missing, err := fs.engine.MissingPaths(context.Background(), paths)
	gone := make(map[string]bool, len(missing))
	for _, p := range missing {
		gone[p] = true
	}
	var still []pathMapping
	for _, m := range vanished {
		if gone[m.path] {
			still = append(still, m)
		}
	}
	fs.mu.Lock()
	fs.unmapLocked(still, writeSeq)
	fs.mu.Unlock()
	return err
}

// renaming marks a Rename op as running until done is called.
func (fs *ArtifactFuse) renaming() (done func()) {
	fs.mu.Lock()
	fs.renameSeq++
	fs.mu.Unlock()
	return func() {
		fs.mu.Lock()
		fs.renameSeq++
		fs.mu.Unlock()
	}
}

// dropLookups drops n of id's lookups. An inode the kernel no longer holds
// leaves inodes, and its path's mapping stays, dormant.
func (fs *ArtifactFuse) dropLookups(id fuseops.InodeID, n uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ref, ok := fs.inodes[id]
	if !ok {
		return
	}
	ref.Refcnt -= int64(n)
	if ref.Refcnt <= 0 && !ref.IsRoot {
		delete(fs.inodes, id)
	}
}

// recordNegative records a negative entry for name in parent, which the
// kernel caches for CacheTTL. Expired records go in sweeps whose cost each
// record pays for once.
func (fs *ArtifactFuse) recordNegative(parent fuseops.InodeID, name string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := time.Now()
	if len(fs.negatives) >= fs.negativeSweepAt {
		for e, lapses := range fs.negatives {
			if !lapses.After(now) {
				delete(fs.negatives, e)
			}
		}
		fs.negativeSweepAt = max(2*len(fs.negatives), 1024)
	}
	fs.negatives[kernelEntry{parent, name}] = now.Add(negativeRecordTTL)
}

// takeNegative removes the live record of a negative entry for path and
// returns it with the notifier to drop it, or false when the kernel holds none
// or nothing reaches the kernel.
func (fs *ArtifactFuse) takeNegative(path string) (kernelEntry, kernelNotifier, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	parent, ok := fs.pathToInode[model.CleanPath(filepath.Dir(path))]
	if !ok {
		return kernelEntry{}, nil, false
	}
	e := kernelEntry{parent, filepath.Base(path)}
	lapses, ok := fs.negatives[e]
	delete(fs.negatives, e)
	return e, fs.notifier, ok && lapses.After(time.Now()) && fs.notifier != nil
}

// setNotifier publishes the Notifier of the attempt that mounted.
func (fs *ArtifactFuse) setNotifier(n kernelNotifier) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.notifier = n
}

// invalidateChanged implements MountedFS.InvalidateChanged. It unmaps each
// mapped path that no longer exists, so a path made there later gets a new
// number. When send is true it also tells the kernel what changed. Every inode
// the kernel holds at a path gets the check for a name that vanished or
// changed type. An inode the kernel holds after its path stopped existing gets
// only the identity comparison, because getattr and reads on it resolve the
// path again. An inode whose identity the kernel received from the base
// snapshot gets the identity comparison. One the kernel last saw from the
// overlay is invalidated when it now resolves to a base blob, whose bytes can
// differ from what was written. An inode whose identity a reply replaced
// since the last pass is invalidated too: the kernel can still cache pages of
// the replaced identity, which report keeps in InodeRef.Prior. An overlay
// entry itself changes only through the mount, which keeps the kernel's view
// of it current. A dormant mapping
// gets no invalidation: the kernel holds nothing for it. A failure skips only
// what it concerns, and the first one is returned.
func (fs *ArtifactFuse) invalidateChanged(send bool) error {
	type known struct {
		pathMapping
		entry    kernelEntry
		reported Identity
		prior    Identity
		base     bool
		mapped   bool
	}
	now := time.Now()
	fs.mu.RLock()
	notifier := fs.notifier
	send = send && notifier != nil
	renameSeq, writeSeq := fs.renameSeq, fs.writeSeq
	var inodes, negatives []known
	// existenceOnly holds every other mapping: those that need only the check
	// that their path exists.
	var existenceOnly []pathMapping
	for path, id := range fs.pathToInode {
		if path == "." || path == ".git" {
			continue
		}
		ref := fs.inodes[id]
		parent, ok := fs.pathToInode[model.CleanPath(filepath.Dir(path))]
		if !send || ref == nil || !ok {
			existenceOnly = append(existenceOnly, pathMapping{id, path})
			continue
		}
		inodes = append(inodes, known{pathMapping: pathMapping{id, path}, entry: kernelEntry{parent, filepath.Base(path)}, reported: ref.identity(), prior: ref.Prior, base: ref.Gen != 0, mapped: true})
	}
	for id, ref := range fs.inodes {
		if send && !ref.IsRoot && !ref.Stale && ref.Path != ".git" && fs.pathToInode[ref.Path] != id {
			inodes = append(inodes, known{pathMapping: pathMapping{id, ref.Path}, reported: ref.identity(), prior: ref.Prior, base: ref.Gen != 0})
		}
	}
	for e, lapses := range fs.negatives {
		if parent := fs.inodes[e.parent]; parent != nil && !parent.Stale && lapses.After(now) {
			negatives = append(negatives, known{pathMapping: pathMapping{path: cleanChildPath(parent.Path, e.name)}, entry: e})
		}
	}
	fs.mu.RUnlock()

	var firstErr error
	failed := func(err error, what string) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", what, err)
		}
	}
	var entries []kernelEntry
	var changed []fuseops.InodeID
	var vanished []pathMapping
	// priors holds the Prior of each inode checked against a Prior.
	priors := map[fuseops.InodeID]Identity{}
	for _, k := range inodes {
		current, exists, err := fs.engine.Identity(k.path)
		if err != nil {
			failed(err, "resolve "+k.path)
			continue
		}
		// A Prior is an identity the kernel can still cache: a reply between
		// the switch and this pass replaced it.
		hidden := k.prior.Type != ""
		if hidden {
			priors[k.id] = k.prior
		}
		switch {
		case !exists || current.Type != k.reported.Type || hidden && current.Type != k.prior.Type:
			// The name of an unmapped inode already left the kernel, through
			// the op or the pass that unmapped it.
			if k.mapped {
				entries = append(entries, k.entry)
			}
			if k.mapped && !exists {
				vanished = append(vanished, k.pathMapping)
			}
		case hidden, k.base && current != k.reported, !k.base && current.Blob != "":
			changed = append(changed, k.id)
		}
	}
	paths := make([]string, len(existenceOnly))
	for i, m := range existenceOnly {
		paths[i] = m.path
	}
	missing, err := fs.engine.MissingPaths(context.Background(), paths)
	failed(err, "list parents")
	gone := make(map[string]bool, len(missing))
	for _, p := range missing {
		gone[p] = true
	}
	for _, m := range existenceOnly {
		if gone[m.path] {
			vanished = append(vanished, m)
		}
	}
	fs.mu.Lock()
	for id, prior := range priors {
		if ref := fs.inodes[id]; ref != nil && ref.Prior == prior {
			ref.Prior = Identity{}
		}
	}
	for _, id := range changed {
		if ref := fs.inodes[id]; ref != nil {
			ref.Invalidated, ref.Prior = true, Identity{}
		}
	}
	overlapped := fs.renameSeq != renameSeq || renameSeq%2 != 0
	if !overlapped {
		fs.unmapLocked(vanished, writeSeq)
	}
	fs.mu.Unlock()
	if overlapped && len(vanished) > 0 {
		failed(fs.unmapStillMissing(vanished, writeSeq), "check vanished paths again")
	}
	if !send {
		return firstErr
	}

	for _, k := range negatives {
		_, exists, err := fs.engine.Identity(k.path)
		if err != nil {
			failed(err, "resolve "+k.path)
			continue
		}
		if exists {
			entries = append(entries, k.entry)
			fs.mu.Lock()
			delete(fs.negatives, k.entry)
			fs.mu.Unlock()
		}
	}
	// ENOENT means the kernel no longer holds the entry or inode.
	sent := func(err error, what string) {
		if !errors.Is(err, syscall.ENOENT) {
			failed(err, "invalidate "+what)
		}
	}
	for _, e := range entries {
		sent(notifier.InvalidateEntry(e.parent, e.name), fmt.Sprintf("entry %q", e.name))
	}
	for _, id := range changed {
		sent(notifier.InvalidateInode(id, 0, 0), fmt.Sprintf("inode %d", id))
	}
	return firstErr
}

// openedBlob records that an open of id serves blob, which is empty for an
// overlay file. It reports whether the kernel may keep the inode's cached
// pages: only when they can hold nothing but that same base blob.
func (fs *ArtifactFuse) openedBlob(id fuseops.InodeID, blob string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ref := fs.inodes[id]
	if ref == nil {
		return false
	}
	keep := keepBasePageCache && blob != "" && ref.PageBlob == blob
	ref.PageBlob = blob
	return keep
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

// markWritten records a write through id at path. Reads then serve the
// overlay, and the kernel's cached pages hold the written bytes, not any base
// blob. A write that recreated the inode's vanished path maps the path back to
// the inode, unless another inode took the path.
func (fs *ArtifactFuse) markWritten(id fuseops.InodeID, path string) {
	fs.mu.Lock()
	if ref := fs.inodes[id]; ref != nil {
		ref.Gen, ref.PageBlob = 0, ""
		fs.writeSeq++
		ref.WrittenAt = fs.writeSeq
		if _, mapped := fs.pathToInode[path]; !mapped && ref.Path == path && !ref.Stale {
			fs.pathToInode[path] = id
		}
	}
	fs.mu.Unlock()
}

// setReported records attrs as what the kernel last received for id.
func (fs *ArtifactFuse) setReported(id fuseops.InodeID, attrs Attrs) {
	fs.mu.Lock()
	if ref := fs.inodes[id]; ref != nil {
		ref.report(attrs)
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
		fs.pathToInode[move.newPath] = move.id
		if ref := fs.inodes[move.id]; ref != nil {
			ref.Path = move.newPath
			ref.Gen = 0
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

// takeCreateOpenPending reports whether id's CreateOpenPending was set, and
// clears it.
func (fs *ArtifactFuse) takeCreateOpenPending(id fuseops.InodeID) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ref := fs.inodes[id]
	if ref == nil {
		return false
	}
	pending := ref.CreateOpenPending
	ref.CreateOpenPending = false
	return pending
}

// handleWritable says id names an open handle whose access mode allows
// writes. A nil id, from truncate(2), names none.
func (fs *ArtifactFuse) handleWritable(id *fuseops.HandleID) bool {
	if id == nil {
		return false
	}
	fh, err := fs.fileHandle(*id)
	if err != nil {
		return false
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.writable
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
			ref := fs.allocEntry(op.Parent, op.Name, ".git", Attrs{Type: "file", Mode: 0o644, Gen: fs.engine.Generation()})
			fs.mu.Unlock()
			op.Entry.Child = ref.ID
			op.Entry.Attributes = fs.gitFileAttrs()
			setChildEntryExpiry(&op.Entry, time.Minute)
			return nil
		}

		attrs, err := fs.engine.Attrs(b, childPath)
		if negativeLookupEntries && errors.Is(err, iofs.ErrNotExist) {
			// Child 0 is a negative entry, which the kernel caches.
			op.Entry = fuseops.ChildInodeEntry{}
			setChildEntryExpiry(&op.Entry, CacheTTL)
			fs.recordNegative(op.Parent, op.Name)
			return nil
		}
		if err != nil {
			return err
		}

		fs.mu.Lock()
		ref := fs.allocEntry(op.Parent, op.Name, childPath, attrs)
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		op.Entry.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		setChildEntryExpiry(&op.Entry, CacheTTL)
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
					op.AttributesExpiration = attrExpiry(CacheTTL)
					return nil
				}
			}
			op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", fs.syntheticTime, fs.syntheticTime)
			op.AttributesExpiration = attrExpiry(CacheTTL)
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
		fs.setReported(op.Inode, attrs)
		op.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		op.AttributesExpiration = attrExpiry(CacheTTL)
		return nil
	})
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	return fs.setInodeAttributes(ctx, op, false)
}

// setInodeAttributes changes an inode's attributes. checkPermissions checks a
// size change against the entry's owner write bit, unless it comes through a
// handle whose access mode allows writes.
func (fs *ArtifactFuse) setInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp, checkPermissions bool) error {
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
		var sizeAccess uint32
		if op.Size != nil && checkPermissions && !fs.handleWritable(op.Handle) {
			sizeAccess = ownerWrite
		}
		attrs, err := fs.engine.SetAttributes(ctx, b, ref.Path, size, sizeAccess, mode, op.Mtime)
		if changesContent {
			fs.closeCachedFilesForPath(ref.Path)
		}
		if err != nil {
			return err
		}
		fs.setReported(op.Inode, attrs)
		op.Attributes = inodeAttrs(attrs.Mode, uint64(attrs.Size), attrs.Type, attrs.Mtime, attrs.Ctime)
		op.AttributesExpiration = attrExpiry(CacheTTL)
		return nil
	})
}

func (fs *ArtifactFuse) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	fs.dropLookups(op.Inode, op.N)
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
			entry := fs.childEntryFromReaddir(dh.inode.ID, childPath, dh.gen, dh.commitTime, e)
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
				fs.dropLookups(entry.Child, 1)
				break
			}
			op.BytesRead += n
		}
		return nil
	})
}

func (fs *ArtifactFuse) childEntryFromReaddir(parent fuseops.InodeID, path string, gen, commitTime int64, e ReaddirEntry) fuseops.ChildInodeEntry {
	if path == ".git" {
		fs.mu.Lock()
		ref := fs.allocEntry(parent, e.Name, path, Attrs{Type: "file", Mode: 0o644, Gen: gen})
		fs.mu.Unlock()
		entry := fuseops.ChildInodeEntry{Child: ref.ID, Attributes: fs.gitFileAttrs()}
		setChildEntryExpiry(&entry, time.Minute)
		return entry
	}
	mode, size, typ, mtime, ctime := readdirAttrs(e, gen, commitTime)
	attrs := Attrs{Type: typ, Mode: mode}
	if !e.FromOverlay {
		attrs.Gen = gen
		attrs.Blob = baseBlob(model.BaseNode{Type: typ, ObjectOID: e.ObjectOID})
	}
	fs.mu.Lock()
	ref := fs.allocEntry(parent, e.Name, path, attrs)
	fs.mu.Unlock()
	entry := fuseops.ChildInodeEntry{
		Child:      ref.ID,
		Attributes: inodeAttrs(mode, uint64(size), typ, mtime, ctime),
	}
	setChildEntryExpiry(&entry, CacheTTL)
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
	return fs.openFile(ctx, op, false)
}

// openFile opens a handle. checkPermissions checks the open against the
// entry's owner permission bits: read and write for the access mode, and
// write for O_TRUNC.
func (fs *ArtifactFuse) openFile(ctx context.Context, op *fuseops.OpenFileOp, checkPermissions bool) error {
	created := fs.takeCreateOpenPending(op.Inode)
	return fs.shared(ctx, func(ctx context.Context, b *Blobs) error {
		ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
		if err != nil {
			return err
		}
		fh := &FileHandle{inode: ref, path: ref.Path, writable: !op.OpenFlags.IsReadOnly()}
		if ref.Path != ".git" {
			var access uint32
			if checkPermissions && !created {
				access = ownerRead | ownerWrite
				if op.OpenFlags.IsReadOnly() {
					access = ownerRead
				} else if op.OpenFlags.IsWriteOnly() {
					access = ownerWrite
				}
				if op.OpenFlags&syscall.O_TRUNC != 0 {
					access |= ownerWrite
				}
			}
			// An open for writing only reads nothing, so it prefetches nothing.
			f, gen, blob, err := fs.engine.OpenFile(ctx, b, ref.Path, ref.Gen, !op.OpenFlags.IsWriteOnly(), access)
			if err != nil {
				return err
			}
			fh.cacheFile, fh.cacheGeneration = f, gen
			op.KeepPageCache = fs.openedBlob(op.Inode, blob)
		}
		fs.mu.Lock()
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.fileHandles[handle] = fh
		fs.mu.Unlock()
		op.Handle = handle
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
		fh.wrote = true
		fh.mu.Unlock()
		// The creator has its descriptor once a write arrives.
		fs.takeCreateOpenPending(op.Inode)
		n, err := fs.engine.Write(ctx, b, path, op.Offset, op.Data)
		// A handle that cached the base content before the write promoted the
		// path, or while another write promoted it, would keep serving the
		// base. Close those caches before the write returns, failed or not.
		fs.closeCachedFilesForPath(path)
		if err != nil {
			return err
		}
		if n != len(op.Data) {
			return syscall.EIO
		}
		fs.markWritten(op.Inode, path)
		// A write recreates a path that vanished while the handle was open.
		// No reply tells the kernel the name exists again.
		if e, notifier, ok := fs.takeNegative(path); ok {
			go func() { _ = notifier.InvalidateEntry(e.parent, e.name) }()
		}
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
		ref := fs.createEntry(op.Parent, op.Name, childPath, Attrs{Type: "file", Mode: uint32(op.Mode)})
		ref.CreateOpenPending = true
		fh := &FileHandle{inode: ref, path: childPath, wrote: true, writable: !op.OpenFlags.IsReadOnly()}
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.fileHandles[handle] = fh
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", now, now)
		setChildEntryExpiry(&op.Entry, CacheTTL)
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
		ref := fs.createEntry(op.Parent, op.Name, childPath, Attrs{Type: "symlink", Mode: 0o120000})
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(0o120000, uint64(len(op.Target)), "symlink", now, now)
		setChildEntryExpiry(&op.Entry, CacheTTL)
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
		ref := fs.createEntry(op.Parent, op.Name, childPath, Attrs{Type: "dir", Mode: uint32(op.Mode)})
		fs.mu.Unlock()

		op.Entry.Child = ref.ID
		now := time.Now()
		op.Entry.Attributes = inodeAttrs(uint32(op.Mode)|uint32(os.ModeDir), 4096, "dir", now, now)
		setChildEntryExpiry(&op.Entry, CacheTTL)
		return nil
	})
}

func (fs *ArtifactFuse) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	return fs.exclusive(ctx, func(ctx context.Context, _ *Blobs) error {
		parent, childPath, err := fs.childPath(op.Parent, op.Name)
		if err != nil {
			return err
		}
		if isGitfile(parent, op.Name) {
			return syscall.EPERM
		}
		if err := fs.engine.Rmdir(ctx, childPath); err != nil {
			return err
		}
		fs.unmapPath(childPath)
		return nil
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
		fs.unmapPath(childPath)
		fs.detachOpenHandles(childPath)
		return nil
	})
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	return fs.exclusive(ctx, func(ctx context.Context, b *Blobs) error {
		defer fs.renaming()()
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
		fs.mu.Lock()
		// The kernel moves its dentry over any negative entry for the new name.
		delete(fs.negatives, kernelEntry{op.NewParent, op.NewName})
		fs.mu.Unlock()
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

func (fs *ArtifactFuse) FlushFile(ctx context.Context, op *fuseops.FlushFileOp) error {
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		fh, err := fs.fileHandle(op.Handle)
		if err != nil {
			return err
		}
		return fs.flushHandle(ctx, fh)
	})
}

// flushHandle commits the writes made through fh's path when fh wrote. A
// detached handle writes to a file no entry names, so it has nothing to
// commit.
func (fs *ArtifactFuse) flushHandle(ctx context.Context, fh *FileHandle) error {
	fh.mu.Lock()
	path, detached, wrote := fh.path, fh.detached, fh.wrote
	fh.mu.Unlock()
	if !wrote || detached || path == ".git" {
		return nil
	}
	return fs.engine.Flush(ctx, path)
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
	return fs.shared(ctx, func(ctx context.Context, _ *Blobs) error {
		fs.mu.Lock()
		fh := fs.fileHandles[op.Handle]
		delete(fs.fileHandles, op.Handle)
		fs.mu.Unlock()
		if fh == nil {
			return nil
		}
		err := fs.flushHandle(ctx, fh)
		fh.release()
		return err
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
	fs      *ArtifactFuse
	backend string
}

func (m *mountedFSWrapper) Backend() string { return m.backend }

func (m *mountedFSWrapper) InvalidateChanged() error {
	return m.fs.invalidateChanged(m.backend != BackendFSKit)
}

// fuseMount is fuse.MountContext. Package tests replace it so they cannot mount.
var fuseMount = fuse.MountContext

// FSName is the file system name of repo's mount, which the Linux mount
// table lists as the mount's source.
func FSName(repo string) string { return "artifact-fs:" + repo }

// FSSubtype is the subtype of every mount, which the Linux mount table lists
// as the type fuse.artifact-fs.
const FSSubtype = "artifact-fs"

// UnmountPath unmounts the mount at dir without the MountedFS that mounted
// it, such as one a daemon process that stopped left behind. It does not
// retry a busy mount.
func UnmountPath(dir string) error { return fuse.Unmount(dir) }

// newFileSystemServer is fuseutil.NewFileSystemServer. Package tests replace
// it to reach the file system each mount attempt serves.
var newFileSystemServer = fuseutil.NewFileSystemServer

// permissionCheckingFS serves an FSKit mount: artifact-fs checks permissions
// itself there. It checks each open, and each size change not made through a
// handle that may write, against the entry's owner permission bits.
type permissionCheckingFS struct {
	*ArtifactFuse
}

func (fs permissionCheckingFS) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	return fs.openFile(ctx, op, true)
}

func (fs permissionCheckingFS) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	return fs.setInodeAttributes(ctx, op, true)
}

// newNotifier is fuse.NewNotifier. Package tests replace it to tell the
// Notifiers of two mount attempts apart.
var newNotifier = fuse.NewNotifier

// fuseErrorLog writes each line the fuse library logs to logger, redacted by
// redact. A bug the library answered with EIO goes out as an error. A short
// FSKit read the library answered with EIO goes out as a warning. Any other op
// error, which the library logs for expected errnos such as EEXIST too, goes
// out at Debug. Anything else, such as a failed reply, goes out as a warning.
type fuseErrorLog struct {
	logger *slog.Logger
	repo   string
	redact func(string) string
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
	l.logger.Log(context.Background(), level, "fuse error", "repo", l.repo, "error", l.redact(msg))
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

// MountRepoWithGate mounts repo, gated by gate unless it is nil. ctx bounds
// the wait for the mount: when it ends first, the error is an AbandonedMount.
// opCtx is the parent of every op's context, so ending it cancels the ops in
// flight. The fuse library's error lines go to logger, after redact removes
// what a log must not show, such as the repo's remote. A nil redact
// redacts only what auth.RedactLogString detects.
func MountRepoWithGate(ctx, opCtx context.Context, repo model.RepoPaths, engine *Engine, gate *ReadyGate, logger *slog.Logger, redact func(string) string) (MountedFS, error) {
	if redact == nil {
		redact = func(msg string) string { return auth.RedactLogString(msg) }
	}
	backend, err := MountBackend()
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}
	fsint := NewArtifactFuse(repo, engine)

	mountCfg := fuse.MountConfig{
		FSName:                  FSName(repo.Name),
		Subtype:                 FSSubtype,
		DisableWritebackCaching: true,
		ErrorLogger:             log.New(fuseErrorLog{logger: logger, repo: repo.Name, redact: redact}, "", 0),
		OpContext:               opCtx,
	}
	// READDIRPLUS would cache unknown blob sizes as zero before lookup can hydrate them.
	platformMountConfig(&mountCfg)
	// notifiers holds each attempt's Notifier, keyed by whether the attempt
	// went through FSKit.
	notifiers := map[bool]*fuse.Notifier{}
	mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
		cfg := mountCfg
		if fskit {
			cfg.Backend = fuse.BackendFSKit
		}
		// A Server must not serve a second mount once a failed one has
		// answered INIT, so each attempt gets its own. The failed Server can
		// keep serving after fuse.Mount returns, so each attempt gets its own
		// Notifier too: a shared one could hand a notification to the failed
		// Server.
		notifier := newNotifier()
		notifiers[fskit] = notifier
		var served fuseutil.FileSystem = fsint
		if fskit {
			served = permissionCheckingFS{fsint}
		}
		server := fuse.NewServerWithNotifier(notifier, newFileSystemServer(NewGatedFileSystem(served, gate)))
		return fuseMount(ctx, repo.MountPath, server, &cfg)
	}

	mfs, mounted, err := platformMount(backend, mount)
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}
	fsint.setNotifier(notifiers[mounted == BackendFSKit])

	return &mountedFSWrapper{MountedFileSystem: mfs, fs: fsint, backend: mounted}, nil
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
