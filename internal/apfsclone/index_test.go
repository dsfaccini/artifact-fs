package apfsclone

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// blobBin is data/blob.bin's content in a fixture.
var blobBin = strings.Repeat("blob\x00\xff", 8192)

// alteringClone clones as clone does, then gives the clone of name in
// worktree other bytes of its size and the mtime that mtime returns for the
// clone's mtime. It stands for a base file that changed after Add's git
// status.
func alteringClone(clone Clonefile, worktree, name string, mtime func(time.Time) time.Time) Clonefile {
	return func(src, dst string) error {
		if err := clone(src, dst); err != nil || dst != filepath.Join(worktree, filepath.FromSlash(name)) {
			return err
		}
		fi, err := os.Lstat(dst)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteAt(flipped(data), 0)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		return os.Chtimes(dst, time.Time{}, mtime(fi.ModTime()))
	}
}

func flipped(data []byte) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = ^b
	}
	return out
}

// runEnv runs git as run does, with env added to its environment.
func runEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr.Bytes())
	}
	return strings.TrimSpace(string(out))
}

// recordedStats returns each path's stat data in the index that env names,
// as git ls-files --debug prints it, without the entry's flags.
func recordedStats(t *testing.T, worktree string, env []string) map[string]string {
	t.Helper()
	stats := map[string]string{}
	name := ""
	for line := range strings.SplitSeq(runEnv(t, worktree, env, "ls-files", "--debug"), "\n") {
		if !strings.HasPrefix(line, "  ") {
			name = line
			continue
		}
		stats[name] += regexp.MustCompile(`\tflags: \S+$`).ReplaceAllString(line, "") + "\n"
	}
	return stats
}

