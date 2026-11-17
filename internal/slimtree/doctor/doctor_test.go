package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	// ino identifies the file to sameFile.
	ino uint64
}

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Ino: f.ino} }

const (
	// fullCaps is the capability mask of a process with every capability.
	fullCaps = "000001ffffffffff"
	noCaps   = "0000000000000000"
	// dockerCaps is Docker's default bounding set, which lacks CAP_SYS_ADMIN.
	dockerCaps = "00000000a80425fb"
	ubuntu     = "ID=ubuntu\nID_LIKE=debian\n"
)

// procStatus is a /proc/self/status with the given capability sets and
// NoNewPrivs. An empty noNewPrivs leaves the field out, as kernels before
// 4.10 do.
func procStatus(capEff, capBnd, noNewPrivs string) string {
	status := "Name:\tslimtree\nCapEff:\t" + capEff + "\nCapBnd:\t" + capBnd + "\n"
	if noNewPrivs != "" {
		status += "NoNewPrivs:\t" + noNewPrivs + "\n"
	}
	return status
}

// withFile serves content at path and every other path from read.
func withFile(read func(string) ([]byte, error), path, content string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if p == path {
			return []byte(content), nil
		}
		return read(p)
	}
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
		MountIDs:  func() (bool, error) { return true, nil },
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
		{goos: "linux", backend: "artifactfs", probe: healthyLinux(), want: map[string]Status{"fusermount": StatusOK, "dev-fuse": StatusOK, "cap-sys-admin": StatusOK, "apparmor": StatusOK, "mount-id": StatusOK, "git": StatusOK}},
		{goos: "darwin", backend: "apfsclone", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "apfs": StatusOK}},
		{goos: "darwin", backend: "artifactfs", probe: healthyDarwin(), want: map[string]Status{"xcode-select": StatusOK, "git": StatusOK, "macos-version": StatusOK, "macfuse-fskit": StatusOK}, reminders: 2},
	} {
		r := Run(tc.probe, tc.goos, tc.backend, "/work")
		if got := statuses(r); !reflect.DeepEqual(got, tc.want) || !r.OK || len(r.Reminders) != tc.reminders {
			t.Errorf("%s/%s: checks %v, ok %v, %d reminders; want %v, ok, %d reminders", tc.goos, tc.backend, got, r.OK, len(r.Reminders), tc.want, tc.reminders)
		}
	}
}

// mount-id fails before Linux 5.8, which reports no mount ID, and warns
// before 6.8, whose mount IDs a later mount can reuse.
func TestMountIDCheck(t *testing.T) {
	for _, tc := range []struct {
		unique bool
		err    error
		want   Result
	}{
		{unique: true, want: Result{Status: StatusOK, Detail: "unique mount IDs"}},
		{want: Result{Status: StatusWarn, Detail: "Linux before 6.8 reuses mount IDs, so slimtree tells a mount from a later one at its path by its ID, device and source", Fix: "upgrade to Linux 6.8 or later"}},
		{err: errors.New("statx returned no mount ID: slimtree needs Linux 5.8 or later"), want: Result{Status: StatusFail, Detail: "statx returned no mount ID: slimtree needs Linux 5.8 or later", Fix: "upgrade to Linux 6.8 or later"}},
	} {
		p := healthyLinux()
		p.MountIDs = func() (bool, error) { return tc.unique, tc.err }
		r := Run(p, "linux", "artifactfs", "")
		tc.want.ID = "mount-id"
		if got := result(t, r, "mount-id"); got != tc.want || r.OK != (tc.want.Status != StatusFail) {
			t.Errorf("mount-id = %+v, report ok %v; want %+v", got, r.OK, tc.want)
		}
	}
}

