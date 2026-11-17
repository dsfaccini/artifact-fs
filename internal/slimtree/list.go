package slimtree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// Worktree states that list reports.
const (
	stateOK      = "ok"
	stateStopped = "stopped"
	stateMissing = "missing"
	// stateUnknown marks a worktree whose repository git did not list in
	// worktreeListTimeout.
	stateUnknown = "unknown"
)

// worktreeListTimeout bounds git worktree list, which stats each unlocked
// worktree's .git, so a hung mount cannot hang list.
const worktreeListTimeout = 10 * time.Second

// worktreeListGrace is how long worktreeList waits for a git it stopped to
// exit. A git stuck in a stat of a hung mount may never exit.
const worktreeListGrace = 2 * time.Second

// errWorktreeListTimeout is worktreeList's error when git did not finish.
var errWorktreeListTimeout = errors.New("git worktree list did not finish within " + worktreeListTimeout.String())

// listVersion is list --json's format. It rises when a field is renamed or
// removed or changes its meaning.
const listVersion = 1

type listedWorktree struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Backend string `json:"backend"`
	State   string `json:"state"`
	// Reason says why the state is unknown.
	Reason  string    `json:"reason,omitempty"`
	Branch  string    `json:"branch"`
	Repo    string    `json:"repo"`
	Created time.Time `json:"created"`
}

type listing struct {
	Version   int              `json:"version"`
	Worktrees []listedWorktree `json:"worktrees"`
}

// listWorktrees lists the worktrees that home's records name, of the
// repository whose common dir is repo, or of every repository when repo is
// "". It reads records and git's worktree list, and stats nothing itself.
// git stats each unlocked worktree's .git, and skips a locked one, so each
// repository's list is bounded by worktreeListTimeout. It passes each record
// it skips, and each repository git did not list in time, to warn.
func listWorktrees(ctx context.Context, home *state.Home, repo string, warn func(error)) (listing, error) {
	records, err := home.Records(warn)
	if err != nil {
		return listing{}, err
	}
	out := listing{Version: listVersion, Worktrees: []listedWorktree{}}
	type repoList struct {
		worktrees map[string]gitWorktree
		err       error
	}
	lists := map[string]repoList{}
	for _, r := range records {
		if repo != "" && r.CommonDir != repo {
			continue
		}
		listed, ok := lists[r.CommonDir]
		if !ok {
			listed.worktrees, listed.err = worktreeList(ctx, r.CommonDir)
			if errors.Is(listed.err, context.Canceled) {
				return listing{}, listed.err
			}
			if errors.Is(listed.err, errWorktreeListTimeout) {
				warn(fmt.Errorf("%w in %s, so its worktrees are unknown", listed.err, r.CommonDir))
			}
			lists[r.CommonDir] = listed
		}
		w := listedWorktree{Name: r.Name(), Path: r.Path, Backend: r.Backend, Repo: r.CommonDir, Created: r.Created}
		gw, ok := listed.worktrees[r.Path]
		switch {
		case errors.Is(listed.err, errWorktreeListTimeout):
			w.State, w.Reason = stateUnknown, listed.err.Error()
		// A repository git cannot read lists no worktree, so each of its
		// records is missing.
		case !ok || gw.prunable:
			w.State = stateMissing
		// Without a reader of the mount table, list cannot tell a mounted
		// artifactfs worktree from a stopped one.
		case r.Backend == string(artifactFS):
			w.State = stateStopped
		default:
			w.State = stateOK
		}
		w.Branch = gw.branch
		out.Worktrees = append(out.Worktrees, w)
	}
	return out, nil
}

// gitWorktree is one worktree that git worktree list reports.
type gitWorktree struct {
	head     string
	branch   string
	prunable bool
}

// worktreeList runs git worktree list in the repository whose common dir is
// commonDir, and returns its worktrees by path. git reads each worktree's
// branch from the HEAD in its admin directory. It stats each unlocked
// worktree's .git and marks the worktree prunable when that is gone. When git
// does not finish within worktreeListTimeout, worktreeList stops it and
// returns errWorktreeListTimeout, without waiting past worktreeListGrace for
// a git that cannot exit. When ctx is canceled, it returns ctx's error.
func worktreeList(ctx context.Context, commonDir string) (map[string]gitWorktree, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+commonDir, "worktree", "list", "--porcelain", "-z")
	gitstore.ConfigureCancelableCommand(cmd)
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		cmd.Stdout = &out
		err := gitstore.RunWithFiles(cmd, "", nil, nil)
		done <- result{out.Bytes(), err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		select {
		case r = <-done:
		case <-time.After(worktreeListGrace):
			r.err = ctx.Err()
		}
	}
	// A canceled list, as on Ctrl-C, tells nothing about the worktrees.
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return nil, errWorktreeListTimeout
	case err != nil:
		return nil, err
	}
	out, err := r.out, r.err
	if err != nil {
		return nil, fmt.Errorf("git worktree list in %s: %w", commonDir, err)
	}
	worktrees := map[string]gitWorktree{}
	var path string
	for field := range bytes.SplitSeq(out, []byte{0}) {
		key, value, _ := strings.Cut(string(field), " ")
		switch key {
		case "worktree":
			path = filepath.Clean(value)
			worktrees[path] = gitWorktree{}
		case "HEAD":
			w := worktrees[path]
			w.head = value
			worktrees[path] = w
		case "branch":
			w := worktrees[path]
			w.branch = strings.TrimPrefix(value, "refs/heads/")
			worktrees[path] = w
		case "prunable":
			w := worktrees[path]
			w.prunable = true
			worktrees[path] = w
		}
	}
	return worktrees, nil
}

// commonDir returns the canonical common git dir of the repository that holds
// the current directory. need names what needs it, for the error.
func commonDir(ctx context.Context, need string) (string, error) {
	out, err := apfsclone.Git(ctx, ".", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s needs a git repository: %w", need, err)
	}
	return state.Canonical(strings.TrimSpace(string(out)))
}

func printListing(w io.Writer, l listing, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(l)
	}
	if len(l.Worktrees) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tBACKEND\tBRANCH\tPATH")
	for _, wt := range l.Worktrees {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", wt.Name, wt.State, wt.Backend, wt.Branch, wt.Path)
	}
	return tw.Flush()
}
