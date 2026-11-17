// Package doctor checks the machine for what each slimtree backend needs. It
// never installs anything: each failed check prints the exact command or
// setting that fixes it.
package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Status is a check's outcome. Only StatusFail fails doctor and Require.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	// StatusSkip marks a check that would have run a prompt or an earlier
	// failure makes moot.
	StatusSkip Status = "skip"
)

// Result is one check's outcome.
type Result struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Report is what doctor prints, as text or as JSON.
type Report struct {
	OS        string   `json:"os"`
	Backend   string   `json:"backend"`
	OK        bool     `json:"ok"`
	Checks    []Result `json:"checks"`
	Reminders []string `json:"reminders,omitempty"`
}

// Probe reads the machine. System returns the real one; tests inject each
// field.
type Probe struct {
	LookPath func(file string) (string, error)
	Stat     func(path string) (fs.FileInfo, error)
	// ReadWrite reports whether the process can read and write path.
	ReadWrite func(path string) error
	ReadFile  func(path string) ([]byte, error)
	// Output runs a command and returns its standard output.
	Output    func(name string, args ...string) (string, error)
	EUID      int
	OSVersion func() (string, error)
	// FSKitAvailable is fuse.FSKitAvailable from the fuse fork.
	FSKitAvailable func() error
	// APFS checks that dir is on APFS and that a file there can be cloned.
	APFS func(dir string) error
}

const (
	minGit   = "2.45"
	minMacOS = 26
)

// check is one row of the table: the platform and backends it applies to.
// A nil backends applies to every backend.
type check struct {
	id       string
	goos     string
	backends []string
	run      func(p Probe, dir string, earlier []Result) Result
}

var checks = []check{
	{id: "fusermount3", goos: "linux", run: checkFusermount},
	{id: "dev-fuse", goos: "linux", run: checkDevFuse},
	{id: "git", goos: "linux", run: checkLinuxGit},
	{id: "xcode-select", goos: "darwin", run: checkXcodeSelect},
	{id: "git", goos: "darwin", run: checkDarwinGit},
	{id: "apfs", goos: "darwin", backends: []string{"apfsclone"}, run: checkAPFS},
	{id: "macos-version", goos: "darwin", backends: []string{"artifactfs"}, run: checkMacOSVersion},
	{id: "macfuse-fskit", goos: "darwin", backends: []string{"artifactfs"}, run: checkFSKit},
}

// reminders are the settings no check can read.
var reminders = map[string][]string{
	"darwin/artifactfs": {
		`Enable "macFUSE" and "macFUSE (local)" under System Settings → General → Login Items & Extensions → File System Extensions.`,
		"Allow Network Volumes for your terminal app under System Settings → Privacy & Security → Files & Folders.",
	},
}

// Run runs every check for goos and backend. dir is the directory a worktree
// would be made in.
func Run(p Probe, goos, backend, dir string) Report {
	r := Report{OS: goos, Backend: backend, OK: true, Checks: []Result{}, Reminders: reminders[goos+"/"+backend]}
	for _, c := range checks {
		if c.goos != goos || c.backends != nil && !slices.Contains(c.backends, backend) {
			continue
		}
		res := c.run(p, dir, r.Checks)
		res.ID = c.id
		r.Checks = append(r.Checks, res)
		if res.Status == StatusFail {
			r.OK = false
		}
	}
	return r
}

