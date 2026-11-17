// Package version describes the running build.
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
)

// A release build sets Version and Commit with
//
//	-ldflags "-X github.com/cloudflare/artifact-fs/internal/version.Version=X.Y.Z -X github.com/cloudflare/artifact-fs/internal/version.Commit=<sha>"
//
// The linker silently skips -X for a variable that does not exist or whose
// initializer is not a constant, so keep both uninitialized.
var (
	// Version is the release version. A leading v is dropped.
	Version string
	// Commit is the commit the release was built from.
	Commit string
)

// Go stamps a build of an untagged commit with a pseudo-version, which ends in
// the commit's 14-digit time and 12-digit hash.
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)

// String describes the build as "X.Y.Z (vX.Y.Z, <commit>, <go version>,
// <os>/<arch>)". Without -ldflags it reads what Go stamped: a go install of a
// release tag reports that tag, and any other build reports
// "dev (untagged, ...)". The commit falls back to the stamped VCS revision,
// with -dirty for modified sources, then to "unknown".
func String() string {
	info, _ := debug.ReadBuildInfo()
	v, commit := resolve(Version, Commit, info)
	tag := "untagged"
	if v != "dev" {
		tag = "v" + v
	}
	return fmt.Sprintf("%s (%s, %s, %s, %s/%s)", v, tag, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func resolve(stampedVersion, stampedCommit string, info *debug.BuildInfo) (string, string) {
	v := strings.TrimPrefix(stampedVersion, "v")
	commit := stampedCommit
	if info != nil {
		if v == "" {
			v = releaseVersion(info.Main.Version)
		}
		if commit == "" {
			commit = vcsCommit(info.Settings)
		}
	}
	if v == "" {
		v = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	return v, commit
}

// releaseVersion returns the version of a module version that names a release
// tag, and "" for a pseudo-version, a modified checkout or no version.
func releaseVersion(moduleVersion string) string {
	if moduleVersion == "" || moduleVersion == "(devel)" || strings.Contains(moduleVersion, "+") || pseudoVersion.MatchString(moduleVersion) {
		return ""
	}
	return strings.TrimPrefix(moduleVersion, "v")
}

func vcsCommit(settings []debug.BuildSetting) string {
	var revision, modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" && modified == "true" {
		revision += "-dirty"
	}
	return revision
}
