package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/cli"
	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// e2eMountsStarted counts the FUSE mounts e2e tests start. macFUSE's FSKit
// extension can stop being found or enabled after a series of mounts in one
// session (https://github.com/macfuse/macfuse/issues/1232), so TestMain reports
// it and skipIfOverMountBudget caps it on FSKit.
var e2eMountsStarted atomic.Int64

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	// AFS_E2E_REPO opts into a real remote, and the README authenticates it
	// through the machine's ambient Git credential helper, so keep that config.
	if os.Getenv("AFS_E2E_REPO") == "" {
		gittest.IsolateConfig()
	}
	disableIdentityGuessing()
	gittest.DisableRaceExitSleep()
	code := m.Run()
	if os.Getenv("AFS_RUN_E2E_TESTS") == "1" {
		fmt.Printf("e2e: started %d FUSE mounts\n", e2eMountsStarted.Load())
	}
	os.Exit(code)
}

func TestGitIgnoresMachineConfig(t *testing.T) {
	if os.Getenv("AFS_E2E_REPO") != "" {
		t.Skip("AFS_E2E_REPO keeps the machine's Git config for its credential helper")
	}
	gittest.RequireConfigIsolated(t)
}

// disableIdentityGuessing sets user.useConfigOnly for git in this process and
// every process it starts, so a commit that names no identity fails instead of
// taking one guessed from the user and host names. That covers the git the
// daemon under test runs and the git that tests run in its mounts. It adds the
// setting after any GIT_CONFIG_* entries the caller set.
func disableIdentityGuessing() {
	n, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	os.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(n), "user.useConfigOnly")
	os.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(n), "true")
	os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
}

func TestGitCommitNeedsAConfiguredIdentity(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cmd := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "no identity")
	// An identity variable, or the machine config that AFS_E2E_REPO keeps,
	// would give the commit an identity. LC_ALL=C keeps git's message English.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_AUTHOR_") && !strings.HasPrefix(kv, "GIT_COMMITTER_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "auto-detection is disabled") {
		t.Fatalf("git commit with no identity: err = %v, want it refused because user.useConfigOnly disables auto-detection\n%s", err, out)
	}
}
