package daemon

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

type sizeUpdateBatcher struct {
	snapshot *snapshot.Store
	logger   *slog.Logger
	repoName string
	interval time.Duration
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	pending  map[int64]map[string]int64
	stopped  bool
}

func newSizeUpdateBatcher(snap *snapshot.Store, logger *slog.Logger, repoName string) *sizeUpdateBatcher {
	return &sizeUpdateBatcher{
		snapshot: snap,
		logger:   logger,
		repoName: repoName,
		interval: sizeUpdateFlushInterval,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		pending:  map[int64]map[string]int64{},
	}
}

func (b *sizeUpdateBatcher) Start(ctx context.Context) {
	go b.run(ctx)
}

func (b *sizeUpdateBatcher) Add(generation int64, objectOID string, size int64) {
	if generation <= 0 || strings.TrimSpace(objectOID) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	if b.pending[generation] == nil {
		b.pending[generation] = map[string]int64{}
	}
	b.pending[generation][objectOID] = size
}

func (b *sizeUpdateBatcher) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		b.mu.Unlock()
		close(b.stopCh)
		<-b.done
		b.Flush()
	})
}

func (b *sizeUpdateBatcher) run(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	defer close(b.done)
	defer b.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-ticker.C:
			b.Flush()
		}
	}
}

func (b *sizeUpdateBatcher) Flush() {
	b.mu.Lock()
	pending := b.pending
	b.pending = map[int64]map[string]int64{}
	b.mu.Unlock()
	for gen, sizes := range pending {
		if err := b.snapshot.UpdateSizes(context.Background(), gen, sizes); err != nil && b.logger != nil {
			b.logger.Warn("snapshot size backfill failed", "repo", b.repoName, "generation", gen, "error", err)
		}
	}
}
