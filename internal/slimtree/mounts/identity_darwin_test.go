package mounts

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func statfs(point, fstype, source string, flagsExt uint32) unix.Statfs_t {
	var st unix.Statfs_t
	copy(st.Mntonname[:], point)
	copy(st.Fstypename[:], fstype)
	copy(st.Mntfromname[:], source)
	st.Flags_ext = flagsExt
	return st
}

func TestIdentityIsTheSourceOfTheFSKitMountOnTop(t *testing.T) {
	t.Parallel()
	const point = "/Users/u/wt"
	fskit := statfs(point, "macfuse", "macfuse://AAAA-1", mntExtFSKit)
	kext := statfs(point, "macfuse", "macfuse@1", 0)
	for _, tc := range []struct {
		name    string
		table   []unix.Statfs_t
		point   string
		want    Identity
		wantErr error
	}{
		{name: "FSKit", table: []unix.Statfs_t{statfs("/", "apfs", "/dev/disk3s1", 0), fskit}, point: point, want: "macfuse://AAAA-1"},
		{name: "FSKit on top of the kernel extension", table: []unix.Statfs_t{kext, fskit}, point: point, want: "macfuse://AAAA-1"},
		{name: "kernel extension on top", table: []unix.Statfs_t{fskit, kext}, point: point, wantErr: ErrUnrelatedMount},
		{name: "APFS", table: []unix.Statfs_t{statfs(point, "apfs", "/dev/disk3s5", 0)}, point: point, wantErr: ErrUnrelatedMount},
		{name: "macFUSE source without the FSKit flag", table: []unix.Statfs_t{statfs(point, "macfuse", "macfuse://AAAA-1", 0)}, point: point, wantErr: ErrUnrelatedMount},
		{name: "FSKit flag on another file system", table: []unix.Statfs_t{statfs(point, "other", "macfuse://AAAA-1", mntExtFSKit)}, point: point, wantErr: ErrUnrelatedMount},
		{name: "another case", table: []unix.Statfs_t{fskit}, point: "/users/U/WT", want: "macfuse://AAAA-1"},
		{name: "nothing mounted", table: []unix.Statfs_t{fskit}, point: point + "-sibling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identityIn(tc.table, tc.point)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("identityIn = %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
