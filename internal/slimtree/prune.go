package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// pruning is what a prune did.
type pruning struct {
	rule state.Retention
	// ruleErr is why the rule file holds no rule. Then prune deletes nothing.
	ruleErr error
	deleted int
	// kept are the archives prune kept with their recovery refs, because
	// their repository is gone.
	kept     []string
	problems []error
	// used is the bytes the archives take, and disk the size of the home's
	// file system. disk is 0 when prune could not measure them.
	used, disk uint64
}

// prune deletes each archive older than the home's retention rule with its
// recovery ref, the ref first, so no ref outlives its archive. It keeps both
// while the archive's repository is gone, as on a volume that is not
// mounted: no later prune could find the ref, which would pin its commits.
// Hold the home's lock. prune never returns an error: each problem goes into
// the result, and the other archives go on.
func prune(ctx context.Context, home *state.Home, now time.Time) pruning {
	var p pruning
	if p.rule, p.ruleErr = home.Retention(); p.ruleErr == nil && p.rule != state.Forever {
		cutoff := now.Add(-time.Duration(p.rule) * 24 * time.Hour)
		archives, err := home.Archives(func(err error) { p.problems = append(p.problems, err) })
		if err != nil {
			p.problems = append(p.problems, err)
		}
		for _, a := range archives {
			if !a.Made.Before(cutoff) {
				continue
			}
			switch kept, err := pruneArchive(ctx, home, a); {
			case err != nil:
				p.problems = append(p.problems, fmt.Errorf("kept %s: %w", a.Dir, err))
			case kept != "":
				p.kept = append(p.kept, fmt.Sprintf("kept %s and %s: its repository %s is gone", a.Dir, a.Ref, kept))
			default:
				p.deleted++
			}
		}
	}
	var err error
	if p.used, p.disk, err = home.ArchivesUse(); err != nil {
		p.problems = append(p.problems, fmt.Errorf("measuring the archives: %w", err))
	}
	return p
}

// pruneArchive deletes a and its recovery ref, or returns the repository
// that holds the ref when that repository is gone.
func pruneArchive(ctx context.Context, home *state.Home, a state.Archived) (string, error) {
	r, err := a.Record()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Archive writes the record before rm writes the ref, so a removal
		// that stopped before the record wrote no ref.
	case err != nil:
		return "", err
	case r.CommonDir == "":
		return "", errors.New("its record names no repository")
	default:
		if _, err := os.Stat(r.CommonDir); errors.Is(err, fs.ErrNotExist) {
			return r.CommonDir, nil
		} else if err != nil {
			return "", err
		}
		// --no-deref deletes the ref itself, never a ref it names. A ref a
		// removal never wrote is no error.
		if _, err := apfsclone.Git(ctx, r.CommonDir, "update-ref", "--no-deref", "-d", a.Ref); err != nil {
			return "", err
		}
	}
	return "", home.RemoveArchive(a)
}

// write writes what p did, for rm and prune.
func (p pruning) write(w io.Writer) {
	if p.disk > 0 {
		fmt.Fprintf(w, "slimtree: archives use %.1f MB, %.3f%% of the disk\n", float64(p.used)/1e6, 100*float64(p.used)/float64(p.disk))
	}
	switch {
	case p.ruleErr != nil:
		fmt.Fprintf(w, "slimtree: pruned nothing: %v\n", p.ruleErr)
	case p.rule == state.Forever:
		fmt.Fprintln(w, "slimtree: archives and their recovery refs are kept forever")
	default:
		fmt.Fprintf(w, "slimtree: rm and prune delete archives older than %d days with their recovery refs; this one deleted %d\n", p.rule, p.deleted)
	}
	for _, k := range p.kept {
		fmt.Fprintln(w, "slimtree:", k)
	}
	for _, err := range p.problems {
		fmt.Fprintln(w, "slimtree: pruning problem:", err)
	}
	fmt.Fprintln(w, "slimtree: change the rule with: slimtree retention "+state.RetentionUsage)
}
