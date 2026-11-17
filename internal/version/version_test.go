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
		"-ldflags", "-s -w -X "+pkg+".Version=1.2.3 -X "+pkg+".Commit=0123456789ab",
		"github.com/cloudflare/artifact-fs/cmd/artifact-fs")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(t.Context(), bin, "--version").Output()
	if err != nil {
		t.Fatalf("artifact-fs --version: %v", err)
	}
	want := fmt.Sprintf("artifact-fs 1.2.3 (v1.2.3, 0123456789ab, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if string(out) != want {
		t.Fatalf("artifact-fs --version = %q, want %q", out, want)
	}
}

func TestResolveReadsTheBuildInfoOfAnUnstampedBuild(t *testing.T) {
	t.Parallel()
	vcs := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified}}
	}
	for _, tc := range []struct {
		name                    string
		version, commit         string
		main                    string
		settings                []debug.BuildSetting
		wantVersion, wantCommit string
	}{
		{name: "go install of a tag", main: "v0.2.0", wantVersion: "0.2.0", wantCommit: "unknown"},
		{name: "go build of a dirty checkout", main: "v0.0.0-20261117041713-9d9223c4abcd+dirty", settings: vcs("9d9223c4abcdef0123456789abcdef0123456789", "true"), wantVersion: "dev", wantCommit: "9d9223c4abcd-dirty"},
		{name: "go build after a tag", main: "v0.2.1-0.20261117041713-9d9223c4abcd", settings: vcs("9d9223c4abcdef0123456789abcdef0123456789", "false"), wantVersion: "dev", wantCommit: "9d9223c4abcd"},
		{name: "go build without VCS", main: "(devel)", wantVersion: "dev", wantCommit: "unknown"},
		{name: "stamped version keeps the VCS commit", version: "v0.2.0", main: "(devel)", settings: vcs("9d9223c4abcdef0123456789abcdef0123456789", "false"), wantVersion: "0.2.0", wantCommit: "9d9223c4abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := &debug.BuildInfo{Main: debug.Module{Version: tc.main}, Settings: tc.settings}
			v, commit := resolve(tc.version, tc.commit, info)
			if v != tc.wantVersion || commit != tc.wantCommit {
				t.Fatalf("resolve = %q, %q, want %q, %q", v, commit, tc.wantVersion, tc.wantCommit)
			}
		})
	}
}
