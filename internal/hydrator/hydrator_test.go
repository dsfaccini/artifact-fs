package hydrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestClassifyPriority(t *testing.T) {
	tests := []struct {
		path string
		want int
	}{
		{path: "README.md", want: PriorityBootstrap},
		{path: "packages/api/package.json", want: PriorityBootstrap},
		{path: "src/main.go", want: PriorityLikelyText},
		{path: "docs/architecture.md", want: PriorityLikelyText},
		{path: "src/generated", want: PriorityNearbyCode},
		{path: "assets/logo.png", want: PriorityBinary},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := ClassifyPriority(tt.path); got != tt.want {
				t.Fatalf("ClassifyPriority(%q) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

func TestEnsureHydratedRefetchesTruncatedKnownBlob(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: 7}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(fetcher.payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(fetcher.payload))
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(fetcher.payload) {
		t.Fatalf("cache contents = %q, want %q", data, fetcher.payload)
	}
}

func TestEnsureHydratedUsesValidKnownBlobCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(payload))}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("new-data"), verifyOK: true}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(payload))
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestEnsureHydratedUsesValidUnknownSizeCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("new-data"), verifyOK: true}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(payload))
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestEnsureHydratedRefetchesUnknownSizeCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("content"), verifyOK: false}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotSize != int64(len(fetcher.payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(fetcher.payload))
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
}

func TestEnsureHydratedVerifiesUnknownCacheHitOnce(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	releaseVerify := make(chan struct{})
	verifyStarted := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true, verifyStarted: verifyStarted, verifyWait: releaseVerify}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const readers = 8
	errCh := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			_, _, err := h.EnsureHydrated(ctx, cfg, node)
			errCh <- err
		})
	}
	<-verifyStarted
	runtime.Gosched()
	close(releaseVerify)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	}
	if fetcher.VerifyCalls() != 1 {
		t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestVerifyBlobOnceReusesVerificationFinishedAfterCheck(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true}
	h := New(fetcher)
	defer h.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// validateCachedBlob checks isVerified and calls verifyBlobOnce under
	// separate locks. Replay a reader whose check runs before another reader's
	// verification finishes and whose verifyBlobOnce runs after it.
	key := taskKey(cfg.ID, node.ObjectOID)
	st, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if h.isVerified(key, st) {
		t.Fatal("cache file verified before any reader")
	}
	if _, _, err := h.EnsureHydrated(ctx, cfg, node); err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	ok, err := h.verifyBlobOnce(ctx, key, st, cachePath, func(verifyCtx context.Context) (bool, error) {
		return fetcher.VerifyBlob(verifyCtx, cfg, node.ObjectOID, cachePath)
	})
	if err != nil || !ok {
		t.Fatalf("verifyBlobOnce = %v, %v; want true, nil", ok, err)
	}
	if fetcher.VerifyCalls() != 1 {
		t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
	}
}

func TestEnsureHydratedJoinsActiveFetch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(payload))}
	releaseFetch := make(chan struct{})
	fetchStarted := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: payload, fetchStarted: fetchStarted, fetchWait: releaseFetch}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const readers = 8
	errCh := make(chan error, readers+1)
	go func() {
		_, _, err := h.EnsureHydrated(ctx, cfg, node)
		errCh <- err
	}()
	<-fetchStarted
	for range readers {
		go func() {
			_, _, err := h.EnsureHydrated(ctx, cfg, node)
			errCh <- err
		}()
	}
	runtime.Gosched()
	close(releaseFetch)
	for range readers + 1 {
		if err := <-errCh; err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
}

func TestEnqueueDedupesAndUpgradesPriority(t *testing.T) {
	t.Parallel()
	h := New(&fakeBlobFetcher{})
	low := model.HydrationTask{RepoID: "repo", Path: "image.png", ObjectOID: "blob", Priority: PriorityBinary, Reason: "prefetch", EnqueuedAt: time.Now()}
	high := model.HydrationTask{RepoID: "repo", Path: "README.md", ObjectOID: "blob", Priority: PriorityBootstrap, Reason: "prefetch", EnqueuedAt: time.Now().Add(time.Second)}

	h.Enqueue(low)
	h.Enqueue(low)
	if got := h.QueueDepth("repo"); got != 1 {
		t.Fatalf("QueueDepth after duplicate enqueue = %d, want 1", got)
	}
	h.EnqueueBatch([]model.HydrationTask{low, high})
	if got := h.QueueDepth("repo"); got != 1 {
		t.Fatalf("QueueDepth after priority upgrade = %d, want 1", got)
	}
	h.mu.Lock()
	got := h.pq[0].task
	h.mu.Unlock()
	if got.Priority != PriorityBootstrap || got.Path != "README.md" {
		t.Fatalf("queued task = %+v, want upgraded README priority", got)
	}
}

func TestQueuedHydrationUsesValidCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeBlobFetcher{payload: []byte("new-data"), verifyOK: true}
	h := New(fetcher)
	hydrated := make(chan struct{})
	var hydratedOnce sync.Once
	h.SetOnHydrated(func(model.RepoID, string, int64) {
		hydratedOnce.Do(func() { close(hydrated) })
	})
	h.Enqueue(model.HydrationTask{
		RepoID:     cfg.ID,
		Path:       "file.txt",
		ObjectOID:  "blob",
		SizeState:  "known",
		SizeBytes:  int64(len(payload)),
		Priority:   PriorityBootstrap,
		Reason:     "prefetch",
		EnqueuedAt: time.Now(),
	})
	h.Start(1, cfg)
	defer h.Stop()

	<-hydrated
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
	if fetcher.VerifyCalls() != 1 {
		t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatalf("cache contents = %q, want %q", data, payload)
	}
}

func TestQueuedHydrationWakesWorkersForBacklog(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	releaseFetch := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: []byte("content"), fetchWait: releaseFetch}
	h := New(fetcher)
	for i := range 4 {
		h.Enqueue(model.HydrationTask{
			RepoID:     cfg.ID,
			Path:       filepath.Join("dir", "file.txt"),
			ObjectOID:  string(rune('a' + i)),
			Priority:   PriorityBootstrap,
			Reason:     "prefetch",
			EnqueuedAt: time.Now(),
		})
	}
	h.Start(4, cfg)
	defer h.Stop()

	waitForFetchCalls(t, fetcher, 4)
	close(releaseFetch)
	waitForHydratorIdle(t, h, cfg.ID)
}

func TestQueuedPrefetchFetchesBlobsInOnePack(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for i := range 8 {
		files[fmt.Sprintf("src/file-%d.go", i)] = fmt.Sprintf("package src // %d\n", i)
	}
	cfg, git, nodes := newBloblessRepo(t, files)
	packs := packCount(t, cfg.GitDir)

	h := New(git)
	h.EnqueueBatch(prefetchTasks(cfg.ID, nodes))
	h.Start(4, cfg)
	defer h.Stop()
	waitForHydratorIdle(t, h, cfg.ID)

	assertCachedFiles(t, cfg, nodes, files)
	// A lazy fetch per blob would add one pack per blob.
	if got := packCount(t, cfg.GitDir); got != packs+1 {
		t.Fatalf("packs = %d after hydrating %d blobs, want %d", got, len(nodes), packs+1)
	}
}

func TestQueuedPrefetchFetchesEachBlobWhenBatchFetchFails(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n", "c.go": "package c\n"}
	cfg, git, nodes := newBloblessRepo(t, files)
	// The remote refuses an object it lacks, which fails the whole batch fetch.
	tasks := append(prefetchTasks(cfg.ID, nodes), model.HydrationTask{
		RepoID:     cfg.ID,
		Path:       "unknown.go",
		ObjectOID:  strings.Repeat("1", 40),
		Priority:   PriorityLikelyText,
		Reason:     "prefetch",
		EnqueuedAt: time.Now(),
	})

	h := New(git)
	h.EnqueueBatch(tasks)
	h.Start(4, cfg)
	defer h.Stop()
	waitForHydratorIdle(t, h, cfg.ID)

	assertCachedFiles(t, cfg, nodes, files)
}

