package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// E2E tests read and write mount files in a child process, because the test
// process also serves the mount. Since Linux commit e780259b54e6 ("exec:
// do_close_on_exec() before taking exec_update_lock"), a vfork child's exec
// closes its copy of each of the parent's close-on-exec descriptors before it
// releases the parent. Closing a FUSE file sends FLUSH, which only the serving
// process can answer, while the thread that forked holds its P with signals
// blocked. With GOMAXPROCS 1, a GC stop-the-world in that window, or every P
// forking at once, nothing answers, and the mount deadlocks. The server forks
// git to hydrate and prefetch blobs, so a mount file the test process holds
// open can deadlock it.

// fileOpArg is the first argument of the test binary run as a child that
// reads or writes mount files.
const fileOpArg = "e2e-file-op"

// fileOpFailed is the exit status of a child whose file operation failed. It
// writes the error's errno, 0 for none, and the error on stderr.
const fileOpFailed = 3

// runFileOp performs the operation args name and returns the exit status, as
// the test binary run with fileOpArg.
func runFileOp(args []string) int {
	err := fileOp(args)
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	errors.As(err, &errno)
	fmt.Fprintf(os.Stderr, "%d\n%v", errno, err)
	return fileOpFailed
}

func fileOp(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "read":
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	case len(args) == 3 && args[0] == "write":
		perm, err := strconv.ParseUint(args[2], 8, 32)
		if err != nil {
			return err
		}
		data, err := readStdin()
		if err != nil {
			return err
		}
		return os.WriteFile(args[1], data, os.FileMode(perm))
	case len(args) == 3 && args[0] == "read-files":
		workers, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		paths, err := readStdin()
		if err != nil {
			return err
		}
		elapsed, total, err := readFilesConcurrently(args[1], strings.Split(strings.TrimSuffix(string(paths), "\x00"), "\x00"), workers)
		if err != nil {
			return err
		}
		_, err = fmt.Printf("%d %d", elapsed.Nanoseconds(), total)
		return err
	case len(args) >= 2 && args[0] == "read-loop":
		return readLoop(args[1:])
	}
	return fmt.Errorf("unknown file operation %q", args)
}

func readStdin() ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(os.Stdin)
	return buf.Bytes(), err
}

// readFilesConcurrently reads each of paths below root with workers readers,
// and returns how long the reads took and how many bytes they read.
func readFilesConcurrently(root string, paths []string, workers int) (time.Duration, int64, error) {
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int64
	var firstErr error
	for range workers {
		wg.Go(func() {
			for path := range jobs {
				data, err := os.ReadFile(filepath.Join(root, path))
				mu.Lock()
				total += int64(len(data))
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("read %s: %w", path, err)
				}
				mu.Unlock()
			}
		})
	}
	start := time.Now()
	for _, path := range paths {
		jobs <- path
	}
	close(jobs)
	wg.Wait()
	return time.Since(start), total, firstErr
}

// readLoop reads each of paths in turn, again and again until stdin closes.
// For each read it writes a line: the path's index, then ok and the quoted
// content, or err, the errno and the quoted error.
func readLoop(paths []string) error {
	var closed atomic.Bool
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		closed.Store(true)
	}()
	out := bufio.NewWriter(os.Stdout)
	for !closed.Load() {
		for i, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				var errno syscall.Errno
				errors.As(err, &errno)
				fmt.Fprintf(out, "%d err %d %q\n", i, errno, err.Error())
			} else {
				fmt.Fprintf(out, "%d ok %q\n", i, data)
			}
			if err := out.Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// childFileOp runs the test binary as a child that performs the file
// operation args name, with stdin as its input, and returns its output. A
// failed operation returns an *os.PathError that carries the child's errno.
func childFileOp(stdin []byte, args ...string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append([]string{fileOpArg}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == fileOpFailed {
		errnoText, message, _ := strings.Cut(stderr.String(), "\n")
		if errno, convErr := strconv.Atoi(errnoText); convErr == nil && errno != 0 {
			return nil, &os.PathError{Op: args[0], Path: args[1], Err: syscall.Errno(errno)}
		}
		return nil, errors.New(message)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s in a child process: %w: %s", args[0], args[1], err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// readMountFile reads path as os.ReadFile does, in a child process.
func readMountFile(path string) ([]byte, error) {
	return childFileOp(nil, "read", path)
}

// writeMountFile writes data to path as os.WriteFile does, in a child process.
func writeMountFile(path string, data []byte, perm os.FileMode) error {
	_, err := childFileOp(data, "write", path, strconv.FormatUint(uint64(perm.Perm()), 8))
	return err
}

// readMountFilesUntil reads each of paths in turn in one child process, again
// and again until stop closes or each returns false, and calls each with every
// read's result. A failed read carries the child's errno, as readMountFile's.
func readMountFilesUntil(paths []string, stop <-chan struct{}, each func(path string, data []byte, err error) bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, append([]string{fileOpArg, "read-loop"}, paths...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var closeOnce sync.Once
	closeStdin := func() { closeOnce.Do(func() { _ = stdin.Close() }) }
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-stop:
		case <-stopped:
		}
		closeStdin()
	}()
	var parseErr error
	lines := bufio.NewScanner(stdout)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		path, data, err, ok := parseReadLine(paths, lines.Text())
		if !ok {
			parseErr = fmt.Errorf("read-loop line %q", lines.Text())
		}
		if !ok || !each(path, data, err) {
			closeStdin()
			_, _ = io.Copy(io.Discard, stdout)
			break
		}
	}
	closeStdin()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("read-loop child: %w: %s", err, stderr.String())
	}
	return parseErr
}

// parseReadLine parses a line readLoop wrote about one of paths.
func parseReadLine(paths []string, line string) (path string, data []byte, readErr error, ok bool) {
	indexText, rest, _ := strings.Cut(line, " ")
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index >= len(paths) {
		return "", nil, nil, false
	}
	path = paths[index]
	if quoted, isData := strings.CutPrefix(rest, "ok "); isData {
		content, err := strconv.Unquote(quoted)
		return path, []byte(content), nil, err == nil
	}
	errnoText, quoted, _ := strings.Cut(strings.TrimPrefix(rest, "err "), " ")
	errno, err := strconv.Atoi(errnoText)
	message, unquoteErr := strconv.Unquote(quoted)
	if err != nil || unquoteErr != nil {
		return "", nil, nil, false
	}
	if errno != 0 {
		return path, nil, &os.PathError{Op: "read", Path: path, Err: syscall.Errno(errno)}, true
	}
	return path, nil, errors.New(message), true
}

// readMountFiles reads each of paths below root in one child process, with
// workers readers, and returns how long the reads took and how many bytes
// they read.
func readMountFiles(root string, paths []string, workers int) (time.Duration, int64, error) {
	out, err := childFileOp([]byte(strings.Join(paths, "\x00")+"\x00"), "read-files", root, strconv.Itoa(workers))
	if err != nil {
		return 0, 0, err
	}
	var nanos, total int64
	if _, err := fmt.Sscanf(string(out), "%d %d", &nanos, &total); err != nil {
		return 0, 0, fmt.Errorf("read-files output %q: %w", out, err)
	}
	return time.Duration(nanos), total, nil
}
