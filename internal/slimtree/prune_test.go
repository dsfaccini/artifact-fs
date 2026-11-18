package slimtree

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// pruneHome sets SLIMTREE_HOME as homeAfterFirstRun does, and returns the
// home with its symlinks resolved, as prune prints its paths. On macOS the
// temporary directory lies below the symlink /var.
func pruneHome(t *testing.T) string {
	t.Helper()
	dir, err := state.Canonical(homeAfterFirstRun(t))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// commitRepo makes a repository with one commit and returns its canonical
// git dir and the commit.
func commitRepo(t *testing.T) (string, string) {
	t.Helper()
	root, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-q", "-b", "main", repo)
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	return filepath.Join(repo, ".git"), strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
}

// archiveAt fabricates the archive rm leaves of the worktree path of the
// repository commonDir, made at made, whose record names head, and its
// recovery ref at refAt unless refAt is "". It returns the archive's
// directory and ref.
func archiveAt(t *testing.T, homeDir, path, commonDir string, made time.Time, head, refAt string) (string, string) {
	t.Helper()
	id, ns := state.ID(path), strconv.FormatInt(made.UnixNano(), 10)
	dir := filepath.Join(homeDir, "archives", id, ns)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state.Record{Version: 1, Backend: "apfsclone", Path: path, CommonDir: commonDir, Branch: "feat", Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worktree.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := "refs/slimtree/archive/" + id + "/" + ns + "/HEAD"
	if refAt != "" {
		gitIn(t, commonDir, "update-ref", ref, refAt)
	}
	return dir, ref
}

// recoveryRefs lists the recovery refs of the repository commonDir.
func recoveryRefs(t *testing.T, commonDir string) string {
	t.Helper()
	return gitIn(t, commonDir, "for-each-ref", "--format=%(refname)", "refs/slimtree/")
}

func requireExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s: %v, want it kept", path, err)
	}
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: %v, want it deleted", path, err)
	}
}

var archivesUse = regexp.MustCompile(`(?m)^slimtree: archives use \d+(\.\d+)? [KMG]?B, (\d+\.\d{3}%|under 0\.001%) of the disk$`)