func TestExplicitReadsDoNotShareBatchWithPrefetch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	h := New(fetcher)
	var prefetch []model.HydrationTask
	for _, oid := range []string{"prefetch-1", "prefetch-2", "prefetch-3"} {
		prefetch = append(prefetch, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
	}
	h.EnqueueBatch(prefetch)
	reads := []string{"read-1", "read-2"}
	errs := make(chan error, len(reads))
	for _, oid := range reads {
		go func() {
			_, _, err := h.EnsureHydrated(context.Background(), cfg, model.BaseNode{RepoID: cfg.ID, Path: oid, ObjectOID: oid})
			errs <- err
		}()
	}
	for h.QueueDepth(cfg.ID) < len(prefetch)+len(reads) {
		time.Sleep(10 * time.Millisecond)
	}
	h.Start(1, cfg)
	defer h.Stop()
	for range reads {
		if err := <-errs; err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	}
	waitForHydratorIdle(t, h, cfg.ID)

	got := fetcher.Batches()
	for _, batch := range got {
		slices.Sort(batch)
	}
	want := [][]string{reads, {"prefetch-1", "prefetch-2", "prefetch-3"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("fetch batches = %v, want %v", got, want)
	}
}

func TestQueuedHydrationCapsFetchBatch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	h := New(fetcher)
	start := time.Now()
	tasks := make([]model.HydrationTask, maxFetchBatch+1)
	for i := range tasks {
		oid := fmt.Sprintf("%040x", i+1)
		tasks[i] = model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: start.Add(time.Duration(i))}
	}
	h.EnqueueBatch(tasks)
	h.Start(1, cfg)
	defer h.Stop()
	waitForHydratorIdle(t, h, cfg.ID)

	// The task left over after a full batch fetches on its own.
	batches := fetcher.Batches()
	if len(batches) != 1 || len(batches[0]) != maxFetchBatch {
		sizes := make([]int, len(batches))
		for i, batch := range batches {
			sizes[i] = len(batch)
		}
		t.Fatalf("fetch batch sizes = %v, want [%d]", sizes, maxFetchBatch)
	}
	if got := fetcher.Calls(); got != len(tasks) {
		t.Fatalf("copies = %d, want %d", got, len(tasks))
	}
}

func TestQueuedHydrationBatchFetchesOnlyMissingBlobs(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	payload := []byte("content")
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true}
	h := New(fetcher)
	var tasks []model.HydrationTask
	// A known size means the object store holds the blob.
	for _, oid := range []string{"local-1", "local-2", "local-3"} {
		tasks = append(tasks, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, SizeState: "known", SizeBytes: int64(len(payload)), Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
	}
	for _, oid := range []string{"cached-1", "cached-2"} {
		if err := os.WriteFile(cachePathFor(cfg, oid), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, SizeState: "unknown", Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
	}
	for _, oid := range []string{"missing-1", "missing-2"} {
		tasks = append(tasks, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, SizeState: "unknown", Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
	}
	h.EnqueueBatch(tasks)
	h.Start(1, cfg)
	defer h.Stop()
	waitForHydratorIdle(t, h, cfg.ID)

	got := fetcher.Batches()
	for _, batch := range got {
		slices.Sort(batch)
	}
	if want := [][]string{{"missing-1", "missing-2"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("fetch batches = %v, want %v", got, want)
	}
	if got, want := fetcher.Calls(), len(tasks)-2; got != want {
		t.Fatalf("copies = %d, want %d: the cached blobs need none", got, want)
	}
}

func TestQueuedHydrationVerifiesCachedBlobsInParallel(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	payload := []byte("content")
	releaseVerify := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true, verifyWait: releaseVerify}
	h := New(fetcher)
	var tasks []model.HydrationTask
	for _, oid := range []string{"cached-1", "cached-2"} {
		if err := os.WriteFile(cachePathFor(cfg, oid), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, SizeState: "known", SizeBytes: int64(len(payload)), Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
	}
	h.EnqueueBatch(tasks)
	h.Start(2, cfg)
	defer h.Stop()

	// The worker that pops the batch must not verify it alone: both
	// verifications run before either finishes.
	waitForVerifyCalls(t, fetcher, len(tasks))
	close(releaseVerify)
	waitForHydratorIdle(t, h, cfg.ID)
	if got := fetcher.Calls(); got != 0 {
		t.Fatalf("copies = %d, want 0: both blobs were cached", got)
	}
}

func TestReadOfFetchingPrefetchBlobGoesAheadOfOtherPrefetch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	started, release := gateBatchFetches(fetcher)
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()
	h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityBinary, "a-0", "a-1"))
	<-started
	// A read of a-0 joins its batch while the batch downloads, and another
	// directory is opened.
	done := make(chan error, 1)
	go func() {
		_, _, err := h.EnsureHydrated(context.Background(), cfg, model.BaseNode{RepoID: cfg.ID, Path: "a-0", ObjectOID: "a-0"})
		done <- err
	}()
	waitForWaiter(t, h, taskKey(cfg.ID, "a-0"))
	h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityLikelyText, "b-0", "b-1"))
	release <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	case oids := <-started:
		if !slices.Contains(fetcher.Copied(), "a-0") {
			t.Fatalf("worker started fetching %v while a read waited on fetched blob a-0", oids)
		}
		if err := <-done; err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	}
}

