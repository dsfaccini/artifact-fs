package hydrator

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type BlobFetcher interface {
	BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error)
	FetchMissingBlobs(ctx context.Context, repo model.RepoConfig, objectOIDs []string) error
	ReadBlob(ctx context.Context, repo model.RepoConfig, objectOID string, maxBytes int64) ([]byte, error)
	VerifyBlob(ctx context.Context, repo model.RepoConfig, objectOID string, cachePath string) (ok bool, err error)
}

// OnHydratedFunc is called after a blob is successfully fetched. Allows the
// caller to update metadata (e.g., backfill file sizes in the snapshot).
type OnHydratedFunc func(repoID model.RepoID, objectOID string, size int64)

type Service struct {
	fetcher    BlobFetcher
	mu         sync.Mutex
	pq         priorityQueue
	queued     map[string]*taskItem
	wait       inflight[result]
	verifying  inflight[verifyResult]
	started    bool
	stopped    bool
	stopOnce   sync.Once
	background sync.WaitGroup
	stopCh     chan struct{}
	workReady  chan struct{} // signaled when new work is enqueued
	// reservedReady wakes the reserved worker, which takes only reads and
	// copies of blobs that are already local.
	reservedReady chan struct{}
	onHydrated    OnHydratedFunc
	verified      map[string]os.FileInfo
	active        map[string]struct{}
}

// maxFetchBatch caps the blobs one fetch requests. It matches the fusefs
// per-directory prefetch cap, so one listing's prefetch is one fetch.
const maxFetchBatch = 256

var ErrStopped = errors.New("hydrator stopped")

type result struct {
	cachePath string
	size      int64
	err       error
}

type verifyResult struct {
	ok  bool
	err error
}

func New(fetcher BlobFetcher) *Service {
	return &Service{
		fetcher:   fetcher,
		queued:    map[string]*taskItem{},
		wait:      newInflight[result](),
		verifying: newInflight[verifyResult](),
		stopCh:    make(chan struct{}),
		workReady: make(chan struct{}, 1),

		reservedReady: make(chan struct{}, 1),
		verified:      map[string]os.FileInfo{},
		active:        map[string]struct{}{},
	}
}

// SetOnHydrated registers a callback invoked after each successful blob fetch.
func (s *Service) SetOnHydrated(fn OnHydratedFunc) {
	s.mu.Lock()
	s.onHydrated = fn
	s.mu.Unlock()
}

// signalWork performs a non-blocking send on workReady and reservedReady to
// wake a worker of each kind.
func (s *Service) signalWork() {
	select {
	case s.workReady <- struct{}{}:
	default: // already signaled, workers will drain the queue
	}
	select {
	case s.reservedReady <- struct{}{}:
	default:
	}
}

func (s *Service) Start(workers int, repo model.RepoConfig) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.background.Add(workers)
	s.mu.Unlock()
	for i := range workers {
		// With more than one worker, one is reserved so a read never waits
		// for a prefetch download. A single worker has to take everything.
		reserved := workers > 1 && i == 0
		go func() {
			defer s.background.Done()
			s.worker(repo, reserved)
		}()
	}
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		close(s.stopCh)
		s.wait.closeAll(result{err: ErrStopped})
		s.verifying.closeAll(verifyResult{err: ErrStopped})
		s.queued = map[string]*taskItem{}
		s.pq = nil
		s.mu.Unlock()
		s.background.Wait()
	})
}

func (s *Service) Enqueue(task model.HydrationTask) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	enqueued := s.enqueueLocked(task)
	s.mu.Unlock()
	if enqueued {
		s.signalWork()
	}
}

