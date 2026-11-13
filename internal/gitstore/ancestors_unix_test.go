//go:build !windows

package gitstore

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// Git runs the hook link by its path, so the hook is the link only while no
// other user can write or rename the state root or a directory above it.
// Group write passes when the directory is the user's and the group is
// private to the user, as every directory is group-writable under umask 002
// with a private group per user. Every other case falls back to the script in
// the git dir.
func TestFSMonitorHookIsTheLinkOnlyUnderPrivateAncestors(t *testing.T) {
	euid := os.Geteuid()
	for _, tc := range []struct {
		name string
		// mode is the parent's mode, and stateMode the state root's, 0700
		// when zero. owner, when not -1, is the uid lstat reports for the
		// parent. The parent's group is the user's primary group, the
		// group file lists members for it, holds extraGroup with %d for the
		// gid, or lacks the gid, another user has it as primary group,
		// written as peerGID when set, or the user database holds a NIS
		// entry. The parent lies on NFS.
		mode, stateMode os.FileMode
		owner           int
		primary         bool
		members         string
		extraGroup      string
		noGroup         bool
		peer            bool
		peerGID         string
		nis             bool
		acl             bool
		nfs             bool
		symlink         bool
		link            bool
	}{
		{name: "private", mode: 0o755, owner: -1, link: true},
		{name: "private group", mode: 0o775, owner: -1, primary: true, link: true},
		{name: "group that lists only the user", mode: 0o775, owner: -1, members: "me", link: true},
		{name: "group without the user", mode: 0o775, owner: -1},
		{name: "group that lists another user", mode: 0o775, owner: -1, primary: true, members: "me,other"},
		{name: "second entry for the group lists another user", mode: 0o775, owner: -1, primary: true, extraGroup: "shared:x:%d:other"},
		{name: "second entry for the group with a leading zero lists another user", mode: 0o775, owner: -1, primary: true, extraGroup: "shared:x:0%d:other"},
		{name: "group entry whose gid is not a number", mode: 0o775, owner: -1, primary: true, extraGroup: "shared:x:+%d:other"},
		{name: "group missing from the group file", mode: 0o775, owner: -1, primary: true, noGroup: true},
		{name: "group another user has as primary", mode: 0o775, owner: -1, primary: true, peer: true},
		{name: "group another user has as primary with a leading zero", mode: 0o775, owner: -1, primary: true, peer: true, peerGID: "0%d"},
		{name: "group another user has as primary after white space", mode: 0o775, owner: -1, primary: true, peer: true, peerGID: " %d"},
		{name: "group another user has as primary with a plus sign", mode: 0o775, owner: -1, primary: true, peer: true, peerGID: "+%d"},
		{name: "group from NIS", mode: 0o775, owner: -1, primary: true, nis: true},
		{name: "others can write", mode: 0o777, owner: -1, primary: true},
		{name: "sticky", mode: 0o777 | os.ModeSticky, owner: -1, primary: true},
		{name: "another user owns it", mode: 0o755, owner: euid + 1},
		{name: "root owns it", mode: 0o755, owner: 0, link: true},
		{name: "root owns it and the user's private group can write it", mode: 0o775, owner: 0, primary: true},
		{name: "access ACL grants another user write", mode: 0o755, owner: -1, primary: true, acl: true},
		{name: "on NFS", mode: 0o755, owner: -1, nfs: true},
		{name: "state root others can write", mode: 0o755, stateMode: 0o777, owner: -1},
		{name: "state root through a symlink", mode: 0o755, owner: -1, symlink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.owner == euid {
				t.Skip("the test runs as the owner it would fake")
			}
			// Under umask 002 the test's temp directory is group-writable,
			// and the fixture databases below describe only the parent.
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(tmp, "repo")
			run(t, "git", "init", repo)
			parent := filepath.Join(tmp, "parent")
			stateRoot := filepath.Join(parent, "state")
			if err := os.MkdirAll(stateRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			for dir, mode := range map[string]os.FileMode{parent: tc.mode, stateRoot: cmp.Or(tc.stateMode, 0o700)} {
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.acl {
				grantWriteACL(t, parent, uint32(euid+1))
			}
			if tc.symlink {
				link := filepath.Join(parent, "link")
				if err := os.Symlink(stateRoot, link); err != nil {
					t.Fatal(err)
				}
				stateRoot = link
			}
			// Directories above the test's own report root's 0755 on a local
			// file system, so the machine's directories decide no row.
			realLstat, realOnNFS := lstat, onNFS
			lstat = func(path string, st *syscall.Stat_t) error {
				err := realLstat(path, st)
				switch {
				case path == parent && tc.owner >= 0:
					st.Uid = uint32(tc.owner)
				case !strings.HasPrefix(path+"/", tmp+"/"):
					st.Uid, st.Mode = 0, syscall.S_IFDIR|0o755
				}
				return err
			}
			onNFS = func(path string) bool {
				if path == parent && tc.nfs {
					return true
				}
				return strings.HasPrefix(path+"/", tmp+"/") && realOnNFS(path)
			}
			t.Cleanup(func() { lstat, onNFS = realLstat, realOnNFS })

			var st syscall.Stat_t
			if err := syscall.Lstat(parent, &st); err != nil {
				t.Fatal(err)
			}
			primary := st.Gid + 1
			if tc.primary {
				primary = st.Gid
			}
			passwd := fmt.Sprintf("me:x:%d:%d::/:/bin/sh\n", euid, primary)
			if tc.peer {
				peerGID := fmt.Sprint(st.Gid)
				if tc.peerGID != "" {
					peerGID = fmt.Sprintf(tc.peerGID, st.Gid)
				}
				passwd += fmt.Sprintf("other:x:%d:%s::/:/bin/sh\n", euid+1, peerGID)
			}
			if tc.nis {
				passwd += "+::::::\n"
			}
			group := fmt.Sprintf("grp:x:%d:%s\n", st.Gid, tc.members)
			if tc.noGroup {
				group = fmt.Sprintf("grp:x:%d:%s\n", st.Gid+1, tc.members)
			}
			if tc.extraGroup != "" {
				group += fmt.Sprintf(tc.extraGroup, st.Gid) + "\n"
			}
			databases := t.TempDir()
			oldGroup, oldPasswd := groupFile, passwdFile
			groupFile, passwdFile = filepath.Join(databases, "group"), filepath.Join(databases, "passwd")
			t.Cleanup(func() { groupFile, passwdFile = oldGroup, oldPasswd })
			if err := os.WriteFile(groupFile, []byte(group), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(passwdFile, []byte(passwd), 0o644); err != nil {
				t.Fatal(err)
			}

			gitDir := filepath.Join(repo, ".git")
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}}
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
