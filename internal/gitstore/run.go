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

func (s *Store) runGit(ctx context.Context, gitDir string, args ...string) (string, error) {
	return s.runGitWithEnv(ctx, gitDir, nil, args...)
}

func (s *Store) runGitWithEnv(ctx context.Context, gitDir string, extraEnv []string, args ...string) (string, error) {
	return s.runGitWithEnvCapture(ctx, gitDir, extraEnv, true, args...)
}

func (s *Store) runGitDiscardOutputWithEnv(ctx context.Context, gitDir string, extraEnv []string, args ...string) error {
	_, err := s.runGitWithEnvCapture(ctx, gitDir, extraEnv, false, args...)
	return err
}

func (s *Store) runGitInput(ctx context.Context, gitDir string, extraEnv []string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	ConfigureCancelableCommand(cmd)
	return s.runInputCommand(ctx, cmd, gitDir, extraEnv, input)
}

// runLazyFetchGitInput runs a Git command that fetches objects as a lazy fetch
// does: with lazyFetchEnv, in its own session, so it cannot prompt.
func (s *Store) runLazyFetchGitInput(ctx context.Context, gitDir string, input []byte, args ...string) (string, error) {
	env, err := s.lazyFetchEnv()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	ConfigureCancelableCommand(cmd)
	configureCommandSession(cmd)
	return s.runInputCommand(ctx, cmd, gitDir, env, input)
}

func (s *Store) runInputCommand(ctx context.Context, cmd *exec.Cmd, gitDir string, extraEnv []string, input []byte) (string, error) {
	setGitWorktreeDirectory(cmd, extraEnv)
	cmd.Env = append(os.Environ(), append([]string{"GIT_DIR=" + gitDir}, extraEnv...)...)
	var stdout bytes.Buffer
	// A fetch's server shapes this stderr, and redaction's cost grows faster
	// than its length.
	stderr := &boundedGitError{limit: maxGitErrorBytes}
	if err := RunWithFiles(cmd, s.tempDir, input, readInto(&stdout), stderr); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(auth.RedactString(msg))
	}
	return stdout.String(), nil
}

// RunWithFiles runs cmd to completion with a file, not a pipe, for each of its
// standard streams. cmd reads input as its stdin when input is not nil. It
// writes stdout and stderr to files. Once cmd exits, RunWithFiles passes the
// stdout file to readStdout and copies the stderr file to stderr. A nil
// readStdout or stderr discards that stream. Each file is unlinked in dir, or
// in the system temp directory when dir is "", before cmd starts.
//
// A pipe ends only when every process that holds its write end closes it.
// os/exec copies a stream that is not an *os.File through a pipe, waits for
// that end at most cmd.WaitDelay after cmd exits, and then fails cmd with
// exec.ErrWaitDelay, even when cmd exited 0. A reader of a pipe from
// StdoutPipe waits until the end. Git can leave a holder behind, such as the
// ssh ControlPersist master of a fetch, which keeps stderr. So can a child
// that another goroutine forked, until it execs (golang/go#22315). A file
// holds all of cmd's output once cmd exits.
func RunWithFiles(cmd *exec.Cmd, dir string, input []byte, readStdout func(io.Reader) error, stderr io.Writer) error {
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	unlinked := func() (*os.File, error) {
		f, err := os.CreateTemp(dir, "artifact-fs-git-")
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		return f, os.Remove(f.Name())
	}
	if input != nil {
		f, err := unlinked()
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(input, 0); err != nil {
			return err
		}
		cmd.Stdin = f
	}
	var stdoutFile, stderrFile *os.File
	if readStdout != nil {
		f, err := unlinked()
		if err != nil {
			return err
		}
		cmd.Stdout, stdoutFile = f, f
	}
	if stderr != nil {
		f, err := unlinked()
		if err != nil {
			return err
		}
		cmd.Stderr, stderrFile = f, f
	}
	runErr := cmd.Run()
	var readErr, copyErr error
	if stdoutFile != nil {
		readErr = readWritten(stdoutFile, readStdout)
	}
	if stderrFile != nil {
		copyErr = readWritten(stderrFile, func(r io.Reader) error {
			_, err := io.Copy(stderr, r)
			return err
		})
	}
	return errors.Join(runErr, readErr, copyErr)
}

// readWritten passes read what f holds. It reads at offsets: a process that
// still holds f shares f's offset, so a seek would move its writes.
func readWritten(f *os.File, read func(io.Reader) error) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return read(io.NewSectionReader(f, 0, info.Size()))
}

// readInto returns a readStdout for RunWithFiles that reads stdout into buf.
func readInto(buf *bytes.Buffer) func(io.Reader) error {
	return func(r io.Reader) error {
		_, err := buf.ReadFrom(r)
		return err
	}
}

func setGitWorktreeDirectory(cmd *exec.Cmd, extraEnv []string) {
	for _, entry := range extraEnv {
		if worktree, ok := strings.CutPrefix(entry, "GIT_WORK_TREE="); ok {
			cmd.Dir = worktree
			return
		}
	}
}

// runNetworkGit runs a Git command that can contact a remote, such as a fetch
// or a read-tree that lazily fetches missing trees, as a lazy fetch does:
// with lazyFetchEnv, then extraEnv, in its own session, so neither Git nor
// SSH can prompt. Without it, git runs in a background process group, and
// the terminal stops it at a prompt. Credential helpers and the askpass
// programs the user set still run.
func (s *Store) runNetworkGit(ctx context.Context, gitDir string, extraEnv []string, args ...string) (string, error) {
	env, err := s.lazyFetchEnv()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	ConfigureCancelableCommand(cmd)
	configureCommandSession(cmd)
	return s.runGitCommand(ctx, cmd, gitDir, append(env, extraEnv...), true)
}

func (s *Store) runGitWithEnvCapture(ctx context.Context, gitDir string, extraEnv []string, captureOutput bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	ConfigureCancelableCommand(cmd)
	return s.runGitCommand(ctx, cmd, gitDir, extraEnv, captureOutput)
}

func (s *Store) runGitCommand(ctx context.Context, cmd *exec.Cmd, gitDir string, extraEnv []string, captureOutput bool) (string, error) {
	setGitWorktreeDirectory(cmd, extraEnv)
	env := os.Environ()
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	var buf bytes.Buffer
	var readStdout func(io.Reader) error
	if captureOutput {
		readStdout = readInto(&buf)
	}
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	runErr := RunWithFiles(cmd, s.tempDir, nil, readStdout, errBuf)
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
	if msg != "" && !(contextErr != nil && isCancellationSignal(msg)) {
		cause = errors.New(msg)
	} else if runErr != nil && !(contextErr != nil && isCancellationSignal(strings.TrimSpace(runErr.Error()))) {
		cause = errors.New(auth.RedactString(runErr.Error()))
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return out, &gitCommandError{cause: cause, contextErr: contextErr, retryable: retryable, exitCode: exitCode}
}

// isCancellationSignal reports whether msg names only a signal that
// terminateCommandProcessGroup sends, which a canceled command's context error
// already explains.
func isCancellationSignal(msg string) bool {
	return strings.EqualFold(msg, "signal: terminated") || strings.EqualFold(msg, "signal: killed")
}

// ConfigureCancelableCommand makes cmd stop as every git command of this
// package stops when its context ends: its process group gets SIGTERM, then
// SIGKILL after GitCommandWaitDelay, through terminateCommandProcessGroup.
func ConfigureCancelableCommand(cmd *exec.Cmd) {
	configureCommandProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateCommandProcessGroup(cmd) }
	cmd.WaitDelay = GitCommandWaitDelay
}
