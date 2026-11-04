package gitstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

func runGit(ctx context.Context, gitDir string, args ...string) (string, error) {
	return runGitWithEnv(ctx, gitDir, nil, args...)
}

func runGitWithEnv(ctx context.Context, gitDir string, extraEnv []string, args ...string) (string, error) {
	return runGitWithEnvCapture(ctx, gitDir, extraEnv, true, args...)
}

func runGitDiscardOutputWithEnv(ctx context.Context, gitDir string, extraEnv []string, args ...string) error {
	_, err := runGitWithEnvCapture(ctx, gitDir, extraEnv, false, args...)
	return err
}

func runGitInput(ctx context.Context, gitDir string, extraEnv []string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	configureCancelableCommand(cmd)
	return runInputCommand(ctx, cmd, gitDir, extraEnv, input)
}

// runLazyFetchGitInput runs a Git command that fetches objects as a lazy fetch
// does: with lazyFetchEnv, in its own session, so it cannot prompt.
func runLazyFetchGitInput(ctx context.Context, gitDir string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	configureCancelableCommand(cmd)
	configureCommandSession(cmd)
	return runInputCommand(ctx, cmd, gitDir, lazyFetchEnv(), input)
}

func runInputCommand(ctx context.Context, cmd *exec.Cmd, gitDir string, extraEnv []string, input []byte) (string, error) {
	setGitWorktreeDirectory(cmd, extraEnv)
	cmd.Env = append(os.Environ(), append([]string{"GIT_DIR=" + gitDir}, extraEnv...)...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	// A fetch's server shapes this stderr, and redaction's cost grows faster
	// than its length.
	stderr := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New(auth.RedactString(strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

func setGitWorktreeDirectory(cmd *exec.Cmd, extraEnv []string) {
	for _, entry := range extraEnv {
		if worktree, ok := strings.CutPrefix(entry, "GIT_WORK_TREE="); ok {
			cmd.Dir = worktree
			return
		}
	}
}

func runGitWithEnvCapture(ctx context.Context, gitDir string, extraEnv []string, captureOutput bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	configureCancelableCommand(cmd)
	setGitWorktreeDirectory(cmd, extraEnv)
	env := os.Environ()
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	buf := &bytes.Buffer{}
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	if captureOutput {
		cmd.Stdout = buf
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if runErr == nil {
		return out, nil
	}
	contextErr := ctx.Err()

	rawMessage := strings.TrimSpace(errBuf.String())
	retryable := errBuf.Retryable()
	msg := rawMessage
	var credentials []string
	for _, env := range extraEnv {
		for _, prefix := range []string{"ARTIFACT_FS_GIT_USERNAME=", "ARTIFACT_FS_GIT_PASSWORD="} {
			if value, ok := strings.CutPrefix(env, prefix); ok && value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	if len(credentials) == 2 && len(credentials[0]) < len(credentials[1]) {
		credentials[0], credentials[1] = credentials[1], credentials[0]
	}
	for _, credential := range credentials {
		msg = strings.ReplaceAll(msg, credential, "REDACTED")
	}
	msg = redactTruncatedCredentialPrefix(msg, credentials)
	msg = auth.RedactString(msg)
	var cause error
	if msg != "" && !(contextErr != nil && strings.EqualFold(msg, "signal: killed")) {
		cause = errors.New(msg)
	} else if runErr != nil && !(contextErr != nil && strings.EqualFold(strings.TrimSpace(runErr.Error()), "signal: killed")) {
		cause = errors.New(auth.RedactString(runErr.Error()))
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return out, &gitCommandError{cause: cause, contextErr: contextErr, retryable: retryable, exitCode: exitCode}
}

func configureCancelableCommand(cmd *exec.Cmd) {
	configureCommandProcessGroup(cmd)
	cmd.Cancel = func() error { return killCommandProcessGroup(cmd) }
	cmd.WaitDelay = gitCommandWaitDelay
}
