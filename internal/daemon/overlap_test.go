package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// fetchProbeGit sends name to fetched as each FetchNonInteractive starts.
// hold, when set, runs once in the next one instead, before it fetches.
type fetchProbeGit struct {
	model.GitStore
	name    string
	fetched chan<- string
	hold    atomic.Pointer[func()]
}

func (g *fetchProbeGit) FetchNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	if hold := g.hold.Swap(nil); hold != nil {
		(*hold)()
	} else {
		select {
		case g.fetched <- g.name:
		default:
		}
	}
	return g.GitStore.FetchNonInteractive(ctx, repo)
}

// overlapRepo is a repo a daemon serves, on which an overlap row runs two
// steps. The daemon's refresh loop ticks only on ticks, and its HEAD watcher
// never polls. ctl stands in for a CLI process: it has its own git store and
// opens the repo's lock files itself, so its flocks and the daemon's exclude
// each other as they do across processes. busy receives once a step of
// either finds the repo's prepare lock held. fetched receives "daemon" or
// "cli" as a fetch of that side's git store starts.
type overlapRepo struct {
	svc       *Service
	ctl       *Controller
	cfg       model.RepoConfig
	remote    string
	rt        *repoRuntime
	ticks     chan time.Time
	busy      chan struct{}
	fetched   chan string
	daemonGit *fetchProbeGit
	// atReady runs once, while the next async preparation holds the
	// prepare lock.
	atReady atomic.Pointer[func()]
}

func newOverlapRepo(t *testing.T, async bool) *overlapRepo {
	t.Helper()
	ctx := t.Context()
	tmp := shortTempDir(t)
	svc, ctl := newDaemonWithRepo(t, tmp, AddRepoOptions{Async: async})
	f := &overlapRepo{svc: svc, ctl: ctl, remote: filepath.Join(tmp, "remote"), ticks: make(chan time.Time), busy: make(chan struct{}, 1), fetched: make(chan string, 8)}
	svc.refreshTicks = f.ticks
	svc.headPollTicks = make(chan time.Time)
	f.daemonGit = &fetchProbeGit{GitStore: svc.git, name: "daemon", fetched: f.fetched}
	svc.git = f.daemonGit
	ctl.git = &fetchProbeGit{GitStore: ctl.git, name: "cli", fetched: f.fetched}
	lockBusy := func(lockName string) {
		if lockName != "repo.prepare.lock" {
			return
		}
		select {
		case f.busy <- struct{}{}:
		default:
		}
	}
	svc.lockBusy, ctl.lockBusy = lockBusy, lockBusy
	// readyRecorded runs while the preparation holds the prepare lock.
	svc.readyRecorded = func() {
		if atReady := f.atReady.Swap(nil); atReady != nil {
			(*atReady)()
		}
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.prepareWorkers.Wait()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg, f.rt = cfg, runningRuntime(svc, cfg.ID)
	if f.rt == nil {
		t.Fatalf("repo is not running after its first sync; prepare state %q (%s)", cfg.PrepareState, cfg.PrepareError)
	}
	return f
}

// overlapStep is a step that runs under the repo's prepare lock.
type overlapStep struct {
	name string
	// fetcher is the side whose git store the step fetches through.
	fetcher string
	// hold runs the step and calls inside once, while the step holds the
	// lock. It returns once inside has returned.
	hold func(t *testing.T, f *overlapRepo, inside func())
	// start starts the step without waiting for it, and returns what waits
	// for the step to end and returns its error. inside can run it off the
	// test's goroutine.
	start func(f *overlapRepo) (wait func() error)
}

var (
	asyncPrepareStep = overlapStep{
		name: "an async re-prepare",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			ctx := t.Context()
			f.atReady.Store(&inside)
			if err := f.svc.registry.BeginPrepare(ctx, f.cfg, model.PrepareStatePreparing, "again"); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.syncRepos(ctx); err != nil {
				t.Fatal(err)
			}
			f.svc.prepareWorkers.Wait()
			cfg, err := f.svc.registry.GetRepo(ctx, f.cfg.Name)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PrepareState != model.PrepareStateReady {
				t.Errorf("prepare state after the re-prepare = %q (%s), want ready", cfg.PrepareState, cfg.PrepareError)
			}
		},
	}
	cliPrepareStep = overlapStep{
		name: "a CLI prepare of a sync repo",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			// The preparation resolves HEAD once it has fetched.
			git := f.ctl.git
			f.ctl.git = &steppedHEADGit{GitStore: git, after: []func(){inside}}
			defer func() { f.ctl.git = git }()
			if _, err := f.ctl.Prepare(t.Context(), f.cfg.Name); err != nil {
				t.Errorf("CLI prepare: %v", err)
			}
		},
	}
	refreshStep = overlapStep{
		name:    "a refresh",
		fetcher: "daemon",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			held := make(chan struct{})
			hold := func() {
				defer close(held)
				inside()
			}
			f.daemonGit.hold.Store(&hold)
			f.ticks <- time.Now()
			<-held
		},
		start: func(f *overlapRepo) func() error {
			f.ticks <- time.Now()
			return func() error {
				// The refresh takes this tick once it has fetched.
				f.ticks <- time.Now()
				f.svc.mu.Lock()
				state := f.rt.fetch.state
				f.svc.mu.Unlock()
				if state != fetchOK {
					return fmt.Errorf("fetch state %q, want %q", state, fetchOK)
				}
				return nil
			}
		},
	}
	fetchCommandStep = overlapStep{
		name:    "a fetch command",
		fetcher: "cli",
		start: func(f *overlapRepo) func() error {
			done := make(chan error, 1)
			go func() { done <- f.ctl.FetchNow(context.Background(), f.cfg.Name) }()
			return func() error { return <-done }
		},
	}
)

// Every step that fetches into the repo's tracking ref runs under the repo's
// prepare lock, and two fetches that overlap fail each other's ref update. In
// each row the contender starts while the holder holds the lock. It must
// start no fetch until the holder releases the lock, and must report the
// lock busy instead. Once the holder releases it, the contender fetches.
func TestFetchesWaitWhileAnotherStepHoldsThePrepareLock(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		holder, contender overlapStep
		async             bool
	}{
		{holder: asyncPrepareStep, contender: refreshStep, async: true},
		{holder: cliPrepareStep, contender: refreshStep},
		{holder: refreshStep, contender: fetchCommandStep},
	} {
		t.Run(row.contender.name+" during "+row.holder.name, func(t *testing.T) {
			t.Parallel()
			f := newOverlapRepo(t, row.async)
			var wait func() error
			row.holder.hold(t, f, func() {
				for len(f.busy) > 0 || len(f.fetched) > 0 {
					select {
					case <-f.busy:
					case <-f.fetched:
					}
				}
				wait = row.contender.start(f)
				select {
				case side := <-f.fetched:
					t.Errorf("a fetch of the %s's git store started while %s held the prepare lock", side, row.holder.name)
				case <-f.busy:
				}
			})
			if wait == nil {
				t.Fatalf("%s never held the prepare lock", row.holder.name)
			}
			if err := wait(); err != nil {
				t.Fatalf("%s after %s released the prepare lock: %v", row.contender.name, row.holder.name, err)
			}
			for {
				select {
				case side := <-f.fetched:
					if side == row.contender.fetcher {
						return
					}
				default:
					if !t.Failed() {
						t.Fatalf("%s did not fetch once %s released the prepare lock", row.contender.name, row.holder.name)
					}
					return
				}
			}
		})
	}
}
