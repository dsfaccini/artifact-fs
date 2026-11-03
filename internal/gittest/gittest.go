// Package gittest holds helpers for tests that run git.
package gittest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// IsolateConfig makes git, in this process and every process it starts, read
// neither the user's global config nor the system config, so tests see git's
// defaults on every machine. Call it from TestMain.
func IsolateConfig() {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// RequireConfigIsolated fails t unless git ignores a global and a system config
// planted for it, which holds only after IsolateConfig ran.
func RequireConfigIsolated(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[gittest]\n\tscope = global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	system := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(system, []byte("[gittest]\n\tscope = system\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	out, err := exec.Command("git", "config", "--get-all", "gittest.scope").Output()
	if len(out) != 0 || err == nil {
		t.Fatalf("git read gittest.scope = %q from a machine config; TestMain must call gittest.IsolateConfig", out)
	}
}

// ExpireOnFile returns a context whose deadline passes once path exists, so a
// test chooses the step a deadline interrupts instead of racing a timer. The
// context reports a deadline, as a real timeout does, and ends early when
// parent ends or cancel is called.
func ExpireOnFile(parent context.Context, path string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	expiring := &expiringContext{Context: ctx, done: make(chan struct{})}
	go func() {
		defer close(expiring.done)
		for {
			if _, err := os.Stat(path); err == nil {
				expiring.expired = true
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return expiring, cancel
}

type expiringContext struct {
	context.Context
	done chan struct{}
	// expired is written before done closes and read only after.
	expired bool
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	if c.expired {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}
