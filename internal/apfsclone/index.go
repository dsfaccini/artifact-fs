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

// recordStats records in the index of worktree each path's stat data, as
// checkout-index records the stat data of a file it writes, so refresh hashes
// none of them. Git still hashes an entry it treats as racily clean: one whose
// mtime is not older than the index file's. It leaves the index alone when it
// is not a regular file, or when it is a split index or has a version that
// recordStats does not know. It writes the index as Git does, through
// index.lock, which it creates only when no other process holds it.
func recordStats(ctx context.Context, worktree string, paths []string, before time.Time) error {
	if len(paths) == 0 {
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
	if fi, err := os.Lstat(indexPath); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return nil
	}
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
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
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	patched, err := patchIndexStats(data, newHash, func(name string) (*unix.Stat_t, error) {
		if !want[name] {
			return nil, nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(worktree, filepath.FromSlash(name)), &st); err != nil {
			return nil, err
		}
		if st.Mtim.Sec >= before.Unix() {
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

// entryStatSize is the size of an index entry's stat fields: ctime, mtime,
// dev, ino, mode, uid, gid and size, each 32 bits.
const entryStatSize = 40

// patchIndexStats sets, in data, an index file, the stat fields of each entry
// that stat returns a result for, and then the trailing checksum, unless it is
// zero, as index.skipHash writes it. It keeps each entry's mode. It returns
// false and leaves data unchanged for a split index or a version other than
// 2, 3 or 4.
func patchIndexStats(data []byte, newHash func() hash.Hash, stat func(name string) (*unix.Stat_t, error)) (bool, error) {
	hashSize := newHash().Size()
	malformed := errors.New("the index is malformed")
	if len(data) < 12+hashSize || string(data[:4]) != "DIRC" {
		return false, malformed
	}
	version := binary.BigEndian.Uint32(data[4:])
	if version < 2 || version > 4 {
		return false, nil
	}
	count := binary.BigEndian.Uint32(data[8:])
	end := len(data) - hashSize
	type patch struct {
		offset int
		st     *unix.Stat_t
	}
	var patches []patch
	offset := 12
	var name []byte
	for range count {
		start := offset
		fixed := entryStatSize + hashSize + 2
		if offset+fixed > end {
			return false, malformed
		}
		flags := binary.BigEndian.Uint16(data[offset+fixed-2:])
		offset += fixed
		if flags&0x4000 != 0 {
			if version < 3 {
				return false, malformed
			}
			offset += 2
		}
		if version == 4 {
			strip, n := decodeVarint(data[offset:end])
			if n == 0 || strip > uint64(len(name)) {
				return false, malformed
			}
			offset += n
			suffix := bytes.IndexByte(data[offset:end], 0)
			if suffix < 0 {
				return false, malformed
			}
			name = append(name[:len(name)-int(strip)], data[offset:offset+suffix]...)
			offset += suffix + 1
		} else {
			length := bytes.IndexByte(data[offset:end], 0)
			if length < 0 {
				return false, malformed
			}
			name = append(name[:0], data[offset:offset+length]...)
			// An entry is padded with 1 to 8 NULs to a multiple of 8 bytes.
			offset = start + (offset-start+length+8)&^7
			if offset > end {
				return false, malformed
			}
		}
		st, err := stat(string(name))
		if err != nil {
			return false, err
		}
		if st != nil {
			patches = append(patches, patch{start, st})
		}
	}
	for offset+8 <= end {
		if string(data[offset:offset+4]) == "link" {
			return false, nil
		}
		size := binary.BigEndian.Uint32(data[offset+4:])
		if uint64(offset)+8+uint64(size) > uint64(end) {
			return false, malformed
		}
		offset += 8 + int(size)
	}
	if offset != end {
		return false, malformed
	}
	if len(patches) == 0 {
		return false, nil
	}
	for _, p := range patches {
		putStat(data[p.offset:], p.st)
	}
	if trailer := data[end:]; !bytes.Equal(trailer, make([]byte, hashSize)) {
		h := newHash()
		h.Write(data[:end])
		copy(trailer, h.Sum(nil))
	}
	return true, nil
}

// putStat writes st into the stat fields at the start of b, an index entry,
// as Git's fill_stat_data does, and keeps the entry's mode.
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
	be.PutUint32(b[36:], uint32(st.Size))
}

// decodeVarint decodes the variable-length integer that index version 4
// prefixes each path with, as Git's decode_varint does. It returns the value
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
