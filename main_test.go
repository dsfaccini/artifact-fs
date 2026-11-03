package main

import (
	"context"
	"fmt"
	"os"
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
