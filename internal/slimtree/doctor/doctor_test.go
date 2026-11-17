package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeInfo struct{ mode fs.FileMode }

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// healthyLinux is a Linux machine where every check passes.
func healthyLinux() Probe {
	return Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Stat: func(path string) (fs.FileInfo, error) {
			if path == "/dev/fuse" {
				return fakeInfo{fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil
			}
			return fakeInfo{fs.ModeSetuid | 0o755}, nil
		},
		ReadWrite: func(string) error { return nil },
		ReadFile:  func(string) ([]byte, error) { return []byte("ID=ubuntu\nID_LIKE=debian\n"), nil },
		Output:    func(string, ...string) (string, error) { return "git version 2.45.0\n", nil },
		EUID:      1000,
	}
}

// healthyDarwin is a macOS machine where every check passes.
func healthyDarwin() Probe {
	return Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Output: func(name string, _ ...string) (string, error) {
			return map[string]string{"xcode-select": "/Library/Developer/CommandLineTools\n", "git": "git version 2.54.0 (Apple Git-157)\n"}[name], nil
		},
		OSVersion:      func() (string, error) { return "26.1", nil },
		FSKitAvailable: func() error { return nil },
		APFS:           func(string) error { return nil },
	}
}

func statuses(r Report) map[string]Status {
	got := map[string]Status{}
	for _, c := range r.Checks {
		got[c.ID] = c.Status
	}
	return got
}

func result(t *testing.T, r Report, id string) Result {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("report has no %s check: %+v", id, r.Checks)
	return Result{}
}

func TestChecksByPlatformAndBackend(t *testing.T) {
	for _, tc := range []struct {
		goos, backend string
		probe         Probe
		want          map[string]Status
		reminders     int
	}{
		{goos: "linux", backend: "artifactfs", probe: healthyLinux(), want: map[string]Status{"fusermount3": StatusOK, "dev-fuse": StatusOK, "git": StatusOK}},
		{goos: "darwin", backend: "apfsclone", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "apfs": StatusOK}},
		{goos: "darwin", backend: "artifactfs", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "macos-version": StatusOK, "macfuse-fskit": StatusOK}, reminders: 2},
	} {
		r := Run(tc.probe, tc.goos, tc.backend, "/work")
		if got := statuses(r); !reflect.DeepEqual(got, tc.want) || !r.OK || len(r.Reminders) != tc.reminders {
			t.Errorf("%s/%s: checks %v, ok %v, %d reminders; want %v, ok, %d reminders", tc.goos, tc.backend, got, r.OK, len(r.Reminders), tc.want, tc.reminders)
		}
	}
}

func TestInstallCommandFromOSRelease(t *testing.T) {
	for _, tc := range []struct{ osRelease, want string }{
		{"ID=ubuntu\nID_LIKE=debian\n", "sudo apt-get install -y fuse3"},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "sudo apt-get install -y fuse3"},
		{"ID=fedora\n", "sudo dnf install -y fuse3"},
		{"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", "sudo dnf install -y fuse3"},
		{"ID=arch\n", "sudo pacman -S --needed fuse3"},
		{"ID=alpine\n", "sudo apk add fuse3"},
		{"ID=\"opensuse-tumbleweed\"\nID_LIKE=\"opensuse suse\"\n", "sudo zypper install -y fuse3"},
		{"ID=nixos\n", "install fuse3 with your distribution's package manager"},
	} {
		if got := installCommand(parseOSRelease(tc.osRelease), "fuse3"); got != tc.want {
			t.Errorf("installCommand(%q) = %q, want %q", tc.osRelease, got, tc.want)
		}
	}
}

func TestGitVersionFloor(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Status
		fix  string
	}{
		{"git version 2.45.0\n", StatusOK, ""},
		{"git version 2.54.0 (Apple Git-157)\n", StatusOK, ""},
		{"git version 3.0\n", StatusOK, ""},
		{"git version 2.43.0\n", StatusWarn, "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"},
		{"git version 2.9.5\n", StatusWarn, "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"},
		{"not git\n", StatusWarn, "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"},
	} {
		p := healthyLinux()
		p.Output = func(string, ...string) (string, error) { return tc.out, nil }
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got.Status != tc.want || got.Fix != tc.fix && tc.want != StatusOK {
			t.Errorf("git check for %q = %+v, want %s with fix %q", tc.out, got, tc.want, tc.fix)
		}
	}
	p := healthyLinux()
	p.LookPath = func(file string) (string, error) {
		if file == "git" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got.Status != StatusFail || got.Fix != "sudo apt-get install -y git" {
		t.Errorf("git check without git = %+v, want a failure with the install command", got)
	}
}

func TestFusermountNeedsSetuidUnlessRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  fs.FileMode
		found bool
		euid  int
		want  Status
		fix   string
	}{
		{name: "setuid", mode: fs.ModeSetuid | 0o755, found: true, euid: 1000, want: StatusOK},
		{name: "not setuid", mode: 0o755, found: true, euid: 1000, want: StatusFail, fix: "sudo chmod u+s /usr/bin/fusermount3"},
		{name: "not setuid as root", mode: 0o755, found: true, euid: 0, want: StatusWarn, fix: "sudo chmod u+s /usr/bin/fusermount3"},
		{name: "missing", euid: 1000, want: StatusFail, fix: "sudo apt-get install -y fuse3"},
		{name: "missing as root", euid: 0, want: StatusWarn, fix: "sudo apt-get install -y fuse3"},
	} {
		p := healthyLinux()
		p.EUID = tc.euid
		p.LookPath = func(file string) (string, error) {
			if file == "fusermount3" && !tc.found {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		}
		p.Stat = func(path string) (fs.FileInfo, error) {
			if path == "/dev/fuse" {
				return fakeInfo{fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil
			}
			return fakeInfo{tc.mode}, nil
		}
		r := Run(p, "linux", "artifactfs", "/work")
		if got := result(t, r, "fusermount3"); got.Status != tc.want || got.Fix != tc.fix && tc.want != StatusOK {
			t.Errorf("%s: fusermount3 check = %+v, want %s with fix %q", tc.name, got, tc.want, tc.fix)
		}
		if r.OK != (tc.want != StatusFail) {
			t.Errorf("%s: report ok = %v with fusermount3 %s", tc.name, r.OK, tc.want)
		}
	}
}

func TestDevFuseFixes(t *testing.T) {
	p := healthyLinux()
	p.Stat = func(path string) (fs.FileInfo, error) {
		if path == "/dev/fuse" {
			return nil, os.ErrNotExist
		}
		return fakeInfo{fs.ModeSetuid | 0o755}, nil
	}
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got.Status != StatusFail || !strings.HasPrefix(got.Fix, "sudo modprobe fuse") {
		t.Errorf("dev-fuse without /dev/fuse = %+v, want the modprobe fix", got)
	}
	p = healthyLinux()
	p.ReadWrite = func(string) error { return os.ErrPermission }
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got.Status != StatusFail || got.Fix != "sudo chmod 0666 /dev/fuse" {
		t.Errorf("dev-fuse not writable = %+v, want the chmod fix", got)
	}
}

func TestDarwinChecksReportTheirCause(t *testing.T) {
	p := healthyDarwin()
	p.Output = func(string, ...string) (string, error) { return "", errors.New("exit status 2") }
	r := Run(p, "darwin", "apfsclone", "/work")
	if got := statuses(r); got["xcode-select"] != StatusFail || got["git"] != StatusSkip || r.OK {
		t.Errorf("without the Command Line Tools: %v, ok %v; want xcode-select failed and git skipped", got, r.OK)
	}

	p = healthyDarwin()
	p.OSVersion = func() (string, error) { return "15.6", nil }
	fskitErr := errors.New("macFUSE FSKit backend is unavailable; install macFUSE 5.5 or later: macFUSE 5.1.3 is installed")
	p.FSKitAvailable = func() error { return fskitErr }
	r = Run(p, "darwin", "artifactfs", "/work")
	if got := result(t, r, "macos-version"); got.Status != StatusFail || got.Fix != "Upgrade macOS: System Settings → General → Software Update" {
		t.Errorf("macOS 15.6 = %+v, want the upgrade fix", got)
	}
	if got := result(t, r, "macfuse-fskit"); got.Status != StatusFail || got.Detail != fskitErr.Error() {
		t.Errorf("FSKit unavailable = %+v, want FSKitAvailable's error verbatim", got)
	}

	p = healthyDarwin()
	p.APFS = func(dir string) error { return errors.New(dir + " is on hfs, not APFS") }
	if got := result(t, Run(p, "darwin", "apfsclone", "/work"), "apfs"); got.Status != StatusFail || got.Detail != "/work is on hfs, not APFS" {
		t.Errorf("an HFS+ directory = %+v, want the APFS failure", got)
	}
}

func TestRequireNamesEachFailedCheck(t *testing.T) {
	p := healthyLinux()
	if err := Require(p, "linux", "artifactfs", "new", "/work"); err != nil {
		t.Fatalf("Require on a healthy machine: %v", err)
	}
	// A git below the floor only warns.
	p.Output = func(string, ...string) (string, error) { return "git version 2.43.0\n", nil }
	if err := Require(p, "linux", "artifactfs", "new", "/work"); err != nil {
		t.Fatalf("Require with an old git: %v, want only a warning", err)
	}
	p.ReadWrite = func(string) error { return os.ErrPermission }
	err := Require(p, "linux", "artifactfs", "new", "/work")
	want := "slimtree new --backend artifactfs needs:\n  dev-fuse: /dev/fuse is not readable and writable: permission denied; fix: sudo chmod 0666 /dev/fuse"
	if err == nil || err.Error() != want {
		t.Fatalf("Require with an unwritable /dev/fuse = %v, want %q", err, want)
	}
}

func TestReportJSONShape(t *testing.T) {
	p := healthyLinux()
	p.Output = func(string, ...string) (string, error) { return "git version 2.43.0\n", nil }
	var out, data bytes.Buffer
	if err := Run(p, "linux", "artifactfs", "/work").WriteJSON(&out); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&data, out.Bytes()); err != nil {
		t.Fatal(err)
	}
	want := `{"os":"linux","backend":"artifactfs","ok":true,"checks":[` +
		`{"id":"fusermount3","status":"ok","detail":"/usr/bin/fusermount3"},` +
		`{"id":"dev-fuse","status":"ok","detail":"/dev/fuse"},` +
		`{"id":"git","status":"warn","detail":"git version 2.43.0 is older than 2.45","fix":"sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"}]}`
	if data.String() != want {
		t.Fatalf("report JSON =\n%s\nwant\n%s", data.String(), want)
	}
}
