package gitstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/execfile"
)

func TestRunGitClassifiesBeforeCredentialRedaction(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'HTTP 500 transport failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := New(nil).runGitWithEnv(context.Background(), "", []string{"ARTIFACT_FS_GIT_PASSWORD=0"}, "version")
	_, _, retryable := gitOperationCauses(err)
	if !retryable {
		t.Fatalf("redacted command error was not retryable: %v", err)
	}
	if strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("command error leaked credential value: %v", err)
	}
}

func TestTerminateCommandProcessGroupReportsExitedProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("git", "version")
	configureCommandProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := terminateCommandProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate exited process error = %v, want os.ErrProcessDone", err)
	}
}

// One writer set as both stdout and stderr gets the two streams in the order
// the command wrote them, as the one pipe os/exec gives such a writer did.
func TestRunWithFilesKeepsTheOrderOfOneWriterForBothStreams(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "echo 1; echo 2 >&2; echo 3")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := RunWithFiles(cmd, t.TempDir(), nil, nil); err != nil || out.String() != "1\n2\n3\n" {
		t.Fatalf("RunWithFiles = %v, output %q; want nil and %q", err, out.String(), "1\n2\n3\n")
	}
}
