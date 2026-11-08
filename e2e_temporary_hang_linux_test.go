package main

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TEMPORARY: freezes the Go runtime, to show the fuse-e2e watchdog's dump.
// A goroutine in a raw read never stops for a stop-the-world, so runtime.GC
// waits forever and no timer, -test.timeout's included, runs again.
func TestE2ETemporaryHang(t *testing.T) {
	if os.Getenv("AFS_TEMPORARY_HANG") != "1" {
		t.Skip("set AFS_TEMPORARY_HANG=1 to freeze the runtime")
	}
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	go func() {
		var b [1]byte
		syscall.RawSyscall(syscall.SYS_READ, uintptr(p[0]), uintptr(unsafe.Pointer(&b[0])), 1)
	}()
	time.Sleep(time.Second)
	t.Log("stopping the world")
	runtime.GC()
	t.Log("the world stopped and restarted")
}