func (s *Service) EnqueueBatch(tasks []model.HydrationTask) {
	if len(tasks) == 0 {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	enqueued := 0
	for _, task := range tasks {
		if s.enqueueLocked(task) {
			enqueued++
		}
	}
	s.mu.Unlock()
	for range enqueued {
		s.signalWork()
	}
}

func (s *Service) enqueueLocked(task model.HydrationTask) bool {
	key := taskKey(task.RepoID, task.ObjectOID)
	if _, ok := s.active[key]; ok {
		return false
	}
	if item, ok := s.queued[key]; ok {
		if task.Priority > item.task.Priority {
			item.task = task
			heap.Fix(&s.pq, item.index)
			return true
		}
		return false
	}
	item := &taskItem{task: task}
	heap.Push(&s.pq, item)
	s.queued[key] = item
	return true
}

func (s *Service) EnsureHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (cachePath string, size int64, err error) {
	cachePath = cachePathFor(repo, node.ObjectOID)
	key := taskKey(repo.ID, node.ObjectOID)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return "", 0, ErrStopped
	}
	s.mu.Unlock()
	if ch, raised, ok := s.joinInflight(key); ok {
		if raised {
			s.signalWork()
		}
		return s.awaitHydration(ctx, key, ch)
	}
	if size, ok, err := s.validateCachedBlob(ctx, repo, cachePath, node); err != nil {
		return "", 0, err
	} else if ok {
		return cachePath, size, nil
	}
	ch := make(chan result, 1)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return "", 0, ErrStopped
	}
	if _, ok := s.active[key]; ok || len(s.wait[key]) > 0 {
		s.wait.add(key, ch)
		raised := s.raiseForReadLocked(key)
		s.mu.Unlock()
		if raised {
			s.signalWork()
		}
		return s.awaitHydration(ctx, key, ch)
	}
	first := s.wait.add(key, ch)
	if first {
		first = s.enqueueLocked(explicitReadTask(repo.ID, node))
	}
	s.mu.Unlock()
	if first {
		s.signalWork()
	}
	return s.awaitHydration(ctx, key, ch)
}

// joinInflight adds a waiter to key's hydration when one is active or queued.
// raised says it moved key's queued task ahead of speculative work, so the
// caller must wake the reserved worker.
func (s *Service) joinInflight(key string) (ch chan result, raised bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.active[key]; !ok && len(s.wait[key]) == 0 {
		return nil, false, false
	}
	ch = make(chan result, 1)
	s.wait.add(key, ch)
	return ch, s.raiseForReadLocked(key), true
}

// raiseForReadLocked moves key's queued task ahead of speculative work, because
// a read now waits on it, and reports whether it did. A fetched task stays in
// queued for this, though it is active, so Enqueue still ignores it.
func (s *Service) raiseForReadLocked(key string) bool {
	item, ok := s.queued[key]
	if !ok || isExplicitRead(item.task) {
		return false
	}
	item.task.Priority = PriorityExplicitRead
	heap.Fix(&s.pq, item.index)
	return true
}

func (s *Service) awaitHydration(ctx context.Context, key string, ch chan result) (cachePath string, size int64, err error) {
	select {
	case <-ctx.Done():
		// Remove our channel from the wait list so the worker doesn't
		// send to an abandoned channel that nobody reads.
		s.mu.Lock()
		s.wait.remove(key, ch)
		s.mu.Unlock()
		return "", 0, ctx.Err()
	case r := <-ch:
		return r.cachePath, r.size, r.err
	}
}

func (s *Service) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	key := taskKey(repo.ID, node.ObjectOID)
	for {
		cachePath, size, err := s.EnsureHydrated(ctx, repo, node)
		if err != nil {
			return nil, 0, err
		}
		f, err := os.Open(cachePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				s.clearVerified(key)
				continue
			}
			return nil, 0, err
		}
		st, err := f.Stat()
		if err == nil && s.isVerified(key, st) {
			return f, size, nil
		}
		_ = f.Close()
		if err != nil {
			return nil, 0, err
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
	}
}

