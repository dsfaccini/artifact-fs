//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The daemon turns on Git's untracked cache beside the fsmonitor hook. Git
// then lists a directory again only when the hook names a path in it, or,
// when the hook answers with a full rescan or Git runs without it, when the
// directory's stat data changed. git status stays right through each change
// below. The HEAD move skips on FSKit, which the restart follows.
func TestE2EGitStatusWithTheUntrackedCache(t *testing.T) {
	skipIfOverMountBudget(t, 2)
	repo := newMountedE2ERepo(t)
	dir := repo.mountPath
	if got := strings.TrimSpace(gitCmd(t, dir, "config", "--get", "core.untrackedCache")); got != "true" {
		t.Fatalf("core.untrackedCache = %q, want true", got)
	}
	// Git lists a directory whose mtime is not older than the index on every
	// status, so base directories, which report the commit time, get a
	// second past it before Git writes the cache.
	commitTime, err := strconv.ParseInt(strings.TrimSpace(gitCmd(t, dir, "log", "-1", "--format=%ct")), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, "a second past the commit time", func() (bool, string) {
		return time.Now().Unix() > commitTime+1, ""
	})

	want := map[string]bool{}
	check := func(t *testing.T, step string, args ...string) {
		t.Helper()
		got, _ := untrackedCacheStatus(t, dir, args...)
		if wantLines := slices.Sorted(maps.Keys(want)); !slices.Equal(got, wantLines) {
			t.Fatalf("git status after %s = %q, want %q", step, got, wantLines)
		}
	}
	check(t, "the mount came up")
	if _, opened := untrackedCacheStatus(t, dir); opened != 0 {
		t.Fatalf("git status with nothing changed opened %d directories, want none", opened)
	}

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		name   string
		change func()
		add    []string
		drop   []string
	}{
		{"a new untracked file at the root", func() { write("root.txt", "root\n") }, []string{"?? root.txt"}, nil},
		{"a new untracked file in a tracked directory", func() { write("packages/wrangler/src/new.js", "new\n") }, []string{"?? packages/wrangler/src/new.js"}, nil},
		{"a new untracked file in a new directory", func() { write("newdir/a/b.txt", "b\n") }, []string{"?? newdir/a/b.txt"}, nil},
		{"an untracked file deleted", func() {
			if err := os.Remove(filepath.Join(dir, "root.txt")); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{"?? root.txt"}},
		{"an untracked file renamed", func() {
			if err := os.Rename(filepath.Join(dir, "packages/wrangler/src/new.js"), filepath.Join(dir, "packages/wrangler/renamed.js")); err != nil {
				t.Fatal(err)
			}
		}, []string{"?? packages/wrangler/renamed.js"}, []string{"?? packages/wrangler/src/new.js"}},
		{"an untracked directory removed", func() {
			if err := os.RemoveAll(filepath.Join(dir, "newdir")); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{"?? newdir/a/b.txt"}},
		{"a new untracked log file", func() { write("packages/miniflare/debug.log", "log\n") }, []string{"?? packages/miniflare/debug.log"}, nil},
		{"a .gitignore that ignores it", func() { write(".gitignore", "*.log\n") }, []string{"?? .gitignore"}, []string{"?? packages/miniflare/debug.log"}},
		{"a tracked file edited", func() { write("README.md", "edited\n") }, []string{" M README.md"}, nil},
		{"a tracked file deleted", func() {
			if err := os.Remove(filepath.Join(dir, "SECURITY.md")); err != nil {
				t.Fatal(err)
			}
		}, []string{" D SECURITY.md"}, nil},
	} {
		step.change()
		for _, line := range step.add {
			want[line] = true
		}
		for _, line := range step.drop {
			delete(want, line)
		}
		check(t, step.name)
	}

	// Git without the hook writes an index whose token the hook does not
	// trust, so the hook's next answer is a full rescan. Both statuses then
	// find the new file through its directory's stat data.
	write("packages/vitest-pool/off.txt", "off\n")
	want["?? packages/vitest-pool/off.txt"] = true
	check(t, "a status without the hook", "-c", "core.fsmonitor=false")
	check(t, "the hook answered a full rescan")

	t.Run("HEAD move", func(t *testing.T) {
		skipIfFSKitHEADMove(t, "TestE2EGitStatusWithTheUntrackedCache")
		// A commit that adds a file the index lacks: HEAD then holds it, so
		// the index stages its deletion, and the mount shows it untracked.
		blobSource := filepath.Join(t.TempDir(), "c1.txt")
		if err := os.WriteFile(blobSource, []byte("c1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		blob := strings.TrimSpace(gitCmd(t, dir, "hash-object", "-w", blobSource))
		index := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
		gitEnv(t, dir, index, "read-tree", "HEAD")
		gitEnv(t, dir, index, "update-index", "--add", "--cacheinfo", "100644,"+blob+",packages/workers-shared/c1.txt")
		tree := gitEnv(t, dir, index, "write-tree")
		commit := strings.TrimSpace(gitCmd(t, dir, "-c", "user.name=E2E Test", "-c", "user.email=e2e@test", "commit-tree", tree, "-p", "HEAD", "-m", "add c1"))
		gitCmd(t, dir, "update-ref", "refs/heads/main", commit)
		waitForCondition(t, 15*time.Second, "daemon HEAD at "+commit, func() (bool, string) {
			st, err := repo.ctl.Status(context.Background(), repoName)
			if err != nil {
				return false, err.Error()
			}
			return st.CurrentHEADOID == commit, "head=" + st.CurrentHEADOID
		})
		want["D  packages/workers-shared/c1.txt"] = true
		want["?? packages/workers-shared/c1.txt"] = true
		check(t, "a HEAD move that adds a file the index lacks")
	})

	write("packages/wrangler/nested/restart.txt", "restart\n")
	want["?? packages/wrangler/nested/restart.txt"] = true
	repo.restart(t)
	check(t, "a daemon restart")
}

// untrackedCacheStatus runs git status in dir with args before the command,
// and returns its sorted porcelain lines and the number of directories Git
// opened. It fails t unless Git used the untracked cache.
func untrackedCacheStatus(t *testing.T, dir string, args ...string) ([]string, int) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace2.json")
	cmd := commandIn(dir, "git", gitArgsWithSafeDirectory(dir, append(args, "status", "--porcelain", "--untracked-files=all")...)...)
	cmd.Env = append(os.Environ(), "GIT_TRACE2_EVENT="+trace)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git status failed: %v\nstderr: %s", err, stderr.String())
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	events, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	opened, used := 0, false
	scanner := bufio.NewScanner(events)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var event struct {
			Category string          `json:"category"`
			Key      string          `json:"key"`
			Value    json.RawMessage `json:"value"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Category != "read_directory" || event.Key != "opendir" {
			continue
		}
		n, err := strconv.Atoi(strings.Trim(string(event.Value), `"`))
		if err != nil {
			t.Fatalf("read_directory opendir value %s: %v", event.Value, err)
		}
		opened += n
		used = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatalf("git status in %s did not use the untracked cache", dir)
	}
	return lines, opened
}

// gitEnv runs git in dir with env added, and returns its trimmed output.
func gitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := commandIn(dir, "git", gitArgsWithSafeDirectory(dir, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
