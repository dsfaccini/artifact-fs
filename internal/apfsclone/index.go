package apfsclone

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// recordStats records in the index of worktree the stat data of each path in
// sizes, as Git records the stat data of a file it checks out, so that Git
// does not hash the file. It records none for a path that is not a regular
// file of the size that sizes holds for it, or whose mtime is not older than
// the second of before. Git still hashes an entry whose mtime is not older
// than the index file's, which it treats as racily clean. recordStats leaves
// the index unchanged when the index is not a regular file, when its version
// is not 2, 3 or 4, when its object format is unknown, and when it has an
// extension that Git must understand to read it, such as a split index's
// link. It writes the index as Git does: it creates index.lock, which fails
// while another process holds it, gives the lock the index's mode, then
// renames the lock over the index. On a failure it removes only that lock.
func recordStats(ctx context.Context, worktree string, sizes map[string]int64, before time.Time) error {
	if len(sizes) == 0 {
		return nil
	}
	out, err := git(ctx, worktree, nil, "rev-parse", "--path-format=absolute", "--git-path", "index", "--show-object-format")
	if err != nil {
		return err
	}
	indexPath, format, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !ok {
		return fmt.Errorf("git rev-parse printed %q", out)
	}
	var newHash func() hash.Hash
	switch format {
	case "sha1":
		newHash = sha1.New
	case "sha256":
		newHash = sha256.New
	default:
		return nil
	}
	fi, err := os.Lstat(indexPath)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	lockPath := indexPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return fmt.Errorf("cannot lock the index of %s: %w", worktree, err)
	}
	committed := false
	defer func() {
		if !committed {
			lock.Close()
			os.Remove(lockPath)
		}
	}()
	// core.sharedRepository can give the index group permissions.
	if err := lock.Chmod(fi.Mode().Perm()); err != nil {
		return err
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	patched, err := patchIndexStats(data, newHash, func(name string) (*unix.Stat_t, error) {
		size, ok := sizes[name]
		if !ok {
			return nil, nil
		}
		// Refresh handles a file that is gone or no longer regular.
		var st unix.Stat_t
		if unix.Lstat(filepath.Join(worktree, filepath.FromSlash(name)), &st) != nil ||
			st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size != size || st.Mtim.Sec >= before.Unix() {
			return nil, nil
		}
		return &st, nil
	})
	if err != nil || !patched {
		return err
	}
	if _, err := lock.Write(data); err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if err := os.Rename(lockPath, indexPath); err != nil {
		return err
	}
	committed = true
	return nil
}

const (
	// entryStatSize is the size of an index entry's stat fields: ctime,
	// mtime, dev, ino, mode, uid, gid and size, each 32 bits.
	entryStatSize = 40
	// entryExtended is the flag of an entry with a second, extended flags
	// field.
	entryExtended = 0x4000
	// entryNameMask masks the name length in an entry's flags. A name of
	// this length or longer stores the mask, and ends at its NUL.
	entryNameMask = 0xfff
)

