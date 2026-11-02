//go:build fskit

package fusefs

import (
	"github.com/jacobsa/fuse"
)

// FSKitBuilt reports whether this build has the FSKit transport. Only the
// github.com/dsfaccini/fuse replacement of github.com/jacobsa/fuse provides it,
// so this file builds only with the fskit build tag.
const FSKitBuilt = true

func platformMount(backend string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	return mountWithBackend(backend, mount, fskitUnavailable, macFUSEKextLoaded)
}
