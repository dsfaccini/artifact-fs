//go:build darwin && cgo && fskit

package fusefs

// fskitSupported mirrors the build constraint of jacobsa/fuse's FSKit transport.
const fskitSupported = true
