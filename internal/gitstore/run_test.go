package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGitClassifiesBeforeCredentialRedaction(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'HTTP 500 transport failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runGitWithEnv(context.Background(), "", []string{"ARTIFACT_FS_GIT_PASSWORD=0"}, "version")
	_, _, retryable := gitOperationCauses(err)
	if !retryable {
		t.Fatalf("redacted command error was not retryable: %v", err)
	}
	if strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("command error leaked credential value: %v", err)
	}
}

func TestTerminateCommandProcessGroupReportsExitedProcess(t *testing.T) {
	cmd := exec.Command("git", "version")
	configureCommandProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := terminateCommandProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate exited process error = %v, want os.ErrProcessDone", err)
	}
}
