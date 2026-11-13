package gitstore

import "testing"

// grantWriteACL skips t: hasACL reads no ACL outside Linux.
func grantWriteACL(t *testing.T, _ string, _ uint32) {
	t.Skip("only Linux shows an ACL as a separate attribute")
}
