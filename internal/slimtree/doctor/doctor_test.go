package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

const (
	// fullCaps is the capability mask of a process with every capability.
	fullCaps = "000001ffffffffff"
	noCaps   = "0000000000000000"
	// dockerCaps is Docker's default bounding set, which lacks CAP_SYS_ADMIN.
	dockerCaps = "00000000a80425fb"
	ubuntu     = "ID=ubuntu\nID_LIKE=debian\n"
)

// procStatus is a /proc/self/status with the given capability sets and
// NoNewPrivs.
func procStatus(capEff, capBnd, noNewPrivs string) string {
	return "Name:\tslimtree\nCapEff:\t" + capEff + "\nCapBnd:\t" + capBnd + "\nNoNewPrivs:\t" + noNewPrivs + "\n"
}

// linuxFiles serves osRelease as /etc/os-release and status as
// /proc/self/status. Every other file is missing.
func linuxFiles(osRelease, status string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch path {
		case "/etc/os-release":
			return []byte(osRelease), nil
		case "/proc/self/status":
			return []byte(status), nil
		}
		return nil, os.ErrNotExist
	}
}

// fuseStat stats /dev/fuse as a character device and every other path as a
// file with mode, owned by uid.
func fuseStat(mode fs.FileMode, uid uint32) func(string) (fs.FileInfo, error) {
	return func(path string) (fs.FileInfo, error) {
		if path == "/dev/fuse" {
			return fakeInfo{mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil
		}
		return fakeInfo{mode: mode, uid: uid}, nil
	}
}

// healthyLinux is an Ubuntu machine where every check passes for a user who
// mounts through the setuid fusermount3.
func healthyLinux() Probe {
	return Probe{
		LookPath:  func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Stat:      fuseStat(fs.ModeSetuid|0o755, 0),
		ReadWrite: func(string) error { return nil },
		ReadFile:  linuxFiles(ubuntu, procStatus(noCaps, fullCaps, "0")),
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
		{goos: "linux", backend: "artifactfs", probe: healthyLinux(), want: map[string]Status{"fusermount": StatusOK, "dev-fuse": StatusOK, "cap-sys-admin": StatusOK, "git": StatusOK}},
		{goos: "darwin", backend: "apfsclone", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "apfs": StatusOK}},
		{goos: "darwin", backend: "artifactfs", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "macos-version": StatusOK, "macfuse-fskit": StatusOK}, reminders: 2},
	} {
		r := Run(tc.probe, tc.goos, tc.backend, "/work")
		if got := statuses(r); !reflect.DeepEqual(got, tc.want) || !r.OK || len(r.Reminders) != tc.reminders {
			t.Errorf("%s/%s: checks %v, ok %v, %d reminders; want %v, ok, %d reminders", tc.goos, tc.backend, got, r.OK, len(r.Reminders), tc.want, tc.reminders)
		}
	}
}

// Without fusermount, the fix installs fuse3 with the package manager that
// os-release names, read from /usr/lib/os-release when /etc/os-release is
// missing.
func TestInstallCommandFromOSRelease(t *testing.T) {
	const aptNote = " (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04)"
	for _, tc := range []struct{ etc, usrLib, want string }{
		{etc: ubuntu, want: "sudo apt-get update && sudo apt-get install -y fuse3" + aptNote},
		{etc: "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", want: "sudo apt-get update && sudo apt-get install -y fuse3" + aptNote},
		{etc: "ID=fedora\n", want: "sudo dnf install -y fuse3"},
		{etc: "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", want: "sudo dnf install -y fuse3"},
		{etc: "ID=arch\n", want: "sudo pacman -S --needed fuse3"},
		{etc: "ID=alpine\n", want: "sudo apk add fuse3"},
		{etc: "ID=\"opensuse-tumbleweed\"\nID_LIKE=\"opensuse suse\"\n", want: "sudo zypper install -y fuse3"},
		{etc: "ID=nixos\n", want: "install fuse3 with your distribution's package manager"},
		{usrLib: "ID=fedora\n", want: "sudo dnf install -y fuse3"},
	} {
		p := healthyLinux()
		p.LookPath = func(file string) (string, error) {
			if strings.HasPrefix(file, "fusermount") {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		}
		p.ReadFile = func(path string) ([]byte, error) {
			files := map[string]string{"/etc/os-release": tc.etc, "/usr/lib/os-release": tc.usrLib, "/proc/self/status": procStatus(noCaps, fullCaps, "0")}
			if files[path] == "" {
				return nil, os.ErrNotExist
			}
			return []byte(files[path]), nil
		}
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "fusermount"); got.Status != StatusFail || got.Fix != tc.want {
			t.Errorf("fusermount check with /etc/os-release %q and /usr/lib/os-release %q = %+v, want a failure with fix %q", tc.etc, tc.usrLib, got, tc.want)
		}
	}
}

