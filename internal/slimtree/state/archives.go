package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Archived is an archive rm left, archives/<ID>/<ns>/.
type Archived struct {
	Dir string
	// Ref is the recovery ref that belongs to the archive.
	Ref string
	// Made is when rm made the archive.
	Made time.Time
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Archives returns every archive in the home. It passes skipped each entry
// of archives/ that is not a directory Archive makes, such as a symlink,
// which a removal would follow out of the home.
func (h *Home) Archives(skipped func(error)) ([]Archived, error) {
	root := filepath.Join(h.dir, archivesDir)
	ids, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	notArchive := func(path string) {
		skipped(fmt.Errorf("kept %s: not an archive slimtree makes", path))
	}
	var archives []Archived
	for _, id := range ids {
		dir := filepath.Join(root, id.Name())
		if !id.IsDir() || !idPattern.MatchString(id.Name()) {
			notArchive(dir)
			continue
		}
		times, err := os.ReadDir(dir)
		if err != nil {
			skipped(err)
			continue
		}
		for _, t := range times {
			ns, err := strconv.ParseInt(t.Name(), 10, 64)
			if !t.IsDir() || err != nil || ns <= 0 || strconv.FormatInt(ns, 10) != t.Name() {
				notArchive(filepath.Join(dir, t.Name()))
				continue
			}
			archives = append(archives, Archived{
				Dir:  filepath.Join(dir, t.Name()),
				Ref:  "refs/slimtree/archive/" + id.Name() + "/" + t.Name() + "/HEAD",
				Made: time.Unix(0, ns),
			})
		}
	}
	return archives, nil
}

// Record reads the copy of the record Archive wrote into a.
func (a Archived) Record() (Record, error) {
	var r Record
	data, err := os.ReadFile(filepath.Join(a.Dir, recordFile))
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	return r, err
}

// RemoveArchive removes a, and its ID's directory once it holds no other
// archive.
func (h *Home) RemoveArchive(a Archived) error {
	if err := os.RemoveAll(a.Dir); err != nil {
		return err
	}
	// The directory keeps the ID's other archives.
	os.Remove(filepath.Dir(a.Dir))
	return nil
}

// ArchivesUse returns the bytes the archives take on disk, and the size of
// the file system that holds the home.
func (h *Home) ArchivesUse() (used, disk uint64, err error) {
	err = filepath.WalkDir(filepath.Join(h.dir, archivesDir), func(path string, _ fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return &os.PathError{Op: "lstat", Path: path, Err: err}
		}
		// st_blocks counts 512-byte units on Linux and macOS.
		used += uint64(st.Blocks) * 512
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	disk, err = diskSize(h.dir)
	return used, disk, err
}
