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
	"strconv"
	"strings"
	"testing"
	"time"
)

// blobBin is data/blob.bin's content in a fixture.
var blobBin = strings.Repeat("blob\x00\xff", 8192)

// alteringClone clones as clone does, then gives the clone of name in
// worktree the bytes that alter returns for its bytes, and the mtime that
// mtime returns for its mtime. It stands for a base file that changed after
// Add's git status.
func alteringClone(clone Clonefile, worktree, name string, alter func([]byte) []byte, mtime func(time.Time) time.Time) Clonefile {
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
		data = alter(data)
		_, err = f.WriteAt(data, 0)
		if err == nil {
			err = f.Truncate(int64(len(data)))
		}
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
				Clone: alteringClone(newRecorder(path).Clone, path, "data/blob.bin", flipped, keep),
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
// other bytes of its size and an mtime that is not older than the second in
// which Add's git status in the base started. Add records no stat data for
// it, so refresh hashes it and Add rewrites it from Git. Git itself hashes an
// entry whose mtime is not older than the index file's, which it treats as
// racily clean, so the case "changed as git status started" alone needs Add's
// check: the file changed in an earlier second than Add writes the index.
func TestAddHashesACloneNewerThanItsBaseStatus(t *testing.T) {
	t.Parallel()
	started := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		now   func() time.Time
		mtime func(time.Time) time.Time
	}{
		{"changed as git status started", func() time.Time { return started },
			func(time.Time) time.Time { return started.Add(500 * time.Millisecond) }},
		{"mtime now", nil, func(time.Time) time.Time { return time.Now() }},
		{"mtime in the future", nil, func(time.Time) time.Time { return time.Now().Add(time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil, nil)
			path := filepath.Join(f.worktrees, "clone")
			var rewritten []string
			var stderr bytes.Buffer
			_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
				AddArgs: []string{"-q", "-b", "feat/clone"}, Stderr: &stderr,
				Clone:        alteringClone(newRecorder(path).Clone, path, "data/blob.bin", flipped, tc.mtime),
				afterRewrite: func(paths []string) { rewritten = paths }, now: tc.now})
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
			if err := recordStats(t.Context(), repo, map[string]int64{"new.txt": 4}, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("recordStats: %v", err)
			}
			if !bytes.Equal(indexFile(t, repo), before) {
				t.Error("recordStats changed the index")
			}
		})
	}
}

// keepMtime keeps a clone's mtime.
func keepMtime(mtime time.Time) time.Time { return mtime }

// addClone runs Add for the worktree clone on the new branch feat/clone,
// cloning with clone, and returns its path and the paths it rewrote.
func addClone(t *testing.T, f fixture, clone func(path string) Clonefile, o Options) (string, []string) {
	t.Helper()
	path := filepath.Join(f.worktrees, "clone")
	var rewritten []string
	var stderr bytes.Buffer
	o.Base, o.Path, o.Commit, o.AddArgs, o.Stderr = f.base, path, "upstream/main", []string{"-q", "-b", "feat/clone"}, &stderr
	o.Clone = clone(path)
	o.afterRewrite = func(paths []string) { rewritten = paths }
	if _, err := Add(t.Context(), o); err != nil {
		t.Fatalf("Add: %v\n%s", err, stderr.Bytes())
	}
	return path, rewritten
}

// TestAddHashesACloneOfAnotherSize gives the clone of data/blob.bin 5 more
// bytes, keeping the base file's mtime. A clone whose size is not its blob's
// cannot hold its blob's bytes, because Add refuses every checkout that
// converts files. Add records no stat data for it, and rewrites it from Git.
func TestAddHashesACloneOfAnotherSize(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	grow := func(data []byte) []byte { return append(data, "grown"...) }
	path, rewritten := addClone(t, f, func(path string) Clonefile {
		return alteringClone(newRecorder(path).Clone, path, "data/blob.bin", grow, keepMtime)
	}, Options{})
	if !slices.Contains(rewritten, "data/blob.bin") {
		t.Errorf("Add rewrote %q, want data/blob.bin among them", rewritten)
	}
	if read(t, filepath.Join(path, "data/blob.bin")) != blobBin {
		t.Errorf("data/blob.bin does not hold its blob")
	}
}

