package fusefs

import "testing"

func setMacFUSEKextLoaded(t *testing.T, loaded bool) {
	t.Helper()
	saved := macFUSEKextLoaded
	macFUSEKextLoaded = func() bool { return loaded }
	t.Cleanup(func() { macFUSEKextLoaded = saved })
}
