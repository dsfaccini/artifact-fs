package mounts

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestSignalReachesOnlyTheProcessItsStartTokenNames(t *testing.T) {
	t.Parallel()
	cat := exec.Command("/bin/cat")
	stdin, err := cat.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cat.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	start(t, cat)
	pid := cat.Process.Pid
	// Taken before the child shows it runs: the token comes from the start
	// time, which fork sets, not from the argv, which exec fills later.
	token, err := StartToken(pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken right after Start = %q, %v", token, err)
	}
	if _, err := io.WriteString(stdin, "ready\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("cat echoed %q, %v", line, err)
	}
	if again, err := StartToken(pid); err != nil || again != token {
		t.Fatalf("StartToken once exec has finished = %q, %v; want %q", again, err, token)
	}

	// A SIGKILL sent despite the wrong token would show in the wait status.
	if err := Signal(pid, token+"0", syscall.SIGKILL); !errors.Is(err, ErrGone) {
		t.Fatalf("Signal with another token = %v; want ErrGone", err)
	}
	if err := Signal(pid, token, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cat.Wait()
	if status, ok := cat.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("cat ended with %v; want SIGTERM", err)
	}
	if got, err := StartToken(pid); err != nil || got == token {
		t.Fatalf("StartToken of a reaped child = %q, %v; want another token", got, err)
	}
	if err := Signal(pid, token, syscall.SIGKILL); !errors.Is(err, ErrGone) {
		t.Fatalf("Signal of a reaped child = %v; want ErrGone", err)
	}
}

// A child that exited and was not reaped is a zombie: gone for Signal,
// whatever StartToken answers. A stop loop in a container whose PID 1 never
// reaps otherwise waits forever.
func TestSignalTreatsAZombieAsGone(t *testing.T) {
	t.Parallel()
	cat := exec.Command("/bin/cat")
	stdin, err := cat.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	start(t, cat)
	pid := cat.Process.Pid
	token, err := StartToken(pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken = %q, %v", token, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for !zombie(t, pid) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := Signal(pid, token, 0); !errors.Is(err, ErrGone) {
		t.Fatalf("Signal 0 of a zombie = %v; want ErrGone", err)
	}
	// Linux answers for a zombie, so a launcher can record the token of a
	// child that already exited. macOS answers nothing.
	want := token
	if runtime.GOOS == "darwin" {
		want = ""
	}
	if got, err := StartToken(pid); err != nil || got != want {
		t.Fatalf("StartToken of a zombie = %q, %v; want %q", got, err, want)
	}
}
