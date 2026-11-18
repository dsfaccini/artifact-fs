package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// mainEnv makes this test binary run main, as the slimtree binary does.
const mainEnv = "SLIMTREE_TEST_MAIN"

// notADaemon is what this test binary says when it runs as the daemon.
const notADaemon = "the slimtree test binary is not a daemon"

func TestMain(m *testing.M) {
	// slimtree starts the daemon from a copy of its own binary, and the
	// daemon's environment keeps mainEnv. Running main as the daemon would
	// mount, and running the tests again would launch the next copy.
	if len(os.Args) > 1 && (os.Args[1] == "daemon" || os.Args[1] == "afs") {
		fmt.Fprintln(os.Stderr, notADaemon)
		os.Exit(2)
	}
	if os.Getenv(mainEnv) == "1" {
		main()
	}
	gittest.IsolateConfig()
	os.Exit(m.Run())
}

// The test binary refuses to run as daemon or afs, also when mainEnv makes
// it run main. The flag after the command fails a real daemon command at
// once, so a regression exits instead of serving.
func TestTestBinaryIsNotADaemonWhenItRunsMain(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"daemon", "afs"} {
		cmd := exec.CommandContext(t.Context(), exe, arg, "--no-such-flag")
		cmd.Env = append(os.Environ(), mainEnv+"=1")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || string(out) != notADaemon+"\n" {
			t.Errorf("the test binary as %s with %s=1: %v, output %q; want exit 2 and %q", arg, mainEnv, err, out, notADaemon)
		}
	}
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }

// A closed terminal sends SIGHUP. main cancels the command on it, as on
// Ctrl-C, which stops the command's git and lets the command end, instead of
// the signal killing slimtree mid-command. A fake git, list --repo's first
// git, marks its start and waits.
func TestSIGHUPCancelsTheCommand(t *testing.T) {
	bin := t.TempDir()
	started, finished := filepath.Join(bin, "started"), filepath.Join(bin, "finished")
	if err := execfile.Write(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > '"+started+"'\nsleep 60\n: > '"+finished+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := gittest.StateRoot(t)
	if err := os.WriteFile(filepath.Join(home, "first-run"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "list", "--repo")
	cmd.Env = append(os.Environ(), mainEnv+"=1", "SLIMTREE_HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("slimtree exited before its git started: %v\n%s", err, stderr.Bytes())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	err = <-exited
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("slimtree after SIGHUP: %v, stderr %q; want exit 1, not killed by the signal", err, stderr.Bytes())
	}
	if _, err := os.Stat(finished); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("git ran to its end: %v", err)
	}
}
