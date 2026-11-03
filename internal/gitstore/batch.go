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

func (s *Store) invalidateBatchPool(gitDir string) {
	s.mu.Lock()
	pool := s.pools[gitDir]
	delete(s.pools, gitDir)
	s.mu.Unlock()
	if pool != nil {
		pool.closeAll()
	}
}

// BlobToCache fetches a git object and writes it to dstPath in a binary-safe manner.
// Uses a persistent cat-file --batch process to amortize process spawn and
// remote connection costs across multiple blob fetches.
func (s *Store) BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error) {
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

func (s *Store) ReadBlob(ctx context.Context, repo model.RepoConfig, objectOID string, maxBytes int64) ([]byte, error) {
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
func (s *Store) VerifyBlob(ctx context.Context, _ model.RepoConfig, objectOID string, cachePath string) (bool, error) {
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
			b, err := newBatchCatFile(p.gitDir, p.logger)
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
}

func newBatchCatFile(gitDir string, logger *slog.Logger) (*batchCatFile, error) {
	cmd := exec.Command("git", "cat-file", "--batch")
	// A lazy fetch must not prompt. nonInteractiveGitEnv's GIT_SSH_COMMAND would
	// override the repo's core.sshCommand, which an adopted worktree keeps.
	// Without a controlling terminal, SSH cannot prompt either, and
	// SSH_ASKPASS_REQUIRE=never keeps it from opening an askpass dialog.
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS_REQUIRE=never")
	stderr := &gitStderrLogger{logger: logger, gitDir: gitDir}
	cmd.Stderr = stderr
	configureCommandSession(cmd)

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
// redaction. A lazy fetch can print the promisor remote's URL, which may embed
// credentials.
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
	if line := strings.TrimSpace(string(w.line)); line != "" {
		w.logger.Warn("git cat-file stderr", "git_dir", w.gitDir, "line", auth.RedactString(line), "truncated", w.truncated)
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

func (b *batchCatFile) shutdown(kill bool) {
	b.closeOnce.Do(func() {
		if kill {
			if b.stdoutPipe != nil {
				_ = b.stdoutPipe.Close()
			}
			_ = killCommandProcessGroup(b.cmd)
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
// and streams the blob content directly to dstPath. Binary-safe (no string
// conversion of blob content).
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

	// Stream blob content to a temp file, then atomic rename. The blob cache is
	// reconstructible from git, so we prefer throughput over per-object fsync.
	f, err := os.CreateTemp(filepath.Dir(dstPath), ".artifact-fs-blob-*")
	if err != nil {
		// Drain the blob content so the protocol stays in sync.
		_, _ = io.CopyN(io.Discard, b.stdout, size+1) // +1 for trailing LF
		return 0, err
	}
	tmp := f.Name()
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
		os.Remove(tmp)
		if copyErr != nil {
			return 0, fmt.Errorf("batch read content: %w", copyErr)
		}
		return 0, fmt.Errorf("short read: got %d, want %d", written, size)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("close temp blob file: %w", closeErr)
	}

	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return 0, err
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
