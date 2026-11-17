// Package execfile writes files that a process may exec.
package execfile

import (
	"io/fs"
	"os"
	"syscall"
)

// testHookWhileOpen, when set, runs in Write while the file is open for
// writing.
var testHookWhileOpen func()

// Write writes data to the file at path as os.WriteFile does, while no fork
// can start. A child forked while the file is open for writing holds the write
// descriptor until the child execs (golang/go#22315), and an exec of the file
// meanwhile fails with ETXTBSY. Git then fails, or for an fsmonitor hook
// retries the hook's v1 protocol, which misreads a v2 answer. Every fork
// takes syscall.ForkLock for writing, so Write holds it for reading.
func Write(path string, data []byte, perm fs.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if testHookWhileOpen != nil {
		testHookWhileOpen()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
