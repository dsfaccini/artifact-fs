package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// A refresh waits while a preparation of its repo holds the repo's prepare
// lock. Both fetch into the same tracking ref, and a refresh fetch that
// overlaps the preparation's fails the preparation's ref update. The refresh
// ticks while a re-prepare holds the lock. A refresh that fetched takes a
// second tick; one that waits for the lock reports the lock busy.
func TestRefreshWaitsForAPreparationOfItsRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ticks := make(chan time.Time)
	busy := make(chan struct{}, 1)
	var armed atomic.Bool
	r := newServedAsyncRepo(t, func(svc *Service) {
		svc.refreshTicks = ticks
		svc.lockBusy = func(lockName string) {
			if lockName != "repo.prepare.lock" {
				return
			}
			select {
			case busy <- struct{}{}:
			default:
			}
		}
		// readyRecorded runs while the preparation holds the prepare lock.
		svc.readyRecorded = func() {
			if !armed.CompareAndSwap(true, false) {
				return
			}
			ticks <- time.Now()
			select {
			case ticks <- time.Now():
				t.Error("a refresh fetched while a preparation of its repo held the prepare lock")
			case <-busy:
			}
		}
	}, nil)

	commitFile(t, r.remote, "README.md", "two\n")
	armed.Store(true)
	if err := r.requestPrepare(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	r.svc.prepareWorkers.Wait()
	if armed.Load() {
		t.Fatal("the preparation never recorded ready")
	}
	cfg, err := r.svc.registry.GetRepo(ctx, r.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrepareState != model.PrepareStateReady {
		t.Fatalf("prepare state after the re-prepare = %q (%s), want ready", cfg.PrepareState, cfg.PrepareError)
	}
	// The refresh takes this tick once it has fetched under the lock.
	ticks <- time.Now()
	r.svc.mu.Lock()
	state := r.rt.fetch.state
	r.svc.mu.Unlock()
	if state != fetchOK {
		t.Fatalf("fetch state after the preparation released the lock = %q, want %q", state, fetchOK)
	}
}
