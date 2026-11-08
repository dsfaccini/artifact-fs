package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type readBlobResult struct {
	data []byte
	err  error
}

type fetchBlobResult struct {
	size int64
	err  error
}

const maxReadBlobBytes int64 = 1<<31 - 1

// CloseBatchPool stops the git cat-file --batch processes that read gitDir's
// objects, and fails the reads waiting for one. A later read starts new ones.
func (s *Store) CloseBatchPool(gitDir string) {
	s.mu.Lock()
	pool := s.pools[gitDir]
	delete(s.pools, gitDir)
	s.mu.Unlock()
	if pool != nil {
		pool.closeAll()
	}
}

// BlobToCache fetches a git object and writes it into dstPath in place, in a
// binary-safe manner. Pass a path no reader opens, such as one
// blobcache.Stage reserved. A failed write removes dstPath. Uses a persistent
// cat-file --batch process to amortize process spawn and remote connection
// costs across multiple blob fetches.
func (s *Store) BlobToCache(ctx context.Context, repo model.RepoPaths, objectOID string, dstPath string) (size int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return 0, err
	}
	pool, err := s.getPool(repo.GitDir)
	if err != nil {
		return 0, err
	}
	batch, err := pool.acquire(ctx)
	if err != nil {
		return 0, err
	}
	size, err = fetchBatchToFile(ctx, batch, objectOID, dstPath)
	if err != nil {
		pool.discard(batch)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, err
		}
		// Process may have died or be desynchronized; discard and retry.
		batch, err = pool.acquire(ctx)
		if err != nil {
			return 0, err
		}
		size, err = fetchBatchToFile(ctx, batch, objectOID, dstPath)
		if err != nil {
			pool.discard(batch)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 0, err
			}
			// Retry also failed; close instead of returning a potentially
			// corrupted process to the pool.
			return 0, err
		}
	}
	pool.release(batch)
	return size, err
}

// FetchMissingBlobs fetches the objects in objectOIDs that the object store
// lacks in one fetch from the promisor remote. It runs the fetch that a lazy
// fetch runs for one missing object, so N blobs cost one fetch and one pack
// instead of N.
func (s *Store) FetchMissingBlobs(ctx context.Context, repo model.RepoPaths, objectOIDs []string) error {
	missing, err := missingObjects(ctx, repo.GitDir, objectOIDs)
	if err != nil || len(missing) == 0 {
		return err
	}
	// A filtered fetch from a remote that is not a promisor records it as one
	// in the repo config, so the fetch goes only to a promisor.
	remote, err := promisorRemote(ctx, repo.GitDir)
	if err == nil && remote == "" {
		err = errors.New("no promisor remote")
	}
	if err == nil {
		// The fetch downloads every wanted object again, present or not, so
		// it asks only for the missing ones.
		_, err = runLazyFetchGitInput(ctx, repo.GitDir, []byte(strings.Join(missing, "\n")+"\n"),
			"-c", "fetch.negotiationAlgorithm=noop", "fetch", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", "--stdin", "--", remote)
	}
	// The hydrator cancels a batch fetch only when it stops, which is not a
	// fetch failure. A timeout still logs.
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		s.logger.Warn("batch blob fetch failed, blobs will fetch one at a time", "repo", repo.Name, "blobs", len(missing), "error", auth.RedactLogString(err.Error()))
	}
	return err
}

// promisorRemote returns the remote git's lazy fetch tries first, and "" when
// there is none.
func promisorRemote(ctx context.Context, gitDir string) (string, error) {
	remotes, err := promisorRemotes(ctx, gitDir)
	if err != nil || len(remotes) == 0 {
		return "", err
	}
	return remotes[0], nil
}