func (s *Service) ReadBlob(ctx context.Context, repo model.RepoConfig, node model.BaseNode, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("negative max bytes: %d", maxBytes)
	}
	if node.SizeState == "known" && node.SizeBytes > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	cachePath := cachePathFor(repo, node.ObjectOID)
	if st, err := os.Stat(cachePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if st.Size() > maxBytes {
		return s.fetcher.ReadBlob(ctx, repo, node.ObjectOID, maxBytes)
	}
	key := taskKey(repo.ID, node.ObjectOID)
	for {
		size, ok, err := s.validateCachedBlob(ctx, repo, cachePath, node)
		if err != nil {
			return nil, err
		}
		if !ok {
			return s.fetcher.ReadBlob(ctx, repo, node.ObjectOID, maxBytes)
		}
		f, err := os.Open(cachePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		st, statErr := f.Stat()
		if statErr == nil && s.isVerified(key, st) {
			data, readErr := readCachedBlobFrom(f, size, maxBytes)
			_ = f.Close()
			return data, readErr
		}
		_ = f.Close()
		if statErr != nil {
			return nil, statErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func readCachedBlob(cachePath string, size int64, maxBytes int64) ([]byte, error) {
	if size > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCachedBlobFrom(f, size, maxBytes)
}

func readCachedBlobFrom(f *os.File, size int64, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("short cache read: got %d bytes, want %d", len(data), size)
	}
	return data, nil
}

func (s *Service) validateCachedBlob(ctx context.Context, repo model.RepoConfig, cachePath string, node model.BaseNode) (size int64, ok bool, err error) {
	st, err := os.Stat(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if node.SizeState == "known" && st.Size() != node.SizeBytes {
		s.clearVerified(taskKey(repo.ID, node.ObjectOID))
		return 0, false, nil
	}
	verifiedFile := st
	key := taskKey(repo.ID, node.ObjectOID)
	if s.isVerified(key, st) {
		return st.Size(), true, nil
	}
	ok, err = s.verifyBlobOnce(ctx, key, verifiedFile, cachePath, func(verifyCtx context.Context) (bool, error) {
		return s.fetcher.VerifyBlob(verifyCtx, repo, node.ObjectOID, cachePath)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, err
		}
		return 0, false, nil
	}
	if !ok {
		s.clearVerified(key)
		return 0, false, nil
	}
	st, err = os.Stat(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !sameFileInfo(verifiedFile, st) {
		s.clearVerified(key)
		return 0, false, nil
	}
	s.rememberVerified(key, st)
	return st.Size(), true, nil
}

func (s *Service) isVerified(key string, current os.FileInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isVerifiedLocked(key, current)
}

func (s *Service) isVerifiedLocked(key string, current os.FileInfo) bool {
	previous, ok := s.verified[key]
	if !ok {
		return false
	}
	if !sameFileInfo(previous, current) {
		delete(s.verified, key)
		return false
	}
	return true
}

func sameFileInfo(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && os.SameFile(a, b)
}

func (s *Service) rememberVerified(key string, info os.FileInfo) {
	s.mu.Lock()
	s.verified[key] = info
	s.mu.Unlock()
}

func (s *Service) clearVerified(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.verified, key)
}

func (s *Service) verifyBlobOnce(ctx context.Context, key string, file os.FileInfo, cachePath string, verify func(context.Context) (bool, error)) (bool, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false, ErrStopped
	}
	// A verification can finish between the caller's isVerified check and this lock.
	if s.isVerifiedLocked(key, file) {
		s.mu.Unlock()
		return true, nil
	}
	ch := make(chan verifyResult, 1)
	first := s.verifying.add(key, ch)
	if first {
		s.background.Add(1)
	}
	s.mu.Unlock()

	if first {
		go func() {
			defer s.background.Done()
			s.runVerification(key, file, cachePath, verify)
		}()
	}
	return s.awaitVerification(ctx, key, ch)
}

func (s *Service) awaitVerification(ctx context.Context, key string, ch chan verifyResult) (bool, error) {
	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.verifying.remove(key, ch)
		s.mu.Unlock()
		return false, ctx.Err()
	case r := <-ch:
		return r.ok, r.err
	}
}

func (s *Service) runVerification(key string, file os.FileInfo, cachePath string, verify func(context.Context) (bool, error)) {
	verifyCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-verifyCtx.Done():
		}
	}()

	ok, err := verify(verifyCtx)
	if err == nil && ok {
		current, statErr := os.Stat(cachePath)
		if statErr != nil || !sameFileInfo(file, current) {
			ok = false
		}
	}
	r := verifyResult{ok: ok, err: err}

	s.mu.Lock()
	if err == nil && ok {
		s.verified[key] = file
	}
	waiters := s.verifying.take(key)
	s.mu.Unlock()

	notifyWaiters(waiters, r)
}

