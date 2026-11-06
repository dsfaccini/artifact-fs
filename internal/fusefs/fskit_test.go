//go:build !windows

package fusefs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dsfaccini/fuse"
)

func TestMountBackendRejectsUnknownValues(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "", want: BackendAuto},
		{value: "auto", want: BackendAuto},
		{value: "fskit", want: BackendFSKit},
		{value: "kernel", want: BackendKernel},
		{value: "FSKit"},
		{value: " fskit"},
		{value: "kext"},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(backendEnv, test.value)
			got, err := MountBackend()
			if test.want == "" {
				if err == nil || !strings.Contains(err.Error(), "unsupported") {
					t.Fatalf("MountBackend() = %q, %v; want an unsupported value error", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("MountBackend() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestMountWithBackendFallsBackOnlyWhenFSKitIsUnavailable(t *testing.T) {
	unavailable := fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable)
	kernelFailure := errors.New("kernel mount failed")
	otherFailure := errors.New("FSKit transport failed")
	tests := []struct {
		name        string
		backend     string
		fskitErr    error
		kernelErr   error
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErrs    []error
	}{
		{name: "kernel without kext", backend: BackendKernel, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{name: "kernel failure", backend: BackendKernel, kernelErr: kernelFailure, wantMounts: []bool{false}, wantBackend: BackendKernel, wantErrs: []error{kernelFailure}},
		{name: "fskit", backend: BackendFSKit, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "fskit unavailable never falls back", backend: BackendFSKit, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{unavailable}},
		{name: "auto mounts FSKit", backend: BackendAuto, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "auto other failure", backend: BackendAuto, fskitErr: otherFailure, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{otherFailure}},
		{name: "auto unavailable with kext", backend: BackendAuto, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
		{name: "auto unavailable without kext", backend: BackendAuto, fskitErr: unavailable, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{unavailable}},
		{name: "auto fallback failure", backend: BackendAuto, fskitErr: unavailable, kernelErr: kernelFailure, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel, wantErrs: []error{unavailable, kernelFailure}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mounts []bool
			mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, test.kernelErr
			}
			_, backend, err := mountWithBackend(test.backend, mount, func() bool { return test.kextLoaded })
			if !slices.Equal(mounts, test.wantMounts) {
				t.Fatalf("mount attempts (true is FSKit) = %v, want %v", mounts, test.wantMounts)
			}
			if backend != test.wantBackend {
				t.Fatalf("backend = %q, want %q", backend, test.wantBackend)
			}
			if len(test.wantErrs) == 0 && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if len(test.wantErrs) > 0 && err == nil {
				t.Fatal("error = nil, want a mount error")
			}
			for _, want := range test.wantErrs {
				if !errors.Is(err, want) {
					t.Fatalf("error = %v, want it to wrap %v", err, want)
				}
			}
		})
	}
}

func TestMountWithBackendFallsBackOnlyOnMachineWideFSKitFailures(t *testing.T) {
	result := func(code int32) error {
		return fmt.Errorf("mount (background): %w", fuse.FSKitMountError{Result: code})
	}
	for name, test := range map[string]struct {
		fskitErr     error
		wantFallback bool
	}{
		"framework unavailable":       {fskitErr: fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable), wantFallback: true},
		"unsupported OS version":      {fskitErr: result(fuse.FSKitMountResultUnsupportedOSVersion), wantFallback: true},
		"helper tools install failed": {fskitErr: result(fuse.FSKitMountResultHelperToolsInstallationFailed), wantFallback: true},
		"extension not found":         {fskitErr: result(fuse.FSKitMountResultFileSystemExtensionNotFound), wantFallback: true},
		"extension requires approval": {fskitErr: result(fuse.FSKitMountResultFileSystemExtensionRequiresApproval), wantFallback: true},
		"mount point failure":         {fskitErr: result(fuse.FSKitMountResultCreatingMountPointFailed)},
		"unexpected failure":          {fskitErr: result(fuse.FSKitMountResultUnexpectedFailure)},
		"other failure":               {fskitErr: errors.New("FSKit transport failed")},
	} {
		t.Run(name, func(t *testing.T) {
			var mounts []bool
			mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, nil
			}
			_, backend, err := mountWithBackend(BackendAuto, mount, func() bool { return true })
			if test.wantFallback {
				assertMountResult(t, mounts, backend, err, []bool{true, false}, BackendKernel, nil)
			} else {
				assertMountResult(t, mounts, backend, err, []bool{true}, BackendFSKit, test.fskitErr)
			}
		})
	}
}

// assertMountResult checks the mount attempts (true is FSKit), the backend, and
// the error that a mountWithBackend or platformMount call produced.
func assertMountResult(t *testing.T, mounts []bool, backend string, err error, wantMounts []bool, wantBackend string, wantErr error) {
	t.Helper()
	if !slices.Equal(mounts, wantMounts) {
		t.Fatalf("mount attempts (true is FSKit) = %v, want %v", mounts, wantMounts)
	}
	if backend != wantBackend {
		t.Fatalf("backend = %q, want %q", backend, wantBackend)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}