// promisorRemotes returns the remotes git's lazy fetch tries, in order,
// following promisor_remote_config in git's promisor-remote.c. In config
// order, a remote is a promisor when remote.<name>.promisor is true or
// remote.<name>.partialclonefilter is set, and the remote that
// extensions.partialClone names goes last.
func promisorRemotes(ctx context.Context, gitDir string) ([]string, error) {
	entries, err := configEntries(ctx, gitDir, `^(remote\..+\.(promisor|partialclonefilter)|extensions\.partialclone)$`)
	if err != nil {
		return nil, err
	}
	// Git reads promisor as a boolean, so git parses it here too: an empty
	// value is false and a nonzero number is true.
	flags, err := configEntries(ctx, gitDir, `^remote\..+\.promisor$`, "--type=bool")
	if err != nil {
		return nil, err
	}
	var remotes []string
	partialClone := ""
	for _, entry := range entries {
		if entry.key == "extensions.partialclone" {
			partialClone = entry.value
			continue
		}
		rest := strings.TrimPrefix(entry.key, "remote.")
		dot := strings.LastIndexByte(rest, '.')
		name, subkey := rest[:dot], rest[dot+1:]
		if subkey == "promisor" {
			if len(flags) == 0 {
				return nil, errors.New("git config listed fewer promisor flags than promisor keys")
			}
			promisor := flags[0].value == "true"
			flags = flags[1:]
			if !promisor {
				continue
			}
		}
		if !slices.Contains(remotes, name) {
			remotes = append(remotes, name)
		}
	}
	if partialClone != "" {
		remotes = append(slices.DeleteFunc(remotes, func(r string) bool { return r == partialClone }), partialClone)
	}
	return remotes, nil
}

type configEntry struct {
	key, value string
}

// configEntries returns, in config order, the config entries whose names
// match pattern.
func configEntries(ctx context.Context, gitDir string, pattern string, typeArgs ...string) ([]configEntry, error) {
	args := append(append([]string{"config", "--null"}, typeArgs...), "--get-regexp", pattern)
	out, err := runGit(ctx, gitDir, args...)
	if err != nil {
		if exitErr, ok := errors.AsType[*gitCommandError](err); ok && exitErr.exitCode == 1 {
			return nil, nil
		}
		return nil, err
	}
	var entries []configEntry
	for record := range strings.SplitSeq(out, "\x00") {
		if record == "" {
			continue
		}
		key, value, _ := strings.Cut(record, "\n")
		entries = append(entries, configEntry{key: key, value: value})
	}
	return entries, nil
}