func TestReadOfFetchedPrefetchBlobGoesAheadOfOtherPrefetch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	started, release := gateBatchFetches(fetcher)
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()
	h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityBinary, "a-0", "a-1"))
	<-started
	h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityLikelyText, "c-0", "c-1"))
	release <- struct{}{}
	// a-0 now waits in the queue, fetched, while the worker fetches c.
	<-started
	done := make(chan error, 1)
	go func() {
		_, _, err := h.EnsureHydrated(context.Background(), cfg, model.BaseNode{RepoID: cfg.ID, Path: "a-0", ObjectOID: "a-0"})
		done <- err
	}()
	waitForWaiter(t, h, taskKey(cfg.ID, "a-0"))
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	waitForHydratorIdle(t, h, cfg.ID)

	want := []string{"a-0", "c-0", "c-1", "a-1"}
	if got := fetcher.Copied(); !slices.Equal(got, want) {
		t.Fatalf("copy order = %v, want %v", got, want)
	}
}

func TestReadOfVerifiedBlobDoesNotWaitForSiblingFetch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	payload := []byte("content")
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true}
	started, release := gateBatchFetches(fetcher)
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()
	cached := model.BaseNode{RepoID: cfg.ID, Path: "cached", ObjectOID: "cached", SizeState: "unknown"}
	if err := os.WriteFile(cachePathFor(cfg, "cached"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.EnsureHydrated(context.Background(), cfg, cached); err != nil {
		t.Fatalf("EnsureHydrated verifying the cached blob: %v", err)
	}
	// The directory is opened: the verified file and two missing siblings.
	h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityLikelyText, "cached", "sib-0", "sib-1"))
	<-started

	h.mu.Lock()
	_, waits := h.active[taskKey(cfg.ID, "cached")]
	h.mu.Unlock()
	if waits {
		t.Fatal("a read of the verified blob would wait for its siblings' fetch")
	}
	if _, _, err := h.EnsureHydrated(context.Background(), cfg, cached); err != nil {
		t.Fatalf("EnsureHydrated during the siblings' fetch: %v", err)
	}
	release <- struct{}{}
}

func TestBlobsNeedingNoFetchDoNotWaitForTheirSiblingsFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
		payload := []byte("content")
		fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true}
		started, release := gateBatchFetches(fetcher)
		// After a daemon restart a cached blob is not verified yet.
		if err := os.WriteFile(cachePathFor(cfg, "cached"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		h := New(fetcher)
		h.Start(2, cfg)
		defer func() {
			close(release)
			h.Stop()
		}()
		tasks := prefetchTasksFor(cfg.ID, PriorityLikelyText, "cached", "sib-0", "sib-1")
		tasks = append(tasks, model.HydrationTask{RepoID: cfg.ID, Path: "local", ObjectOID: "local", SizeState: "known", SizeBytes: int64(len(payload)), Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()})
		h.EnqueueBatch(tasks)
		// One worker waits in the siblings' fetch; the other is free.
		synctest.Wait()
		if got := len(started); got != 1 {
			t.Fatalf("batch fetches = %d, want 1", got)
		}

		for _, oid := range []string{"cached", "local"} {
			h.mu.Lock()
			_, active := h.active[taskKey(cfg.ID, oid)]
			h.mu.Unlock()
			if active {
				t.Fatalf("a read of %s would wait for its siblings' fetch", oid)
			}
		}
	})
}

