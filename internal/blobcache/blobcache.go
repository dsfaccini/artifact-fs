// Package blobcache owns the hydrated blob cache on disk: its sharded layout,
// the counters status reports, and eviction.
//
// A cache directory may be shared by repos and by daemons with different state
// roots. Every change to its blobs and its counters happens under an exclusive
// flock on its stats file, so the counters match the blobs across processes.
package blobcache

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	statsName = "stats"
	// statsSize is the size of the counters, two little-endian int64 values.
	// A nonzero byte after them marks counters that an Install has not
	// written yet.
	statsSize = 16
	// installPrefix names a staged blob. Counting and eviction skip it.
	installPrefix = ".artifact-fs-install-"
	// evictBatch caps the blobs one lock hold removes, so a long eviction
	// does not stall installs.
	evictBatch = 128
)

// Stats counts the blobs in a cache directory.
type Stats struct {
	Blobs int64
	Bytes int64
}

// EvictResult reports what Evict removed and what remains.
type EvictResult struct {
	Evicted      int64
	EvictedBytes int64
	Remaining    Stats
}

// Path returns oid's cache path, <dir>/<first two characters of oid>/<oid>.
func Path(dir, oid string) string {
	if len(oid) < 2 {
		return filepath.Join(dir, oid)
	}
	return filepath.Join(dir, oid[:2], oid)
}

// Stage reserves a path in oid's shard for a blob that Install moves into
// place.
func Stage(dir, oid string) (string, error) {
	shard := filepath.Dir(Path(dir, oid))
	if err := os.MkdirAll(shard, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(shard, installPrefix+"*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// Staged is a blob in the file Stage reserved for it.
type Staged struct {
	OID  string
	Path string
}

// testHookAfterInstallMove, when set, runs after Install moves each blob.
var testHookAfterInstallMove func()

// Install moves staged blobs into their cache paths in order and counts them,
// all under one lock hold. A blob it replaces leaves the counters. It returns
// how many blobs it moved and the error that stopped the next one. From its
// first move until it writes the counters, the stats file marks them pending,
// so after a crash in between the next reader recounts. When it cannot count
// a moved blob or write the counters, it leaves them to that recount too.
func Install(dir string, blobs []Staged) (int, error) {
	lock, err := lockStats(dir)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	st, valid, err := readStats(lock)
	if err == nil && valid {
		_, err = lock.WriteAt([]byte{1}, statsSize)
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for ; n < len(blobs); n++ {
		path := Path(dir, blobs[n].OID)
		old, oldErr := os.Lstat(path)
		if err = os.Rename(blobs[n].Path, path); err != nil {
			break
		}
		if testHookAfterInstallMove != nil {
			testHookAfterInstallMove()
		}
		cur, curErr := os.Lstat(path)
		if curErr != nil {
			valid = false
		}
		if !valid {
			continue
		}
		if oldErr == nil && old.Mode().IsRegular() {
			st.Blobs--
			st.Bytes -= old.Size()
		}
		st.Blobs++
		st.Bytes += cur.Size()
	}
	if valid {
		_ = writeStats(lock, st)
	} else {
		_, _, _ = recountLocked(context.Background(), dir, lock)
	}
	return n, err
}

// Recount moves blobs from the flat layout of earlier versions, <dir>/<oid>,
// into their shards, and recounts the counters from the blobs on disk. The
// hydrator runs it when it starts, which corrects counters that a crash
// between a blob change and its counter update, or a change made outside
// this package, left wrong.
func Recount(dir string) (Stats, error) {
	if strings.TrimSpace(dir) == "" {
		return Stats{}, nil
	}
	lock, err := lockStats(dir)
	if err != nil {
		return Stats{}, err
	}
	defer lock.Close()
	st, _, err := recountLocked(context.Background(), dir, lock)
	return st, err
}

// ReadStats returns dir's counters. It recounts a directory whose counters are
// missing or pending.
func ReadStats(dir string) (Stats, error) {
	if strings.TrimSpace(dir) == "" {
		return Stats{}, nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return Stats{}, nil
	}
	lock, err := lockStats(dir)
	if err != nil {
		return Stats{}, err
	}
	defer lock.Close()
	st, valid, err := readStats(lock)
	if err != nil || valid {
		return st, err
	}
	st, _, err = recountLocked(context.Background(), dir, lock)
	return st, err
}

type candidate struct {
	path  string
	info  os.FileInfo
	atime time.Time
}

// testHookBeforeEvictBatch, when set, runs after Evict lists the cache and
// before each batch takes the lock.
var testHookBeforeEvictBatch func()

// Evict removes blobs in order of least recent access time until the cache
// holds at most maxBytes, or until ctx ends. A maxBytes of 0 empties the
// cache, the empty blob included. It first recounts the cache, so
// it also corrects counters that drifted. It unlinks each blob, so a handle
// open on it keeps reading the blob, and the next open of its path hydrates
// it again. A blob read or replaced after Evict listed it stays.
func Evict(ctx context.Context, dir string, maxBytes int64) (EvictResult, error) {
	if maxBytes < 0 {
		return EvictResult{}, errors.New("max bytes must not be negative")
	}
	if strings.TrimSpace(dir) == "" {
		return EvictResult{}, nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return EvictResult{}, nil
	}
	lock, err := lockStats(dir)
	if err != nil {
		return EvictResult{}, err
	}
	st, candidates, err := recountLocked(ctx, dir, lock)
	_ = lock.Close()
	res := EvictResult{Remaining: st}
	if err != nil || fits(st, maxBytes) {
		return res, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].atime.Before(candidates[j].atime) })
	for len(candidates) > 0 {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if testHookBeforeEvictBatch != nil {
			testHookBeforeEvictBatch()
		}
		var done bool
		candidates, done, err = evictBatchLocked(ctx, dir, maxBytes, candidates, &res)
		if err != nil || done {
			return res, err
		}
	}
	return res, nil
}

// evictBatchLocked removes up to evictBatch candidates under one lock hold and
// returns the candidates left. done says the cache fits. It writes the
// counters for every blob it removed, even when it fails.
func evictBatchLocked(ctx context.Context, dir string, maxBytes int64, candidates []candidate, res *EvictResult) (left []candidate, done bool, err error) {
	lock, err := lockStats(dir)
	if err != nil {
		return nil, false, err
	}
	defer lock.Close()
	st, valid, err := readStats(lock)
	if err == nil && !valid {
		st, _, err = recountLocked(ctx, dir, lock)
	}
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if writeErr := writeStats(lock, st); err == nil {
			err = writeErr
		}
		res.Remaining = st
	}()
	n := 0
	for ; n < len(candidates) && n < evictBatch && !fits(st, maxBytes); n++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		c := candidates[n]
		cur, statErr := os.Lstat(c.path)
		if statErr != nil || !os.SameFile(c.info, cur) || !accessTime(cur).Equal(c.atime) {
			continue
		}
		if err := os.Remove(c.path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, false, err
		}
		st.Blobs--
		st.Bytes -= cur.Size()
		res.Evicted++
		res.EvictedBytes += cur.Size()
	}
	return candidates[n:], fits(st, maxBytes), nil
}