// commitInBase commits README.md with other bytes of its size in the base,
// and gives the file an mtime older than any Add's clock.
func commitInBase(t *testing.T, f fixture) {
	t.Helper()
	readme := filepath.Join(f.base, "README.md")
	write(t, readme, "README\n", 0o644)
	run(t, f.base, "commit", "-qm", "edit README.md", "README.md")
	if err := os.Chtimes(readme, time.Time{}, baseMtime); err != nil {
		t.Fatal(err)
	}
}

// TestAddReadsTheBasesHEADOnceItsClockStarts commits README.md in the base
// when Add reads its clock, as a commit, checkout or pull that writes the
// file a second earlier would. Add then reads the new HEAD, whose README.md
// differs from upstream main's, so it writes README.md from Git. Reading
// HEAD before the clock trusted a clone of the new README.md. Add's HEAD
// check before registration also catches this commit, so the test fails
// only when both are gone.
func TestAddReadsTheBasesHEADOnceItsClockStarts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	path, _ := addClone(t, f, func(path string) Clonefile { return newRecorder(path).Clone },
		Options{now: func() time.Time { commitInBase(t, f); return time.Now() }})
	if got := read(t, filepath.Join(path, "README.md")); got != "readme\n" {
		t.Errorf("README.md = %q, want upstream main's", got)
	}
}

// TestAddTrustsNoCloneOnceTheBasesHEADMoves commits a README.md edit in the
// base after its git status, which therefore did not list the edit. Add
// finds HEAD moved before it registers the worktree, trusts no clone, and
// rewrites README.md from Git.
func TestAddTrustsNoCloneOnceTheBasesHEADMoves(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	path, rewritten := addClone(t, f, func(path string) Clonefile { return newRecorder(path).Clone },
		Options{beforeRegister: func() { commitInBase(t, f) }})
	if !slices.Contains(rewritten, "README.md") {
		t.Errorf("Add rewrote %q, want README.md among them", rewritten)
	}
	if got := read(t, filepath.Join(path, "README.md")); got != "readme\n" {
		t.Errorf("README.md = %q, want upstream main's", got)
	}
}

// TestAddHashesAPathTheBasesAttributesConvert gives the base an untracked
// .gitattributes whose filter swaps a and b in README.md, and a README.md of
// swapped bytes. git status compares README.md through the filter, so it
// lists nothing, and upstream main's attributes convert nothing. Add records
// no stat data for README.md, and rewrites it from Git.
func TestAddHashesAPathTheBasesAttributesConvert(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	run(t, f.base, "config", "filter.swap.clean", "tr ab ba")
	run(t, f.base, "config", "filter.swap.smudge", "tr ab ba")
	write(t, filepath.Join(f.base, ".gitattributes"), "README.md filter=swap\n", 0o644)
	readme := filepath.Join(f.base, "README.md")
	write(t, readme, "rebdme\n", 0o644)
	if err := os.Chtimes(readme, time.Time{}, baseMtime); err != nil {
		t.Fatal(err)
	}
	if got := run(t, f.base, "status", "--porcelain", "--untracked-files=no", "--", "README.md"); got != "" {
		t.Fatalf("git status in the base lists %q", got)
	}
	path, rewritten := addClone(t, f, func(path string) Clonefile { return newRecorder(path).Clone }, Options{})
	if !slices.Contains(rewritten, "README.md") {
		t.Errorf("Add rewrote %q, want README.md among them", rewritten)
	}
	if got := read(t, filepath.Join(path, "README.md")); got != "readme\n" {
		t.Errorf("README.md = %q, want its blob", got)
	}
}

