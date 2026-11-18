package slimtree

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

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
// repository commonDir, made at made, with its recovery ref at commit unless
// commit is "". It returns the archive's directory and ref.
func archiveAt(t *testing.T, homeDir, path, commonDir string, made time.Time, commit string) (string, string) {
	t.Helper()
	id, ns := state.ID(path), strconv.FormatInt(made.UnixNano(), 10)
	dir := filepath.Join(homeDir, "archives", id, ns)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state.Record{Version: 1, Backend: "apfsclone", Path: path, CommonDir: commonDir, Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worktree.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := "refs/slimtree/archive/" + id + "/" + ns + "/HEAD"
	if commit != "" {
		gitIn(t, commonDir, "update-ref", ref, commit)
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

var archivesUse = regexp.MustCompile(`(?m)^slimtree: archives use \d+\.\d MB, \d+\.\d{3}% of the disk$`)

// prune deletes each archive older than the rule, a month by default, with
// its recovery ref, and keeps a newer one with its ref. It deletes an
// archive whose removal stopped before the ref, or before the record, by the
// same rule.
func TestPruneDeletesArchivesOlderThanTheRuleWithTheirRecoveryRefs(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	commonDir, commit := commitRepo(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(commonDir)), "repo-feat")
	now := time.Now()
	old, oldRef := archiveAt(t, homeDir, work, commonDir, now.Add(-31*24*time.Hour), commit)
	recent, recentRef := archiveAt(t, homeDir, work, commonDir, now.Add(-29*24*time.Hour), commit)
	noRef, _ := archiveAt(t, homeDir, work, commonDir, now.Add(-32*24*time.Hour), "")
	noRecord, _ := archiveAt(t, homeDir, work, commonDir, now.Add(-33*24*time.Hour), "")
	if err := os.Remove(filepath.Join(noRecord, "worktree.json")); err != nil {
		t.Fatal(err)
	}
	if got := recoveryRefs(t, commonDir); !strings.Contains(got, oldRef+"\n") || !strings.Contains(got, recentRef+"\n") {
		t.Fatalf("recovery refs before prune = %q, want %s and %s", got, oldRef, recentRef)
	}

	code, stdout, stderr := slimtree(t.Context(), "prune")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "slimtree: rm and prune delete archives older than 30 days with their recovery refs; this one deleted 3\n") || !archivesUse.MatchString(stderr) {
		t.Fatalf("prune: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireGone(t, old)
	requireGone(t, noRef)
	requireGone(t, noRecord)
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
	homeDir := homeAfterFirstRun(t)
	commonDir, commit := commitRepo(t)
	repo := filepath.Dir(commonDir)
	dir, ref := archiveAt(t, homeDir, repo+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit)
	if err := os.Rename(repo, repo+".away"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := slimtree(t.Context(), "prune")
	kept := "slimtree: kept " + dir + " and " + ref + ": its repository " + commonDir + " is gone\n"
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
	homeDir := homeAfterFirstRun(t)
	commonDir, commit := commitRepo(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(commonDir)), "repo-feat")
	tenDays, tenDaysRef := archiveAt(t, homeDir, work, commonDir, time.Now().Add(-10*24*time.Hour), commit)

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

	if code, _, stderr := slimtree(t.Context(), "prune"); code != 0 || !strings.Contains(stderr, "slimtree: archives and their recovery refs are kept forever\n") {
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
	homeDir := homeAfterFirstRun(t)
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
	homeDir := homeAfterFirstRun(t)
	commonDir, commit := commitRepo(t)
	dir, ref := archiveAt(t, homeDir, filepath.Dir(commonDir)+"-feat", commonDir, time.Now().Add(-40*24*time.Hour), commit)
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
// recovery ref, keeps the archive and ref it made, and reports the archives'
// disk use.
func TestRmPrunesAfterwards(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	commonDir := filepath.Join(main, ".git")
	head := strings.TrimSpace(gitIn(t, main, "rev-parse", "HEAD"))
	old, oldRef := archiveAt(t, homeDir, filepath.Join(filepath.Dir(main), "main-gone"), commonDir, time.Now().Add(-31*24*time.Hour), head)
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := slimtree(t.Context(), "rm", "main-feat")
	m := regexp.MustCompile(`; (refs/slimtree/archive/\S+) keeps its HEAD`).FindStringSubmatch(stdout)
	if code != 0 || m == nil || !strings.Contains(stderr, "this one deleted 1\n") || !archivesUse.MatchString(stderr) {
		t.Fatalf("rm main-feat: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireGone(t, old)
	if got := recoveryRefs(t, commonDir); got != m[1]+"\n" {
		t.Fatalf("recovery refs = %q, want only %s, not %s", got, m[1], oldRef)
	}
	requireExists(t, filepath.Join(homeDir, "archives", strings.TrimSuffix(strings.TrimPrefix(m[1], "refs/slimtree/archive/"), "/HEAD"), "worktree.json"))
}