// Without fusermount, the fix installs fuse3 with the package manager that
// os-release names, read from /usr/lib/os-release when /etc/os-release is
// missing.
func TestInstallCommandFromOSRelease(t *testing.T) {
	for _, tc := range []struct{ etc, usrLib, want string }{
		{etc: ubuntu, want: "sudo apt-get update && sudo apt-get install fuse3"},
		{etc: "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", want: "sudo apt-get update && sudo apt-get install fuse3"},
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

// A fix is a command to paste, so a note on what it does goes in the detail.
// apt-get asks before it removes FUSE 2's fuse package.
func TestFuse3InstallNoteIsInTheDetail(t *testing.T) {
	p := healthyLinux()
	p.LookPath = func(file string) (string, error) {
		if strings.HasPrefix(file, "fusermount") {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}
	want := Result{ID: "fusermount", Status: StatusFail, Detail: "neither fusermount3 nor fusermount is on PATH, and the fuse library unmounts only through it; apt-get asks before it removes FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04", Fix: "sudo apt-get update && sudo apt-get install fuse3"}
	if got := result(t, Run(p, "linux", "artifactfs", "/work"), "fusermount"); got != want {
		t.Errorf("fusermount on Ubuntu without fuse3 = %+v, want %+v", got, want)
	}
}

// An old git's fix upgrades it: through the git-core PPA on Ubuntu and its
// derivatives, and elsewhere with the package manager's upgrade. The detail
// says what the upgrade does.
func TestGitUpgradeFixPerDistribution(t *testing.T) {
	const ppa = "sudo apt-get update && sudo apt-get install -y software-properties-common && sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"
	const old = "git version 2.39.5 is older than 2.45"
	const stays = old + "; if git stays older than 2.45, your release ships no newer git"
	for _, tc := range []struct{ osRelease, fix, detail string }{
		{ubuntu, ppa, old},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", ppa, old},
		{"ID=debian\n", "sudo apt-get update && sudo apt-get install -y --only-upgrade git", stays},
		{"ID=fedora\n", "sudo dnf upgrade -y git", stays},
		{"ID=arch\n", "sudo pacman -Syu git", old + "; pacman -Syu upgrades the whole system"},
		{"ID=alpine\n", "sudo apk add -U --upgrade git", stays},
		{"ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n", "sudo zypper update -y git", stays},
		{"ID=nixos\n", "upgrade git to 2.45 or later with your distribution's package manager", old},
	} {
		p := healthyLinux()
		p.ReadFile = linuxFiles(tc.osRelease, procStatus(noCaps, fullCaps, "0"))
		p.Output = func(string, ...string) (string, error) { return "git version 2.39.5\n", nil }
		want := Result{ID: "git", Status: StatusWarn, Detail: tc.detail, Fix: tc.fix}
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "git"); got != want {
			t.Errorf("old git on %q = %+v, want %+v", tc.osRelease, got, want)
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
		{name: "not setuid with CapEff", mode: 0o755, found: []string{"fusermount3"}, euid: 1000, caps: fullCaps, want: StatusWarn, fix: "sudo chmod u+s /usr/bin/fusermount3"},
		{name: "missing", euid: 1000, caps: noCaps, want: StatusFail, fix: "sudo apt-get update && sudo apt-get install fuse3"},
		{name: "not setuid as root", mode: 0o755, found: []string{"fusermount3"}, euid: 0, caps: noCaps, want: StatusOK},
		// The fuse library unmounts only through fusermount.
		{name: "missing as root", euid: 0, caps: fullCaps, want: StatusFail, fix: "apt-get update && apt-get install fuse3"},
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
		want := Result{ID: "dev-fuse", Status: StatusFail, Detail: "/dev/fuse is not a character device; in a container, start it with --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined", Fix: "sudo modprobe fuse"}
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "dev-fuse"); got != want {
			t.Errorf("dev-fuse with /dev/fuse %s = %+v, want %+v", tc.name, got, want)
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
		`{"id":"apparmor","status":"ok","detail":"no AppArmor profile"},` +
		`{"id":"mount-id","status":"ok","detail":"unique mount IDs"},` +
		`{"id":"git","status":"warn","detail":"git version 2.43.0 is older than 2.45","fix":"sudo apt-get update && sudo apt-get install -y software-properties-common && sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"}],` +
		`"reminders":[]}`
	if data.String() != want {
		t.Fatalf("report JSON =\n%s\nwant\n%s", data.String(), want)
	}
}

// A container without --cap-add SYS_ADMIN cannot mount, whether or not it has
// /dev/fuse. NoNewPrivs keeps fusermount3 from gaining the capability, and so
// does a user namespace that does not map fusermount3's owner.
func TestCapSysAdminGatesEveryMount(t *testing.T) {
	const containerFix = "in a container, run it with --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined"
	for _, tc := range []struct {
		name           string
		euid           int
		capEff, capBnd string
		noNewPrivs     string
		// unmapped makes fusermount3's owner the overflow uid.
		unmapped bool
		want     Status
		fix      string
	}{
		{name: "full bounding set", euid: 1000, capEff: noCaps, capBnd: fullCaps, noNewPrivs: "0", want: StatusOK},
		{name: "kernel without NoNewPrivs", euid: 1000, capEff: noCaps, capBnd: fullCaps, want: StatusOK},
		{name: "docker default", euid: 1000, capEff: noCaps, capBnd: dockerCaps, noNewPrivs: "0", want: StatusFail, fix: containerFix},
		{name: "docker default with SYS_ADMIN", euid: 1000, capEff: noCaps, capBnd: "00000000a82425fb", noNewPrivs: "0", want: StatusOK},
		{name: "no new privileges", euid: 1000, capEff: noCaps, capBnd: fullCaps, noNewPrivs: "1", want: StatusFail, fix: noNewPrivsFix},
		{name: "unmapped fusermount3 owner", euid: 1000, capEff: noCaps, capBnd: fullCaps, noNewPrivs: "0", unmapped: true, want: StatusFail, fix: outsideUserNamespace},
		{name: "root with it", euid: 0, capEff: fullCaps, capBnd: dockerCaps, noNewPrivs: "1", want: StatusOK},
		{name: "root regains it through an unmapped fusermount3", euid: 0, capEff: dockerCaps, capBnd: fullCaps, noNewPrivs: "0", unmapped: true, want: StatusOK},
		{name: "root regains it through fusermount3", euid: 0, capEff: dockerCaps, capBnd: fullCaps, noNewPrivs: "0", want: StatusOK},
		{name: "root without it", euid: 0, capEff: dockerCaps, capBnd: dockerCaps, noNewPrivs: "0", want: StatusFail, fix: containerFix},
		{name: "root with no new privileges", euid: 0, capEff: dockerCaps, capBnd: fullCaps, noNewPrivs: "1", want: StatusFail, fix: noNewPrivsFix},
	} {
		p := healthyLinux()
		p.EUID = tc.euid
		p.ReadFile = linuxFiles(ubuntu, procStatus(tc.capEff, tc.capBnd, tc.noNewPrivs))
		if tc.unmapped {
			p.ReadFile = withFile(p.ReadFile, "/proc/sys/kernel/overflowuid", "65534\n")
			p.Stat = fuseStat(fs.ModeSetuid|0o755, 65534)
		}
		r := Run(p, "linux", "artifactfs", "/work")
		got := result(t, r, "cap-sys-admin")
		if got.Status != tc.want || got.Fix != tc.fix || (got.Status == StatusFail) == r.OK {
			t.Errorf("%s: cap-sys-admin = %+v, report ok %v, want %s with fix %q", tc.name, got, r.OK, tc.want, tc.fix)
		}
	}
}

// A setuid fusermount3 that root does not own, such as Homebrew's, runs as
// its owner and cannot mount.
func TestFusermountMustBeRootOwned(t *testing.T) {
	const brew = "/home/linuxbrew/.linuxbrew/bin/fusermount3"
	const reinstall = "sudo apt-get update && sudo apt-get install -y --reinstall fuse3"
	type distro int
	const (
		noDistro    distro = iota
		otherDistro        // /usr/bin/fusermount3 is another, root-owned file
		sameDistro         // /usr/bin/fusermount3 is the file found on PATH
	)
	for _, tc := range []struct {
		name      string
		path      string
		owner     uint32
		distro    distro
		osRelease string
		want      Result
	}{
		{name: "Homebrew's over the distribution's", path: brew, distro: otherDistro, want: Result{Detail: brew + " is not owned by root, so it cannot mount, and it shadows /usr/bin/fusermount3", Fix: "brew unlink libfuse"}},
		{name: "Homebrew's alone", path: brew, want: Result{Detail: brew + " is not owned by root, so it cannot mount; apt-get asks before it removes FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04; then brew unlink libfuse", Fix: "sudo apt-get update && sudo apt-get install fuse3"}},
		{name: "another over the distribution's", path: "/opt/fuse/bin/fusermount3", distro: otherDistro, want: Result{Detail: "/opt/fuse/bin/fusermount3 is not owned by root, so it cannot mount, and it shadows /usr/bin/fusermount3", Fix: "put /usr/bin before /opt/fuse/bin on PATH"}},
		// Its owner can have rewritten it, so the fix reinstalls rather than
		// making it setuid root.
		{name: "the distribution's", path: "/usr/bin/fusermount3", distro: sameDistro, want: Result{Detail: "/usr/bin/fusermount3 is not owned by root, so it cannot mount", Fix: reinstall}},
		{name: "the distribution's on Fedora", path: "/usr/bin/fusermount3", distro: sameDistro, osRelease: "ID=fedora\n", want: Result{Detail: "/usr/bin/fusermount3 is not owned by root, so it cannot mount", Fix: "sudo dnf reinstall -y fuse3"}},
		// /bin links to usr/bin, so both paths name one file.
		{name: "the distribution's through /bin", path: "/bin/fusermount3", distro: sameDistro, want: Result{Detail: "/bin/fusermount3 is not owned by root, so it cannot mount", Fix: reinstall}},
		{name: "FUSE 2's", path: "/usr/bin/fusermount", distro: sameDistro, want: Result{Detail: "/usr/bin/fusermount is not owned by root, so it cannot mount", Fix: "reinstall the package that ships /usr/bin/fusermount with your distribution's package manager"}},
		{name: "in a user namespace", path: "/usr/bin/fusermount3", owner: 65534, distro: sameDistro, want: Result{Detail: "/usr/bin/fusermount3's owner has no mapping in this user namespace, so the kernel ignores its setuid bit", Fix: outsideUserNamespace}},
	} {
		p := healthyLinux()
		if tc.osRelease != "" {
			p.ReadFile = linuxFiles(tc.osRelease, procStatus(noCaps, fullCaps, "0"))
		}
		p.ReadFile = withFile(p.ReadFile, "/proc/sys/kernel/overflowuid", "65534\n")
		owner := tc.owner
		if owner == 0 {
			owner = 1000
		}
		p.LookPath = func(file string) (string, error) {
			switch {
			case file == filepath.Base(tc.path):
				return tc.path, nil
			case strings.HasPrefix(file, "fusermount"):
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		}
		p.Stat = func(path string) (fs.FileInfo, error) {
			switch {
			case path == "/dev/fuse":
				return fakeInfo{mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil
			case path == tc.path, path == "/usr/bin/fusermount3" && tc.distro == sameDistro:
				return fakeInfo{mode: fs.ModeSetuid | 0o755, uid: owner, ino: 1}, nil
			case path == "/usr/bin/fusermount3" && tc.distro == otherDistro:
				return fakeInfo{mode: fs.ModeSetuid | 0o755, ino: 2}, nil
			}
			return nil, os.ErrNotExist
		}
		tc.want.ID, tc.want.Status = "fusermount", StatusFail
		if got := result(t, Run(p, "linux", "artifactfs", "/work"), "fusermount"); got != tc.want {
			t.Errorf("%s: fusermount = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Docker's default AppArmor profile denies mount, so a mount fails even with
// CAP_SYS_ADMIN. Another enforcing profile can allow it.
func TestAppArmorProfileCanDenyMount(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  Result
	}{
		{"unconfined\n", Result{Status: StatusOK, Detail: "unconfined"}},
		{"docker-default (enforce)\n", Result{Status: StatusFail, Detail: "Docker's default AppArmor profile denies mount", Fix: "start the container with --security-opt apparmor=unconfined"}},
		{"docker-default (complain)\n", Result{Status: StatusOK, Detail: "docker-default (complain)"}},
		{"lxc-container-default-cgns (enforce)\n", Result{Status: StatusWarn, Detail: "AppArmor enforces the lxc-container-default-cgns profile, which can deny mount", Fix: "allow mount in the lxc-container-default-cgns profile, or run slimtree unconfined"}},
		{"system_u:system_r:container_t:s0:c1,c2\x00", Result{Status: StatusOK, Detail: "system_u:system_r:container_t:s0:c1,c2"}},
	} {
		p := healthyLinux()
		p.ReadFile = withFile(p.ReadFile, "/proc/self/attr/current", tc.label)
		r := Run(p, "linux", "artifactfs", "/work")
		tc.want.ID = "apparmor"
		if got := result(t, r, "apparmor"); got != tc.want || r.OK != (tc.want.Status != StatusFail) {
			t.Errorf("label %q: apparmor = %+v, report ok %v, want %+v", tc.label, got, r.OK, tc.want)
		}
	}
}