// missingObjects returns the objects in oids that the object store lacks.
// GIT_NO_LAZY_FETCH keeps the check from fetching them one at a time.
func missingObjects(ctx context.Context, gitDir string, oids []string) ([]string, error) {
	out, err := runGitInput(ctx, gitDir, []string{"GIT_NO_LAZY_FETCH=1"}, []byte(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var missing []string
	for line := range strings.Lines(out) {
		if oid, ok := strings.CutSuffix(strings.TrimSuffix(line, "\n"), " missing"); ok {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}

func fetchBatchToFile(ctx context.Context, batch *batchCatFile, objectOID string, dstPath string) (int64, error) {
	ch := make(chan fetchBlobResult, 1)
	go func() {
		size, err := batch.fetchToFile(objectOID, dstPath)
		ch <- fetchBlobResult{size: size, err: err}
	}()
	select {
	case r := <-ch:
		return r.size, r.err
	case <-ctx.Done():
		batch.kill()
		<-ch
		return 0, ctx.Err()
	}
}

func (s *Store) ReadBlob(ctx context.Context, repo model.RepoPaths, objectOID string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("negative max bytes: %d", maxBytes)
	}
	pool, err := s.getPool(repo.GitDir)
	if err != nil {
		return nil, err
	}
	batch, err := pool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	data, err := readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err == nil {
		pool.release(batch)
		return data, nil
	}
	if errors.Is(err, model.ErrBlobTooLarge) {
		pool.discard(batch)
		return nil, err
	}
	pool.discard(batch)

	batch, err = pool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	data, err = readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err != nil {
		if errors.Is(err, model.ErrBlobTooLarge) {
			pool.discard(batch)
			return nil, err
		}
		pool.discard(batch)
		return nil, err
	}
	pool.release(batch)
	return data, nil
}

func readBatchBlob(ctx context.Context, batch *batchCatFile, objectOID string, maxBytes int64) ([]byte, error) {
	ch := make(chan readBlobResult, 1)
	go func() {
		data, err := batch.readBlob(objectOID, maxBytes)
		ch <- readBlobResult{data: data, err: err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		batch.kill()
		return nil, ctx.Err()
	}
}

// VerifyBlob hashes in process instead of spawning git: it runs on the first
// read of every cached blob after a daemon start.
func (s *Store) VerifyBlob(ctx context.Context, _ model.RepoPaths, objectOID string, cachePath string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return model.FileMatchesBlobOID(cachePath, objectOID)
}

func (s *Store) getPool(gitDir string) (*batchPool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("git store closed")
	}
	if p, ok := s.pools[gitDir]; ok {
		return p, nil
	}
	p := &batchPool{gitDir: gitDir, logger: s.logger, maxSize: s.poolMaxSize, all: map[*batchCatFile]struct{}{}, changed: make(chan struct{})}
	s.pools[gitDir] = p
	return p, nil
}

// batchPool maintains a pool of reusable cat-file --batch processes so
// multiple hydrator workers can fetch blobs concurrently.
type batchPool struct {
	mu       sync.Mutex
	free     []*batchCatFile
	gitDir   string
	logger   *slog.Logger
	maxSize  int
	creating int
	all      map[*batchCatFile]struct{}
	closed   bool
	changed  chan struct{}
}

func (p *batchPool) acquire(ctx context.Context) (*batchCatFile, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("git batch pool closed")
		}
		if n := len(p.free); n > 0 {
			b := p.free[n-1]
			p.free = p.free[:n-1]
			p.mu.Unlock()
			if b.alive() {
				return b, nil
			}
			p.discard(b)
			continue
		}
		if len(p.all)+p.creating < p.maxSize {
			p.creating++
			p.mu.Unlock()
			b, err := newBatchCatFile(ctx, p.gitDir, p.logger)
			p.mu.Lock()
			p.creating--
			if err == nil && !p.closed {
				p.all[b] = struct{}{}
			}
			closed := p.closed
			p.signalLocked()
			p.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if closed {
				b.kill()
				return nil, errors.New("git batch pool closed")
			}
			return b, nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (p *batchPool) release(b *batchCatFile) {
	if !b.alive() {
		p.discard(b)
		return
	}
	p.mu.Lock()
	if !p.closed && len(p.all) <= p.maxSize {
		p.free = append(p.free, b)
		p.signalLocked()
		p.mu.Unlock()
		return
	}
	delete(p.all, b)
	p.signalLocked()
	p.mu.Unlock()
	b.close()
}

func (p *batchPool) discard(b *batchCatFile) {
	p.mu.Lock()
	if _, ok := p.all[b]; ok {
		delete(p.all, b)
		p.signalLocked()
	}
	p.mu.Unlock()
	b.kill()
}

func (p *batchPool) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *batchPool) closeAll() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	all := make([]*batchCatFile, 0, len(p.all))
	for b := range p.all {
		all = append(all, b)
	}
	p.all = map[*batchCatFile]struct{}{}
	p.free = nil
	p.signalLocked()
	p.mu.Unlock()
	// Signal every process before waiting for any, so a close waits for the
	// slowest one, not for each in turn.
	for _, b := range all {
		b.terminate()
	}
	for _, b := range all {
		b.kill()
	}
}

func (p *batchPool) setMaxSize(n int) {
	var extras []*batchCatFile
	p.mu.Lock()
	p.maxSize = n
	for len(p.all) > n && len(p.free) > 0 {
		idx := len(p.free) - 1
		b := p.free[idx]
		p.free = p.free[:idx]
		delete(p.all, b)
		extras = append(extras, b)
	}
	p.signalLocked()
	p.mu.Unlock()
	for _, b := range extras {
		b.close()
	}
}

// batchCatFile manages a persistent `git cat-file --batch` process. The
// persistent process amortizes process startup and (on blobless clones)
// remote connection costs across multiple blob fetches. Callers must ensure
// exclusive access (the batchPool handles this).
type batchCatFile struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutPipe io.ReadCloser
	stdout     *bufio.Reader
	stderr     *gitStderrLogger
	logger     *slog.Logger
	closeOnce  sync.Once
	// terminateOnce guards the signals, which closeAll sends before kill.
	terminateOnce sync.Once
}

func newBatchCatFile(ctx context.Context, gitDir string, logger *slog.Logger) (*batchCatFile, error) {
	env, err := lazyFetchEnv(ctx, gitDir, nil)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Env = append(append(os.Environ(), "GIT_DIR="+gitDir), env...)
	stderr := &gitStderrLogger{logger: logger, gitDir: gitDir}
	cmd.Stderr = stderr
	configureCommandSession(cmd)
	// A lazy fetch's descendant can keep the stderr pipe open after a kill.
	cmd.WaitDelay = gitCommandWaitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("batch cat-file stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("batch cat-file stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("batch cat-file start: %w", err)
	}
	return &batchCatFile{
		cmd:        cmd,
		stdin:      stdin,
		stdoutPipe: stdout,
		stdout:     bufio.NewReaderSize(stdout, 256*1024),
		stderr:     stderr,
		logger:     logger,
	}, nil
}

// gitStderrLogger logs each stderr line of a long-lived git process through
// auth.RedactLogString, like the retry logs of every other Git command. A lazy
// fetch can print the promisor remote, which may embed credentials or name a
// private repository; the log keeps Git's error and drops the remote. It
// buffers no more of a line than it logs: the server shapes this stderr, and
// redaction's cost grows faster than the line's length.
type gitStderrLogger struct {
	logger    *slog.Logger
	gitDir    string
	line      []byte
	truncated bool
}

func (w *gitStderrLogger) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk, rest, found := bytes.Cut(p, []byte{'\n'})
		if room := maxGitErrorBytes - len(w.line); len(chunk) > room {
			chunk = chunk[:room]
			w.truncated = true
		}
		w.line = append(w.line, chunk...)
		if !found {
			break
		}
		w.flush()
		p = rest
	}
	return n, nil
}

