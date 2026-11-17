// Package doctor checks the machine for what each slimtree backend needs. It
// never installs anything: each failed check prints the exact command or
// setting that fixes it.
package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	// Version is the JSON format's version. It rises when a field is renamed
	// or removed or changes its meaning.
	Version   int      `json:"version"`
	OS        string   `json:"os"`
	Backend   string   `json:"backend"`
	OK        bool     `json:"ok"`
	Checks    []Result `json:"checks"`
	Reminders []string `json:"reminders"`
}

const reportVersion = 1

// WriteJSON writes r as indented JSON and leaves &, < and > unescaped, so a
// fix reads as the command to run.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Probe reads the machine. System returns the real one; tests inject each
// field.
type Probe struct {
	LookPath func(file string) (string, error)
	Stat     func(path string) (fs.FileInfo, error)
	// ReadWrite opens path for reading and writing, then closes it.
	ReadWrite func(path string) error
	ReadFile  func(path string) ([]byte, error)
	// Output runs a command and returns its standard output. A failed
	// command's error carries its standard error.
	Output    func(name string, args ...string) (string, error)
	EUID      int
	OSVersion func() (string, error)
	// FSKitAvailable is fuse.FSKitAvailable from the fuse fork.
	FSKitAvailable func() error
	// APFS checks that dir is on APFS and that a file there can be cloned. It
	// wraps errNotWritable when it cannot write its probe file in dir.
	APFS func(dir string) error
}

// Needs is what a command does, which decides the checks Require runs. Every
// command runs git, so the git checks always run.
type Needs struct {
	// Mount is set when the command mounts or unmounts an artifactfs
	// worktree. Without it, Require skips the checks that guard a mount, so
	// a command that only cleans up runs where no mount can.
	Mount bool
	// Dir is an existing directory on the volume that will hold a new
	// apfsclone worktree. The APFS check writes and clones a probe file in
	// it, and skips when Dir is "".
	Dir string
}

// errNotWritable marks an APFS probe that could not write its probe file,
// such as in the read-only system volume at /.
var errNotWritable = errors.New("cannot write a probe file")

const (
	minGit   = "2.45"
	minMacOS = 26
)

// check is one row of the table: the platform and backends it applies to.
// A nil backends applies to every backend. mount marks a check that guards a
// mount.
type check struct {
	id       string
	goos     string
	backends []string
	mount    bool
	run      func(p Probe, dir string, earlier []Result) Result
}

var checks = []check{
	{id: "fusermount", goos: "linux", mount: true, run: checkFusermount},
	{id: "dev-fuse", goos: "linux", mount: true, run: checkDevFuse},
	{id: "cap-sys-admin", goos: "linux", mount: true, run: checkCapSysAdmin},
	{id: "git", goos: "linux", run: checkLinuxGit},
	{id: "xcode-select", goos: "darwin", run: checkXcodeSelect},
	{id: "git", goos: "darwin", run: checkDarwinGit},
	{id: "apfs", goos: "darwin", backends: []string{"apfsclone"}, run: checkAPFS},
	{id: "macos-version", goos: "darwin", backends: []string{"artifactfs"}, mount: true, run: checkMacOSVersion},
	{id: "macfuse-fskit", goos: "darwin", backends: []string{"artifactfs"}, mount: true, run: checkFSKit},
}

// reminders are the settings no check can read.
var reminders = map[string][]string{
	"darwin/artifactfs": {
		`Enable "macFUSE" and "macFUSE (local)" under System Settings → General → Login Items & Extensions → File System Extensions.`,
		"Allow Network Volumes for your terminal app under System Settings → Privacy & Security → Files & Folders.",
	},
}

// Run runs every check for goos and backend. The APFS check probes dir, and
// skips when dir is "", because its probe file shows in dir while it runs.
func Run(p Probe, goos, backend, dir string) Report {
	return report(p, goos, backend, Needs{Mount: true, Dir: dir})
}