func TestOneWorkerStaysFreeForReadsDuringPrefetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const workers = 4
		cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
		fetcher := &fakeBlobFetcher{payload: []byte("content")}
		started, release := gateBatchFetches(fetcher)
		h := New(fetcher)
		h.Start(workers, cfg)
		defer func() {
			close(release)
			h.Stop()
		}()
		oids := make([]string, workers*maxFetchBatch)
		for i := range oids {
			oids[i] = fmt.Sprintf("prefetch-%d", i)
		}
		h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityLikelyText, oids...))
		synctest.Wait()
		if got := len(started); got != workers-1 {
			t.Fatalf("prefetch batch fetches = %d, want %d: one worker stays free for reads", got, workers-1)
		}

		// The free worker serves an unrelated read while the prefetches download.
		if _, _, err := h.EnsureHydrated(context.Background(), cfg, model.BaseNode{RepoID: cfg.ID, Path: "read", ObjectOID: "read"}); err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	})
}

func TestStopCancelsBatchFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
		fetcher := &fakeBlobFetcher{payload: []byte("content")}
		started, _ := gateBatchFetches(fetcher)
		h := New(fetcher)
		h.Start(1, cfg)
		h.EnqueueBatch(prefetchTasksFor(cfg.ID, PriorityLikelyText, "a-0", "a-1"))
		<-started

		begin := time.Now()
		h.Stop()
		if waited := time.Since(begin); waited != 0 {
			t.Fatalf("Stop waited %v for the batch fetch to time out", waited)
		}
	})
}

func TestExplicitReadCopiesBeforeFetchedPrefetch(t *testing.T) {
	t.Parallel()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
	copyStarted := make(chan struct{})
	releaseCopy := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: []byte("content"), fetchStarted: copyStarted, fetchWait: releaseCopy}
	h := New(fetcher)
	start := time.Now()
	var prefetch []model.HydrationTask
	for i, oid := range []string{"prefetch-1", "prefetch-2", "prefetch-3"} {
		prefetch = append(prefetch, model.HydrationTask{RepoID: cfg.ID, Path: oid, ObjectOID: oid, Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: start.Add(time.Duration(i))})
	}
	h.EnqueueBatch(prefetch)
	h.Start(1, cfg)
	defer h.Stop()
	<-copyStarted
	// The batch is fetched and its first copy is running; a read arrives.
	errs := make(chan error, 1)
	go func() {
		_, _, err := h.EnsureHydrated(context.Background(), cfg, model.BaseNode{RepoID: cfg.ID, Path: "read", ObjectOID: "read"})
		errs <- err
	}()
	waitForQueued(t, h, taskKey(cfg.ID, "read"))
	close(releaseCopy)
	if err := <-errs; err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	waitForHydratorIdle(t, h, cfg.ID)

	want := []string{"prefetch-1", "read", "prefetch-2", "prefetch-3"}
	if got := fetcher.Copied(); !slices.Equal(got, want) {
		t.Fatalf("copy order = %v, want %v", got, want)
	}
}

func TestEnsureHydratedVerificationIgnoresLeaderTimeout(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// In a synctest bubble the leader's deadline passes only once the follower
	// is blocked on the running verification.
	synctest.Test(t, func(t *testing.T) {
		releaseVerify := make(chan struct{})
		verifyStarted := make(chan struct{})
		fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true, verifyStarted: verifyStarted, verifyWait: releaseVerify}
		h := New(fetcher)

		leaderCtx, leaderCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer leaderCancel()
		leaderErrCh := make(chan error, 1)
		go func() {
			_, _, err := h.EnsureHydrated(leaderCtx, cfg, node)
			leaderErrCh <- err
		}()
		<-verifyStarted

		followerCtx, followerCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer followerCancel()
		followerErrCh := make(chan error, 1)
		go func() {
			_, _, err := h.EnsureHydrated(followerCtx, cfg, node)
			followerErrCh <- err
		}()

		leaderErr := <-leaderErrCh
		if leaderErr == nil {
			t.Fatal("leader call unexpectedly succeeded")
		}
		close(releaseVerify)
		if err := <-followerErrCh; err != nil {
			t.Fatalf("follower EnsureHydrated: %v", err)
		}
		if fetcher.VerifyCalls() != 1 {
			t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
		}
	})
}