// An old git's fix upgrades it: through the git-core PPA on Ubuntu and its
// derivatives, and elsewhere with the package manager's upgrade, which stops
// at the newest git the release ships.
func TestGitUpgradeFixPerDistribution(t *testing.T) {
	const ppa = "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"
	const stays = "; if git stays older than 2.45, your release ships no newer git"
	for _, tc := range []struct{ osRelease, want string }{
		{ubuntu, ppa},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", ppa},
		{"ID=debian\n", "sudo apt-get update && sudo apt-get install -y --only-upgrade git" + stays},
		{"ID=fedora\n", "sudo dnf upgrade -y git" + stays},
		{"ID=arch\n", "sudo pacman -Syu git" + stays},
		{"ID=alpine\n", "sudo apk add -U --upgrade git" + stays},
		{"ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n", "sudo zypper update -y git" + stays},
		{"ID=nixos\n", "upgrade git to 2.45 or later with your distribution's package manager"},
	} {
		p := healthyLinux()
		p.ReadFile = linuxFiles(tc.osRelease, procStatus(noCaps, fullCaps, "0"))
		p.Output = func(string, ...string) (string, error) { return "git version 2.39.5\n", nil }
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got.Status != StatusWarn || got.Fix != tc.want {
			t.Errorf("old git on %q = %+v, want a warning with fix %q", tc.osRelease, got, tc.want)
		}
	}
}

func TestGitVersionFloor(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Status
	}{
		{"git version 2.45.0\n", StatusOK},
		{"git version 2.54.0 (Apple Git-157)\n", StatusOK},
		{"git version 3.0\n", StatusOK},
		{"git version 2.43.0\n", StatusWarn},
		{"git version 2.9.5\n", StatusWarn},
		{"not git\n", StatusWarn},
	} {
		p := healthyLinux()
		p.Output = func(string, ...string) (string, error) { return tc.out, nil }
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got.Status != tc.want {
			t.Errorf("git check for %q = %+v, want %s", tc.out, got, tc.want)
		}
	}
	p := healthyLinux()
	p.LookPath = func(file string) (string, error) {
		if file == "git" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got.Status != StatusFail || got.Fix != "sudo apt-get update && sudo apt-get install -y git" {
		t.Errorf("git check without git = %+v, want a failure with the install command", got)
	}
}

// The fuse library runs fusermount3, else fusermount, only when its own
// mount(2) fails, which it does unless CapEff holds CAP_SYS_ADMIN. Root runs
// fusermount with CapBnd's capabilities, so it needs no setuid bit.
func TestFusermountNeedsSetuidUnlessRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  fs.FileMode
		found []string
		euid  int
		caps  string
		want  Status
		fix   string
	}{
		{name: "setuid", mode: fs.ModeSetuid | 0o755, found: []string{"fusermount3"}, euid: 1000, caps: noCaps, want: StatusOK},
		{name: "FUSE 2's fusermount", mode: fs.ModeSetuid | 0o755, found: []string{"fusermount"}, euid: 1000, caps: noCaps, want: StatusOK},
		{name: "not setuid", mode: 0o755, found: []string{"fusermount3"}, euid: 1000, caps: noCaps, want: StatusFail, fix: "sudo chmod u+s /usr/bin/fusermount3"},
		{name: "missing", euid: 1000, caps: noCaps, want: StatusFail, fix: "sudo apt-get update && sudo apt-get install -y fuse3 (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04)"},
		{name: "not setuid as root", mode: 0o755, found: []string{"fusermount3"}, euid: 0, caps: noCaps, want: StatusOK},
		{name: "missing as root", euid: 0, caps: fullCaps, want: StatusWarn, fix: "apt-get update && apt-get install -y fuse3 (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04)"},
		{name: "missing as root without CapEff", euid: 0, caps: noCaps, want: StatusFail, fix: "apt-get update && apt-get install -y fuse3 (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04)"},
	} {
		p := healthyLinux()
		p.EUID = tc.euid
		p.ReadFile = linuxFiles(ubuntu, procStatus(tc.caps, fullCaps, "0"))
		p.LookPath = func(file string) (string, error) {
			if strings.HasPrefix(file, "fusermount") && !slices.Contains(tc.found, file) {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		}
		p.Stat = fuseStat(tc.mode, 0)
		r := Run(p, "linux", "artifactfs", "/work")
		if got := result(t, r, "fusermount"); got.Status != tc.want || got.Fix != tc.fix {
			t.Errorf("%s: fusermount check = %+v, want %s with fix %q", tc.name, got, tc.want, tc.fix)
		}
		if r.OK != (tc.want != StatusFail) {
			t.Errorf("%s: report ok = %v with fusermount %s", tc.name, r.OK, tc.want)
		}
	}
}