func indexFile(t *testing.T, worktree string) []byte {
	t.Helper()
	data, err := os.ReadFile(run(t, worktree, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestAddRecordsTheStatDataOfItsClones gives the clone of data/blob.bin other
// bytes of its size and the base file's mtime, as a base file that changed
// after Add's git status without a new mtime would. Add records the clone's
// stat data, so Git does not hash the clone: the clone keeps the other bytes,
// and git status lists nothing. That is the trade-off of recording stat data:
// Add no longer checks such a clone against its blob. Each case makes Git
// write another index format. Git must accept the index Add edited, and every
// other entry must hold the stat data that a fully hashed index holds.
func TestAddRecordsTheStatDataOfItsClones(t *testing.T) {
	for _, tc := range []struct {
		name string
		// format is the object format, and config the base's config.
		format string
		config map[string]string
		// skipWorktree marks Makefile skip-worktree in the new index, which
		// Git then writes as version 3, with extended flags.
		skipWorktree bool
		version      uint32
	}{
		{name: "version 2", format: "sha1", version: 2},
		{name: "version 3 with extended flags", format: "sha1", skipWorktree: true, version: 3},
		{name: "version 4", format: "sha1", config: map[string]string{"index.version": "4"}, version: 4},
		{name: "sha256", format: "sha256", version: 2},
		{name: "skipHash", format: "sha1", config: map[string]string{"index.skipHash": "true"}, version: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.format == "sha256" {
				// Serial: it sets the environment.
				t.Setenv("GIT_DEFAULT_HASH", "sha256")
			} else {
				t.Parallel()
			}
			// Paths with a common prefix, which version 4 compresses.
			f := newFixture(t, func(upstream string) {
				write(t, filepath.Join(upstream, "data/nested/a.txt"), "a\n", 0o644)
				write(t, filepath.Join(upstream, "data/nested/b.txt"), "b\n", 0o644)
				run(t, upstream, "add", "-A")
				run(t, upstream, "commit", "-qm", "nested")
			}, nil)
			for key, value := range tc.config {
				run(t, f.base, "config", key, value)
			}
			path := filepath.Join(f.worktrees, "clone")
			keep := func(mtime time.Time) time.Time { return mtime }
			var stderr bytes.Buffer
			_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
				AddArgs: []string{"-q", "-b", "feat/clone"}, Stderr: &stderr,
				Clone: alteringClone(newRecorder(path).Clone, path, "data/blob.bin", keep),
				afterRegister: func() {
					if tc.skipWorktree {
						run(t, path, "update-index", "--skip-worktree", "Makefile")
					}
				}})
			if err != nil {
				t.Fatalf("Add: %v\n%s", err, stderr.Bytes())
			}

			if got := run(t, path, "rev-parse", "--show-object-format"); got != tc.format {
				t.Fatalf("object format = %s, want %s", got, tc.format)
			}
			index := indexFile(t, path)
			if got := binary.BigEndian.Uint32(index[4:]); got != tc.version {
				t.Fatalf("index version = %d, want %d", got, tc.version)
			}
			hashSize := map[string]int{"sha1": 20, "sha256": 32}[tc.format]
			if skipped := bytes.Equal(index[len(index)-hashSize:], make([]byte, hashSize)); skipped != (tc.config["index.skipHash"] == "true") {
				t.Fatalf("index checksum is zero: %t", skipped)
			}
			if tc.skipWorktree {
				if got := run(t, path, "ls-files", "-v", "Makefile"); got != "S Makefile" {
					t.Fatalf("git ls-files -v Makefile = %q, want it skip-worktree", got)
				}
			}

			if got := read(t, filepath.Join(path, "data/blob.bin")); got != string(flipped([]byte(blobBin))) {
				t.Errorf("Add rewrote data/blob.bin, whose stat data it records")
			}
			if got := run(t, path, "status", "--porcelain", "--untracked-files=no"); got != "" {
				t.Errorf("git status lists %q", got)
			}
			if got := run(t, path, "diff-files", "--name-only"); got != "" {
				t.Errorf("git diff-files lists %q", got)
			}
			// git fsck checks the index's checksum, which other commands skip.
			run(t, path, "fsck", "--no-progress", "--no-dangling")

			hashed := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
			runEnv(t, path, hashed, "read-tree", "HEAD")
			runEnv(t, path, hashed, "update-index", "-q", "--refresh")
			got, want := recordedStats(t, path, nil), recordedStats(t, path, hashed)
			if len(got) != len(target(t, path)) {
				t.Fatalf("the index holds %d entries, want %d", len(got), len(target(t, path)))
			}
			// A fully hashed index records no stat data for the altered clone.
			delete(got, "data/blob.bin")
			delete(want, "data/blob.bin")
			if !maps.Equal(got, want) {
				for _, p := range slices.Sorted(maps.Keys(want)) {
					if got[p] != want[p] {
						t.Errorf("%s: Add's index records\n%s\na fully hashed index records\n%s", p, got[p], want[p])
					}
				}
			}
		})
	}
}

// target returns the paths of HEAD's tree in worktree.
func target(t *testing.T, worktree string) []string {
	t.Helper()
	return strings.Split(run(t, worktree, "ls-tree", "-r", "--name-only", "HEAD"), "\n")
}

// TestAddHashesACloneNewerThanItsBaseStatus gives the clone of data/blob.bin
// other bytes of its size and an mtime that is not older than Add's git
// status in the base. Add records no stat data for it, so refresh hashes it
// and Add rewrites it from Git. Git itself hashes an entry whose mtime is not
// older than the index file's, which it treats as racily clean. Add's check
// covers every such entry, since Add writes the index after its git status.
func TestAddHashesACloneNewerThanItsBaseStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		mtime func(time.Time) time.Time
	}{
		{"mtime now", func(time.Time) time.Time { return time.Now() }},
		{"mtime in the future", func(time.Time) time.Time { return time.Now().Add(time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil, nil)
			path := filepath.Join(f.worktrees, "clone")
			var rewritten []string
			var stderr bytes.Buffer
			_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
				AddArgs: []string{"-q", "-b", "feat/clone"}, Stderr: &stderr,
				Clone:        alteringClone(newRecorder(path).Clone, path, "data/blob.bin", tc.mtime),
				afterRewrite: func(paths []string) { rewritten = paths }})
			if err != nil {
				t.Fatalf("Add: %v\n%s", err, stderr.Bytes())
			}
			if !slices.Contains(rewritten, "data/blob.bin") {
				t.Errorf("Add rewrote %q, want data/blob.bin among them", rewritten)
			}
			if read(t, filepath.Join(path, "data/blob.bin")) != blobBin {
				t.Errorf("data/blob.bin does not hold its blob")
			}
		})
	}
}

