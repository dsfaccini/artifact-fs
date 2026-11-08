// Package gittest holds helpers for tests that run git.
package gittest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PartialCloneRemote commits files on branch main of a bare repository that
// serves partial clones, and returns its URL. A blobless clone of it holds no
// blobs, so every read of one fetches from it.
func PartialCloneRemote(t testing.TB, files map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	bare := filepath.Join(tmp, "remote.git")
	run(t, "", "init", "-q", "-b", "main", work)
	for name, content := range files {
		path := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, work, "add", ".")
	run(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "files")
	run(t, "", "init", "-q", "--bare", "-b", "main", bare)
	run(t, bare, "config", "uploadpack.allowFilter", "true")
	run(t, work, "push", "-q", bare, "main")
	return "file://" + bare
}

func run(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

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

// raceExitSleepOff is the GORACE that DisableRaceExitSleep sets.
const raceExitSleepOff = "atexit_sleep_ms=0"

// raceExitSleep is how long a race-built binary sleeps after it exits 0, by
// default.
const raceExitSleep = time.Second

// DisableRaceExitSleep stops each race-built binary this process starts from
// sleeping raceExitSleep after it exits 0. It leaves a GORACE the caller set.
// Call it from TestMain in a package whose test binary git runs as the
// FSMonitor hook: there the sleep adds a second to every git command that
// reads the index.
func DisableRaceExitSleep() {
	if _, ok := os.LookupEnv("GORACE"); !ok {
		os.Setenv("GORACE", raceExitSleepOff)
	}
}

// RequireHookSkipsRaceExitSleep fails t unless git status in worktree runs
// its core.fsmonitor hook, the hook exits 0, and it returns before
// raceExitSleep ends, which holds in a race build only after
// DisableRaceExitSleep ran.
func RequireHookSkipsRaceExitSleep(t *testing.T, worktree string) {
	t.Helper()
	if v, ok := os.LookupEnv("GORACE"); ok && v != raceExitSleepOff {
		t.Skipf("GORACE=%q from the caller decides the exit sleep", v)
	}
	out, err := exec.Command("git", "-C", worktree, "config", "--get", "core.fsmonitor").Output()
	if err != nil {
		t.Fatalf("read core.fsmonitor: %v", err)
	}
	hook := strings.TrimSpace(string(out))
	trace := filepath.Join(t.TempDir(), "trace2.json")
	cmd := exec.Command("git", "-C", worktree, "status", "--short")
	cmd.Env = append(os.Environ(), "GIT_TRACE2_EVENT="+trace)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	type child struct {
		sid string
		id  int
	}
	hooks := map[child]bool{}
	ran := false
	for line := range bytes.Lines(data) {
		var ev struct {
			Event   string   `json:"event"`
			SID     string   `json:"sid"`
			ChildID int      `json:"child_id"`
			Argv    []string `json:"argv"`
			Code    int      `json:"code"`
			TRel    float64  `json:"t_rel"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("trace2 event %q: %v", line, err)
		}
		c := child{ev.SID, ev.ChildID}
		switch {
		case ev.Event == "child_start" && len(ev.Argv) > 0 && ev.Argv[0] == hook:
			hooks[c] = true
		case ev.Event == "child_exit" && hooks[c]:
			ran = true
			// Only an exit 0 sleeps, so a failed hook proves nothing.
			if ev.Code != 0 {
				t.Fatalf("FSMonitor hook exited %d, want 0", ev.Code)
			}
			if ev.TRel >= raceExitSleep.Seconds() {
				t.Fatalf("FSMonitor hook took %.2fs, at least the race runtime's exit sleep; TestMain must call gittest.DisableRaceExitSleep", ev.TRel)
			}
		}
	}
	if !ran {
		t.Fatalf("git status ran no FSMonitor hook %s", hook)
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
