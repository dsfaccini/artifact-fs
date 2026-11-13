package gitstore

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// hasACL reports an ACL when it cannot read one, so the check fails closed.
func TestHasACLFailsClosedWhenItCannotRead(t *testing.T) {
	t.Parallel()
	if !hasACL(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("hasACL of a path it cannot read = false, want true")
	}
}

// grantWriteACL gives uid write to dir through an access ACL, as
// setfacl -m u:<uid>:rwx does. It skips t on a file system without ACLs.
func grantWriteACL(t *testing.T, dir string, uid uint32) {
	t.Helper()
	// The attribute holds version 2, then each entry as a 16-bit tag, 16-bit
	// permissions and a 32-bit ID, little-endian and sorted by tag.
	const undefined = 0xffffffff
	acl := binary.LittleEndian.AppendUint32(nil, 2)
	for _, entry := range []struct {
		tag, perm uint16
		id        uint32
	}{
		{0x01, 7, undefined}, // the owner
		{0x02, 7, uid},       // the named user
		{0x04, 5, undefined}, // the owning group
		{0x10, 7, undefined}, // the mask
		{0x20, 5, undefined}, // others
	} {
		acl = binary.LittleEndian.AppendUint16(acl, entry.tag)
		acl = binary.LittleEndian.AppendUint16(acl, entry.perm)
		acl = binary.LittleEndian.AppendUint32(acl, entry.id)
	}
	if err := unix.Setxattr(dir, "system.posix_acl_access", acl, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skipf("the file system takes no ACL: %v", err)
		}
		t.Fatal(err)
	}
}