func (s *Service) QueueDepth(repoID model.RepoID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for _, item := range s.pq {
		if item.task.RepoID == repoID {
			c++
		}
	}
	return c
}

func (s *Service) worker(repo model.RepoConfig, reserved bool) {
	ready := s.workReady
	if reserved {
		ready = s.reservedReady
	}
	for {
		select {
		case <-s.stopCh:
			return
		case <-ready:
			// Drain the queue: process all available items before waiting
			// for the next signal. Re-signal if items remain so other
			// workers can help.
			for {
				if !s.step(repo, reserved) {
					break
				}
				s.signalWork()
			}
		}
	}
}

// step processes the top of the queue: it hydrates a fetched task, or pops a
// batch and fetches its missing blobs together. Returns true if it popped an
// item, false if the queue was empty or, for the reserved worker, held only
// prefetch work that downloads.
func (s *Service) step(repo model.RepoConfig, reserved bool) bool {
	s.mu.Lock()
	// Everything the reserved worker may take ranks above prefetch downloads,
	// so the top of the queue decides.
	if len(s.pq) == 0 || reserved && s.pq[0].rank() == rankDownload {
		s.mu.Unlock()
		return false
	}
	if s.pq[0].fetched {
		item := heap.Pop(&s.pq).(*taskItem)
		delete(s.queued, taskKey(item.task.RepoID, item.task.ObjectOID))
		more := len(s.pq) > 0
		s.mu.Unlock()
		if more {
			s.signalWork()
		}
		s.hydrate(repo, item.task)
		return true
	}
	batch := s.popBatchLocked()
	more := len(s.pq) > 0
	s.mu.Unlock()
	if more {
		s.signalWork()
	}
	s.fetchBatch(repo, batch)
	return true
}

// popBatchLocked pops up to maxFetchBatch unfetched tasks. Explicit reads and
// speculative prefetches never share a batch, so a read does not wait for a
// directory's prefetch to download.
func (s *Service) popBatchLocked() []model.HydrationTask {
	var batch []model.HydrationTask
	for len(s.pq) > 0 && len(batch) < maxFetchBatch && !s.pq[0].fetched {
		if len(batch) > 0 && isExplicitRead(s.pq[0].task) != isExplicitRead(batch[0]) {
			break
		}
		item := heap.Pop(&s.pq).(*taskItem)
		key := taskKey(item.task.RepoID, item.task.ObjectOID)
		delete(s.queued, key)
		if _, ok := s.active[key]; ok {
			continue
		}
		s.active[key] = struct{}{}
		batch = append(batch, item.task)
	}
	return batch
}

// fetchBatch finishes the batch's verified cache hits and handles the tasks
// that need no fetch, then fetches the missing blobs with one fetch and
// requeues them, so every worker can copy them in priority order.
func (s *Service) fetchBatch(repo model.RepoConfig, batch []model.HydrationTask) {
	var local, missing []model.HydrationTask
	for _, task := range batch {
		key := taskKey(task.RepoID, task.ObjectOID)
		cachePath := cachePathFor(repo, task.ObjectOID)
		st, err := os.Stat(cachePath)
		// A read of a verified blob must not wait for its siblings' fetch.
		// Verifying an unverified one hashes it, so hydrate does that.
		if err == nil && (task.SizeState != "known" || st.Size() == task.SizeBytes) && s.isVerified(key, st) {
			s.finishHydration(key, result{cachePath: cachePath, size: st.Size()})
			s.notifyHydrated(task.RepoID, task.ObjectOID, st.Size())
			continue
		}
		// A known size means batchResolveSizes or an earlier hydration found
		// the blob in the object store.
		if err == nil || task.SizeState == "known" {
			local = append(local, task)
		} else {
			missing = append(missing, task)
		}
	}
	s.requeueFetched(local, true)
	// A single blob's own lazy fetch costs the same as a batch fetch.
	if len(missing) < 2 {
		s.requeueFetched(missing, false)
		return
	}
	// A read of a blob that needs no fetch must not wait for its siblings'
	// fetch. Free workers copy those blobs in parallel, and the worker that
	// took the read batch hydrates the ones no other worker has taken before it
	// fetches. With a single worker, it hydrates them all.
	if isExplicitRead(batch[0]) {
		for _, task := range local {
			if s.takeQueued(taskKey(task.RepoID, task.ObjectOID)) {
				s.hydrate(repo, task)
			}
		}
	}
	oids := make([]string, len(missing))
	for i, task := range missing {
		oids[i] = task.ObjectOID
	}
	ctx, cancel := s.fetchContext()
	// A failed batch leaves each blob to its own lazy fetch, so an object the
	// remote refuses fails only its own task.
	err := s.fetcher.FetchMissingBlobs(ctx, repo, oids)
	cancel()
	s.requeueFetched(missing, err == nil)
}

