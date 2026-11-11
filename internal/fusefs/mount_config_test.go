//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

// mountAttempt is one fuse.Mount call.
type mountAttempt struct {
	cfg    fuse.MountConfig
	server fuse.Server
}

// mountRepoAttempts runs MountRepoWithGate with fuse.Mount replaced and returns
// each call it made. Every attempt fails, so nothing mounts. An FSKit attempt
// fails as unavailable, which lets auto try the kernel extension.
func mountRepoAttempts(t *testing.T, repo model.RepoPaths, logger *slog.Logger, redact func(string) string) []mountAttempt {
	t.Helper()
	t.Setenv(backendEnv, "")
	var attempts []mountAttempt
	saved := fuseMount
	fuseMount = func(_ context.Context, _ string, server fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		attempts = append(attempts, mountAttempt{cfg: *cfg, server: server})
		if cfg.Backend == fuse.BackendFSKit {
			return nil, fuse.ErrFSKitUnavailable
		}
		return nil, errors.New("package tests do not mount")
	}
	t.Cleanup(func() { fuseMount = saved })
	if _, err := MountRepoWithGate(context.Background(), context.Background(), repo, nil, nil, logger, redact); err == nil {
		t.Fatal("MountRepoWithGate succeeded without a mount")
	}
	if len(attempts) == 0 {
		t.Fatal("MountRepoWithGate never reached fuse.Mount")
	}
	return attempts
}

// unredacted is a mount's redact that keeps each line as it is.
func unredacted(line string) string { return line }

// The fuse library reports op errors and failed replies only to an
// ErrorLogger. A bug it answered with EIO reaches the daemon log as an error,
// and a short FSKit read it answered with EIO as a warning. Any other op error,
// which the library also logs for expected errnos, goes out at Debug, and a
// failed reply as a warning. Each carries the repo and goes through the
// mount's redact. The remote is a local path, which only that redact removes.
func TestMountRepoLogsFuseErrorsRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	remote := "/srv/git/private/repo.git"
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	redact := func(line string) string { return auth.RedactLogString(line, remote) }
	for _, attempt := range mountRepoAttempts(t, repo, logger, redact) {
		cfg := attempt.cfg
		if cfg.ErrorLogger == nil {
			t.Fatal("mount config has no ErrorLogger")
		}
		// The library's formats for an op error and for a failed reply.
		for _, line := range []struct {
			log       func()
			wantLevel string
			wantText  string
			remote    bool
		}{
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> Error: %q", 2, &fuseops.ReadFileOp{}, "fetch "+remote+": exit status 128")
				},
				wantLevel: "DEBUG",
				wantText:  "*fuseops.ReadFileOp",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: panic: %v\n%s", 3, &fuseops.LookUpInodeOp{}, "Error", "fetch "+remote+": nil map", "goroutine 7 [running]:\nruntime/debug.Stack()")
				},
				wantLevel: "ERROR",
				wantText:  "panic: fetch",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: panic: %v\n%s", 4, &fuseops.WriteFileOp{}, "Callback", "callback for "+remote, "goroutine 8 [running]:")
				},
				wantLevel: "ERROR",
				wantText:  "Callback: panic",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 5, &fuseops.OpenFileOp{}, "Error", fmt.Errorf("fuseutil: the method exited without returning: %w", syscall.EIO))
				},
				wantLevel: "ERROR",
				wantText:  "exited without returning",
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 6, &fuseops.ReadDirOp{}, "Error", fmt.Errorf("%T.BytesRead is %d, outside its %d-byte buffer: %w", &fuseops.ReadDirOp{}, 5000, 4096, syscall.EIO))
				},
				wantLevel: "ERROR",
				wantText:  "outside its 4096-byte buffer",
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 7, &fuseops.ReadFileOp{}, "Error", fmt.Errorf("read of inode %d at offset %d returned %d bytes, want %d for size %d: %w", 5, 0, 3, 10, 10, syscall.EIO))
				},
				wantLevel: "WARN",
				wantText:  "returned 3 bytes, want 10",
			},
			{
				log: func() {
					cfg.ErrorLogger.Print("writeMessage: no such device [16 0 0 0 254 255 255 255 2 0 0 0 0 0 0 0]")
				},
				wantLevel: "WARN",
				wantText:  "writeMessage",
			},
		} {
			buf.Reset()
			line.log()
			var rec struct{ Level, Repo, Error string }
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log output %q is not one JSON record: %v", buf.String(), err)
			}
			if rec.Level != line.wantLevel || rec.Repo != repo.Name || !strings.Contains(rec.Error, line.wantText) {
				t.Fatalf("log record = %+v, want level %s for repo %q containing %q", rec, line.wantLevel, repo.Name, line.wantText)
			}
			if strings.Contains(rec.Error, remote) || line.remote && !strings.Contains(rec.Error, "REDACTED_REMOTE") {
				t.Fatalf("log output %q does not redact the remote", buf.String())
			}
		}
	}
}

