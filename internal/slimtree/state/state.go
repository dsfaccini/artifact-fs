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
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
)

// recordsDir holds one directory per worktree, named by its ID.
const recordsDir = "w"

// archivesDir holds what rm keeps of each worktree it removed, by ID and time.
const archivesDir = "archives"

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
	resolved, err := daemon.EnsurePrivateRoot(dir, "slimtree home", "slimtree home", recordsDir)
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

// Remove removes the record of the worktree at path, with its directory.
func (h *Home) Remove(path string) error {
	return os.RemoveAll(filepath.Join(h.dir, recordsDir, ID(path)))
}

// Archive writes a copy of r to archives/<ID>/<ns>/worktree.json, ns being
// the time in nanoseconds, and returns the recovery ref that belongs to that
// archive: refs/slimtree/archive/<ID>/<ns>/HEAD. Write the copy before the
// ref, so the copy names the repository of every ref a removal leaves.
func (h *Home) Archive(r Record) (string, error) {
	r.Version = recordVersion
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	id, ns := ID(r.Path), strconv.FormatInt(time.Now().UnixNano(), 10)
	dir := filepath.Join(h.dir, archivesDir, id, ns)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeFile(dir, recordFile, append(data, '\n')); err != nil {
		return "", err
	}
	return "refs/slimtree/archive/" + id + "/" + ns + "/HEAD", nil
}

// Records returns every record, sorted by path. It skips an entry of w/
// that holds no readable record, such as a stray file or a record a crash
// left empty, and passes each skip's cause to skipped when it is not nil.
func (h *Home) Records(skipped func(error)) ([]Record, error) {
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
		if e.IsDir() && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var r Record
		if err == nil {
			err = json.Unmarshal(data, &r)
		}
		if err != nil {
			if skipped != nil {
				skipped(fmt.Errorf("skipping %s, which holds no readable record: %w", filepath.Join(h.dir, recordsDir, e.Name()), err))
			}
			continue
		}
		records = append(records, r)
	}
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.Path, b.Path) })
	return records, nil
}

// Resolve returns the record of the worktree arg names. An arg that holds a
// path separator, or is . or .., is a path. Any other arg is a worktree's
// name, and then a path below the current directory. Resolve refuses a name
// that also names an existing path below the current directory other than
// that worktree, so a command never acts on another worktree than the
// directory the user sees.
func (h *Home) Resolve(arg string) (Record, error) {
	records, err := h.Records(nil)
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
			if _, err := os.Lstat(arg); err == nil {
				if here, err := Canonical(arg); err != nil || here != named[0].Path {
					return Record{}, fmt.Errorf("%s names the worktree %s and also ./%s; name the one you mean by its path", arg, named[0].Path, arg)
				}
			}
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

// writeFile writes data to name in dir through a temporary file, synced,
// and a rename, so neither a failed write nor a crash leaves a partial file.
func writeFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	if fileCreated != nil {
		fileCreated(f)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
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