func TestDevFuseFixes(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat func(string) (fs.FileInfo, error)
	}{
		{name: "missing", stat: func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist }},
		{name: "a regular file", stat: func(string) (fs.FileInfo, error) { return fakeInfo{mode: 0o666}, nil }},
	} {
		p := healthyLinux()
		p.Stat = tc.stat
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got.Status != StatusFail || !strings.HasPrefix(got.Fix, "sudo modprobe fuse") {
			t.Errorf("dev-fuse with /dev/fuse %s = %+v, want the modprobe fix", tc.name, got)
		}
	}
	p := healthyLinux()
	p.ReadWrite = func(string) error { return os.ErrPermission }
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got.Status != StatusFail || got.Fix != "sudo chmod 0666 /dev/fuse" {
		t.Errorf("dev-fuse not writable = %+v, want the chmod fix", got)
	}
	p.EUID = 0
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got.Fix != "chmod 0666 /dev/fuse" {
		t.Errorf("dev-fuse not writable for root = %+v, want the chmod fix without sudo", got)
	}
}

func TestDarwinChecksReportTheirCause(t *testing.T) {
	p := healthyDarwin()
	p.Output = func(string, ...string) (string, error) { return "", errors.New("exit status 2") }
	r := Run(p, "darwin", "apfsclone", "/work")
	if got := statuses(r); got["xcode-select"] != StatusFail || got["git"] != StatusSkip || r.OK {
		t.Errorf("without the Command Line Tools: %v, ok %v; want xcode-select failed and git skipped", got, r.OK)
	}

	// Below macOS 26, FSKitAvailable fails on the version, whatever macFUSE
	// is installed, so its install advice would be wrong.
	p = healthyDarwin()
	p.OSVersion = func() (string, error) { return "15.6", nil }
	p.FSKitAvailable = func() error { return errors.New("FSKit mounts need macOS 26 or later") }
	r = Run(p, "darwin", "artifactfs", "/work")
	if got := result(t, r, "macos-version"); got.Status != StatusFail || got.Fix != "Upgrade macOS: System Settings → General → Software Update" {
		t.Errorf("macOS 15.6 = %+v, want the upgrade fix", got)
	}
	if got := result(t, r, "macfuse-fskit"); got.Status != StatusSkip {
		t.Errorf("macfuse-fskit on macOS 15.6 = %+v, want it skipped", got)
	}

	p = healthyDarwin()
	fskitErr := errors.New("macFUSE FSKit backend is unavailable; install macFUSE 5.5 or later: macFUSE 5.1.3 is installed")
	p.FSKitAvailable = func() error { return fskitErr }
	if got := result(t, Run(p, "darwin", "artifactfs", "/work"), "macfuse-fskit"); got.Status != StatusFail || got.Detail != fskitErr.Error() {
		t.Errorf("FSKit unavailable = %+v, want FSKitAvailable's error verbatim", got)
	}
}

