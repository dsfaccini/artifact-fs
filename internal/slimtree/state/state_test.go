package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openHome(t *testing.T) *Home {
	t.Helper()
	h, err := Open(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRecordRoundTrip(t *testing.T) {
	h := openHome(t)
	r := Record{Backend: "artifactfs", Path: "/work/a", CommonDir: "/src/repo/.git", Branch: "feature", Created: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), SlimtreeVersion: "0.1.0", StateDir: "/home/w/state"}
	if err := h.Write(r); err != nil {
		t.Fatal(err)
	}
	got, err := h.Records()
	r.Version = 1
	if err != nil || !reflect.DeepEqual(got, []Record{r}) {
		t.Fatalf("Records() = %+v, %v; want %+v", got, err, r)
	}
	// The record lives at w/<the first 16 hex digits of the path's SHA-256>,
	// with no temporary file beside it.
	dir := filepath.Join(h.Dir(), "w", "d7f9747246691548")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "worktree.json" {
		t.Fatalf("%s holds %v (%v), want only worktree.json", dir, entries, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "worktree.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("worktree.json: %v, %v; want mode 0600", info, err)
	}
}

// A worktree resolves by its directory's name, or by its path, absolute or
// relative, even once the directory is gone.
func TestResolveByNameOrPath(t *testing.T) {
	h := openHome(t)
	base, err := Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]Record{}
	for _, p := range []string{"src/repo-a", "other/repo-a", "src/repo-b"} {
		r := Record{Backend: "apfsclone", Path: filepath.Join(base, p)}
		if err := h.Write(r); err != nil {
			t.Fatal(err)
		}
		r.Version = 1
		records[p] = r
	}
	t.Chdir(base)
	for _, tc := range []struct{ arg, want string }{
		{"repo-b", "src/repo-b"},
		{filepath.Join(base, "src", "repo-a"), "src/repo-a"},
		{"./other/repo-a", "other/repo-a"},
		{"src/../src/repo-b", "src/repo-b"},
	} {
		if got, err := h.Resolve(tc.arg); err != nil || got != records[tc.want] {
			t.Errorf("Resolve(%q) = %+v, %v; want %+v", tc.arg, got, err, records[tc.want])
		}
	}
	_, err = h.Resolve("repo-a")
	if want := "2 worktrees are named repo-a; name one by its path: " + filepath.Join(base, "other", "repo-a") + ", " + filepath.Join(base, "src", "repo-a"); err == nil || err.Error() != want {
		t.Errorf("Resolve(repo-a) = %v, want %q", err, want)
	}
	if _, err := h.Resolve("nope"); err == nil || err.Error() != "slimtree made no worktree nope" {
		t.Errorf("Resolve(nope) = %v, want no worktree", err)
	}
}

// A command that gives up waiting for the lock names who holds it.
func TestLockWaitNamesTheHolder(t *testing.T) {
	h := openHome(t)
	held, err := h.Lock(t.Context(), "/work/a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	lockWaiting = cancel
	t.Cleanup(func() { lockWaiting = nil })
	want := regexp.MustCompile(`^slimtree's lock is busy: pid ` + regexp.QuoteMeta(strconv.Itoa(os.Getpid())+" (slimtree "+strings.Join(os.Args[1:], " ")+")") + ` has held it on /work/a for \d+\.\d s; rerun the command once it ends$`)
	if _, err := h.Lock(ctx, ""); !errors.Is(err, ErrLockBusy) || !want.MatchString(err.Error()) {
		t.Fatalf("Lock while another holds it = %v, want %s", err, want)
	}
	lockWaiting = nil
	if _, err := h.TryLock(""); !errors.Is(err, ErrLockBusy) || !want.MatchString(err.Error()) {
		t.Fatalf("TryLock while another holds it = %v, want %s", err, want)
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	// The given-up wait releases the lock once its flock lands, so a later
	// Lock gets it.
	again, err := h.Lock(t.Context(), "")
	if err != nil {
		t.Fatalf("Lock after the holder released it: %v", err)
	}
	if err := again.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesAGroupWritableHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || err.Error() != "refusing state directory "+resolved+": writable by group or others (mode 0770)" {
		t.Fatalf("Open of a group-writable home = %v, want the refusal", err)
	}
}

func TestOnFUSEByPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos  string
		magic int64
		name  string
		want  bool
	}{
		{goos: "linux", magic: 0x65735546, want: true},
		{goos: "linux", magic: 0xef53},
		{goos: "darwin", name: "macfuse", want: true},
		{goos: "darwin", name: "apfs"},
	} {
		if got := onFUSE(tc.goos, tc.magic, tc.name); got != tc.want {
			t.Errorf("onFUSE(%s, %#x, %q) = %v, want %v", tc.goos, tc.magic, tc.name, got, tc.want)
		}
	}
}

// A write that fails after the record's create leaves no record and no
// temporary file.
func TestFailedRecordWriteLeavesNothing(t *testing.T) {
	h := openHome(t)
	fileCreated = func(f *os.File) {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { fileCreated = nil })
	if err := h.Write(Record{Backend: "apfsclone", Path: "/work/a"}); err == nil {
		t.Fatal("Write with a failing write succeeded")
	}
	dir := filepath.Join(h.Dir(), "w", ID("/work/a"))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("after the failed write, %s holds %v (%v), want nothing", dir, entries, err)
	}
}

// Lock gives up once LockWait passes, even when its caller's context lasts.
func TestLockGivesUpAfterLockWait(t *testing.T) {
	h := openHome(t)
	held, err := h.Lock(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Unlock() })
	lockWait = time.Millisecond
	t.Cleanup(func() { lockWait = LockWait })
	if _, err := h.Lock(t.Context(), ""); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("Lock while another holds it = %v, want ErrLockBusy", err)
	}
}
