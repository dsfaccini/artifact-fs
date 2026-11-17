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
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := RunWithFiles(cmd, s.tempDir, input, nil); err != nil {
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

// RunWithFiles runs cmd to completion as cmd.Run does, with a file, not a
// pipe, for each standard stream that is not an *os.File. cmd reads input as
// its stdin when input is not nil. A writer the caller set as cmd.Stdout or
// cmd.Stderr gets what cmd wrote there once cmd exits, whatever its exit
// status. One writer set as both gets one file, as os/exec gives it one pipe,
// so the two streams keep their order. An *os.File stays as the caller set
// it, so a hook can still reach the terminal. When readStdout is not nil, it replaces cmd.Stdout and gets
// the stdout file only once cmd exits 0: a git that failed, as on a full
// disk, can leave its last line cut. Each file is unlinked in dir, or in the
// system temp directory when dir is "", before cmd starts.
//
// A pipe ends only when every process that holds its write end closes it.
// os/exec copies a stream that is not an *os.File through a pipe, waits for
// that end at most cmd.WaitDelay after cmd exits, and then fails cmd with
// exec.ErrWaitDelay, even when cmd exited 0. A reader of a pipe from
// StdoutPipe waits until the end. Git can leave a holder behind, such as the
// ssh ControlPersist master of a fetch, which keeps stderr. So can a child
// that another goroutine forked, until it execs (golang/go#22315). A file
// holds all of cmd's output once cmd exits.
func RunWithFiles(cmd *exec.Cmd, dir string, input []byte, readStdout func(io.Reader) error) error {
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
	type copyBack struct {
		file   *os.File
		writer io.Writer
	}
	var copies []copyBack
	toFile := func(w io.Writer) (io.Writer, error) {
		if _, ok := w.(*os.File); ok || w == nil {
			return w, nil
		}
		f, err := unlinked()
		if err != nil {
			return nil, err
		}
		copies = append(copies, copyBack{f, w})
		return f, nil
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
	var stdoutFile *os.File
	var err error
	callerStdout := cmd.Stdout
	if readStdout != nil {
		if stdoutFile, err = unlinked(); err != nil {
			return err
		}
		cmd.Stdout = stdoutFile
	} else if cmd.Stdout, err = toFile(cmd.Stdout); err != nil {
		return err
	}
	if readStdout == nil && sameWriter(cmd.Stderr, callerStdout) {
		cmd.Stderr = cmd.Stdout
	} else if cmd.Stderr, err = toFile(cmd.Stderr); err != nil {
		return err
	}
	runErr := cmd.Run()
	var readErr error
	for _, c := range copies {
		readErr = errors.Join(readErr, readWritten(c.file, func(r io.Reader) error {
			_, err := io.Copy(c.writer, r)
			return err
		}))
	}
	if runErr == nil && stdoutFile != nil {
		readErr = errors.Join(readErr, readWritten(stdoutFile, readStdout))
	}
	return errors.Join(runErr, readErr)
}

// sameWriter reports whether a and b are one writer, as os/exec checks
// before it gives stdout and stderr one pipe. Writers of a type that cannot
// be compared are never one writer.
func sameWriter(a, b io.Writer) (same bool) {
	defer func() { _ = recover() }()
	return a != nil && a == b
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
	if captureOutput {
		cmd.Stdout = &buf
	}
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stderr = errBuf
	runErr := RunWithFiles(cmd, s.tempDir, nil, nil)
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