// A mount without a redact still removes the remotes auth.RedactLogString
// detects from the fuse library's error lines.
func TestMountRepoWithoutRedactRedactsDetectedRemotes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	for _, attempt := range mountRepoAttempts(t, model.RepoPaths{Name: "repo", MountPath: t.TempDir()}, logger, nil) {
		buf.Reset()
		attempt.cfg.ErrorLogger.Print("writeMessage: fetch https://git.example.com/private/repo.git failed")
		var rec struct{ Error string }
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("log output %q is not one JSON record: %v", buf.String(), err)
		}
		if strings.Contains(rec.Error, "git.example.com") || !strings.Contains(rec.Error, "REDACTED_REMOTE") {
			t.Fatalf("log error %q does not redact the remote", rec.Error)
		}
	}
}

// The fuse library replies EIO to an op error that carries no errno and logs
// only the error's text. So the error names the op's inode and its path, and
// for an op on a name, the parent's inode and the child's path.
func TestOpErrorWithoutErrnoNamesItsInodeAndPath(t *testing.T) {
	unsized := model.BaseNode{Path: "dir/unsized.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob-unsized", SizeState: "unknown"}
	f := newCacheFixture(t, baseDir("dir", "tree"), unsized, baseFile("dir/sized.txt", "blob-sized", 0o100644))
	dir := f.lookup(fuseops.RootInodeID, "dir")
	sized := f.lookup(dir, "sized.txt")
	f.blobs.errorsByOID = map[string]error{
		"blob-unsized": errors.New("fetch blob-unsized failed"),
		"blob-sized":   errors.New("fetch blob-sized failed"),
	}
	for _, c := range []struct {
		op   string
		err  error
		want string
	}{
		{"lookup", f.fs.LookUpInode(f.ctx, &fuseops.LookUpInodeOp{Parent: dir, Name: "unsized.txt"}), fmt.Sprintf("parent inode %d, path dir/unsized.txt: fetch blob-unsized failed", dir)},
		{"open", f.fs.OpenFile(f.ctx, &fuseops.OpenFileOp{Inode: sized}), fmt.Sprintf("inode %d, path dir/sized.txt: fetch blob-sized failed", sized)},
	} {
		if errno := kernelErrno(c.err); errno != syscall.EIO || c.err.Error() != c.want {
			t.Errorf("%s = %v (errno %v), want %q (EIO)", c.op, c.err, errno, c.want)
		}
	}
}

// An op error that carries an errno reaches the kernel with it, as toErrno
// returns it.
func TestOpErrorWithErrnoKeepsIt(t *testing.T) {
	f := newCacheFixture(t, baseDir("dir", "tree"))
	for _, c := range []struct {
		op    string
		err   error
		errno syscall.Errno
		want  string
	}{
		{"mkdir of an existing name", f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "dir", Mode: 0o755}), syscall.EEXIST, "file exists: file already exists"},
		{"rmdir of .git", f.fs.RmDir(f.ctx, &fuseops.RmDirOp{Parent: fuseops.RootInodeID, Name: ".git"}), syscall.EPERM, "operation not permitted"},
	} {
		if errno := kernelErrno(c.err); errno != c.errno || c.err.Error() != c.want {
			t.Errorf("%s = %v (errno %v), want %q (%v)", c.op, c.err, errno, c.want, c.errno)
		}
	}
}

// replyRecord is a record logReply writes.
type replyRecord struct {
	Level, Msg, Repo, Op string
	Inode                uint64
	Type                 string
	Generation           int64
	Path                 string
}