// TestAddHashesAPathTheBaseHidesFromGitStatus covers a base file whose local
// edit git status does not list, because the base's index marks its path
// assume-unchanged or skip-worktree. The file is older than Add's git status,
// so only the mark keeps Add from recording its clone's stat data. Refresh
// then hashes the clone, and Add rewrites it from Git.
func TestAddHashesAPathTheBaseHidesFromGitStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, path, mark string }{
		{"assume-unchanged", "hidden.txt", ""},
		{"skip-worktree", "dirty.txt", "--skip-worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The fixture marks hidden.txt assume-unchanged, and edits it
			// and dirty.txt.
			f := newFixture(t, nil, nil)
			if tc.mark != "" {
				run(t, f.base, "update-index", tc.mark, tc.path)
			}
			if got := run(t, f.base, "status", "--porcelain", "--untracked-files=no", "--", tc.path); got != "" {
				t.Fatalf("git status in the base lists %q", got)
			}
			path := filepath.Join(f.worktrees, "clone")
			rec := newRecorder(path)
			var rewritten []string
			var stderr bytes.Buffer
			_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
				AddArgs: []string{"-q", "-b", "feat/clone"}, Stderr: &stderr, Clone: rec.Clone,
				afterRewrite: func(paths []string) { rewritten = paths }})
			if err != nil {
				t.Fatalf("Add: %v\n%s", err, stderr.Bytes())
			}
			if !slices.Contains(rec.cloned, tc.path) {
				t.Fatalf("Add cloned %q, want %s among them", rec.cloned, tc.path)
			}
			if !slices.Contains(rewritten, tc.path) {
				t.Errorf("Add rewrote %q, want %s among them", rewritten, tc.path)
			}
			if got := read(t, filepath.Join(path, tc.path)); got != "committed\n" {
				t.Errorf("%s = %q, want its blob", tc.path, got)
			}
		})
	}
}

// TestRecordStatsLeavesAnIndexItDoesNotKnow gives recordStats an index whose
// entry it could patch, in a format it does not edit: a split index whose
// main file holds the entry, and a version past 4. No Add makes either: Git
// splits the new worktree's index only when checkout-index writes it, and
// keeps the entries that it does not write in the shared index.
func TestRecordStatsLeavesAnIndexItDoesNotKnow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		index func(t *testing.T, repo string)
	}{
		{"split index", func(t *testing.T, repo string) {
			run(t, repo, "update-index", "--split-index")
			oid := run(t, repo, "hash-object", "-w", "new.txt")
			run(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+oid+",new.txt")
			if !bytes.Contains(indexFile(t, repo), []byte("new.txt\x00")) {
				t.Fatal("the split index's main file does not hold new.txt")
			}
		}},
		{"version 5", func(t *testing.T, repo string) {
			oid := run(t, repo, "hash-object", "-w", "new.txt")
			run(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+oid+",new.txt")
			index := indexFile(t, repo)
			binary.BigEndian.PutUint32(index[4:], 5)
			sum := sha1.Sum(index[:len(index)-sha1.Size])
			copy(index[len(index)-sha1.Size:], sum[:])
			if err := os.WriteFile(filepath.Join(repo, ".git", "index"), index, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			run(t, repo, "init", "-q")
			// Enough entries that adding one stays below
			// splitIndex.maxPercentChange, so Git writes new.txt to the main
			// file rather than a new shared index.
			for i := range 10 {
				write(t, filepath.Join(repo, fmt.Sprintf("%d.txt", i)), "a\n", 0o644)
			}
			run(t, repo, "add", "-A")
			run(t, repo, "commit", "-qm", "files")
			write(t, filepath.Join(repo, "new.txt"), "new\n", 0o644)
			tc.index(t, repo)
			before := indexFile(t, repo)
			if err := recordStats(t.Context(), repo, []string{"new.txt"}, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("recordStats: %v", err)
			}
			if !bytes.Equal(indexFile(t, repo), before) {
				t.Error("recordStats changed the index")
			}
		})
	}
}
