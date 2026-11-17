package mounts

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
)

// start starts cmd and stops it, and its process group when it leads one, at
// cleanup.
func start(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func holderPIDs(hs []Holder) []int {
	var pids []int
	for _, h := range hs {
		pids = append(pids, h.PID)
	}
	slices.Sort(pids)
	return pids
}

// On a plain directory, no mount needed: each process that holds the
// directory by its working directory, an open file or a mapping.
func TestHoldersNamesEachProcessOnTheMountExceptTheIgnoredOneAndItsDescendants(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "workspace")
	notes := filepath.Join(root, "notes.txt")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"-sibling", 0o755); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the daemon: a shell whose child inherits its working
	// directory.
	daemon := exec.Command("/bin/sh", "-c", "sleep 60 & wait")
	daemon.Dir = filepath.Join(root, "sub")
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	start(t, daemon)
	inCwd := exec.Command("sleep", "60")
	inCwd.Dir = filepath.Join(root, "sub")
	start(t, inCwd)
	withFile := start(t, exec.Command("/bin/sh", "-c", `exec sleep 60 3<"$1"`, "sh", notes))
	inSibling := exec.Command("sleep", "60")
	inSibling.Dir = root + "-sibling"
	start(t, inSibling)
	mapper := helperCommand("map", notes)
	stdout, err := mapper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	start(t, mapper)
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "mapped\n" {
		t.Fatalf("mapper said %q, %v", line, err)
	}

	key, err := KeyOf(root)
	if err != nil {
		t.Fatal(err)
	}
	// The shells reach their sleeps and their redirect on their own time.
	var holders []Holder
	for len(holders) < 5 {
		if holders, err = Holders(t.Context(), root, key, 0); err != nil {
			t.Fatal(err)
		}
	}
	pids := holderPIDs(holders)
	for _, pid := range []int{daemon.Process.Pid, inCwd.Process.Pid, withFile.Process.Pid, mapper.Process.Pid} {
		if !slices.Contains(pids, pid) {
			t.Fatalf("holders %+v lack pid %d", holders, pid)
		}
	}
	if len(holders) != 5 || slices.Contains(pids, inSibling.Process.Pid) {
		t.Fatalf("holders = %+v; want the daemon, its sleep, and the processes with a working directory, an open file or a mapping", holders)
	}
	for _, h := range holders {
		if h.PID == inCwd.Process.Pid && (h.Command != "sleep" || h.ParentPID != os.Getpid()) {
			t.Fatalf("holder %+v; want sleep with this test as its parent", h)
		}
	}

	ignoring, err := Holders(t.Context(), root, key, daemon.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{inCwd.Process.Pid, withFile.Process.Pid, mapper.Process.Pid}
	slices.Sort(want)
	if got := holderPIDs(ignoring); !slices.Equal(got, want) {
		t.Fatalf("holders ignoring the daemon = %v; want %v, without the daemon and its child", got, want)
	}

	// The same paths on another mount, as a process in the directory
	// beneath a mount sees them.
	elsewhere, err := Holders(t.Context(), root, Key{ID: key.ID + 1, Dev: key.Dev + 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(elsewhere) != 0 {
		t.Fatalf("holders on another mount = %+v; want none", elsewhere)
	}
}

func TestKeyOfTakesAMountPointsDeviceFromTheMountTable(t *testing.T) {
	t.Parallel()
	mount := "/"
	if runtime.GOOS == "darwin" {
		mount = "/System/Volumes/Data"
	}
	key, err := KeyOf(mount)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(mount, &st); err != nil {
		t.Fatal(err)
	}
	want := uint64(st.Dev)
	if runtime.GOOS == "darwin" {
		want = uint64(uint32(st.Dev))
	}
	if key.Dev != want {
		t.Fatalf("KeyOf(%s).Dev = %#x; want %#x", mount, key.Dev, want)
	}
}