// prune deletes each archive older than the rule, a month by default, with
// its recovery ref, and keeps a newer one with its ref. It deletes an
// archive whose removal stopped before the ref, or before the record, by the
// same rule.
func TestPruneDeletesArchivesOlderThanTheRuleWithTheirRecoveryRefs(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(commonDir)), "repo-feat")
	now := time.Now()
	old, oldRef := archiveAt(t, homeDir, work, commonDir, now.Add(-31*24*time.Hour), commit, commit)
	recent, recentRef := archiveAt(t, homeDir, work, commonDir, now.Add(-29*24*time.Hour), commit, commit)
	noRef, _ := archiveAt(t, homeDir, work, commonDir, now.Add(-32*24*time.Hour), commit, "")
	noRecord, _ := archiveAt(t, homeDir, work, commonDir, now.Add(-33*24*time.Hour), commit, "")
	if err := os.Remove(filepath.Join(noRecord, "worktree.json")); err != nil {
		t.Fatal(err)
	}
	// slimtree 0.1.0's rm wrote no head into the archived record.
	legacy, legacyRef := archiveAt(t, homeDir, work, commonDir, now.Add(-34*24*time.Hour), "", commit)
	// Finder leaves .DS_Store files, which are no archives.
	for _, dir := range []string{filepath.Join(homeDir, "archives"), filepath.Dir(old)} {
		if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := recoveryRefs(t, commonDir); !strings.Contains(got, oldRef+"\n") || !strings.Contains(got, recentRef+"\n") || !strings.Contains(got, legacyRef+"\n") {
		t.Fatalf("recovery refs before prune = %q, want %s, %s and %s", got, oldRef, recentRef, legacyRef)
	}

	code, stdout, stderr := slimtree(t.Context(), "prune")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "slimtree: rm and prune delete archives older than 30 days with their recovery refs; this one deleted 4\n") || !archivesUse.MatchString(stderr) {
		t.Fatalf("prune: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireGone(t, old)
	requireGone(t, noRef)
	requireGone(t, noRecord)
	requireGone(t, legacy)
	requireExists(t, recent)
	if got := recoveryRefs(t, commonDir); got != recentRef+"\n" {
		t.Fatalf("recovery refs after prune = %q, want only %s", got, recentRef)
	}
	if got := strings.TrimSpace(gitIn(t, commonDir, "rev-parse", "main")); got != commit {
		t.Fatalf("main = %s after prune, want %s", got, commit)
	}
}

// prune keeps an old archive and its recovery ref while the repository is
// gone, as on a volume that is not mounted, and deletes both once it is back.
func TestPruneKeepsAnArchiveWhoseRepositoryIsGone(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	repo := filepath.Dir(commonDir)
	dir, ref := archiveAt(t, homeDir, repo+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit, commit)
	if err := os.Rename(repo, repo+".away"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := slimtree(t.Context(), "prune")
	kept := "slimtree: kept " + dir + " and " + ref + ": its repository " + commonDir + " is gone. Mount it and prune again, or, if it is gone for good, drop the archive with: rm -r '" + dir + "'\n"
	if code != 0 || stdout != "" || !strings.Contains(stderr, kept) || !strings.Contains(stderr, "this one deleted 0\n") {
		t.Fatalf("prune: exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, kept)
	}
	requireExists(t, dir)
	if err := os.Rename(repo+".away", repo); err != nil {
		t.Fatal(err)
	}
	if got := recoveryRefs(t, commonDir); got != ref+"\n" {
		t.Fatalf("recovery refs = %q, want %s kept", got, ref)
	}
	if code, _, stderr := slimtree(t.Context(), "prune"); code != 0 || !strings.Contains(stderr, "this one deleted 1\n") {
		t.Fatalf("prune with the repository back: exit %d, stderr %q", code, stderr)
	}
	requireGone(t, dir)
	if got := recoveryRefs(t, commonDir); got != "" {
		t.Fatalf("recovery refs = %q, want none", got)
	}
}

// retention shows the rule, a month by default, and sets week, month,
// forever or a number of days. Under forever, prune deletes nothing. Under a
// rule file that holds no rule, prune deletes nothing and fails.
func TestRetentionRuleDecidesWhatPruneDeletes(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(commonDir)), "repo-feat")
	tenDays, tenDaysRef := archiveAt(t, homeDir, work, commonDir, time.Now().Add(-10*24*time.Hour), commit, commit)

	for _, tc := range []struct{ args, want string }{
		{"", "month\n"},
		{"week", "week\n"},
		{"14d", "14d\n"},
		{"36500d", "36500d\n"},
		{"forever", "forever\n"},
	} {
		args := []string{"retention"}
		if tc.args != "" {
			args = append(args, tc.args)
		}
		if code, stdout, stderr := slimtree(t.Context(), args...); code != 0 || stdout != tc.want {
			t.Fatalf("retention %s: exit %d, stdout %q, stderr %q; want %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
	for _, bad := range []string{"0d", "36501d", "2w", "d", "-1d"} {
		want := `expected week, month, forever, or a number of days from 1d to 36500d, not "` + bad + `"` + "\n"
		if code, stdout, stderr := slimtree(t.Context(), "retention", "--", bad); code != 1 || stdout != "" || stderr != want {
			t.Fatalf("retention %s: exit %d, stdout %q, stderr %q; want exit 1 and %q", bad, code, stdout, stderr, want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(homeDir, "retention.json")); err != nil || string(data) != `{"max_age_days":null}`+"\n" {
		t.Fatalf("retention.json = %q, %v; want forever, unchanged by the refused rules", data, err)
	}

	if code, _, stderr := slimtree(t.Context(), "prune"); code != 0 || !strings.Contains(stderr, "slimtree: archives, their recovery refs and blobs are kept forever\n") {
		t.Fatalf("prune under forever: exit %d, stderr %q", code, stderr)
	}
	requireExists(t, tenDays)

	if err := os.WriteFile(filepath.Join(homeDir, "retention.json"), []byte(`{"max_age_days": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := "slimtree: pruned nothing: " + filepath.Join(homeDir, "retention.json") + " holds no valid retention rule: max_age_days 0 is not from 1 to 36500\n"
	if code, _, stderr := slimtree(t.Context(), "prune"); code != 1 || !strings.Contains(stderr, broken) {
		t.Fatalf("prune under a broken rule: exit %d, stderr %q; want exit 1 and %q", code, stderr, broken)
	}
	requireExists(t, tenDays)

	if code, stdout, stderr := slimtree(t.Context(), "retention", "week"); code != 0 || stdout != "week\n" {
		t.Fatalf("retention week over a broken rule: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := slimtree(t.Context(), "prune"); code != 0 || !strings.Contains(stderr, "older than 7 days with their recovery refs; this one deleted 1\n") {
		t.Fatalf("prune under week: exit %d, stderr %q", code, stderr)
	}
	requireGone(t, tenDays)
	if got := recoveryRefs(t, commonDir); got != "" {
		t.Fatalf("recovery refs = %q, want %s deleted", got, tenDaysRef)
	}
}

// prune never follows a symlink in archives/ out of the home, and reports
// it.
func TestPruneKeepsASymlinkedArchive(t *testing.T) {
	homeDir := pruneHome(t)
	outside := t.TempDir()
	old := filepath.Join(outside, strconv.FormatInt(time.Now().Add(-40*24*time.Hour).UnixNano(), 10))
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	id := filepath.Join(homeDir, "archives", state.ID("/elsewhere"))
	if err := os.MkdirAll(filepath.Dir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, id); err != nil {
		t.Fatal(err)
	}
	want := "slimtree: pruning problem: kept " + id + ": not an archive slimtree makes\n"
	if code, _, stderr := slimtree(t.Context(), "prune"); code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("prune: exit %d, stderr %q; want exit 1 and %q", code, stderr, want)
	}
	requireExists(t, old)
	requireExists(t, id)
}

// prune deletes nothing, and succeeds, while another command holds the lock.
func TestPruneSkipsWhileAnotherCommandHoldsTheLock(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	dir, ref := archiveAt(t, homeDir, filepath.Dir(commonDir)+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit, commit)
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := home.Lock(t.Context(), "/held")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	code, stdout, stderr := slimtree(t.Context(), "prune")
	if code != 0 || stdout != "" || !strings.HasPrefix(stderr, "slimtree: pruned nothing: slimtree's lock is busy: pid ") || !strings.Contains(stderr, " on /held for ") {
		t.Fatalf("prune under a held lock: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireExists(t, dir)
	if got := recoveryRefs(t, commonDir); got != ref+"\n" {
		t.Fatalf("recovery refs = %q, want %s kept", got, ref)
	}
}

// rm prunes once it removed the worktree: it deletes an old archive with its
// recovery ref, keeps the archive and ref it made, whose record names the
// ref's commit, and reports the archives' disk use. A pruning problem leaves
// rm's exit 0, and goes to stderr.
func TestRmPrunesAfterwards(t *testing.T) {
	homeDir := pruneHome(t)
	cloneWith(t, nil)
	main := newRepo(t)
	commonDir := filepath.Join(main, ".git")
	head := strings.TrimSpace(gitIn(t, main, "rev-parse", "HEAD"))
	old, oldRef := archiveAt(t, homeDir, filepath.Join(filepath.Dir(main), "main-gone"), commonDir, time.Now().Add(-31*24*time.Hour), head, head)
	stray := filepath.Join(homeDir, "archives", "stray")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := slimtree(t.Context(), "rm", "main-feat")
	m := regexp.MustCompile(`; (refs/slimtree/archive/(\S+)/HEAD) keeps its HEAD`).FindStringSubmatch(stdout)
	problem := "slimtree: pruning problem: kept " + stray + ": not an archive slimtree makes\n"
	if code != 0 || m == nil || !strings.Contains(stderr, "this one deleted 1\n") || !strings.Contains(stderr, problem) || !archivesUse.MatchString(stderr) {
		t.Fatalf("rm main-feat: exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, problem)
	}
	requireGone(t, old)
	if got := recoveryRefs(t, commonDir); got != m[1]+"\n" {
		t.Fatalf("recovery refs = %q, want only %s, not %s", got, m[1], oldRef)
	}
	data, err := os.ReadFile(filepath.Join(homeDir, "archives", m[2], "worktree.json"))
	if err != nil || !strings.Contains(string(data), `"head": "`+head+`"`) {
		t.Fatalf("archived record = %s, %v; want one that names the ref's commit %s", data, err, head)
	}
}

// prune deletes the ref first, and keeps the archive when git cannot delete
// the ref, as while another git holds its lock.
func TestPruneKeepsTheArchiveWhenGitCannotDeleteTheRef(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	dir, ref := archiveAt(t, homeDir, filepath.Dir(commonDir)+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit, commit)
	if _, err := os.Stat(filepath.Join(commonDir, ref)); err != nil {
		t.Fatalf("the recovery ref is not a loose ref, which this test locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, ref+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := slimtree(t.Context(), "prune")
	want := "slimtree: pruning problem: kept " + dir + ": git update-ref failed: "
	if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "this one deleted 0\n") {
		t.Fatalf("prune: exit %d, stderr %q; want exit 1 and %q", code, stderr, want)
	}
	requireExists(t, dir)
	if got := recoveryRefs(t, commonDir); got != ref+"\n" {
		t.Fatalf("recovery refs = %q, want %s kept", got, ref)
	}
}

// prune deletes a ref only at the commit rm kept in it. It keeps a ref that
// moved, and its archive, and prints the command that drops both.
func TestPruneKeepsARecoveryRefThatMoved(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	repo := filepath.Dir(commonDir)
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "later")
	later := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	dir, ref := archiveAt(t, homeDir, repo+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit, later)
	code, _, stderr := slimtree(t.Context(), "prune")
	drop := "git -C '" + commonDir + "' update-ref -d " + ref + " && rm -r '" + dir + "'"
	want := "slimtree: pruning problem: kept " + dir + ": " + ref + " points at " + later + ", not at " + commit + ", the commit rm kept; drop both with: " + drop + "\n"
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("prune: exit %d, stderr %q; want exit 1 and %q", code, stderr, want)
	}
	requireExists(t, dir)
	if got := gitIn(t, commonDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/slimtree/"); got != ref+" "+later+"\n" {
		t.Fatalf("recovery refs = %q, want %s kept at %s", got, ref, later)
	}
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", drop).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", drop, err, out)
	}
	requireGone(t, dir)
	if got := recoveryRefs(t, commonDir); got != "" {
		t.Fatalf("recovery refs after the printed command = %q, want none", got)
	}
}

// prune gives up on a repository that does not answer, as on a hung mount,
// and keeps the archive and its ref. A fake git hangs in update-ref, and the
// deadline expires once it started.
func TestPruneGivesUpOnARepositoryThatDoesNotAnswer(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	dir, ref := archiveAt(t, homeDir, filepath.Dir(commonDir)+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit, commit)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = update-ref ]; then : > '" + started + "'; sleep 60; exit 1; fi\ndone\nexec '" + realGit + "' \"$@\"\n"
	if err := execfile.Write(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := gittest.ExpireOnFile(t.Context(), started)
	defer cancel()
	code, _, stderr := slimtree(ctx, "prune")
	want := "slimtree: pruning problem: kept " + dir + ": its repository " + commonDir + " did not answer within 10s\n"
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("prune: exit %d, stderr %q; want exit 1 and %q", code, stderr, want)
	}
	requireExists(t, dir)
	if got := recoveryRefs(t, commonDir); got != ref+"\n" {
		t.Fatalf("recovery refs = %q, want %s kept", got, ref)
	}
}

// The archives' disk use reads in a unit that keeps a small total above
// zero.
func TestDiskUseKeepsASmallTotalReadable(t *testing.T) {
	for _, tc := range []struct {
		used, disk uint64
		want       string
	}{
		{0, 1e12, "0 B, 0.000% of the disk"},
		{512, 1e12, "512 B, under 0.001% of the disk"},
		{8192, 500e9, "8.2 KB, under 0.001% of the disk"},
		{25e6, 1e9, "25.0 MB, 2.500% of the disk"},
		{3.2e9, 1e12, "3.20 GB, 0.320% of the disk"},
	} {
		if got := diskUse(tc.used, tc.disk); got != tc.want {
			t.Errorf("diskUse(%d, %d) = %q, want %q", tc.used, tc.disk, got, tc.want)
		}
	}
}

// installBlob adds a blob of content to the cache dir as the hydrator does,
// read last at read, and returns its path.
func installBlob(t *testing.T, dir, oid, content string, read time.Time) string {
	t.Helper()
	staged, err := blobcache.Stage(dir, oid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blobcache.Install(dir, oid, staged); err != nil {
		t.Fatal(err)
	}
	path := blobcache.Path(dir, oid)
	if err := os.Chtimes(path, read, read); err != nil {
		t.Fatal(err)
	}
	return path
}

// prune evicts the blobs of the shared cache not read within the rule, and
// keeps the others. Under forever it evicts nothing.
func TestPruneEvictsBlobsNotReadWithinTheRule(t *testing.T) {
	homeDir := pruneHome(t)
	blobs := filepath.Join(homeDir, "blobs")
	now := time.Now()
	old := installBlob(t, blobs, strings.Repeat("a", 40), "old", now.Add(-40*24*time.Hour))
	recent := installBlob(t, blobs, strings.Repeat("b", 40), "recent", now.Add(-10*24*time.Hour))

	if code, _, stderr := slimtree(t.Context(), "retention", "forever"); code != 0 {
		t.Fatalf("retention forever: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := slimtree(t.Context(), "prune"); code != 0 || !strings.Contains(stderr, "blobs are kept forever\n") {
		t.Fatalf("prune under forever: exit %d, stderr %q", code, stderr)
	}
	requireExists(t, old)
	requireExists(t, recent)

	if code, _, stderr := slimtree(t.Context(), "retention", "month"); code != 0 {
		t.Fatalf("retention month: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr := slimtree(t.Context(), "prune")
	if want := "slimtree: they evict blobs not read for 30 days; this one evicted 1, 3 B\n"; code != 0 || !strings.Contains(stderr, want) {
		t.Fatalf("prune: exit %d, stderr %q; want exit 0 and %q", code, stderr, want)
	}
	requireGone(t, old)
	requireExists(t, recent)
	if st, err := blobcache.ReadStats(blobs); err != nil || st.Blobs != 1 || st.Bytes != int64(len("recent")) {
		t.Fatalf("cache stats = %+v, %v; want the one recent blob", st, err)
	}
}
