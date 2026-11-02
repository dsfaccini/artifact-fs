//go:build !fskit

package fusefs

import (
	"errors"

	"github.com/jacobsa/fuse"
)

// FSKitBuilt reports whether this build has the FSKit transport; see fskit_darwin.go.
const FSKitBuilt = false

var errFSKitNotBuilt = errors.New("this build has no FSKit transport; build with -tags fskit")

// platformMount never requests FSKit: upstream github.com/jacobsa/fuse has no
// FSKit transport, so this build mounts only through the kernel extension.
func platformMount(backend string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	kernelOnly := func(fskit bool) (*fuse.MountedFileSystem, error) {
		if fskit {
			return nil, errFSKitNotBuilt
		}
		return mount(false)
	}
	notBuilt := func(err error) bool { return errors.Is(err, errFSKitNotBuilt) }
	return mountWithBackend(backend, kernelOnly, notBuilt, macFUSEKextLoaded)
}