// replyOps runs one op of each kind that replies with an entry or with
// attributes, on a fixture that serves secret/file.txt. It returns the
// records the ops should log at Debug, with each path through redactSecret.
func replyOps(t *testing.T, f *cacheFixture) []replyRecord {
	t.Helper()
	gen := f.resolver.Generation()
	dir := f.lookup(fuseops.RootInodeID, "secret")
	f.getattr(dir)
	if _, ok := readDirPlus(t, f.fs, dir)["file.txt"]; !ok {
		t.Fatal("ReadDirPlus listed no file.txt")
	}
	// A path keeps its inode number, so the lookup names the inode the
	// listing replied with.
	file := f.lookup(dir, "file.txt")
	mkdir := &fuseops.MkDirOp{Parent: dir, Name: "new", Mode: 0o755}
	if err := f.fs.MkDir(f.ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	create := &fuseops.CreateFileOp{Parent: dir, Name: "created.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
	})
	symlink := &fuseops.CreateSymlinkOp{Parent: dir, Name: "link", Target: "file.txt"}
	if err := f.fs.CreateSymlink(f.ctx, symlink); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o600)
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	record := func(msg, op string, inode fuseops.InodeID, typ string, gen int64, path string) replyRecord {
		return replyRecord{Level: "DEBUG", Msg: msg, Repo: "repo", Op: op, Inode: uint64(inode), Type: typ, Generation: gen, Path: path}
	}
	return []replyRecord{
		record("fuse entry reply", "LookUpInode", dir, "dir", gen, "REDACTED"),
		record("fuse attr reply", "GetInodeAttributes", dir, "dir", gen, "REDACTED"),
		record("fuse entry reply", "ReadDirPlus", file, "file", gen, "REDACTED/file.txt"),
		record("fuse entry reply", "LookUpInode", file, "file", gen, "REDACTED/file.txt"),
		record("fuse entry reply", "MkDir", mkdir.Entry.Child, "dir", 0, "REDACTED/new"),
		record("fuse entry reply", "CreateFile", create.Entry.Child, "file", 0, "REDACTED/created.txt"),
		record("fuse entry reply", "CreateSymlink", symlink.Entry.Child, "symlink", 0, "REDACTED/link"),
		record("fuse attr reply", "SetInodeAttributes", create.Entry.Child, "file", 0, "REDACTED/created.txt"),
	}
}

// redactSecret is a mount's redact that hides the word secret.
func redactSecret(line string) string { return strings.ReplaceAll(line, "secret", "REDACTED") }

// At Debug, each entry and attribute reply logs its op, the inode, its type,
// the generation of its attributes, and its path, redacted as the fuse
// library's lines are.
func TestMountLogsEachReplyAtDebug(t *testing.T) {
	var buf bytes.Buffer
	f := newLoggedCacheFixture(t, BackendKernel, logging.NewJSONLogger(&buf, slog.LevelDebug), redactSecret, baseDir("secret", "tree"), baseFile("secret/file.txt", "blob-file", 0o100644))
	want := replyOps(t, f)
	var got []replyRecord
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var rec replyRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not a JSON record: %v", line, err)
		}
		got = append(got, rec)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reply records:\n%+v\nwant:\n%+v", got, want)
	}
}

// Below Debug, a reply logs nothing and never reaches the mount's redact, so
// a lookup or a getattr pays only the level check.
func TestMountLogsNoReplyBelowDebug(t *testing.T) {
	var buf bytes.Buffer
	redacted := 0
	redact := func(line string) string {
		redacted++
		return redactSecret(line)
	}
	f := newLoggedCacheFixture(t, BackendKernel, logging.NewJSONLogger(&buf, slog.LevelInfo), redact, baseDir("secret", "tree"), baseFile("secret/file.txt", "blob-file", 0o100644))
	replyOps(t, f)
	if buf.Len() != 0 || redacted != 0 {
		t.Fatalf("at Info, replies logged %q and redacted %d strings, want nothing", buf.String(), redacted)
	}
}

// The mount waits only as long as ctx, and every op's context derives from
// opCtx, so the daemon cancels a stopped runtime's ops in flight.
func TestMountRepoBoundsTheMountAndParentsOpsOnOpContext(t *testing.T) {
	t.Setenv(backendEnv, BackendFSKit)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "mount")
	opCtx := context.WithValue(context.Background(), key{}, "ops")
	saved := fuseMount
	t.Cleanup(func() { fuseMount = saved })
	var mountCtx, gotOpCtx context.Context
	fuseMount = func(c context.Context, _ string, _ fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		mountCtx, gotOpCtx = c, cfg.OpContext
		return nil, errors.New("package tests do not mount")
	}
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	if _, err := MountRepoWithGate(ctx, opCtx, repo, nil, nil, slog.New(slog.DiscardHandler), unredacted); err == nil {
		t.Fatal("MountRepoWithGate succeeded without a mount")
	}
	if mountCtx == nil || mountCtx.Value(key{}) != "mount" || gotOpCtx == nil || gotOpCtx.Value(key{}) != "ops" {
		t.Fatalf("mount context %v and op context %v, want the mount bounded by ctx and ops parented on opCtx", mountCtx, gotOpCtx)
	}
}
