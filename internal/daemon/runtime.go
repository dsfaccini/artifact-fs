package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/cloudflare/artifact-fs/internal/watcher"
)

type repoRuntime struct {
	cfg         model.RepoConfig
	ctx         context.Context
	cancel      context.CancelFunc
	snapshot    *snapshot.Store
	overlay     *overlay.Store
	hydrator    *hydrator.Service
	sizes       *sizeUpdateBatcher
	resolver    *fusefs.Resolver
	engine      *fusefs.Engine
	mfs         fusefs.MountedFS
	gate        *fusefs.ReadyGate
	state       model.RepoRuntimeState
	active      bool
	refresh     chan time.Duration
	headChanges chan struct{}
	joinDone    chan struct{}
	stopping    bool
	detached    bool
	headMu      sync.Mutex
	workers     sync.WaitGroup
	mounts      sync.WaitGroup
	// failedMount is a mount whose connection failed and that still holds the mount point.
	failedMount fusefs.MountedFS
}

func (s *Service) startRuntime(rt *repoRuntime) {
	s.mu.Lock()
	s.running[rt.cfg.ID] = rt
	s.mu.Unlock()

	s.joinRuntimeMount(rt)
}

func (s *Service) startRepoBackground(rt *repoRuntime) {
	s.mu.Lock()
	if rt.active || rt.stopping {
		s.mu.Unlock()
		return
	}
	rt.active = true
	gitDir := rt.cfg.GitDir
	fixedBase := rt.cfg.RequiredCommit != ""
	workerCount := 3
	if fixedBase {
		workerCount = 1
	} else {
		rt.headChanges = make(chan struct{}, 1)
	}
	rt.workers.Add(workerCount)
	s.mu.Unlock()

	go func() {
		defer rt.workers.Done()
		s.refreshLoop(rt)
	}()

	if !fixedBase {
		go func() {
			defer rt.workers.Done()
			s.headReconcileLoop(rt)
		}()

		w := watcher.New(500 * time.Millisecond)
		go func() {
			defer rt.workers.Done()
			w.Watch(rt.ctx, gitDir, func() {
				s.signalHEADReconcile(rt)
			})
		}()
	}
}

func (s *Service) unmount(id model.RepoID) error {
	s.mu.Lock()
	rt, ok := s.running[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	if rt.stopping {
		s.mu.Unlock()
		return errors.New("runtime is already stopping")
	}
	rt.stopping = true
	s.mu.Unlock()
	if err := s.stopRuntime(rt); err != nil {
		s.mu.Lock()
		rt.stopping = false
		s.mu.Unlock()
		s.signalHEADReconcile(rt)
		return err
	}
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
	return nil
}

func (s *Service) stopRuntime(rt *repoRuntime) error {
	rt.mounts.Wait()
	if err := s.unmountFailedMount(rt); err != nil {
		return err
	}
	s.mu.Lock()
	mfs := rt.mfs
	detached := rt.detached
	s.mu.Unlock()
	if mfs != nil && !detached {
		if err := mfs.Unmount(); err != nil {
			return fmt.Errorf("unmount %s: %w", rt.cfg.Name, err)
		}
		s.mu.Lock()
		rt.detached = true
		s.mu.Unlock()
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.gate != nil {
		rt.gate.MarkFailed(context.Canceled)
	}
	rt.workers.Wait()
	s.joinRuntimeMount(rt)
	s.mu.Lock()
	joinDone := rt.joinDone
	s.mu.Unlock()
	if joinDone != nil {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-joinDone:
		case <-timer.C:
			return fmt.Errorf("timed out draining unmounted repo %s", rt.cfg.Name)
		}
	}
	if rt.hydrator != nil {
		rt.hydrator.Stop()
	}
	if rt.sizes != nil {
		rt.sizes.Stop()
	}
	if rt.snapshot != nil {
		_ = rt.snapshot.Close()
	}
	if rt.overlay != nil {
		_ = rt.overlay.Close()
	}
	return nil
}