// fits reports whether a cache with st fits maxBytes. Only an empty cache
// fits 0, because the empty blob holds no bytes.
func fits(st Stats, maxBytes int64) bool {
	return st.Bytes <= maxBytes && (maxBytes > 0 || st.Blobs == 0)
}

// lockStats opens dir's stats file and takes an exclusive flock on it.
// Closing the file releases the lock.
func lockStats(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, statsName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readStats reads the counters. valid is false when the file holds none yet,
// or holds counters an Install left pending.
func readStats(f *os.File) (st Stats, valid bool, err error) {
	var buf [statsSize + 1]byte
	n, err := f.ReadAt(buf[:], 0)
	if n < statsSize || buf[statsSize] != 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return Stats{}, false, err
		}
		return Stats{}, false, nil
	}
	st.Blobs = int64(binary.LittleEndian.Uint64(buf[:8]))
	st.Bytes = int64(binary.LittleEndian.Uint64(buf[8:]))
	return st, true, nil
}

// writeStats writes the counters and clears their pending mark.
func writeStats(f *os.File, st Stats) error {
	var buf [statsSize + 1]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(st.Blobs))
	binary.LittleEndian.PutUint64(buf[8:], uint64(st.Bytes))
	_, err := f.WriteAt(buf[:], 0)
	return err
}

// recountLocked moves flat-layout blobs into shards, recounts the counters
// from the blobs it lists, and returns them with the list. When ctx ends it
// returns without writing the counters, which leaves them as they were.
func recountLocked(ctx context.Context, dir string, lock *os.File) (Stats, []candidate, error) {
	if err := migrateFlat(ctx, dir); err != nil {
		return Stats{}, nil, err
	}
	blobs, err := listBlobs(ctx, dir)
	if err != nil {
		return Stats{}, nil, err
	}
	var st Stats
	for _, b := range blobs {
		st.Blobs++
		st.Bytes += b.info.Size()
	}
	return st, blobs, writeStats(lock, st)
}

// migrateFlat moves each blob of the flat layout into its shard. It stops
// between blobs when ctx ends. A blob it already moved stays valid.
func migrateFlat(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !isObjectOIDName(e.Name()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := Path(dir, e.Name())
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// listBlobs lists the blobs in dir's shards with their access times. It
// stops between shards when ctx ends.
func listBlobs(ctx context.Context, dir string) ([]candidate, error) {
	shards, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []candidate
	for _, shard := range shards {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		shardDir := filepath.Join(dir, shard.Name())
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !isObjectOIDName(e.Name()) || !strings.HasPrefix(e.Name(), shard.Name()) {
				continue
			}
			path := filepath.Join(shardDir, e.Name())
			info, err := os.Lstat(path)
			if err != nil {
				continue
			}
			out = append(out, candidate{path: path, info: info, atime: accessTime(info)})
		}
	}
	return out, nil
}

func isObjectOIDName(name string) bool {
	if len(name) != 40 && len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
