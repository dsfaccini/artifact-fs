package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// fetchProbeGit sends name to fetched as each FetchNonInteractive starts.
// hold, when set, runs once after the next one has fetched, instead.
type fetchProbeGit struct {
	model.GitStore
	name    string
	fetched chan<- string
	hold    atomic.Pointer[func()]
}

func (g *fetchProbeGit) FetchNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	hold := g.hold.Swap(nil)
	if hold == nil {
		select {
		case g.fetched <- g.name:
		default:
		}
	}
	err := g.GitStore.FetchNonInteractive(ctx, repo)
	if hold != nil {
		(*hold)()
	}
	return err
}

// overlapRepo is a repo a daemon serves, on which an overlap row runs two
// steps. The daemon's refresh loop ticks only on ticks, and its HEAD watcher
// never polls. ctl stands in for a CLI process: it has its own git store, so
// fetched tells its fetches from the daemon's. busy receives once a step of
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
	cliGit    *fetchProbeGit
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
	f.cliGit = &fetchProbeGit{GitStore: ctl.git, name: "cli", fetched: f.fetched}
	svc.git, ctl.git = f.daemonGit, f.cliGit
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

// commitToRemote commits to the remote's main and returns the commit. It
// can run off the test's goroutine.
func (f *overlapRepo) commitToRemote() (string, error) {
	commit := exec.Command("git", "-C", f.remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-q", "-m", "overlap")
	if out, err := commit.CombinedOutput(); err != nil {
		return "", fmt.Errorf("commit to the remote: %v\n%s", err, out)
	}
	tip, err := exec.Command("git", "-C", f.remote, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve the remote's tip: %v", err)
	}
	return strings.TrimSpace(string(tip)), nil
}

// overlapStep is a step that runs under the repo's prepare lock.
type overlapStep struct {
	name string
	// hold runs the step and calls inside once, after the step's fetch while
	// it holds the lock. It returns once the step has ended.
	hold func(t *testing.T, f *overlapRepo, inside func())
	// start starts the step without waiting for it, and returns a channel
	// that receives the step's error once it has ended. inside can call it
	// off the test's goroutine.
	start func(f *overlapRepo) <-chan error
}

var (
	asyncPrepareStep = overlapStep{
		name: "an async re-prepare",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			ctx := t.Context()
			f.atReady.Store(&inside)
			defer f.atReady.Store(nil)
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
		start: func(f *overlapRepo) <-chan error {
			ended := make(chan error, 1)
			go func() {
				ctx := context.Background()
				if err := f.svc.registry.BeginPrepare(ctx, f.cfg, model.PrepareStatePreparing, "again"); err != nil {
					ended <- err
					return
				}
				if err := f.svc.syncRepos(ctx); err != nil {
					ended <- err
					return
				}
				f.svc.prepareWorkers.Wait()
				cfg, err := f.svc.registry.GetRepo(ctx, f.cfg.Name)
				if err == nil && cfg.PrepareState != model.PrepareStateReady {
					err = fmt.Errorf("prepare state %q (%s), want ready", cfg.PrepareState, cfg.PrepareError)
				}
				ended <- err
			}()
			return ended
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
		start: func(f *overlapRepo) <-chan error {
			ended := make(chan error, 1)
			go func() {
				_, err := f.ctl.Prepare(context.Background(), f.cfg.Name)
				ended <- err
			}()
			return ended
		},
	}
	refreshStep = overlapStep{
		name: "a refresh",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			held := make(chan struct{})
			hold := func() {
				defer close(held)
				inside()
			}
			f.daemonGit.hold.Store(&hold)
			f.ticks <- time.Now()
			// The refresh takes a second tick once it has ended its
			// iteration. One that ended without a fetch left hold set.
			select {
			case <-held:
			case f.ticks <- time.Now():
				if f.daemonGit.hold.Swap(nil) == nil {
					<-held
				}
			}
		},
		start: func(f *overlapRepo) <-chan error {
			f.ticks <- time.Now()
			ended := make(chan error, 1)
			go func() {
				// The refresh takes this tick once it has ended its iteration.
				select {
				case f.ticks <- time.Now():
				case <-f.rt.ctx.Done():
					ended <- f.rt.ctx.Err()
					return
				}
				f.svc.mu.Lock()
				state := f.rt.fetch.state
				f.svc.mu.Unlock()
				if state != fetchOK {
					ended <- fmt.Errorf("fetch state %q, want %q", state, fetchOK)
					return
				}
				ended <- nil
			}()
			return ended
		},
	}
	fetchCommandStep = overlapStep{
		name: "a fetch command",
		hold: func(t *testing.T, f *overlapRepo, inside func()) {
			f.cliGit.hold.Store(&inside)
			defer f.cliGit.hold.Store(nil)
			if err := f.ctl.FetchNow(t.Context(), f.cfg.Name); err != nil {
				t.Errorf("fetch command: %v", err)
			}
		},
		start: func(f *overlapRepo) <-chan error {
			ended := make(chan error, 1)
			go func() { ended <- f.ctl.FetchNow(context.Background(), f.cfg.Name) }()
			return ended
		},
	}
)

// Every step that fetches into the repo's tracking ref runs under the repo's
// prepare lock, and two fetches that overlap fail each other's ref update. In
// each row the holder has fetched and holds the lock when the remote gains a
// commit and the contender starts. The contender must start no fetch until
// the holder releases the lock, and must report the lock busy instead. Once
// the holder releases it, the contender fetches the commit.
func TestFetchesWaitWhileAnotherStepHoldsThePrepareLock(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		holder, contender overlapStep
		async             bool
	}{
		{holder: asyncPrepareStep, contender: refreshStep, async: true},
		{holder: asyncPrepareStep, contender: fetchCommandStep, async: true},
		{holder: cliPrepareStep, contender: refreshStep},
		{holder: refreshStep, contender: cliPrepareStep},
		{holder: refreshStep, contender: asyncPrepareStep, async: true},
		{holder: refreshStep, contender: fetchCommandStep},
		{holder: fetchCommandStep, contender: refreshStep},
	} {
		t.Run(row.contender.name+" during "+row.holder.name, func(t *testing.T) {
			t.Parallel()
			f := newOverlapRepo(t, row.async)
			var tip string
			var ended <-chan error
			row.holder.hold(t, f, func() {
				for len(f.busy) > 0 || len(f.fetched) > 0 {
					select {
					case <-f.busy:
					case <-f.fetched:
					}
				}
				var err error
				if tip, err = f.commitToRemote(); err != nil {
					t.Error(err)
					return
				}
				ended = row.contender.start(f)
				select {
				case side := <-f.fetched:
					t.Errorf("a fetch of the %s's git store started while %s held the prepare lock", side, row.holder.name)
				case err := <-ended:
					t.Errorf("%s ended with %v while %s held the prepare lock", row.contender.name, err, row.holder.name)
				case <-f.busy:
				}
			})
			if ended == nil && !t.Failed() {
				t.Fatalf("%s never held the prepare lock", row.holder.name)
			}
			if t.Failed() {
				return
			}
			if err := <-ended; err != nil {
				t.Fatalf("%s after %s released the prepare lock: %v", row.contender.name, row.holder.name, err)
			}
			if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", f.cfg.GitDir, "rev-parse", "refs/remotes/origin/main")); got != tip {
				t.Fatalf("origin/main after %s = %s, want the remote's tip %s, which only %s fetched", row.contender.name, got, tip, row.contender.name)
			}
		})
	}
}