// patchIndexStats sets, in data, an index file, the stat fields of each entry
// that stat returns a stat for, and then the trailing checksum, unless it is
// zero, as index.skipHash writes it. It keeps each entry's mode and every
// other byte. It parses the entries as Git's create_from_disk does. It returns
// false and leaves data unchanged for a version other than 2, 3 or 4, for an
// extension whose signature does not start with an uppercase letter, which
// Git must understand to read the index, and when it patches no entry.
func patchIndexStats(data []byte, newHash func() hash.Hash, stat func(name string) (*unix.Stat_t, error)) (bool, error) {
	hashSize := newHash().Size()
	malformed := errors.New("the index is malformed")
	if len(data) < 12+hashSize || string(data[:4]) != "DIRC" {
		return false, malformed
	}
	be := binary.BigEndian
	version := be.Uint32(data[4:])
	if version < 2 || version > 4 {
		return false, nil
	}
	end := len(data) - hashSize
	var patches []int
	var stats []*unix.Stat_t
	offset := 12
	var name []byte
	for range be.Uint32(data[8:]) {
		start := offset
		nameAt := start + entryStatSize + hashSize + 2
		if nameAt > end {
			return false, malformed
		}
		flags := be.Uint16(data[nameAt-2:])
		if flags&entryExtended != 0 {
			nameAt += 2
		}
		length := int(flags & entryNameMask)
		if version == 4 {
			strip, n := decodeVarint(data[min(nameAt, end):end])
			if n == 0 || strip > uint64(len(name)) {
				return false, malformed
			}
			name = name[:len(name)-int(strip)]
			nameAt += n
			suffix := length - len(name)
			if length == entryNameMask {
				suffix = bytes.IndexByte(data[min(nameAt, end):end], 0)
			}
			if suffix < 0 || nameAt+suffix >= end || data[nameAt+suffix] != 0 {
				return false, malformed
			}
			name = append(name, data[nameAt:nameAt+suffix]...)
			offset = nameAt + suffix + 1
		} else {
			if length == entryNameMask {
				length = bytes.IndexByte(data[min(nameAt, end):end], 0)
			}
			if length < 0 || nameAt+length >= end || data[nameAt+length] != 0 {
				return false, malformed
			}
			name = append(name[:0], data[nameAt:nameAt+length]...)
			// The name ends with 1 to 8 NULs, to a multiple of 8 bytes.
			offset = start + (nameAt-start+length+8)&^7
			if offset > end {
				return false, malformed
			}
		}
		st, err := stat(string(name))
		if err != nil {
			return false, err
		}
		if st != nil {
			patches = append(patches, start)
			stats = append(stats, st)
		}
	}
	for offset+8 <= end {
		if signature := data[offset]; signature < 'A' || signature > 'Z' {
			return false, nil
		}
		size := uint64(be.Uint32(data[offset+4:]))
		if uint64(offset)+8+size > uint64(end) {
			return false, malformed
		}
		offset += 8 + int(size)
	}
	if len(patches) == 0 {
		return false, nil
	}
	for i, start := range patches {
		putStat(data[start:], stats[i])
	}
	if trailer := data[end:]; !bytes.Equal(trailer, make([]byte, hashSize)) {
		h := newHash()
		h.Write(data[:end])
		copy(trailer, h.Sum(nil))
	}
	return true, nil
}

// putStat writes st into the stat fields at the start of b, an index entry,
// as Git's fill_stat_data and munge_st_size do, and keeps the entry's mode.
func putStat(b []byte, st *unix.Stat_t) {
	be := binary.BigEndian
	be.PutUint32(b[0:], uint32(st.Ctim.Sec))
	be.PutUint32(b[4:], uint32(st.Ctim.Nsec))
	be.PutUint32(b[8:], uint32(st.Mtim.Sec))
	be.PutUint32(b[12:], uint32(st.Mtim.Nsec))
	be.PutUint32(b[16:], uint32(st.Dev))
	be.PutUint32(b[20:], uint32(st.Ino))
	be.PutUint32(b[28:], st.Uid)
	be.PutUint32(b[32:], st.Gid)
	// A size that is a nonzero multiple of 4 GiB would truncate to 0, which
	// marks an entry racily smudged.
	size := uint32(st.Size)
	if size == 0 && st.Size != 0 {
		size = 0x80000000
	}
	be.PutUint32(b[36:], size)
}

// decodeVarint decodes the variable-length integer that index version 4
// puts before each name, as Git's decode_varint does. It returns the value
// and the bytes read, or 0 bytes on overflow or truncation.
func decodeVarint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	val := uint64(c & 127)
	n := 1
	for c&128 != 0 {
		val++
		if val == 0 || val>>57 != 0 || n >= len(b) {
			return 0, 0
		}
		c = b[n]
		n++
		val = val<<7 + uint64(c&127)
	}
	return val, n
}
