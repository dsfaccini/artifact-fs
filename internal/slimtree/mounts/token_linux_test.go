package mounts

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A recorded PID that now names a thread: pidfd_open fails for it, with
// EINVAL before Linux 6.16 and ENOENT from 6.16, and its token differs.
func TestSignalOfAPIDThatNamesAThreadIsGone(t *testing.T) {
	t.Parallel()
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	tid := 0
	for _, task := range tasks {
		if n, err := strconv.Atoi(task.Name()); err == nil && n != os.Getpid() {
			tid = n
			break
		}
	}
	if tid == 0 {
		t.Fatalf("this process has no thread besides its leader: %v", tasks)
	}
	if fd, err := unix.PidfdOpen(tid, 0); err == nil {
		unix.Close(fd)
		t.Fatalf("pidfd_open of thread %d succeeded; the case needs it to fail", tid)
	}
	// A SIGKILL that landed would end this test process.
	if err := Signal(tid, "recorded:1", syscall.SIGKILL); !errors.Is(err, ErrGone) {
		t.Fatalf("Signal of a thread's ID with another token = %v; want ErrGone", err)
	}
}

func zombie(t *testing.T, pid int) bool {
	t.Helper()
	fields, err := readStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	return fields[0] == "Z"
}
