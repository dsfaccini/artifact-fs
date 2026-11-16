package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// lockedPreparation is an async repo whose refresh loop ticks only on ticks.
// atLock runs once, while the next preparation of the repo holds its prepare
// lock, and busy receives once a refresh or a fetch command finds that lock
// held.
type lockedPreparation struct {
	*servedAsyncRepo
	ticks chan time.Time
	busy  chan struct{}
}

func newLockedPreparation(t *testing.T, atLock func(*lockedPreparation)) *lockedPreparation {
	t.Helper()
	f := &lockedPreparation{ticks: make(chan time.Time), busy: make(chan struct{}, 1)}
	var armed atomic.Bool
	f.servedAsyncRepo = newServedAsyncRepo(t, func(svc *Service) {
		svc.refreshTicks = f.ticks
		svc.lockBusy = func(lockName string) {
			if lockName != "repo.prepare.lock" {
				return
			}
			select {
			case f.busy <- struct{}{}:
			default:
			}
		}
		// readyRecorded runs while the preparation holds the prepare lock.
		svc.readyRecorded = func() {
			if armed.CompareAndSwap(true, false) {
				atLock(f)
			}
		}
	}, nil)
	commitFile(t, f.remote, "README.md", "two\n")
	armed.Store(true)
	if err := f.requestPrepare(t.Context(), "again"); err != nil {
		t.Fatal(err)
	}
	f.svc.prepareWorkers.Wait()
	if armed.Load() {
		t.Fatal("the preparation never recorded ready")
	}
	return f
}

// A stop of the runtime while its refresh waits for the prepare lock ends
// the refresh without a fetch, so repo_runtime keeps no fetch failure. The
// stop's drain waits for the refresh loop, so any write of the refresh has
// landed once the stop returns.
func TestStopWhileARefreshWaitsForThePrepareLockRecordsNoFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var before model.RepoRuntimeRecord
	stopped := make(chan error, 1)
	f := newLockedPreparation(t, func(f *lockedPreparation) {
		rec, ok, err := f.svc.registry.GetRuntime(ctx, f.cfg.ID)
		if err != nil || !ok {
			t.Errorf("runtime record before the stop = %v, %v; want it recorded", ok, err)
		}
		before = rec
		f.ticks <- time.Now()
		select {
		case f.ticks <- time.Now():
			stopped <- errors.New("a refresh fetched while a preparation of its repo held the prepare lock")
			return
		case <-f.busy:
		}
		// The preparation is one of the runtime's workers, which the stop's
		// drain waits for, so it goes on once the stop ended rt.ctx, or once
		// a stop that failed before that returned.
		stopDone := make(chan struct{})
		go func() {
			stopped <- f.svc.unmount(f.cfg.ID)
			close(stopDone)
		}()
		select {
		case <-f.rt.ctx.Done():
		case <-stopDone:
		}
	})
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	rec, ok, err := f.svc.registry.GetRuntime(ctx, f.cfg.ID)
	if err != nil || !ok {
		t.Fatalf("runtime record after the stop = %v, %v; want it recorded", ok, err)
	}
	if rec.LastError != "" || rec.LastErrorPhase != "" || rec.Attempts != 0 || !rec.NextRetryAt.IsZero() {
		t.Fatalf("runtime record after the stop has error %q in phase %q, %d attempts, next retry %v; want no failure", rec.LastError, rec.LastErrorPhase, rec.Attempts, rec.NextRetryAt)
	}
	if rec.LastFetchResult != before.LastFetchResult || !rec.LastFetchAt.Equal(before.LastFetchAt) {
		t.Fatalf("last fetch after the stop = %q at %v, want %q at %v as before the refresh", rec.LastFetchResult, rec.LastFetchAt, before.LastFetchResult, before.LastFetchAt)
	}
}

// The fetch command reads the repo again once it holds the prepare lock: what
// held the lock can have changed the repo's fetch ref or refresh policy. Here
// set-refresh turns the refresh off while the command waits, and the command
// refuses the repo instead of fetching it.
func TestFetchCommandReadsTheRepoAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fetched := make(chan error, 1)
	newLockedPreparation(t, func(f *lockedPreparation) {
		go func() { fetched <- f.svc.ctl().FetchNow(ctx, f.cfg.Name) }()
		select {
		case err := <-fetched:
			fetched <- fmt.Errorf("the fetch command returned %v while a preparation of its repo held the prepare lock", err)
			return
		case <-f.busy:
		}
		if err := f.svc.ctl().SetRefresh(ctx, f.cfg.Name, model.RefreshPolicy{RemoteRefreshDisabled: true}); err != nil {
			t.Errorf("set-refresh while the fetch command waits: %v", err)
		}
	})
	if err := <-fetched; err == nil || !strings.Contains(err.Error(), "remote refresh is disabled") {
		t.Fatalf("fetch command after set-refresh turned the refresh off = %v, want it refused", err)
	}
}