// TestRecordStatsPatchesEveryIndexGitWrites has Git write indexes in each
// object format and version, with and without the EOIE and IEOT extensions
// that index.threads records, and with and without index.skipHash. Their
// names are 0xFFE, 0xFFF, 0x1000 and 5000 bytes long, share long prefixes,
// and one name of 0xFFF bytes is skip-worktree, which needs extended flags.
// recordStats patches the entries of the files on disk, and the index must
// then read back as the same index after Git's own refresh, and pass git
// fsck. A parse that lands on the wrong entry writes stat data there under a
// valid checksum.
func TestRecordStatsPatchesEveryIndexGitWrites(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return "long/" + strings.Repeat("n", n-len("long/")) }
	prefix := "pre/" + strings.Repeat("p", 200) + "/"
	files := []string{"a.txt", prefix + "a.txt", prefix + "b.txt", "zz.txt"}
	for i := range 20 {
		files = append(files, fmt.Sprintf("z/%02d.txt", i))
	}
	names := []string{long(0xffe), long(0xfff), long(0x1000), long(5000)}
	for _, format := range []string{"sha1", "sha256"} {
		for _, version := range []int{2, 3, 4} {
			for _, threads := range []bool{false, true} {
				for _, skipHash := range []bool{false, true} {
					name := fmt.Sprintf("%s/version %d/threads %t/skipHash %t", format, version, threads, skipHash)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						repo := t.TempDir()
						run(t, repo, "init", "-q", "--object-format="+format)
						run(t, repo, "config", "index.skipHash", strconv.FormatBool(skipHash))
						if threads {
							run(t, repo, "config", "index.threads", "4")
							run(t, repo, "config", "index.recordEndOfIndexEntries", "true")
							run(t, repo, "config", "index.recordOffsetTable", "true")
						} else {
							run(t, repo, "config", "index.threads", "1")
						}
						var info strings.Builder
						sizes := map[string]int64{}
						for _, p := range files {
							content := "file " + p + "\n"
							write(t, filepath.Join(repo, p), content, 0o644)
							if err := os.Chtimes(filepath.Join(repo, p), time.Time{}, baseMtime); err != nil {
								t.Fatal(err)
							}
							sizes[p] = int64(len(content))
						}
						for _, p := range append(slices.Clone(files), names...) {
							oid := runInput(t, repo, "content of "+p, "hash-object", "-w", "--stdin")
							if slices.Contains(files, p) {
								oid = run(t, repo, "hash-object", "-w", p)
							}
							fmt.Fprintf(&info, "100644 %s\t%s\n", oid, p)
						}
						runInput(t, repo, info.String(), "update-index", "--add", "--index-info")
						if version > 2 {
							run(t, repo, "update-index", "--skip-worktree", long(0xfff))
						}
						run(t, repo, "update-index", "--index-version", strconv.Itoa(version))
						index := indexFile(t, repo)
						if got := binary.BigEndian.Uint32(index[4:]); got != uint32(version) {
							t.Fatalf("index version = %d, want %d", got, version)
						}
						for _, ext := range []string{"EOIE", "IEOT"} {
							if got := bytes.Contains(index, []byte(ext)); got != threads {
								t.Fatalf("the index holds %s: %t", ext, got)
							}
						}
						hashed := filepath.Join(t.TempDir(), "index")
						if err := os.WriteFile(hashed, index, 0o644); err != nil {
							t.Fatal(err)
						}
						runEnv(t, repo, []string{"GIT_INDEX_FILE=" + hashed}, "update-index", "-q", "--ignore-missing", "--refresh")

						if err := recordStats(t.Context(), repo, sizes, time.Now().Add(time.Hour)); err != nil {
							t.Fatalf("recordStats: %v", err)
						}
						patched := indexFile(t, repo)
						hashSize := map[string]int{"sha1": 20, "sha256": 32}[format]
						if zero := bytes.Equal(patched[len(patched)-hashSize:], make([]byte, hashSize)); zero != skipHash {
							t.Fatalf("the index's checksum is zero: %t", zero)
						}
						got := runEnv(t, repo, nil, "ls-files", "--debug")
						if want := runEnv(t, repo, []string{"GIT_INDEX_FILE=" + hashed}, "ls-files", "--debug"); got != want {
							t.Errorf("git ls-files --debug after recordStats:\n%s\nafter Git's refresh:\n%s", got, want)
						}
						for _, p := range files {
							if !strings.Contains(got, p+"\n  ctime: ") || strings.Contains(got, p+"\n  ctime: 0:0\n") {
								t.Errorf("recordStats recorded no stat data for %s", p)
							}
						}
						run(t, repo, "fsck", "--no-progress", "--no-dangling")
					})
				}
			}
		}
	}
}