func (w *gitStderrLogger) flush() {
	text := string(w.line)
	if w.truncated {
		// A line with no space keeps nothing, and is still logged as truncated.
		text = withoutPartialLastToken(text)
	}
	line := auth.RedactLogString(strings.TrimSpace(text))
	truncated := w.truncated
	if len(line) > maxGitErrorBytes {
		line, truncated = line[:maxGitErrorBytes], true
	}
	if line != "" || truncated {
		w.logger.Warn("git cat-file stderr", "git_dir", w.gitDir, "line", line, "truncated", truncated)
	}
	w.line, w.truncated = w.line[:0], false
}

func (b *batchCatFile) alive() bool {
	return b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil
}

func (b *batchCatFile) close() {
	b.shutdown(false)
}

func (b *batchCatFile) kill() {
	b.shutdown(true)
}

// terminate closes b's stdout and stops its session with
// terminateCommandProcessGroup, so a lazy fetch it runs removes its locks. It
// does not wait for b to exit.
func (b *batchCatFile) terminate() {
	b.terminateOnce.Do(func() {
		if b.stdoutPipe != nil {
			_ = b.stdoutPipe.Close()
		}
		_ = terminateCommandProcessGroup(b.cmd)
	})
}

func (b *batchCatFile) shutdown(kill bool) {
	b.closeOnce.Do(func() {
		if kill {
			b.terminate()
		}
		if b.stdin != nil {
			_ = b.stdin.Close()
		}
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Wait()
			b.stderr.flush()
		}
	})
}