// Require runs the checks for goos and backend, and returns an error that
// names each failed check and its fix. Call it before cmd changes anything.
func Require(p Probe, goos, backend, cmd, dir string) error {
	var failed []string
	for _, res := range Run(p, goos, backend, dir).Checks {
		if res.Status == StatusFail {
			failed = append(failed, fmt.Sprintf("%s: %s; fix: %s", res.ID, res.Detail, res.Fix))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("slimtree %s --backend %s needs:\n  %s", cmd, backend, strings.Join(failed, "\n  "))
}

func checkFusermount(p Probe, _ string, _ []Result) Result {
	fail := StatusFail
	// Root mounts through /dev/fuse without fusermount3.
	if p.EUID == 0 {
		fail = StatusWarn
	}
	path, err := p.LookPath("fusermount3")
	if err != nil {
		return Result{Status: fail, Detail: "fusermount3 is not on PATH", Fix: installCommand(osRelease(p), "fuse3")}
	}
	info, err := p.Stat(path)
	if err != nil {
		return Result{Status: fail, Detail: err.Error(), Fix: installCommand(osRelease(p), "fuse3")}
	}
	if info.Mode()&fs.ModeSetuid == 0 {
		return Result{Status: fail, Detail: path + " is not setuid", Fix: "sudo chmod u+s " + path}
	}
	return Result{Status: StatusOK, Detail: path}
}

func checkDevFuse(p Probe, _ string, _ []Result) Result {
	const dev = "/dev/fuse"
	info, err := p.Stat(dev)
	if err != nil || info.Mode()&fs.ModeCharDevice == 0 {
		return Result{Status: StatusFail, Detail: dev + " is not a character device", Fix: "sudo modprobe fuse, or in a container run with --device /dev/fuse --cap-add SYS_ADMIN"}
	}
	if err := p.ReadWrite(dev); err != nil {
		return Result{Status: StatusFail, Detail: dev + " is not readable and writable: " + err.Error(), Fix: "sudo chmod 0666 /dev/fuse"}
	}
	return Result{Status: StatusOK, Detail: dev}
}

func checkLinuxGit(p Probe, _ string, _ []Result) Result {
	release := osRelease(p)
	upgrade := installCommand(release, "git")
	if isUbuntu(release) {
		upgrade = "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"
	}
	return gitResult(p, installCommand(release, "git"), upgrade)
}

func checkXcodeSelect(p Probe, _ string, _ []Result) Result {
	path, err := p.Output("xcode-select", "-p")
	if err != nil {
		return Result{Status: StatusFail, Detail: "no Command Line Tools", Fix: "xcode-select --install"}
	}
	return Result{Status: StatusOK, Detail: strings.TrimSpace(path)}
}

func checkDarwinGit(p Probe, _ string, earlier []Result) Result {
	// Without the Command Line Tools, /usr/bin/git opens their install dialog.
	for _, r := range earlier {
		if r.ID == "xcode-select" && r.Status != StatusOK {
			return Result{Status: StatusSkip, Detail: "needs the Command Line Tools first"}
		}
	}
	return gitResult(p, "xcode-select --install", "brew install git")
}

func gitResult(p Probe, install, upgrade string) Result {
	if _, err := p.LookPath("git"); err != nil {
		return Result{Status: StatusFail, Detail: "git is not on PATH", Fix: install}
	}
	out, err := p.Output("git", "version")
	if err != nil {
		return Result{Status: StatusFail, Detail: "git version: " + err.Error(), Fix: install}
	}
	out = strings.TrimSpace(out)
	if !atLeast(gitVersion(out), minGit) {
		return Result{Status: StatusWarn, Detail: out + " is older than " + minGit, Fix: upgrade}
	}
	return Result{Status: StatusOK, Detail: out}
}

func checkAPFS(p Probe, dir string, _ []Result) Result {
	if err := p.APFS(dir); err != nil {
		return Result{Status: StatusFail, Detail: err.Error(), Fix: "make the worktree on an APFS volume, or use --backend artifactfs"}
	}
	return Result{Status: StatusOK, Detail: dir}
}

func checkMacOSVersion(p Probe, _ string, _ []Result) Result {
	v, err := p.OSVersion()
	if err != nil {
		return Result{Status: StatusFail, Detail: err.Error(), Fix: "Upgrade macOS: System Settings → General → Software Update"}
	}
	if major, _, _ := strings.Cut(v, "."); !atLeast(major, strconv.Itoa(minMacOS)) {
		return Result{Status: StatusFail, Detail: "macOS " + v + " has no FSKit mounts for macFUSE", Fix: "Upgrade macOS: System Settings → General → Software Update"}
	}
	return Result{Status: StatusOK, Detail: "macOS " + v}
}

func checkFSKit(p Probe, _ string, _ []Result) Result {
	if err := p.FSKitAvailable(); err != nil {
		return Result{Status: StatusFail, Detail: err.Error(), Fix: "install macFUSE 5.5 or later from https://github.com/macfuse/macfuse/releases/latest (needs an admin)"}
	}
	return Result{Status: StatusOK, Detail: "macFUSE FSKit backend loads"}
}

var gitVersionPattern = regexp.MustCompile(`^git version (\d+\.\d+(?:\.\d+)?)`)

// gitVersion returns the version in git version's output, or "".
func gitVersion(out string) string {
	m := gitVersionPattern.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// atLeast compares dotted numeric versions. An unparsable version is never
// at least min.
func atLeast(version, min string) bool {
	have, want := strings.Split(version, "."), strings.Split(min, ".")
	for i, w := range want {
		wn, _ := strconv.Atoi(w)
		if i >= len(have) {
			return wn == 0
		}
		hn, err := strconv.Atoi(have[i])
		if err != nil {
			return false
		}
		if hn != wn {
			return hn > wn
		}
	}
	return true
}

func osRelease(p Probe) map[string]string {
	data, err := p.ReadFile("/etc/os-release")
	if errors.Is(err, fs.ErrNotExist) {
		data, err = p.ReadFile("/usr/lib/os-release")
	}
	if err != nil {
		return nil
	}
	return parseOSRelease(string(data))
}

func parseOSRelease(text string) map[string]string {
	fields := map[string]string{}
	for line := range strings.Lines(text) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[k] = strings.Trim(v, `"'`)
		}
	}
	return fields
}

func isUbuntu(release map[string]string) bool {
	return release["ID"] == "ubuntu" || slices.Contains(strings.Fields(release["ID_LIKE"]), "ubuntu")
}

// installCommand returns the command that installs pkg on the distribution
// os-release describes.
func installCommand(release map[string]string, pkg string) string {
	ids := append([]string{release["ID"]}, strings.Fields(release["ID_LIKE"])...)
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "sudo apt-get install -y " + pkg
		case "fedora", "rhel", "centos":
			return "sudo dnf install -y " + pkg
		case "arch":
			return "sudo pacman -S --needed " + pkg
		case "alpine":
			return "sudo apk add " + pkg
		case "suse", "opensuse":
			return "sudo zypper install -y " + pkg
		}
	}
	return "install " + pkg + " with your distribution's package manager"
}