func TestValidateCachedBlobKeepsFileOnVerifyError(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: payload, verifyErr: errors.New("verify failed")}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	size, ok, err := h.validateCachedBlob(ctx, cfg, cachePath, node)
	if err != nil {
		t.Fatalf("validateCachedBlob: %v", err)
	}
	if ok {
		t.Fatalf("validateCachedBlob unexpectedly trusted cache file with verify error")
	}
	if size != 0 {
		t.Fatalf("size = %d, want 0 when validation falls back to refetch", size)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file should remain on verify error: %v", err)
	}

	fetcher.verifyErr = nil
	fetcher.verifyOK = true
	size, ok, err = h.validateCachedBlob(ctx, cfg, cachePath, node)
	if err != nil {
		t.Fatalf("validateCachedBlob after recovery: %v", err)
	}
	if !ok {
		t.Fatal("validateCachedBlob did not trust recovered cache file")
	}
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
}

func TestEnsureHydratedRevalidatesChangedCacheFile(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: 7}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeBlobFetcher{payload: []byte("correct"), verifyOK: true}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	if _, _, err := h.EnsureHydrated(context.Background(), cfg, node); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(cachePath, future, future); err != nil {
		t.Fatal(err)
	}
	fetcher.mu.Lock()
	fetcher.verifyOK = false
	fetcher.mu.Unlock()

	if _, _, err := h.EnsureHydrated(context.Background(), cfg, node); err != nil {
		t.Fatal(err)
	}
	if fetcher.VerifyCalls() != 3 {
		t.Fatalf("verify calls = %d, want 3", fetcher.VerifyCalls())
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
}

func TestValidateCachedBlobRejectsReplacementDuringVerification(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, ObjectOID: "blob", SizeState: "known", SizeBytes: 7}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	fetcher := &fakeBlobFetcher{verifyOK: true, verifyStarted: started, verifyWait: release}
	h := New(fetcher)
	defer h.Stop()
	type validation struct {
		ok  bool
		err error
	}
	done := make(chan validation, 1)
	go func() {
		_, ok, err := h.validateCachedBlob(context.Background(), cfg, cachePath, node)
		done <- validation{ok: ok, err: err}
	}()
	<-started
	replacement := filepath.Join(tmp, "replacement")
	if err := os.WriteFile(replacement, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, cachePath); err != nil {
		t.Fatal(err)
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.ok {
		t.Fatal("replacement cache file was trusted without verification")
	}
}

func TestOpenHydratedPinsVerifiedCacheFile(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, ObjectOID: "blob", SizeState: "known", SizeBytes: 7}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(&fakeBlobFetcher{verifyOK: true})
	file, _, err := h.OpenHydrated(context.Background(), cfg, node)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	replacement := filepath.Join(tmp, "replacement")
	if err := os.WriteFile(replacement, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, cachePath); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("opened data = %q, want verified content", data)
	}
}

func TestReadCachedBlobRejectsShortRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCachedBlob(path, 10, 10); err == nil {
		t.Fatal("expected short cache read error")
	}
}

func TestStopWaitsForWorkerAndRejectsLaterWork(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		fetcher := &fakeBlobFetcher{payload: []byte("content"), fetchStarted: started, fetchWait: release}
		h := New(fetcher)
		h.Start(1, cfg)
		go func() { _, _, _ = h.EnsureHydrated(context.Background(), cfg, node) }()
		<-started

		stopped := make(chan struct{})
		go func() {
			h.Stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while worker was still running")
		default:
		}
		close(release)
		<-stopped
		if _, _, err := h.EnsureHydrated(context.Background(), cfg, node); !errors.Is(err, ErrStopped) {
			t.Fatalf("EnsureHydrated after Stop err = %v, want ErrStopped", err)
		}
		h.Enqueue(model.HydrationTask{RepoID: cfg.ID, ObjectOID: "other"})
		if depth := h.QueueDepth(cfg.ID); depth != 0 {
			t.Fatalf("queue depth after Stop = %d, want 0", depth)
		}
	})
}

func TestReadBlobRejectsKnownOversizedWithoutFetch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "link", ObjectOID: "blob", SizeState: "known", SizeBytes: 6}
	fetcher := &fakeBlobFetcher{payload: []byte("target")}
	h := New(fetcher)

	_, err := h.ReadBlob(context.Background(), cfg, node, 5)
	if !errors.Is(err, model.ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("BlobToCache calls = %d, want 0", fetcher.Calls())
	}
	if fetcher.ReadBlobCalls() != 0 {
		t.Fatalf("ReadBlob calls = %d, want 0", fetcher.ReadBlobCalls())
	}
}

