package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
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
				p.kept = append(p.kept, fmt.Sprintf("kept %s and %s: its repository %s is gone. Mount it and prune again, or, if it is gone for good, drop the archive with: rm -r %s", a.Dir, a.Ref, kept, shellQuote(a.Dir)))
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

// pruneRepoTimeout bounds pruneArchive's stat and git commands in an
// archive's repository. rm holds the global lock while it prunes, so a
// repository on a hung mount must not hang it.
const pruneRepoTimeout = 10 * time.Second

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
		gone, err := inRepo(ctx, r.CommonDir, func(ctx context.Context) (bool, error) {
			return deleteRecoveryRef(ctx, r, a)
		})
		if err != nil {
			return "", err
		}
		if gone {
			return r.CommonDir, nil
		}
	}
	return "", home.RemoveArchive(a)
}

// deleteRecoveryRef deletes a's recovery ref while it points at r.Head, or
// reports that r's repository is gone. --no-deref deletes the ref itself,
// never a ref it names. A record without Head, which slimtree 0.1.0's rm
// writes, deletes the ref wherever it points.
func deleteRecoveryRef(ctx context.Context, r state.Record, a state.Archived) (bool, error) {
	if _, err := os.Stat(r.CommonDir); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	args := []string{"update-ref", "--no-deref", "-d", a.Ref}
	if r.Head == "" {
		// Without an old value, a ref a removal never wrote is no error.
		_, err := apfsclone.Git(ctx, r.CommonDir, args...)
		return false, err
	}
	_, err := apfsclone.Git(ctx, r.CommonDir, append(args, r.Head)...)
	if err == nil {
		return false, nil
	}
	out, readErr := apfsclone.Git(ctx, r.CommonDir, "rev-parse", "--verify", "--quiet", a.Ref)
	var exit *exec.ExitError
	at := strings.TrimSpace(string(out))
	switch {
	// rev-parse --verify --quiet exits 1 for a ref that does not exist: the
	// removal stopped before it wrote the ref.
	case errors.As(readErr, &exit) && exit.ExitCode() == 1:
		return false, nil
	case readErr == nil && at != r.Head:
		return false, fmt.Errorf("%s points at %s, not at %s, the commit rm kept; drop both with: git -C %s update-ref -d %s && rm -r %s", a.Ref, at, r.Head, shellQuote(r.CommonDir), a.Ref, shellQuote(a.Dir))
	}
	return false, err
}

// inRepo runs f with ctx bounded by pruneRepoTimeout. When the deadline
// passes, it returns an error, without waiting past worktreeListGrace for an
// f stuck in a stat or a git on a hung mount. That f may still delete the
// ref, which leaves the archive to the next prune.
func inRepo(ctx context.Context, commonDir string, f func(context.Context) (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, pruneRepoTimeout)
	defer cancel()
	type result struct {
		gone bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		gone, err := f(ctx)
		done <- result{gone, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		select {
		case r = <-done:
		case <-time.After(worktreeListGrace):
		}
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return false, fmt.Errorf("its repository %s did not answer within %s", commonDir, pruneRepoTimeout)
	case err != nil:
		return false, err
	}
	return r.gone, r.err
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// diskUse describes used bytes of a disk of disk bytes.
func diskUse(used, disk uint64) string {
	var size string
	switch {
	case used < 1e3:
		size = fmt.Sprintf("%d B", used)
	case used < 1e6:
		size = fmt.Sprintf("%.1f KB", float64(used)/1e3)
	case used < 1e9:
		size = fmt.Sprintf("%.1f MB", float64(used)/1e6)
	default:
		size = fmt.Sprintf("%.2f GB", float64(used)/1e9)
	}
	share := 100 * float64(used) / float64(disk)
	if used > 0 && share < 0.001 {
		return size + ", under 0.001% of the disk"
	}
	return fmt.Sprintf("%s, %.3f%% of the disk", size, share)
}

// write writes what p did, for rm and prune.
func (p pruning) write(w io.Writer) {
	if p.disk > 0 {
		fmt.Fprintf(w, "slimtree: archives use %s\n", diskUse(p.used, p.disk))
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
