package mounts

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// helper is the first argument that runs this test binary as a helper
// process instead of the tests.
const helper = "mounts-test-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helper {
		os.Exit(runHelper(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// helperCommand runs this test binary as the helper mode with args.
func helperCommand(mode string, args ...string) *exec.Cmd {
	return exec.Command(os.Args[0], append([]string{helper, mode}, args...)...)
}

// runHelper runs one helper mode. Each one that waits stops after a minute,
// so a regression cannot leave it running.
func runHelper(mode string, args []string) int {
	switch mode {
	case "map":
		// Maps the file args[0], closes it, and reports on stdout: only the
		// mapping holds the file.
		f, err := os.Open(args[0])
		if err != nil {
			return fail(err)
		}
		data, err := unix.Mmap(int(f.Fd()), 0, 1, unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return fail(err)
		}
		f.Close()
		fmt.Println("mapped")
		time.Sleep(time.Minute)
		_ = unix.Munmap(data)
	case "session":
		sid, err := unix.Getsid(0)
		if err != nil {
			return fail(err)
		}
		fmt.Println(sid, os.Getpid())
	case "hold":
		// Leaves a child that holds stdout and stderr, then exits 0.
		child := exec.Command("sleep", "60")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return fail(err)
		}
		token, err := StartToken(child.Process.Pid)
		if err != nil {
			return fail(err)
		}
		fmt.Println(child.Process.Pid, token)
	case "hang":
		// Starts a sleeper, which joins its process group, then waits.
		child := helperCommand("sleeper", args[0])
		if err := child.Start(); err != nil {
			return fail(err)
		}
		time.Sleep(time.Minute)
	case "sleeper":
		// Writes started in the directory args[0] once it catches SIGTERM,
		// then writes end: "term" on SIGTERM, or "timeout" after a minute.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		if err := writeMarker(args[0], "started", ""); err != nil {
			return fail(err)
		}
		end := "timeout"
		select {
		case <-sig:
			end = "term"
		case <-time.After(time.Minute):
		}
		if err := writeMarker(args[0], "end", end); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown helper mode %q", mode))
	}
	return 0
}

// writeMarker writes content to dir/name whole, so a reader that sees the
// file sees its content.
func writeMarker(dir, name, content string) error {
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 1
}