// A directory the probe cannot write, such as the read-only system volume at
// /, needs another directory, not another file system.
func TestAPFSFixFitsTheFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Result
	}{
		{name: "not APFS", err: errors.New("/work is on hfs, not APFS"), want: Result{ID: "apfs", Status: StatusFail, Detail: "/work is on hfs, not APFS", Fix: "make the worktree on an APFS volume, or use --backend artifactfs"}},
		{name: "read-only", err: fmt.Errorf("%w in /: %w", errNotWritable, syscall.EROFS), want: Result{ID: "apfs", Status: StatusFail, Detail: "cannot write a probe file in /: read-only file system", Fix: "use a directory you can write, on the APFS volume that will hold the worktree"}},
	} {
		p := healthyDarwin()
		p.APFS = func(string) error { return tc.err }
		if got := result(t, Run(p, "darwin", "apfsclone", "/work"), "apfs"); got != tc.want {
			t.Errorf("%s: apfs = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Without a directory, the APFS check writes no probe file.
func TestAPFSSkipsWithoutADirectory(t *testing.T) {
	p := healthyDarwin()
	p.APFS = func(dir string) error {
		t.Errorf("APFS probed %q", dir)
		return nil
	}
	r := Run(p, "darwin", "apfsclone", "")
	if got := result(t, r, "apfs"); got.Status != StatusSkip || !r.OK {
		t.Errorf("apfs without a directory = %+v, report ok %v; want a skip", got, r.OK)
	}
}

func TestRequireNamesEachFailedCheck(t *testing.T) {
	mount := Needs{Mount: true, Dir: "/work"}
	p := healthyLinux()
	if err := Require(p, "linux", "artifactfs", "new", mount); err != nil {
		t.Fatalf("Require on a healthy machine: %v", err)
	}
	// A git below the floor only warns.
	p.Output = func(string, ...string) (string, error) { return "git version 2.43.0\n", nil }
	if err := Require(p, "linux", "artifactfs", "new", mount); err != nil {
		t.Fatalf("Require with an old git: %v, want only a warning", err)
	}
	p.ReadWrite = func(string) error { return os.ErrPermission }
	err := Require(p, "linux", "artifactfs", "new", mount)
	want := "slimtree new --backend artifactfs needs:\n  dev-fuse: /dev/fuse is not readable and writable: permission denied; fix: sudo chmod 0666 /dev/fuse"
	if err == nil || err.Error() != want {
		t.Fatalf("Require with an unwritable /dev/fuse = %v, want %q", err, want)
	}
}

// A command that mounts nothing, such as prune, runs where no mount can, and
// one without a directory never probes APFS.
func TestRequireRunsOnlyTheChecksItsCommandNeeds(t *testing.T) {
	p := healthyLinux()
	p.ReadWrite = func(string) error { return os.ErrPermission }
	if err := Require(p, "linux", "artifactfs", "prune", Needs{}); err != nil {
		t.Errorf("Require without a mount on a machine that cannot mount: %v", err)
	}
	p.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := Require(p, "linux", "artifactfs", "prune", Needs{}); err == nil || !strings.Contains(err.Error(), "\n  git: git is not on PATH;") {
		t.Errorf("Require without a mount or git = %v, want the git failure", err)
	}

	d := healthyDarwin()
	probed := ""
	d.APFS = func(dir string) error {
		probed = dir
		return fmt.Errorf("%w in %s: %w", errNotWritable, dir, syscall.EROFS)
	}
	if err := Require(d, "darwin", "apfsclone", "rm", Needs{}); err != nil || probed != "" {
		t.Errorf("Require without a directory = %v, probed %q; want no probe", err, probed)
	}
	err := Require(d, "darwin", "apfsclone", "new", Needs{Dir: "/"})
	want := "slimtree new --backend apfsclone needs:\n  apfs: cannot write a probe file in /: read-only file system; fix: use a directory you can write, on the APFS volume that will hold the worktree"
	if err == nil || err.Error() != want || probed != "/" {
		t.Errorf("Require in / = %v, probed %q; want %q", err, probed, want)
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
	want := `{"version":1,"os":"linux","backend":"artifactfs","ok":true,"checks":[` +
		`{"id":"fusermount","status":"ok","detail":"/usr/bin/fusermount3"},` +
		`{"id":"dev-fuse","status":"ok","detail":"/dev/fuse"},` +
		`{"id":"cap-sys-admin","status":"ok","detail":"CapBnd holds CAP_SYS_ADMIN"},` +
		`{"id":"git","status":"warn","detail":"git version 2.43.0 is older than 2.45","fix":"sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"}],` +
		`"reminders":[]}`
	if data.String() != want {
		t.Fatalf("report JSON =\n%s\nwant\n%s", data.String(), want)
	}
}

// A container without --cap-add SYS_ADMIN cannot mount, whether or not it has
// /dev/fuse, and NoNewPrivs keeps fusermount3 from gaining the capability.
func TestCapSysAdminGatesEveryMount(t *testing.T) {
	const containerFix = "in a container, run it with --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined"
	for _, tc := range []struct {
		name           string
		euid           int
		capEff, capBnd string
		noNewPrivs     string
		want           Status
		fix            string
	}{
		{name: "full bounding set", euid: 1000, capEff: noCaps, capBnd: fullCaps, noNewPrivs: "0", want: StatusOK},
		{name: "docker default", euid: 1000, capEff: noCaps, capBnd: dockerCaps, noNewPrivs: "0", want: StatusFail, fix: containerFix},
		{name: "docker default with SYS_ADMIN", euid: 1000, capEff: noCaps, capBnd: "00000000a82425fb", noNewPrivs: "0", want: StatusOK},
		{name: "no new privileges", euid: 1000, capEff: noCaps, capBnd: fullCaps, noNewPrivs: "1", want: StatusFail, fix: noNewPrivsFix},
		{name: "root with it", euid: 0, capEff: fullCaps, capBnd: dockerCaps, noNewPrivs: "1", want: StatusOK},
		{name: "root regains it through fusermount3", euid: 0, capEff: dockerCaps, capBnd: fullCaps, noNewPrivs: "0", want: StatusOK},
		{name: "root without it", euid: 0, capEff: dockerCaps, capBnd: dockerCaps, noNewPrivs: "0", want: StatusFail, fix: containerFix},
		{name: "root with no new privileges", euid: 0, capEff: dockerCaps, capBnd: fullCaps, noNewPrivs: "1", want: StatusFail, fix: noNewPrivsFix},
	} {
		p := healthyLinux()
		p.EUID = tc.euid
		p.ReadFile = linuxFiles(ubuntu, procStatus(tc.capEff, tc.capBnd, tc.noNewPrivs))
		r := Run(p, "linux", "artifactfs", "/work")
		got := result(t, r, "cap-sys-admin")
		if got.Status != tc.want || got.Fix != tc.fix || r.OK != (tc.want == StatusOK) {
			t.Errorf("%s: cap-sys-admin = %+v, report ok %v, want %s with fix %q", tc.name, got, r.OK, tc.want, tc.fix)
		}
	}
}

// A setuid fusermount3 that root does not own, such as Homebrew's, runs as
// its owner and cannot mount.
func TestFusermountMustBeRootOwned(t *testing.T) {
	const brew = "/home/linuxbrew/.linuxbrew/bin/fusermount3"
	for _, tc := range []struct {
		name   string
		path   string
		distro bool
		want   Result
	}{
		{name: "Homebrew's over the distribution's", path: brew, distro: true, want: Result{Detail: brew + " is not owned by root, so it cannot mount, and it shadows /usr/bin/fusermount3", Fix: "brew unlink libfuse"}},
		{name: "Homebrew's alone", path: brew, want: Result{Detail: brew + " is not owned by root, so it cannot mount", Fix: "sudo apt-get update && sudo apt-get install -y fuse3 (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04), then brew unlink libfuse"}},
		{name: "another over the distribution's", path: "/opt/fuse/bin/fusermount3", distro: true, want: Result{Detail: "/opt/fuse/bin/fusermount3 is not owned by root, so it cannot mount, and it shadows /usr/bin/fusermount3", Fix: "put /usr/bin before /opt/fuse/bin on PATH"}},
		{name: "the distribution's", path: "/usr/bin/fusermount3", distro: true, want: Result{Detail: "/usr/bin/fusermount3 is not owned by root, so it cannot mount", Fix: "sudo chown root:root /usr/bin/fusermount3 && sudo chmod u+s /usr/bin/fusermount3"}},
	} {
		p := healthyLinux()
		p.LookPath = func(file string) (string, error) {
			if file == "fusermount3" {
				return tc.path, nil
			}
			return "/usr/bin/" + file, nil
		}
		p.Stat = func(path string) (fs.FileInfo, error) {
			switch {
			case path == "/dev/fuse":
				return fakeInfo{mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil
			case path == tc.path:
				return fakeInfo{mode: fs.ModeSetuid | 0o755, uid: 1000}, nil
			case path == "/usr/bin/fusermount3" && tc.distro:
				return fakeInfo{mode: fs.ModeSetuid | 0o755}, nil
			}
			return nil, os.ErrNotExist
		}
		tc.want.ID, tc.want.Status = "fusermount", StatusFail
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "fusermount"); got != tc.want {
			t.Errorf("%s: fusermount = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
