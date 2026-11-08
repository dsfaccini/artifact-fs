//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse/fuseops"
)

// ReadSymlink reads a base target with one bounded ReadBlob and hydrates
// nothing. A size the snapshot knows to be out of range fails before any read.
func TestReadSymlinkReadsBaseTargetsBounded(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sizeState     string
		size          int64
		hydrator      fakeHydrator
		want          string
		wantErr       error
		wantReadBlobs int
	}{
		{"known oversized", "known", model.MaxSymlinkTargetBytes + 1, fakeHydrator{}, "", syscall.ENAMETOOLONG, 0},
		{"known negative", "known", -1, fakeHydrator{}, "", syscall.EIO, 0},
		{"unknown oversized", "unknown", 0, fakeHydrator{err: model.ErrBlobTooLarge}, "", syscall.ENAMETOOLONG, 1},
		{"unknown", "unknown", 0, fakeHydrator{data: []byte("../target")}, "../target", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := model.RepoPaths{ID: "repo"}
			link := model.BaseNode{RepoID: repo.ID, Path: "link", Type: "symlink", Mode: 0o120000, ObjectOID: "blob", SizeState: tc.sizeState, SizeBytes: tc.size}
			resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"link": link}}, newOverlay(t))
			fs := NewArtifactFuse(repo, &Engine{Resolver: resolver, Repo: repo, Hydrator: &tc.hydrator})
			// The test maps the inode itself, so that a lookup reads nothing.
			fs.mu.Lock()
			ref := fs.allocInode("link", Attrs{Type: "symlink", Mode: 0o120000, Gen: fs.engine.Generation()})
			fs.mu.Unlock()

			op := &fuseops.ReadSymlinkOp{Inode: ref.ID}
			err := fs.ReadSymlink(context.Background(), op)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.hydrator.calls != 0 || tc.hydrator.readBlobCalls != tc.wantReadBlobs {
				t.Fatalf("hydrations = %d, ReadBlob calls = %d; want 0 and %d", tc.hydrator.calls, tc.hydrator.readBlobCalls, tc.wantReadBlobs)
			}
			if op.Target != tc.want {
				t.Fatalf("target = %q, want %q", op.Target, tc.want)
			}
		})
	}
}
