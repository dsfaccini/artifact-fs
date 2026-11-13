//go:build !windows

package gitstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// Git runs the hook link by its path, so the hook is the link only while no
// other user can rename a directory above the state root. Group write passes
// when the group is private to the user, as every directory is group-writable
// under umask 002 with a private group per user. A sticky directory passes, as
// /tmp does. Every other case falls back to the script in the git dir.
func TestFSMonitorHookIsTheLinkOnlyUnderPrivateAncestors(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		// The parent's group is the user's primary group, the group file
		// lists members for it, another user's primary group is it, or the
		// user database holds a NIS entry.
		primary bool
		members string
		peer    bool
		nis     bool
		link    bool
	}{
		{name: "private", mode: 0o755, link: true},
		{name: "private group", mode: 0o775, primary: true, link: true},
		{name: "group that lists only the user", mode: 0o775, members: "me", link: true},
		{name: "group without the user", mode: 0o775},
		{name: "group that lists another user", mode: 0o775, primary: true, members: "me,other"},
		{name: "group another user has as primary", mode: 0o775, primary: true, peer: true},
		{name: "group from NIS", mode: 0o775, primary: true, nis: true},
		{name: "others can write", mode: 0o777},
		{name: "sticky", mode: 0o777 | os.ModeSticky, link: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Under umask 002 the test's temp directory is group-writable,
			// and the fixture databases below describe only the parent.
			tmp := t.TempDir()
			if err := os.Chmod(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(tmp, "repo")
			run(t, "git", "init", repo)
			parent := filepath.Join(tmp, "parent")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, tc.mode); err != nil {
				t.Fatal(err)
			}
			var st syscall.Stat_t
			if err := syscall.Lstat(parent, &st); err != nil {
				t.Fatal(err)
			}
			euid := os.Geteuid()
			primary := st.Gid + 1
			if tc.primary {
				primary = st.Gid
			}
			passwd := fmt.Sprintf("me:x:%d:%d::/:/bin/sh\n", euid, primary)
			if tc.peer {
				passwd += fmt.Sprintf("other:x:%d:%d::/:/bin/sh\n", euid+1, st.Gid)
			}
			if tc.nis {
				passwd += "+::::::\n"
			}
			databases := t.TempDir()
			oldGroup, oldPasswd := groupFile, passwdFile
			groupFile, passwdFile = filepath.Join(databases, "group"), filepath.Join(databases, "passwd")
			t.Cleanup(func() { groupFile, passwdFile = oldGroup, oldPasswd })
			if err := os.WriteFile(groupFile, fmt.Appendf(nil, "grp:x:%d:%s\n", st.Gid, tc.members), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(passwdFile, []byte(passwd), 0o644); err != nil {
				t.Fatal(err)
			}

			gitDir := filepath.Join(repo, ".git")
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}}
			stateRoot := filepath.Join(parent, "state")
			if err := New(nil).ConfigureFSMonitorHook(t.Context(), cfg, stateRoot); err != nil {
				t.Fatal(err)
			}
			hook, err := runGit(t.Context(), gitDir, "config", "--get", "core.fsmonitor")
			if _, unsetErr := runGit(context.Background(), gitDir, "config", "--unset", "core.fsmonitor"); unsetErr != nil {
				t.Fatalf("disable test fsmonitor hook: %v", unsetErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := fsmonitorHookPath(gitDir)
			if tc.link {
				want = FSMonitorHookLink(stateRoot, cfg.ID)
			}
			if hook != want {
				t.Fatalf("core.fsmonitor = %q, want %q", hook, want)
			}
		})
	}
}