// fetchToFile writes oid to the batch process stdin, reads the response header
// and streams the blob content into dstPath in place. Binary-safe (no string
// conversion of blob content). A failed write removes dstPath.
func (b *batchCatFile) fetchToFile(oid string, dstPath string) (int64, error) {
	if b.cmd == nil || b.stdin == nil {
		return 0, errors.New("batch cat-file process not running")
	}

	// Request the object
	if _, err := fmt.Fprintf(b.stdin, "%s\n", oid); err != nil {
		return 0, fmt.Errorf("batch write: %w", err)
	}

	size, err := b.readObjectSize(oid)
	if err != nil {
		return 0, err
	}

	// The blob cache is reconstructible from git, so we prefer throughput over
	// per-object fsync.
	f, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		// Drain the blob content so the protocol stays in sync.
		_, _ = io.CopyN(io.Discard, b.stdout, size+1) // +1 for trailing LF
		return 0, err
	}
	written, copyErr := io.CopyN(f, b.stdout, size)
	// Read the trailing LF that git appends after the content. If this fails
	// the batch protocol is desynchronized and the caller must discard the
	// process.
	if lf, lfErr := b.stdout.ReadByte(); copyErr == nil {
		if lfErr != nil {
			copyErr = fmt.Errorf("batch read trailing LF: %w", lfErr)
		} else if lf != '\n' {
			copyErr = fmt.Errorf("batch read trailing byte: got %#x, want newline", lf)
		}
	}
	closeErr := f.Close()

	if copyErr != nil || written != size {
		os.Remove(dstPath)
		if copyErr != nil {
			return 0, fmt.Errorf("batch read content: %w", copyErr)
		}
		return 0, fmt.Errorf("short read: got %d, want %d", written, size)
	}
	if closeErr != nil {
		os.Remove(dstPath)
		return 0, fmt.Errorf("close blob file: %w", closeErr)
	}
	return size, nil
}

func (b *batchCatFile) readBlob(oid string, maxBytes int64) ([]byte, error) {
	if b.cmd == nil || b.stdin == nil {
		return nil, errors.New("batch cat-file process not running")
	}
	if _, err := fmt.Fprintf(b.stdin, "%s\n", oid); err != nil {
		return nil, fmt.Errorf("batch write: %w", err)
	}
	size, err := b.readObjectSize(oid)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("negative blob size: %d", size)
	}
	if size > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	if size > maxReadBlobBytes {
		return nil, model.ErrBlobTooLarge
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(b.stdout, data); err != nil {
		return nil, fmt.Errorf("batch read content: %w", err)
	}
	lf, err := b.stdout.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("batch read trailing LF: %w", err)
	}
	if lf != '\n' {
		return nil, fmt.Errorf("batch read trailing byte: got %#x, want newline", lf)
	}
	return data, nil
}

func (b *batchCatFile) readObjectSize(oid string) (int64, error) {
	// Read response header: "<oid> SP <type> SP <size> LF" or "<oid> SP missing LF"
	header, err := b.stdout.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("batch read header: %w", err)
	}
	header = strings.TrimRight(header, "\n")
	fields := strings.Fields(header)
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected batch header: %q", header)
	}
	if fields[0] != oid {
		return 0, fmt.Errorf("unexpected batch object: got %q, want %q", fields[0], oid)
	}
	if fields[1] == "missing" {
		return 0, fmt.Errorf("object %s missing", oid)
	}
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected batch header: %q", header)
	}
	if fields[1] != "blob" {
		return 0, fmt.Errorf("unexpected batch object type: %q", fields[1])
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", fields[2], err)
	}
	return size, nil
}
