// Package version describes the running build.
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
)

// A release build sets Version, Tag and Commit with
//
//	-ldflags "-X github.com/cloudflare/artifact-fs/internal/version.Version=X.Y.Z -X github.com/cloudflare/artifact-fs/internal/version.Tag=vX.Y.Z -X github.com/cloudflare/artifact-fs/internal/version.Commit=<sha>"
//
// The linker silently skips -X for a variable that does not exist or whose
// initializer is not a constant, so keep all three uninitialized.
var (
	// Version is the release version, in the form the package index publishes,
	// such as 0.1.0rc1. A leading v is dropped.
	Version string
	// Tag is the git tag of the release, such as v0.1.0-rc.1. It defaults to v
	// followed by Version.
	Tag string
	// Commit is the commit the release was built from.
	Commit string
)

const commitLength = 12

// Go stamps a build of an untagged commit with a pseudo-version, which ends in
// the commit's 14-digit time and 12-digit hash.
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)

// String describes the build as "<version> (<tag>, <commit>, <go version>,
// <os>/<arch>)". Without -ldflags it reads what Go stamped: a go build from a
// clean checkout at a release tag reports that tag, and any other build reports
// "dev (untagged, ...)". The commit falls back to the stamped VCS revision,
// with -dirty for modified sources, then to "unknown".
func String() string {
	info, _ := debug.ReadBuildInfo()
	v, tag, commit := resolve(Version, Tag, Commit, info)
	return fmt.Sprintf("%s (%s, %s, %s, %s/%s)", v, tag, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func resolve(stampedVersion, stampedTag, stampedCommit string, info *debug.BuildInfo) (string, string, string) {
	v := strings.TrimPrefix(stampedVersion, "v")
	tag := stampedTag
	if v != "" && tag == "" {
		tag = "v" + v
	}
	commit := shortCommit(stampedCommit)
	if info != nil {
		if v == "" {
			tag = releaseTag(info.Main.Version)
			v = strings.TrimPrefix(tag, "v")
		}
		if commit == "" {
			commit = vcsCommit(info.Settings)
		}
	}
	if v == "" {
		v, tag = "dev", "untagged"
	}
	if commit == "" {
		commit = "unknown"
	}
	return v, tag, commit
}

// releaseTag returns a module version that names a release tag, and "" for a
// pseudo-version, a modified checkout or no version.
func releaseTag(moduleVersion string) string {
	if moduleVersion == "" || moduleVersion == "(devel)" || strings.Contains(moduleVersion, "+") || pseudoVersion.MatchString(moduleVersion) {
		return ""
	}
	return moduleVersion
}

func vcsCommit(settings []debug.BuildSetting) string {
	var revision, modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = shortCommit(s.Value)
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision != "" && modified == "true" {
		revision += "-dirty"
	}
	return revision
}

func shortCommit(commit string) string {
	if len(commit) > commitLength {
		return commit[:commitLength]
	}
	return commit
}