// requeueFetched queues tasks whose batch fetch ran, or that needed none, for
// a worker to hydrate. local says their blobs need no download. A task a read
// waits on goes ahead of speculative work.
func (s *Service) requeueFetched(tasks []model.HydrationTask, local bool) {
	if len(tasks) == 0 {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	for _, task := range tasks {
		key := taskKey(task.RepoID, task.ObjectOID)
		if len(s.wait[key]) > 0 && !isExplicitRead(task) {
			task.Priority = PriorityExplicitRead
		}
		item := &taskItem{task: task, fetched: true, local: local}
		heap.Push(&s.pq, item)
		s.queued[key] = item
	}
	s.mu.Unlock()
	s.signalWork()
}

// takeQueued removes key's fetched task from the queue so the caller hydrates
// it. It reports false when another worker took the task or the service
// stopped.
func (s *Service) takeQueued(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.queued[key]
	if !ok {
		return false
	}
	heap.Remove(&s.pq, item.index)
	delete(s.queued, key)
	return true
}

// hydrate serves task from a valid cached blob, or else copies the blob into
// the cache, fetching it if it is still missing.
func (s *Service) hydrate(repo model.RepoConfig, task model.HydrationTask) {
	key := taskKey(task.RepoID, task.ObjectOID)
	cachePath := cachePathFor(repo, task.ObjectOID)
	if size, ok, err := s.validateCachedBlob(context.Background(), repo, cachePath, taskBaseNode(task)); err != nil {
		s.finishHydration(key, result{err: err})
		return
	} else if ok {
		s.finishHydration(key, result{cachePath: cachePath, size: size})
		s.notifyHydrated(task.RepoID, task.ObjectOID, size)
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		s.finishHydration(key, result{err: err})
		return
	}
	ctx, cancel := s.fetchContext()
	defer cancel()
	size, err := s.fetcher.BlobToCache(ctx, repo, task.ObjectOID, cachePath)
	if err != nil {
		s.finishHydration(key, result{err: fmt.Errorf("hydrate %s: %w", task.Path, err)})
		return
	}
	s.mu.Lock()
	if s.stopped {
		delete(s.active, key)
		s.mu.Unlock()
		return
	}
	if st, statErr := os.Stat(cachePath); statErr == nil {
		s.verified[key] = st
	}
	waits := s.wait.take(key)
	delete(s.active, key)
	s.mu.Unlock()
	notifyWaiters(waits, result{cachePath: cachePath, size: size, err: nil})
	s.notifyHydrated(task.RepoID, task.ObjectOID, size)
}

// fetchContext bounds a fetch so a stuck one cannot block a worker forever,
// and cancels it when the service stops.
func (s *Service) fetchContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func isExplicitRead(task model.HydrationTask) bool {
	return task.Priority >= PriorityExplicitRead
}

func taskBaseNode(task model.HydrationTask) model.BaseNode {
	return model.BaseNode{
		RepoID:    task.RepoID,
		Path:      task.Path,
		ObjectOID: task.ObjectOID,
		SizeState: task.SizeState,
		SizeBytes: task.SizeBytes,
	}
}

func (s *Service) notifyHydrated(repoID model.RepoID, objectOID string, size int64) {
	s.mu.Lock()
	fn := s.onHydrated
	s.mu.Unlock()
	if fn != nil {
		fn(repoID, objectOID, size)
	}
}

func (s *Service) finishHydration(key string, r result) {
	s.mu.Lock()
	waits := s.wait.take(key)
	delete(s.active, key)
	s.mu.Unlock()
	notifyWaiters(waits, r)
}

func taskKey(repoID model.RepoID, oid string) string {
	return string(repoID) + ":" + oid
}

func cachePathFor(repo model.RepoConfig, oid string) string {
	return filepath.Join(repo.BlobCacheDir, oid)
}

func explicitReadTask(repoID model.RepoID, node model.BaseNode) model.HydrationTask {
	return model.HydrationTask{
		RepoID:     repoID,
		Path:       node.Path,
		ObjectOID:  node.ObjectOID,
		SizeState:  node.SizeState,
		SizeBytes:  node.SizeBytes,
		Priority:   PriorityExplicitRead,
		Reason:     "explicit read",
		EnqueuedAt: time.Now(),
	}
}

const (
	PriorityExplicitRead = 1000
	PrioritySibling      = 800
	PriorityBootstrap    = 700
	PriorityLikelyText   = 500
	PriorityNearbyCode   = 400
	PriorityBinary       = 100
)

func ClassifyPriority(path string) int {
	base := filepath.Base(path)
	ext := filepath.Ext(path)
	switch {
	case base == "README" || base == "README.md" || base == "LICENSE" || base == "Makefile" || base == ".gitignore":
		return PriorityBootstrap
	case base == "go.mod" || base == "go.sum" || base == "Cargo.toml" || base == "package.json" || base == "pnpm-lock.yaml" || base == "pyproject.toml":
		return PriorityBootstrap
	case isCodeExtension(ext):
		return PriorityLikelyText
	case ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".zip" || ext == ".pdf" || ext == ".tar" || ext == ".gz" || ext == ".mp4" || ext == ".mov" || ext == ".avi":
		return PriorityBinary
	default:
		return PriorityNearbyCode
	}
}

func isCodeExtension(ext string) bool {
	switch ext {
	case ".go", ".rs", ".zig", ".py", ".ts", ".tsx", ".js", ".jsx",
		".java", ".c", ".cc", ".cpp", ".h", ".hpp",
		".json", ".yaml", ".yml", ".toml", ".md":
		return true
	}
	return false
}

type taskItem struct {
	task  model.HydrationTask
	index int
	// fetched marks a task whose batch fetch already ran. It stays active, so
	// Enqueue ignores it, and a worker hydrates it alone.
	fetched bool
	// local marks a fetched task whose blob is cached, of known size, or
	// downloaded by its batch fetch, so hydrating it downloads nothing.
	local bool
}

const (
	rankDownload = iota
	rankLocalCopy
	rankRead
)

// rank orders the queue ahead of priority: reads, then copies of prefetch
// blobs that are already local, then prefetch work that downloads. A blob a
// batch fetch already downloaded is copied before more prefetches download.
func (item *taskItem) rank() int {
	switch {
	case isExplicitRead(item.task):
		return rankRead
	case item.local:
		return rankLocalCopy
	}
	return rankDownload
}

type priorityQueue []*taskItem

func (p priorityQueue) Len() int { return len(p) }
func (p priorityQueue) Less(i, j int) bool {
	if ri, rj := p[i].rank(), p[j].rank(); ri != rj {
		return ri > rj
	}
	if p[i].task.Priority == p[j].task.Priority {
		return p[i].task.EnqueuedAt.Before(p[j].task.EnqueuedAt)
	}
	return p[i].task.Priority > p[j].task.Priority
}
func (p priorityQueue) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
	p[i].index, p[j].index = i, j
}
func (p *priorityQueue) Push(x any) {
	item := x.(*taskItem)
	item.index = len(*p)
	*p = append(*p, item)
}
func (p *priorityQueue) Pop() any {
	old := *p
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*p = old[:n-1]
	return item
}