func TestReadBlobUsesBoundedFetcherForUnknownSize(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "link", ObjectOID: "blob", SizeState: "unknown"}
	fetcher := &fakeBlobFetcher{payload: []byte("target")}
	h := New(fetcher)

	_, err := h.ReadBlob(context.Background(), cfg, node, 5)
	if !errors.Is(err, model.ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("BlobToCache calls = %d, want 0", fetcher.Calls())
	}
	if fetcher.ReadBlobCalls() != 1 {
		t.Fatalf("ReadBlob calls = %d, want 1", fetcher.ReadBlobCalls())
	}
}

func TestReadBlobSkipsVerificationForOversizedCache(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "link", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("oversized"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeBlobFetcher{payload: []byte("ok"), verifyOK: true}
	h := New(fetcher)

	data, err := h.ReadBlob(context.Background(), cfg, node, 5)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("data = %q, want ok", data)
	}
	if fetcher.VerifyCalls() != 0 {
		t.Fatalf("VerifyBlob calls = %d, want 0", fetcher.VerifyCalls())
	}
	if fetcher.ReadBlobCalls() != 1 {
		t.Fatalf("ReadBlob calls = %d, want 1", fetcher.ReadBlobCalls())
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file should be left alone: %v", err)
	}
}

// newBloblessRepo makes a blobless clone of a remote holding files and returns
// it with its store and its file nodes.
func newBloblessRepo(t *testing.T, files map[string]string) (model.RepoConfig, *gitstore.Store, []model.BaseNode) {
	t.Helper()
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:           "repo",
		Name:         "repo",
		GitDir:       filepath.Join(tmp, "repo.git"),
		RemoteURL:    gittest.PartialCloneRemote(t, files),
		Branch:       "main",
		BlobCacheDir: filepath.Join(tmp, "cache"),
	}
	git := gitstore.New(nil)
	t.Cleanup(git.Close)
	ctx := context.Background()
	if err := git.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	head, _, err := git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	all, err := git.BuildTreeIndex(ctx, cfg, head)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	var nodes []model.BaseNode
	for _, n := range all {
		if n.Type == "file" {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) != len(files) {
		t.Fatalf("file nodes = %d, want %d", len(nodes), len(files))
	}
	return cfg, git, nodes
}

func prefetchTasks(repoID model.RepoID, nodes []model.BaseNode) []model.HydrationTask {
	tasks := make([]model.HydrationTask, len(nodes))
	for i, n := range nodes {
		tasks[i] = model.HydrationTask{RepoID: repoID, Path: n.Path, ObjectOID: n.ObjectOID, SizeState: n.SizeState, SizeBytes: n.SizeBytes, Priority: PriorityLikelyText, Reason: "prefetch", EnqueuedAt: time.Now()}
	}
	return tasks
}

func assertCachedFiles(t *testing.T, cfg model.RepoConfig, nodes []model.BaseNode, files map[string]string) {
	t.Helper()
	for _, n := range nodes {
		data, err := os.ReadFile(cachePathFor(cfg, n.ObjectOID))
		if err != nil {
			t.Fatalf("cached %s: %v", n.Path, err)
		}
		if string(data) != files[n.Path] {
			t.Fatalf("cached %s = %q, want %q", n.Path, data, files[n.Path])
		}
	}
}

