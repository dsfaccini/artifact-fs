//go:build !windows && !(darwin && cgo && fskit)

package fusefs

// fskitSupported mirrors the build constraint of jacobsa/fuse's FSKit transport.
const fskitSupported = false
