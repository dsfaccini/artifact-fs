// Package state keeps slimtree's home: the global lock and one record per
// worktree slimtree made.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
)

// recordsDir holds one directory per worktree, named by its ID.
const recordsDir = "w"

// recordFile is the record inside a worktree's directory.
const recordFile = "worktree.json"

// recordVersion is Record's format. It rises when a field is renamed or
// removed or changes its meaning.
const recordVersion = 1

// Home is slimtree's home directory, private and on a native file system.
type Home struct {
	dir string
}

// Open creates dir 0700 when it is missing, with its records directory, and
// checks that both are private and on a file system no FUSE mount serves. A
// daemon must not serve its own state, and another user who can write there
// could replace what the daemon runs.
func Open(dir string) (*Home, error) {
	resolved, err := daemon.EnsurePrivateRoot(dir, recordsDir)
	if err != nil {
		return nil, err
	}
	magic, name, err := fileSystem(resolved)
	if err != nil {
		return nil, &os.PathError{Op: "statfs", Path: resolved, Err: err}
	}
	if onFUSE(runtime.GOOS, magic, name) {
		return nil, fmt.Errorf("refusing slimtree home %s: on a FUSE mount; set SLIMTREE_HOME to a directory on a native file system", resolved)
	}
	return &Home{dir: resolved}, nil
}

// Dir is the home, absolute and with its symlinks resolved.
func (h *Home) Dir() string { return h.dir }

// Record is what slimtree keeps about a worktree it made, in
// w/<ID>/worktree.json.
type Record struct {
	Version int    `json:"version"`
	Backend string `json:"backend"`
	// Path is the worktree, canonical as Canonical returns it.
	Path string `json:"path"`
	// CommonDir is the repository's common git dir.
	CommonDir string `json:"common_dir"`
	// Branch is the branch the worktree was made on.
	Branch          string    `json:"branch"`
	Created         time.Time `json:"created"`
	SlimtreeVersion string    `json:"slimtree_version"`
	// StateDir is an artifactfs worktree's artifact-fs state root.
	StateDir string `json:"state_dir,omitempty"`
}

// Name is the worktree's directory name, by which commands find it.
func (r Record) Name() string { return filepath.Base(r.Path) }

// ID names a worktree's record directory: the first 16 hex digits of the
// SHA-256 of its canonical path. Short IDs keep the control socket of a
// daemon whose state lives below the record within macOS's 104 bytes.
func ID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:16]
}

// Write writes r atomically, so a reader never sees half a record.
func (h *Home) Write(r Record) error {
	r.Version = recordVersion
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(h.dir, recordsDir, ID(r.Path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile(dir, recordFile, append(data, '\n'))
}

// Records returns every record, sorted by path.
func (h *Home) Records() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(h.dir, recordsDir))
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, e := range entries {
		path := filepath.Join(h.dir, recordsDir, e.Name(), recordFile)
		data, err := os.ReadFile(path)
		// A directory without a record holds a worktree's state from a make
		// that has not written its record yet.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		records = append(records, r)
	}
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.Path, b.Path) })
	return records, nil
}

// Resolve returns the record of the worktree arg names. An arg that holds a
// path separator, or is . or .., is a path. Any other arg is a worktree's
// name, and then a path below the current directory.
func (h *Home) Resolve(arg string) (Record, error) {
	records, err := h.Records()
	if err != nil {
		return Record{}, err
	}
	if !strings.ContainsRune(arg, filepath.Separator) && arg != "." && arg != ".." {
		var named []Record
		for _, r := range records {
			if r.Name() == arg {
				named = append(named, r)
			}
		}
		switch len(named) {
		case 1:
			return named[0], nil
		case 0:
		default:
			paths := make([]string, len(named))
			for i, r := range named {
				paths[i] = r.Path
			}
			return Record{}, fmt.Errorf("%d worktrees are named %s; name one by its path: %s", len(named), arg, strings.Join(paths, ", "))
		}
	}
	path, err := Canonical(arg)
	if err != nil {
		return Record{}, err
	}
	for _, r := range records {
		if r.Path == path {
			return r, nil
		}
	}
	return Record{}, fmt.Errorf("slimtree made no worktree %s", arg)
}

// Canonical returns path absolute, with the symlinks of its longest existing
// prefix resolved, so a worktree that is gone keeps the path it was made at.
func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(abs) == abs {
		return "", err
	}
	parent, err := Canonical(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// fileCreated, when set, runs on writeFile's temporary file right after its
// create. Tests use it to fail the write that follows.
var fileCreated func(*os.File)

// writeFile writes data to name in dir through a temporary file and a
// rename, so a failed write leaves no partial file.
func writeFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	if fileCreated != nil {
		fileCreated(f)
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