func packCount(t *testing.T, gitDir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

// waitForQueued polls until key waits in the queue; go test -timeout is its
// only bound.
func waitForQueued(t *testing.T, h *Service, key string) {
	t.Helper()
	for {
		h.mu.Lock()
		_, ok := h.queued[key]
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForVerifyCalls polls until fetcher has seen want verifications; go test
// -timeout is its only bound.
func waitForVerifyCalls(t *testing.T, fetcher *fakeBlobFetcher, want int) {
	t.Helper()
	for fetcher.VerifyCalls() < want {
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForFetchCalls polls until fetcher has seen want calls; go test -timeout
// is its only bound.
func waitForFetchCalls(t *testing.T, fetcher *fakeBlobFetcher, want int) {
	t.Helper()
	for fetcher.Calls() < want {
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForHydratorIdle(t *testing.T, h *Service, repoID model.RepoID) {
	t.Helper()
	for {
		h.mu.Lock()
		queued := 0
		for _, item := range h.pq {
			if item.task.RepoID == repoID {
				queued++
			}
		}
		idle := queued == 0 && len(h.active) == 0
		h.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeBlobFetcher struct {
	mu            sync.Mutex
	calls         int
	readBlobCalls int
	verifyCalls   int
	payload       []byte
	readBlobErr   error
	verifyOK      bool
	verifyErr     error
	fetchStarted  chan struct{}
	fetchWait     <-chan struct{}
	fetchDelay    time.Duration
	verifyStarted chan struct{}
	verifyWait    <-chan struct{}
	fetchOnce     sync.Once
	verifyOnce    sync.Once
	batches       [][]string
	copied        []string
	// fetchMissing, when set, runs inside FetchMissingBlobs.
	fetchMissing func(ctx context.Context, objectOIDs []string) error
}

func (f *fakeBlobFetcher) BlobToCache(_ context.Context, _ model.RepoConfig, objectOID string, dstPath string) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.copied = append(f.copied, objectOID)
	f.mu.Unlock()
	if f.fetchStarted != nil {
		f.fetchOnce.Do(func() { close(f.fetchStarted) })
	}
	if f.fetchWait != nil {
		<-f.fetchWait
	}
	if f.fetchDelay > 0 {
		time.Sleep(f.fetchDelay)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(dstPath, f.payload, 0o644); err != nil {
		return 0, err
	}
	return int64(len(f.payload)), nil
}

func (f *fakeBlobFetcher) FetchMissingBlobs(ctx context.Context, _ model.RepoConfig, objectOIDs []string) error {
	f.mu.Lock()
	f.batches = append(f.batches, append([]string(nil), objectOIDs...))
	f.mu.Unlock()
	if f.fetchMissing != nil {
		return f.fetchMissing(ctx, objectOIDs)
	}
	return nil
}

// gateBatchFetches makes each FetchMissingBlobs call report its OIDs on the
// returned channel, then block until a value arrives on release or ctx ends.
func gateBatchFetches(f *fakeBlobFetcher) (started <-chan []string, release chan<- struct{}) {
	startedCh := make(chan []string, 16)
	releaseCh := make(chan struct{})
	f.fetchMissing = func(ctx context.Context, objectOIDs []string) error {
		startedCh <- objectOIDs
		select {
		case <-releaseCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return startedCh, releaseCh
}

func prefetchTasksFor(repoID model.RepoID, priority int, oids ...string) []model.HydrationTask {
	start := time.Now()
	tasks := make([]model.HydrationTask, len(oids))
	for i, oid := range oids {
		tasks[i] = model.HydrationTask{RepoID: repoID, Path: oid, ObjectOID: oid, SizeState: "unknown", Priority: priority, Reason: "prefetch", EnqueuedAt: start.Add(time.Duration(i))}
	}
	return tasks
}

// waitForWaiter polls until a read waits on key; go test -timeout is its only
// bound.
func waitForWaiter(t *testing.T, h *Service, key string) {
	t.Helper()
	for {
		h.mu.Lock()
		n := len(h.wait[key])
		h.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeBlobFetcher) Copied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.copied...)
}

func (f *fakeBlobFetcher) Batches() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.batches...)
}

func (f *fakeBlobFetcher) ReadBlob(_ context.Context, _ model.RepoConfig, _ string, maxBytes int64) ([]byte, error) {
	f.mu.Lock()
	f.readBlobCalls++
	f.mu.Unlock()
	if f.readBlobErr != nil {
		return nil, f.readBlobErr
	}
	if int64(len(f.payload)) > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	return f.payload, nil
}

func (f *fakeBlobFetcher) VerifyBlob(_ context.Context, _ model.RepoConfig, _ string, _ string) (bool, error) {
	f.mu.Lock()
	f.verifyCalls++
	f.mu.Unlock()
	if f.verifyStarted != nil {
		f.verifyOnce.Do(func() { close(f.verifyStarted) })
	}
	if f.verifyWait != nil {
		<-f.verifyWait
	}
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	return f.verifyOK, nil
}

func (f *fakeBlobFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBlobFetcher) ReadBlobCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readBlobCalls
}

func (f *fakeBlobFetcher) VerifyCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifyCalls
}
