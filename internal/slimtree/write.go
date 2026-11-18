package slimtree

import (
	"context"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
)

// writeGrace is how long a git that gitWrite runs keeps running once its
// context ends.
var writeGrace = gitstore.WriteGrace

// writeOutlived, when set, runs when the context of a git that gitWrite runs
// ends before the git returns. Production code leaves it nil.
var writeOutlived func()

// gitWrite runs apfsclone.Git for a git that writes the user's refs or config
// and contacts no remote, under gitstore.WriteContext. The git starts only
// while ctx lives, and runs for at most writeGrace once ctx ends, so its stop
// finds the handler that removes its lock files installed.
func gitWrite(ctx context.Context, dir string, args ...string) ([]byte, error) {
	writeCtx, done, err := gitstore.WriteContext(ctx, writeGrace, writeOutlived)
	if err != nil {
		return nil, err
	}
	defer done()
	return apfsclone.Git(writeCtx, dir, args...)
}