// Require runs the checks for goos and backend that guard what cmd needs,
// and returns an error that names each failed check and its fix. Call it
// before cmd changes anything.
func Require(p Probe, goos, backend, cmd string, needs Needs) error {
	var failed []string
	for _, res := range report(p, goos, backend, needs).Checks {
		if res.Status == StatusFail {
			failed = append(failed, fmt.Sprintf("%s: %s; fix: %s", res.ID, res.Detail, res.Fix))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("slimtree %s --backend %s needs:\n  %s", cmd, backend, strings.Join(failed, "\n  "))
}

func report(p Probe, goos, backend string, needs Needs) Report {
	r := Report{Version: reportVersion, OS: goos, Backend: backend, OK: true, Checks: []Result{}, Reminders: append([]string{}, reminders[goos+"/"+backend]...)}
	for _, c := range checks {
		if c.goos != goos || c.backends != nil && !slices.Contains(c.backends, backend) || c.mount && !needs.Mount {
			continue
		}
		res := c.run(p, needs.Dir, r.Checks)
		res.ID = c.id
		// Root needs no sudo, and a minimal image has none.
		if p.EUID == 0 {
			res.Fix = strings.ReplaceAll(res.Fix, "sudo ", "")
		}
		r.Checks = append(r.Checks, res)
		if res.Status == StatusFail {
			r.OK = false
		}
	}
	return r
}

// checkFusermount finds the fusermount the fuse library runs: fusermount3,
// else fusermount. The library runs it only when its own mount(2) fails, so
// a process whose CapEff holds CAP_SYS_ADMIN only warns.
func checkFusermount(p Probe, _ string, _ []Result) Result {
	fail := StatusFail
	if priv, err := readPrivileges(p); err == nil && priv.effective || err != nil && p.EUID == 0 {
		fail = StatusWarn
	}
	path, err := p.LookPath("fusermount3")
	if err != nil {
		path, err = p.LookPath("fusermount")
	}
	if err != nil {
		return Result{Status: fail, Detail: "neither fusermount3 nor fusermount is on PATH", Fix: installFuse3(p)}
	}
	// Root runs it with CapBnd's capabilities, whatever its owner and mode.
	if p.EUID == 0 {
		return Result{Status: StatusOK, Detail: path}
	}
	info, err := p.Stat(path)
	if err != nil {
		return Result{Status: fail, Detail: err.Error(), Fix: installFuse3(p)}
	}
	if uid, ok := ownerUID(info); !ok || uid != 0 {
		return notRootOwned(p, path, fail)
	}
	if info.Mode()&fs.ModeSetuid == 0 {
		return Result{Status: fail, Detail: path + " is not setuid", Fix: "sudo chmod u+s " + path}
	}
	return Result{Status: StatusOK, Detail: path}
}

// notRootOwned explains a fusermount that root does not own, such as
// Homebrew's. Its setuid bit runs it as its owner, who cannot mount.
func notRootOwned(p Probe, path string, status Status) Result {
	detail := path + " is not owned by root, so it cannot mount"
	distro := "/usr/bin/" + filepath.Base(path)
	if path == distro {
		return Result{Status: status, Detail: detail, Fix: "sudo chown root:root " + path + " && sudo chmod u+s " + path}
	}
	_, err := p.Stat(distro)
	shadows := err == nil
	if shadows {
		detail += ", and it shadows " + distro
	}
	uncover := "put /usr/bin before " + filepath.Dir(path) + " on PATH"
	// Unlinking the formula uncovers /usr/bin's binary without hiding every
	// other Homebrew tool that /usr/bin duplicates.
	if strings.Contains(path, "/.linuxbrew/") {
		formula := "libfuse"
		if filepath.Base(path) == "fusermount" {
			formula = "libfuse@2"
		}
		uncover = "brew unlink " + formula
	}
	if !shadows {
		uncover = installFuse3(p) + ", then " + uncover
	}
	return Result{Status: status, Detail: detail, Fix: uncover}
}

func checkDevFuse(p Probe, _ string, _ []Result) Result {
	const dev = "/dev/fuse"
	info, err := p.Stat(dev)
	if err != nil || info.Mode()&fs.ModeCharDevice == 0 {
		return Result{Status: StatusFail, Detail: dev + " is not a character device", Fix: "sudo modprobe fuse, or in a container run with " + containerFlags}
	}
	if err := p.ReadWrite(dev); err != nil {
		return Result{Status: StatusFail, Detail: dev + " is not readable and writable: " + err.Error(), Fix: "sudo chmod 0666 /dev/fuse"}
	}
	return Result{Status: StatusOK, Detail: dev}
}

// containerFlags let a container mount: the device, the capability every FUSE
// mount needs, and no AppArmor profile, since Docker's default denies mount.
const containerFlags = "--device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined"

// capSysAdmin is CAP_SYS_ADMIN's bit in /proc/self/status's capability masks.
const capSysAdmin = 21

// privileges are the fields of /proc/self/status that decide whether a mount
// gets CAP_SYS_ADMIN.
type privileges struct {
	// effective is set when CapEff holds it, so mount(2) can succeed.
	effective bool
	// bounding is set when CapBnd holds it, so an exec can gain it.
	bounding bool
	// noNewPrivs is set when NoNewPrivs is 1, so no exec gains a capability.
	noNewPrivs bool
}

func readPrivileges(p Probe) (privileges, error) {
	data, err := p.ReadFile("/proc/self/status")
	if err != nil {
		return privileges{}, fmt.Errorf("reading /proc/self/status: %w", err)
	}
	fields := map[string]string{}
	for line := range strings.Lines(string(data)) {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	has := map[string]bool{}
	for _, f := range []string{"CapEff", "CapBnd"} {
		bits, err := strconv.ParseUint(fields[f], 16, 64)
		if err != nil {
			return privileges{}, errors.New("/proc/self/status has no " + f)
		}
		has[f] = bits&(1<<capSysAdmin) != 0
	}
	return privileges{effective: has["CapEff"], bounding: has["CapBnd"], noNewPrivs: fields["NoNewPrivs"] == "1"}, nil
}

// noNewPrivsFix lists the settings that set NoNewPrivs, which bwrap always
// sets.
const noNewPrivsFix = "run slimtree without no_new_privs: drop Docker's --security-opt no-new-privileges, set allowPrivilegeEscalation: true in Kubernetes, or NoNewPrivileges=no in systemd"

// checkCapSysAdmin fails where no mount can get CAP_SYS_ADMIN. The fuse
// library mounts with mount(2) when CapEff holds it, and otherwise runs
// fusermount, which gains it from CapBnd through its setuid bit, or as root
// through the exec. A container started without --cap-add SYS_ADMIN lacks it
// in CapBnd, and NoNewPrivs keeps an exec from gaining it.
func checkCapSysAdmin(p Probe, _ string, _ []Result) Result {
	priv, err := readPrivileges(p)
	switch {
	case err != nil:
		return Result{Status: StatusWarn, Detail: err.Error()}
	case priv.effective:
		return Result{Status: StatusOK, Detail: "CapEff holds CAP_SYS_ADMIN"}
	case !priv.bounding:
		return Result{Status: StatusFail, Detail: "CapBnd lacks CAP_SYS_ADMIN, so no FUSE mount can succeed", Fix: "in a container, run it with " + containerFlags}
	case priv.noNewPrivs:
		return Result{Status: StatusFail, Detail: "NoNewPrivs is set, so fusermount cannot gain CAP_SYS_ADMIN", Fix: noNewPrivsFix}
	}
	return Result{Status: StatusOK, Detail: "CapBnd holds CAP_SYS_ADMIN"}
}

func ownerUID(info fs.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func checkLinuxGit(p Probe, _ string, _ []Result) Result {
	return gitResult(p, installCommand(p, "git"), upgradeGit(p))
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
	if dir == "" {
		return Result{Status: StatusSkip, Detail: "no directory to probe; run slimtree doctor where the worktree will go"}
	}
	err := p.APFS(dir)
	switch {
	case err == nil:
		return Result{Status: StatusOK, Detail: dir}
	case errors.Is(err, errNotWritable):
		return Result{Status: StatusFail, Detail: err.Error(), Fix: "use a directory you can write, on the APFS volume that will hold the worktree"}
	}
	return Result{Status: StatusFail, Detail: err.Error(), Fix: "make the worktree on an APFS volume, or use --backend artifactfs"}
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

func checkFSKit(p Probe, _ string, earlier []Result) Result {
	// FSKitAvailable checks the macOS version first, and its error then says
	// to install macFUSE whatever is installed.
	for _, r := range earlier {
		if r.ID == "macos-version" && r.Status != StatusOK {
			return Result{Status: StatusSkip, Detail: "needs macOS 26 or later first"}
		}
	}
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

// packageManager holds a distribution's install and upgrade commands, with
// %s for the package. A fresh Debian or Ubuntu image has no package lists, so
// apt-get updates them first.
type packageManager struct{ install, upgrade string }

var (
	apt    = packageManager{"sudo apt-get update && sudo apt-get install -y %s", "sudo apt-get update && sudo apt-get install -y --only-upgrade %s"}
	dnf    = packageManager{"sudo dnf install -y %s", "sudo dnf upgrade -y %s"}
	pacman = packageManager{"sudo pacman -S --needed %s", "sudo pacman -Syu %s"}
	apk    = packageManager{"sudo apk add %s", "sudo apk add -U --upgrade %s"}
	zypper = packageManager{"sudo zypper install -y %s", "sudo zypper update -y %s"}
)

// distroPackageManager returns the package manager of the distribution
// os-release describes.
func distroPackageManager(p Probe) (packageManager, bool) {
	release := osRelease(p)
	ids := append([]string{release["ID"]}, strings.Fields(release["ID_LIKE"])...)
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return apt, true
		case "fedora", "rhel", "centos":
			return dnf, true
		case "arch":
			return pacman, true
		case "alpine":
			return apk, true
		case "suse", "opensuse":
			return zypper, true
		}
	}
	return packageManager{}, false
}

func installCommand(p Probe, pkg string) string {
	pm, ok := distroPackageManager(p)
	if !ok {
		return "install " + pkg + " with your distribution's package manager"
	}
	return fmt.Sprintf(pm.install, pkg)
}

// installFuse3 is installCommand for fuse3. On Ubuntu 26.04 fuse3 conflicts
// with FUSE 2's fuse package, so apt-get removes that package.
func installFuse3(p Probe) string {
	cmd := installCommand(p, "fuse3")
	if pm, _ := distroPackageManager(p); pm == apt {
		cmd += " (this can remove FUSE 2's fuse package, which conflicts with fuse3 on Ubuntu 26.04)"
	}
	return cmd
}

// upgradeGit returns the command that upgrades git. Ubuntu's releases lag
// git, so on Ubuntu it adds the git-core PPA. Elsewhere the upgrade stops at
// the newest git the release ships.
func upgradeGit(p Probe) string {
	if isUbuntu(osRelease(p)) {
		return "sudo add-apt-repository -y ppa:git-core/ppa && sudo apt-get update && sudo apt-get install -y git"
	}
	pm, ok := distroPackageManager(p)
	if !ok {
		return "upgrade git to " + minGit + " or later with your distribution's package manager"
	}
	return fmt.Sprintf(pm.upgrade, "git") + "; if git stays older than " + minGit + ", your release ships no newer git"
}
