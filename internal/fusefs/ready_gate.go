package fusefs

import (
	"context"
	"errors"
	"sync"
)

var ErrRepoNotReady = errors.New("repo is not ready")

type ReadyGate struct {
	mu     sync.Mutex
	ready  bool
	err    error
	done   chan struct{}
	closed bool
	// passes counts the switch passes that run. A gated lookup holds its
	// parent directory's lock, which a pass's entry invalidation takes, so a
	// Reset while passes run holds ops only once the last pass ends.
	passes   int
	resetDue bool
}

func NewReadyGate(ready bool) *ReadyGate {
	g := &ReadyGate{ready: ready, done: make(chan struct{})}
	if ready {
		close(g.done)
		g.closed = true
	}
	return g
}

func (g *ReadyGate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.ready {
		g.mu.Unlock()
		return nil
	}
	if g.err != nil {
		err := g.err
		g.mu.Unlock()
		return err
	}
	done := g.done
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ready {
		return nil
	}
	if g.err != nil {
		return g.err
	}
	return ErrRepoNotReady
}

func (g *ReadyGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ready && g.err == nil {
		return
	}
	if g.passes > 0 {
		g.resetDue = true
		return
	}
	g.reset()
}

func (g *ReadyGate) reset() {
	g.ready = false
	g.err = nil
	g.done = make(chan struct{})
	g.closed = false
}

func (g *ReadyGate) MarkReady() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetDue = false
	if g.ready {
		return
	}
	g.ready = true
	g.err = nil
	if !g.closed {
		close(g.done)
		g.closed = true
	}
}

func (g *ReadyGate) MarkFailed(err error) {
	if g == nil {
		return
	}
	if err == nil {
		err = ErrRepoNotReady
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resetDue {
		g.resetDue = false
		g.ready = false
	}
	if g.err != nil {
		g.err = err
		return
	}
	if g.ready {
		return
	}
	g.err = err
	if !g.closed {
		close(g.done)
		g.closed = true
	}
}

// StartPass starts a switch pass unless g holds ops, and reports whether it
// did. EndPass ends a pass that started.
func (g *ReadyGate) StartPass() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ready && g.err == nil {
		return false
	}
	g.passes++
	return true
}

// WaitPass waits until g holds no ops, then starts a switch pass. It returns
// ctx's error, and starts no pass, once ctx ends.
func (g *ReadyGate) WaitPass(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if g.StartPass() {
			return nil
		}
		_ = g.Wait(ctx)
	}
}

func (g *ReadyGate) EndPass() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.passes--
	if g.passes == 0 && g.resetDue {
		g.resetDue = false
		g.reset()
	}
}
