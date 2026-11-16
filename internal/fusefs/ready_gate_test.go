package fusefs

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestReadyGateWaitsUntilReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewReadyGate(false)
		done := make(chan error, 1)
		go func() {
			done <- g.Wait(context.Background())
		}()

		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Wait returned before ready: %v", err)
		default:
		}

		g.MarkReady()
		if err := <-done; err != nil {
			t.Fatalf("Wait returned error after ready: %v", err)
		}
	})
}

func TestReadyGateFailureFailsFastAndCanReset(t *testing.T) {
	g := NewReadyGate(false)
	g.MarkFailed(errors.New("clone failed"))

	if err := g.Wait(context.Background()); err == nil || err.Error() != "clone failed" {
		t.Fatalf("Wait error = %v, want clone failed", err)
	}

	g.Reset()
	g.MarkReady()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after reset/ready: %v", err)
	}
}

func TestReadyGateMarkReadyAfterFailureRecovers(t *testing.T) {
	g := NewReadyGate(false)
	g.MarkFailed(errors.New("clone failed"))
	g.MarkReady()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after failure/ready: %v", err)
	}
}

// A Reset while a switch pass runs holds ops only once the pass ends. A
// MarkReady before then replaces the Reset, as when a preparation ends ready
// during a pass that waited for the gate, so the gate stays open.
func TestReadyGateMarkReadyDuringAPassReplacesItsReset(t *testing.T) {
	g := NewReadyGate(true)
	if !g.StartPass() {
		t.Fatal("StartPass on an open gate started no pass")
	}
	g.Reset()
	g.MarkReady()
	g.EndPass()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(canceled); err != nil {
		t.Fatalf("Wait after the pass = %v, want the gate open", err)
	}
}
