package mounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/execfile"
)

// fakeFusermount is a fusermount that logs each call's arguments. It exits
// with the code in the file named after its first argument, 0 when there is
// none, and on exit 0 writes the file "gone".
const fakeFusermount = `#!/bin/sh
dir=${0%/*}
echo "$*" >> "$dir/log"
code=0
[ -f "$dir/exit$1" ] && read -r code < "$dir/exit$1"
[ "$code" = 0 ] && : > "$dir/gone"
exit "$code"
`

// Serial: it sets PATH and identifyHook.
func TestUnmountTriesALazyUnmountAndChecksTheMountWent(t *testing.T) {
	point := t.TempDir()
	for _, tc := range []struct {
		name string
		// program is the fake's name; PATH holds no other fusermount.
		program string
		// exits maps a flag to the fake's exit code for it.
		exits map[string]string
		// stays keeps the mount after a fake that exits 0.
		stays   bool
		mounted Identity
		// afterFailedU, when set, is what is mounted once a normal unmount
		// ran and failed.
		afterFailedU *Identity
		wantLog      string
		wantErr      string
	}{
		{name: "normal", program: "fusermount3", mounted: "id", wantLog: "-u " + point},
		{name: "lazy after a failed normal unmount", program: "fusermount3", exits: map[string]string{"-u": "1"}, mounted: "id", wantLog: "-u " + point + "\n-uz " + point},
		{name: "both fail", program: "fusermount3", exits: map[string]string{"-u": "1", "-uz": "1"}, mounted: "id", wantLog: "-u " + point + "\n-uz " + point, wantErr: "lazy unmount"},
		{name: "still mounted", program: "fusermount3", stays: true, mounted: "id", wantLog: "-u " + point, wantErr: "still mounted"},
		{name: "another mount", program: "fusermount3", mounted: "other", wantErr: ErrOtherMount.Error()},
		{name: "nothing mounted", program: "fusermount3", mounted: ""},
		{name: "fusermount without fusermount3", program: "fusermount", mounted: "id", wantLog: "-u " + point},
		{name: "gone after a failed normal unmount", program: "fusermount3", exits: map[string]string{"-u": "1"}, mounted: "id", afterFailedU: new(Identity), wantLog: "-u " + point},
		{name: "another mount after a failed normal unmount", program: "fusermount3", exits: map[string]string{"-u": "1"}, mounted: "id", afterFailedU: new(Identity("other")), wantLog: "-u " + point, wantErr: ErrOtherMount.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := execfile.Write(filepath.Join(bin, tc.program), []byte(fakeFusermount), 0o755); err != nil {
				t.Fatal(err)
			}
			for flag, code := range tc.exits {
				if err := os.WriteFile(filepath.Join(bin, "exit"+flag), []byte(code+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			identifyHook = func(path string) (Identity, error) {
				if path != point {
					t.Errorf("Unmount identified %s; want %s", path, point)
				}
				if _, err := os.Stat(filepath.Join(bin, "gone")); err == nil && !tc.stays {
					return "", nil
				}
				if log, _ := os.ReadFile(filepath.Join(bin, "log")); len(log) > 0 && tc.afterFailedU != nil {
					return *tc.afterFailedU, nil
				}
				return tc.mounted, nil
			}
			t.Cleanup(func() { identifyHook = nil })

			err := Unmount(t.Context(), point, "id")
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Unmount = %v; want an error with %q", err, tc.wantErr)
			}
			log, err := os.ReadFile(filepath.Join(bin, "log"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(log)); got != tc.wantLog {
				t.Fatalf("fusermount calls = %q; want %q", got, tc.wantLog)
			}
		})
	}
}
