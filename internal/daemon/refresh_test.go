package daemon

import (
	"errors"
	"fmt"
	"os/exec"
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

// A refresh waits while a preparation of its repo holds the repo's prepare
// lock. Both fetch into the same tracking ref, and a refresh fetch that
// overlaps the preparation's fails the preparation's ref update. The refresh
// ticks while a re-prepare holds the lock. A refresh that fetched takes a
// second tick; one that waits for the lock reports the lock busy.
func TestRefreshWaitsForAPreparationOfItsRepo(t *testing.T) {
	t.Parallel()
	f := newLockedPreparation(t, func(f *lockedPreparation) {
		f.ticks <- time.Now()
		select {
		case f.ticks <- time.Now():
			t.Error("a refresh fetched while a preparation of its repo held the prepare lock")
		case <-f.busy:
		}
	})
	cfg, err := f.svc.registry.GetRepo(t.Context(), f.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrepareState != model.PrepareStateReady {
		t.Fatalf("prepare state after the re-prepare = %q (%s), want ready", cfg.PrepareState, cfg.PrepareError)
	}
	// The refresh takes this tick once it has fetched under the lock.
	f.ticks <- time.Now()
	f.svc.mu.Lock()
	state := f.rt.fetch.state
	f.svc.mu.Unlock()
	if state != fetchOK {
		t.Fatalf("fetch state after the preparation released the lock = %q, want %q", state, fetchOK)
	}
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

// The fetch command waits while a preparation of its repo holds the repo's
// prepare lock, as a refresh does. The registry shows the repo ready once the
// preparation recorded ready, before it releases the lock, so the command's
// state checks pass there. A fetch command that fetched returns while the
// preparation holds the lock; one that waits reports the lock busy, then
// fetches the commit the remote gained.
func TestFetchCommandWaitsForAPreparationOfItsRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fetched := make(chan error, 1)
	f := newLockedPreparation(t, func(f *lockedPreparation) {
		// The remote gains a commit only the fetch command can fetch.
		commit := exec.Command("git", "-C", f.remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-q", "-m", "three")
		if out, err := commit.CombinedOutput(); err != nil {
			fetched <- fmt.Errorf("commit to the remote: %v\n%s", err, out)
			return
		}
		go func() { fetched <- f.svc.ctl().FetchNow(ctx, f.cfg.Name) }()
		select {
		case err := <-fetched:
			fetched <- fmt.Errorf("the fetch command returned %v while a preparation of its repo held the prepare lock", err)
		case <-f.busy:
		}
	})
	if err := <-fetched; err != nil {
		t.Fatal(err)
	}
	tip := strings.TrimSpace(runCmdOutput(t, "git", "-C", f.remote, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", f.cfg.GitDir, "rev-parse", "refs/remotes/origin/main")); got != tip {
		t.Fatalf("origin/main after the fetch command = %s, want the remote's tip %s", got, tip)
	}
}
