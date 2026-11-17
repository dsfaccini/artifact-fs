package slimtree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// Worktree states that list reports.
const (
	stateOK      = "ok"
	stateStopped = "stopped"
	stateMissing = "missing"
)

// listVersion is list --json's format. It rises when a field is renamed or
// removed or changes its meaning.
const listVersion = 1

type listedWorktree struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Backend string    `json:"backend"`
	State   string    `json:"state"`
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
// "". It reads records and git's worktree list, and never stats a worktree,
// because a hung mount would hang the stat.
func listWorktrees(ctx context.Context, home *state.Home, repo string) (listing, error) {
	records, err := home.Records()
	if err != nil {
		return listing{}, err
	}
	out := listing{Version: listVersion, Worktrees: []listedWorktree{}}
	gitWorktrees := map[string]map[string]gitWorktree{}
	for _, r := range records {
		if repo != "" && r.CommonDir != repo {
			continue
		}
		listed, ok := gitWorktrees[r.CommonDir]
		if !ok {
			// A repository git cannot read lists no worktree, so each of its
			// records is missing.
			listed, _ = worktreeList(ctx, r.CommonDir)
			gitWorktrees[r.CommonDir] = listed
		}
		w := listedWorktree{Name: r.Name(), Path: r.Path, Backend: r.Backend, Repo: r.CommonDir, Created: r.Created}
		gw, ok := listed[r.Path]
		switch {
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
	branch   string
	prunable bool
}

// worktreeList runs git worktree list in the repository whose common dir is
// commonDir, and returns its worktrees by path. git reads each worktree's
// branch from its own HEAD, and marks prunable a worktree whose .git file is
// gone, so the worktree directory itself is never read.
func worktreeList(ctx context.Context, commonDir string) (map[string]gitWorktree, error) {
	out, err := exec.CommandContext(ctx, "git", "--git-dir="+commonDir, "worktree", "list", "--porcelain", "-z").Output()
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
// the current directory.
func commonDir(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("--repo needs a git repository: git rev-parse: %w", err)
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
