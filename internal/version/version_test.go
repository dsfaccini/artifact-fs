package version

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// The release build stamps the version with -ldflags -X, which the linker
// skips without a word when the variable's path or initializer is wrong, so
// only a linked binary shows the stamp works.
func TestVersionFlagPrintsTheStampedVersion(t *testing.T) {
	t.Parallel()
	const pkg = "github.com/cloudflare/artifact-fs/internal/version"
	bin := filepath.Join(t.TempDir(), "artifact-fs")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", bin,
		"-ldflags", "-s -w -X "+pkg+".Version=0.1.0rc1 -X "+pkg+".Tag=v0.1.0-rc.1 -X "+pkg+".Commit=0123456789abcdef0123456789abcdef01234567",
		"github.com/cloudflare/artifact-fs/cmd/artifact-fs")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(t.Context(), bin, "--version").Output()
	if err != nil {
		t.Fatalf("artifact-fs --version: %v", err)
	}
	want := fmt.Sprintf("artifact-fs 0.1.0rc1 (v0.1.0-rc.1, 0123456789ab, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if string(out) != want {
		t.Fatalf("artifact-fs --version = %q, want %q", out, want)
	}
}

func TestResolveReadsTheBuildInfoOfAnUnstampedBuild(t *testing.T) {
	t.Parallel()
	vcs := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified}}
	}
	const revision = "9d9223c4abcdef0123456789abcdef0123456789"
	for _, tc := range []struct {
		name                             string
		version, tag, commit             string
		main                             string
		settings                         []debug.BuildSetting
		wantVersion, wantTag, wantCommit string
	}{
		{name: "go build at a release tag", main: "v0.2.0", settings: vcs(revision, "false"), wantVersion: "0.2.0", wantTag: "v0.2.0", wantCommit: "9d9223c4abcd"},
		{name: "go build of a dirty checkout", main: "v0.0.0-20261117041713-9d9223c4abcd+dirty", settings: vcs(revision, "true"), wantVersion: "dev", wantTag: "untagged", wantCommit: "9d9223c4abcd-dirty"},
		{name: "go build after a tag", main: "v0.2.1-0.20261117041713-9d9223c4abcd", settings: vcs(revision, "false"), wantVersion: "dev", wantTag: "untagged", wantCommit: "9d9223c4abcd"},
		{name: "go build without VCS", main: "(devel)", wantVersion: "dev", wantTag: "untagged", wantCommit: "unknown"},
		{name: "stamped version without a tag", version: "v0.2.0", main: "(devel)", settings: vcs(revision, "false"), wantVersion: "0.2.0", wantTag: "v0.2.0", wantCommit: "9d9223c4abcd"},
		{name: "stamped commit wins over the VCS revision", version: "0.2.0", commit: "0123456789abcdef0123456789abcdef01234567", main: "(devel)", settings: vcs(revision, "false"), wantVersion: "0.2.0", wantTag: "v0.2.0", wantCommit: "0123456789ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := &debug.BuildInfo{Main: debug.Module{Version: tc.main}, Settings: tc.settings}
			v, tag, commit := resolve(tc.version, tc.tag, tc.commit, info)
			if v != tc.wantVersion || tag != tc.wantTag || commit != tc.wantCommit {
				t.Fatalf("resolve = %q, %q, %q, want %q, %q, %q", v, tag, commit, tc.wantVersion, tc.wantTag, tc.wantCommit)
			}
		})
	}
}
