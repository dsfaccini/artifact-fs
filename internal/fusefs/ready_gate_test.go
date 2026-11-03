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
