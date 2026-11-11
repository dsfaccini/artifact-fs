package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const fetchedFullRefRemoteTrackingRef = "refs/remotes/artifact-fs/fetch-ref"

const verifiedSourceTrackingRef = "refs/remotes/artifact-fs/source"

type fetchRefInfo struct {
	sourceRef string
	remoteRef string
	branch    string
}

// refspec fetches the remote's sourceRef into remoteRef.
func (t fetchRefInfo) refspec() string { return "+" + t.sourceRef + ":" + t.remoteRef }

func (s *Store) CloneBlobless(ctx context.Context, cfg model.RepoConfig) error {
	if _, err := os.Stat(cfg.GitDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}

	// Strip credentials from the CLI-visible URL; pass them via a credential helper
	// so they don't appear in ps output.
	safeURL, env, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return err
	}

	var target string
	attempt := func() error {
		var err error
		// A failed git clone can leave files in its destination, so every retry
		// gets a fresh directory rather than risking a false "already exists".
		target, err = os.MkdirTemp(parent, ".clone-*")
		if err != nil {
			return fmt.Errorf("mktemp clone dir: %w", err)
		}
		sourceRef := strings.TrimSpace(cfg.Branch)
		if branch := model.BranchName(sourceRef); branch != "" {
			args := []string{"clone", "--filter=blob:none"}
			if cfg.HistoryDepth > 0 {
				args = append(args, fmt.Sprintf("--depth=%d", cfg.HistoryDepth))
			}
			args = append(args, "--no-checkout", "--single-branch", "--no-tags", "--branch", branch, "--", safeURL, target)
			_, err = s.runNetworkGit(ctx, "", env, args...)
		} else {
			err = s.cloneExactRef(ctx, target, safeURL, env, sourceRef, cfg.HistoryDepth)
		}
		if err != nil {
			_ = os.RemoveAll(target)
			target = ""
		}
		return err
	}
	if err := s.retryGitOperationForRemotes(ctx, GitOperationClone, cfg.Name, []string{cfg.RemoteURL}, attempt); err != nil {
		return err
	}
	defer os.RemoveAll(target)

	targetGitDir := filepath.Join(target, ".git")

	if err := configureArtifactWorktree(ctx, targetGitDir); err != nil {
		return err
	}
	// Populate the index so git status works inside the mount.
	if _, err := s.runNetworkGit(ctx, targetGitDir, nil, "read-tree", "HEAD"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(target, ".git"), cfg.GitDir); err != nil {
		return err
	}
	s.CloseBatchPool(cfg.GitDir)
	return nil
}

// PrepareSource acquires and verifies one exact remote source revision.
func (s *Store) PrepareSource(ctx context.Context, cfg model.RepoConfig, requirement model.SourceRequirement) (model.PreparedSource, error) {
	requirement.Ref = strings.TrimSpace(requirement.Ref)
	requirement.RequiredCommit = strings.ToLower(strings.TrimSpace(requirement.RequiredCommit))
	if requirement.Ref == "" || !strings.HasPrefix(requirement.Ref, "refs/") {
		return model.PreparedSource{}, errors.New("source ref must be a canonical refs/... name")
	}
	if _, err := runGit(ctx, "", "check-ref-format", requirement.Ref); err != nil {
		return model.PreparedSource{}, fmt.Errorf("invalid source ref %q", requirement.Ref)
	}
	if requirement.RequiredCommit == "" {
		return model.PreparedSource{}, errors.New("required commit is required")
	}
	if requirement.Depth < 0 {
		return model.PreparedSource{}, errors.New("history depth must not be negative")
	}

	prepared := model.PreparedSource{Ref: requirement.Ref, Commit: requirement.RequiredCommit, Verified: true}
	if cfg.AcquiredRef == requirement.Ref && strings.EqualFold(cfg.AcquiredCommit, requirement.RequiredCommit) {
		available, err := requiredCommitAvailable(ctx, cfg.GitDir, requirement.RequiredCommit)
		if err != nil {
			return model.PreparedSource{}, err
		}
		if available {
			if err := s.prepareFixedHEAD(ctx, cfg.GitDir, requirement.RequiredCommit, false); err != nil {
				return model.PreparedSource{}, err
			}
			return prepared, nil
		}
	}

	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return model.PreparedSource{}, err
	}
	safeURL, env, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return model.PreparedSource{}, err
	}
	var candidateGitDir string
	attempt := func() error {
		if candidateGitDir != "" {
			_ = os.RemoveAll(candidateGitDir)
		}
		candidateGitDir, err = os.MkdirTemp(parent, ".source-*")
		if err != nil {
			return fmt.Errorf("mktemp source dir: %w", err)
		}
		initArgs := []string{"init", "--bare"}
		if len(requirement.RequiredCommit) == 64 {
			initArgs = append(initArgs, "--object-format=sha256")
		}
		initArgs = append(initArgs, candidateGitDir)
		if _, err := runGitWithEnv(ctx, "", env, initArgs...); err != nil {
			return err
		}
		// The Git directory is exposed through the mount's .git file, so Git
		// must treat callers in the mounted directory as worktree operations.
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "config", "core.bare", "false"); err != nil {
			return err
		}
		if err := configureArtifactWorktree(ctx, candidateGitDir); err != nil {
			return err
		}
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "remote", "add", "--", "origin", safeURL); err != nil {
			return err
		}
		refspec := "+" + requirement.Ref + ":" + verifiedSourceTrackingRef
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "config", "--replace-all", "remote.origin.fetch", refspec); err != nil {
			return err
		}
		args := []string{"fetch", "--filter=blob:none", "--no-tags"}
		if requirement.Depth > 0 {
			args = append(args, fmt.Sprintf("--depth=%d", requirement.Depth))
		}
		args = append(args, "origin", refspec)
		if _, err := s.runNetworkGit(ctx, candidateGitDir, env, args...); err != nil {
			return err
		}
		observed, err := runGit(ctx, candidateGitDir, "rev-parse", "--verify", verifiedSourceTrackingRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("source ref %s did not resolve to a commit: %w", requirement.Ref, err)
		}
		observed = strings.ToLower(strings.TrimSpace(observed))
		if observed != requirement.RequiredCommit {
			return fmt.Errorf("source changed: %s resolved to %s, required %s", requirement.Ref, observed, requirement.RequiredCommit)
		}
		if err := s.prepareFixedHEAD(ctx, candidateGitDir, requirement.RequiredCommit, true); err != nil {
			return err
		}
		return nil
	}
	if err := s.retryGitOperationForRemotes(ctx, GitOperationClone, cfg.Name, []string{cfg.RemoteURL}, attempt); err != nil {
		if candidateGitDir != "" {
			_ = os.RemoveAll(candidateGitDir)
		}
		return model.PreparedSource{}, err
	}
	defer os.RemoveAll(candidateGitDir)
	if err := installGitDir(candidateGitDir, cfg.GitDir); err != nil {
		return model.PreparedSource{}, err
	}
	candidateGitDir = ""
	s.CloseBatchPool(cfg.GitDir)
	prepared.Acquired = true
	return prepared, nil
}

func configureArtifactWorktree(ctx context.Context, gitDir string) error {
	// Git's clone-time probes inspect the state filesystem, not the later FUSE
	// mount. Record the semantics ArtifactFS actually exposes.
	settings := [][2]string{
		{"core.filemode", "true"},
		{"core.ignorecase", "false"},
		{"core.symlinks", "true"},
	}
	for _, setting := range settings {
		if _, err := runGit(ctx, gitDir, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) cloneExactRef(ctx context.Context, target string, safeURL string, env []string, ref string, depth int) error {
	advertisement, err := s.runNetworkGit(ctx, "", env, "ls-remote", "--", safeURL, ref)
	if err != nil {
		return err
	}
	advertisedOID := ""
	for line := range strings.SplitSeq(advertisement, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			advertisedOID = strings.ToLower(fields[0])
			break
		}
	}
	if advertisedOID == "" {
		return fmt.Errorf("source ref %s was not advertised by the remote", ref)
	}
	initArgs := []string{"init"}
	switch len(advertisedOID) {
	case 40:
	case 64:
		initArgs = append(initArgs, "--object-format=sha256")
	default:
		return fmt.Errorf("source ref %s advertised an unsupported object ID", ref)
	}
	initArgs = append(initArgs, target)
	if _, err := runGitWithEnv(ctx, "", env, initArgs...); err != nil {
		return err
	}
	gitDir := filepath.Join(target, ".git")
	if _, err := runGitWithEnv(ctx, gitDir, env, "remote", "add", "--", "origin", safeURL); err != nil {
		return err
	}
	refspec := "+" + ref + ":" + fetchedFullRefRemoteTrackingRef
	if _, err := runGitWithEnv(ctx, gitDir, env, "config", "--replace-all", "remote.origin.fetch", refspec); err != nil {
		return err
	}
	fetchArgs := []string{"fetch", "--filter=blob:none", "--no-tags"}
	if depth > 0 {
		fetchArgs = append(fetchArgs, fmt.Sprintf("--depth=%d", depth))
	}
	fetchArgs = append(fetchArgs, "origin", refspec)
	if _, err := s.runNetworkGit(ctx, gitDir, env, fetchArgs...); err != nil {
		return err
	}
	commit, err := runGit(ctx, gitDir, "rev-parse", "--verify", fetchedFullRefRemoteTrackingRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("source ref %s did not resolve to a commit: %w", ref, err)
	}
	_, err = runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(commit))
	return err
}

func requiredCommitAvailable(ctx context.Context, gitDir string, commit string) (bool, error) {
	resolved, err := runGit(ctx, gitDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), commit) {
		return false, nil
	}
	index, err := os.Open(filepath.Join(gitDir, "index"))
	if err != nil {
		return false, nil
	}
	defer index.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(index, header); err != nil {
		return false, nil
	}
	return string(header) == "DIRC", nil
}

func (s *Store) prepareFixedHEAD(ctx context.Context, gitDir string, commit string, resetIndex bool) error {
	resolved, err := runGit(ctx, gitDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return fmt.Errorf("required commit %s is unavailable: %w", commit, err)
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), commit) {
		return fmt.Errorf("required commit %s resolved unexpectedly", commit)
	}
	if _, err := runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", commit); err != nil {
		return err
	}
	if !resetIndex {
		return nil
	}
	_, err = s.runNetworkGit(ctx, gitDir, nil, "read-tree", commit)
	return err
}

func installGitDir(candidate string, destination string) error {
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		return os.Rename(candidate, destination)
	} else if err != nil {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(destination), ".replaced-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(candidate, destination); err != nil {
		_ = os.Rename(backup, destination)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

// FetchNonInteractive fetches, for the daemon's refresh and the fetch
// command, what preparation fetches, into the same ref: a verified source's
// ref, or else the fetch ref. It passes that refspec explicitly: the git dir's
// configured refspec can name every branch, and a --depth git dir would get
// each new one in full. Like every Git command that contacts a remote, it runs
// through runNetworkGit, so a remote that needs a prompt makes it fail.
func (s *Store) FetchNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	refspec, tracking, err := refreshTarget(repo)
	if err != nil {
		return err
	}
	args := []string{"fetch", "--no-tags"}
	if depth := refreshDepth(ctx, repo, tracking); depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", depth))
	}
	args = append(args, "origin", refspec)
	remotesForLogging, stopLookup := s.startRemotesForLogging(ctx, repo)
	defer stopLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		_, err := s.runNetworkGit(ctx, repo.GitDir, nil, args...)
		return err
	})
}

// refreshTarget returns the refspec FetchNonInteractive fetches for repo and
// the tracking ref it fetches into, which ComputeAheadBehind compares HEAD
// with.
func refreshTarget(repo model.RepoConfig) (refspec, trackingRef string, err error) {
	if repo.RequiredCommit != "" {
		return "+" + repo.Branch + ":" + verifiedSourceTrackingRef, verifiedSourceTrackingRef, nil
	}
	target, err := fetchRefTarget(repo.FetchRef)
	if err != nil {
		return "", "", err
	}
	return target.refspec(), target.remoteRef, nil
}

// refreshDepth returns the --depth of FetchNonInteractive's fetch into
// tracking: fetchDepth's, and none once tracking exists. A fetch onto the
// tracking ref's history then transfers only the new commits and keeps them
// connected to HEAD. A fetch with --depth would cut the new tip off from HEAD
// and shorten HEAD's history to the depth. A verified source keeps
// fetchDepth's depth, which bounds its history on every refresh.
func refreshDepth(ctx context.Context, repo model.RepoConfig, tracking string) int {
	depth := fetchDepth(ctx, repo, model.BranchName(repo.FetchRef))
	if depth <= 0 || repo.RequiredCommit != "" {
		return depth
	}
	if _, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "--quiet", tracking); err == nil {
		return 0
	}
	return depth
}

// fetchDepth returns the --depth of a fetch onto branch: repo's history depth
// until branch exists locally, then none. A fetch with --depth marks its tip
// shallow even when the git dir holds the tip's parent, which hides the old
// tip from the check that a prepared git dir's branch only moves forward. A
// fetch onto a branch's history transfers only the new commits anyway. A
// fetch of a ref that is not a branch keeps the depth.
func fetchDepth(ctx context.Context, repo model.RepoConfig, branch string) int {
	if repo.HistoryDepth <= 0 || branch == "" {
		return repo.HistoryDepth
	}
	if _, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return 0
	}
	return repo.HistoryDepth
}

// FetchRefAmbient fetches ref with the credentials Git finds on its own, for a
// repo whose remote holds none.
func (s *Store) FetchRefAmbient(ctx context.Context, repo model.RepoConfig, ref string) error {
	return s.fetchRef(ctx, repo, ref, nil)
}

func (s *Store) FetchRefWithCredentials(ctx context.Context, repo model.RepoConfig, ref string) error {
	_, env, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	return s.fetchRef(ctx, repo, ref, env)
}

func (s *Store) fetchRef(ctx context.Context, repo model.RepoConfig, ref string, env []string) error {
	target, err := fetchRefTarget(ref)
	if err != nil {
		return err
	}
	refspec := target.refspec()
	args := []string{"fetch", "--filter=blob:none", "--no-tags"}
	if depth := fetchDepth(ctx, repo, target.branch); depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", depth))
	}
	args = append(args, "origin", refspec)
	remotesForLogging, stopLookup := s.startRemotesForLogging(ctx, repo)
	defer stopLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		_, err := s.runNetworkGit(ctx, repo.GitDir, env, args...)
		return err
	})
}

// startRemotesForLogging looks up origin's URL in the background, for the
// logs of the operation it runs beside. The returned remotes func waits for
// the lookup, so a failure that comes first still redacts against origin and
// keeps its cause. When ctx ends while the lookup still runs, it reports the
// remotes incomplete. The returned stop cancels the lookup and returns once
// its git has exited, at most about gitCommandWaitDelay later, so no git the
// operation started outlives it.
func (s *Store) startRemotesForLogging(ctx context.Context, repo model.RepoConfig) (func() ([]string, bool), func()) {
	type lookupResult struct {
		remote string
		err    error
	}
	lookupCtx, cancel := context.WithCancel(ctx)
	result := make(chan lookupResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		remote, err := runGit(lookupCtx, repo.GitDir, "remote", "get-url", "origin")
		result <- lookupResult{remote: strings.TrimSpace(remote), err: err}
	}()

	finished := false
	known := false
	actualRemote := ""
	return func() ([]string, bool) {
		if !finished {
			select {
			case <-done:
			case <-ctx.Done():
			}
			// A select picks at random among ready cases, so take a finished
			// lookup here even when ctx has ended too.
			select {
			case lookup := <-result:
				finished = true
				known = lookup.err == nil
				actualRemote = lookup.remote
			default:
			}
		}
		remotes := []string{repo.RemoteURL}
		if known && actualRemote != "" && actualRemote != strings.TrimSpace(repo.RemoteURL) {
			remotes = append(remotes, actualRemote)
		}
		return remotes, known
	}, func() { cancel(); <-done }
}

// ConfigureRemoteAmbient points origin at repo's remote, which holds no
// credentials.
func (s *Store) ConfigureRemoteAmbient(ctx context.Context, repo model.RepoConfig) error {
	return s.configureRemote(ctx, repo, repo.RemoteURL, nil)
}

func (s *Store) ConfigureRemoteWithCredentials(ctx context.Context, repo model.RepoConfig) error {
	safeURL, env, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	return s.configureRemote(ctx, repo, safeURL, env)
}

func (s *Store) configureRemote(ctx context.Context, repo model.RepoConfig, targetRemote string, env []string) error {
	actualRemote, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(actualRemote) != strings.TrimSpace(targetRemote) {
		if _, err := runGitWithEnv(ctx, repo.GitDir, env, "remote", "set-url", "--", "origin", targetRemote); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ValidateAmbientRemote(repo model.RepoConfig) error {
	if strings.TrimSpace(repo.RemoteURL) == "" {
		return errors.New("remote URL is required")
	}
	safeURL, _, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	if safeURL != repo.RemoteURL {
		return errors.New("remote must use ambient credentials")
	}
	return nil
}

func (s *Store) PrepareFetchedBranch(ctx context.Context, repo model.RepoConfig, ref string) error {
	target, err := fetchRefTarget(ref)
	if err != nil {
		return err
	}
	oid, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", target.remoteRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("remote ref %s missing after fetch: %w", target.remoteRef, err)
	}
	oid = strings.TrimSpace(oid)
	oldOID, err := currentOrEmptyTreeOID(ctx, repo.GitDir)
	if err != nil {
		return err
	}
	if target.branch == "" {
		index := indexFile(repo.GitDir)
		if err := s.readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
			return s.rollbackLandedReadTree(ctx, repo.GitDir, index, oldOID, oid, err)
		}
		oldHeadOID, err := refOIDOrZero(ctx, repo.GitDir, "HEAD")
		if err != nil {
			return s.rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		}
		updateArgs := []string{"update-ref", "--no-deref", "HEAD", oid}
		if strings.Trim(oldHeadOID, "0") != "" {
			updateArgs = append(updateArgs, oldHeadOID)
		}
		if _, err := runGit(ctx, repo.GitDir, updateArgs...); err != nil {
			return s.rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		}
		return nil
	}
	refName := "refs/heads/" + target.branch
	var oldBranchOID string
	if repo.PreparedGitDir {
		oldBranchOID, err = s.preparedBranchExpectedOID(ctx, repo, target.branch, oid)
	} else {
		oldBranchOID, err = refOIDOrZero(ctx, repo.GitDir, refName)
	}
	if err != nil {
		return err
	}
	index := indexFile(repo.GitDir)
	if err := s.readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
		return s.rollbackLandedReadTree(ctx, repo.GitDir, index, oldOID, oid, err)
	}
	if _, err := runGit(ctx, repo.GitDir, "update-ref", refName, oid, oldBranchOID); err != nil {
		return s.rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
	}
	if _, err := runGit(ctx, repo.GitDir, "symbolic-ref", "HEAD", refName); err != nil {
		ctx, cancel := s.rollbackContext(ctx)
		defer cancel()
		// HEAD resolves to the fetched commit when it was on the branch
		// already, or when symbolic-ref finished before a cancel stopped it.
		// The git dir then holds the whole transition.
		if headAt(ctx, repo.GitDir, oid) {
			return err
		}
		// HEAD is not on the branch, so a branch ref left at the fetched
		// commit is harmless, while an index that differs from HEAD reaches
		// the next preparation's read-tree as staged changes. The index goes
		// first, so a ref restore that uses up the deadline cannot keep it
		// from HEAD.
		indexErr := s.restoreIndex(ctx, repo.GitDir, oid, oldOID, err)
		return errors.Join(indexErr, restoreRef(ctx, repo.GitDir, refName, oldBranchOID, oid))
	}
	return s.trackRemoteBranch(ctx, repo, target.branch)
}

// trackRemoteBranch sets branch's upstream to origin's branch. Git tracks a
// branch only through a fetch refspec that maps it, and a --single-branch
// clone's refspec maps only the branch it cloned. When Git cannot track the
// branch on origin, trackRemoteBranch adds the branch's own line to origin's
// refspec and tries once more. Without the upstream, a plain git pull on a
// --single-branch clone pulls the cloned branch into the current one. When Git
// still refuses, as when a negative refspec excludes the branch from a pattern
// line, trackRemoteBranch logs a warning with the fix and returns nil, so
// preparation serves the branch without an upstream on origin.
func (s *Store) trackRemoteBranch(ctx context.Context, repo model.RepoConfig, branch string) error {
	refusal, err := setOriginUpstream(ctx, repo.GitDir, branch)
	if err != nil || refusal == nil {
		return err
	}
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	refspecs, err := configEntries(ctx, repo.GitDir, `^remote\.origin\.fetch$`)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(refspecs, func(e configEntry) bool { return e.value == refspec }) {
		if _, err := runGit(ctx, repo.GitDir, "config", "--add", "remote.origin.fetch", refspec); err != nil {
			return err
		}
	}
	if refusal, err = setOriginUpstream(ctx, repo.GitDir, branch); err != nil || refusal == nil {
		return err
	}
	tracking := "refs/remotes/origin/" + branch
	s.logger.WarnContext(ctx, "git cannot track the fetched branch on origin",
		"repo", auth.RedactLogString(repo.Name),
		"branch", branch,
		"error", auth.RedactLogString(refusal.Error()),
		"fix", fmt.Sprintf("make origin's fetch refspec, and no other remote's, map refs/heads/%s to %s, for example by removing a negative refspec that excludes the branch, then run: git --git-dir %s branch --set-upstream-to %s -- %s",
			branch, tracking, shellQuote(repo.GitDir), shellQuote(tracking), shellQuote(branch)))
	return nil
}

// setOriginUpstream sets branch's upstream to refs/remotes/origin/<branch>.
// The full name keeps a local branch named origin/<branch> from making the
// name ambiguous. Git tracks the branch on whichever remote's refspec maps
// that ref, so setOriginUpstream removes an upstream on another remote. It
// returns Git's refusal, or the upstream it removed, as refusal, and every
// other failure as err.
func setOriginUpstream(ctx context.Context, gitDir, branch string) (refusal, err error) {
	if _, err := runGit(ctx, gitDir, "branch", "--set-upstream-to", "refs/remotes/origin/"+branch, "--", branch); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return err, nil
	}
	remote, err := runGit(ctx, gitDir, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return nil, err
	}
	if remote == "origin" {
		return nil, nil
	}
	if _, err := runGit(ctx, gitDir, "branch", "--unset-upstream", "--", branch); err != nil {
		return nil, err
	}
	return fmt.Errorf("git tracks branch %s on remote %s, not origin", branch, remote), nil
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func refOIDOrZero(ctx context.Context, gitDir string, refName string) (string, error) {
	oid, err := runGit(ctx, gitDir, "rev-parse", "--verify", refName+"^{commit}")
	if err == nil {
		return strings.TrimSpace(oid), nil
	}
	return zeroOIDForGitDir(ctx, gitDir)
}

// defaultRollbackTimeout bounds a rollback, all its steps together. A
// rollback runs after its caller's context ended too: a canceled preparation
// must still restore the index and refs it changed.
const defaultRollbackTimeout = 20 * time.Second

// RollbackBound bounds how long PrepareFetchedBranch runs once its context
// ends: the git command the cancel stops exits within gitCommandWaitDelay,
// then the rollback ends at its deadline, plus gitCommandWaitDelay for the git
// command that deadline stops. The daemon's drain must outlast it.
const RollbackBound = defaultRollbackTimeout + 2*gitCommandWaitDelay

// defaultRollbackContext is the context of one rollback: it outlives ctx's
// cancellation and ends after defaultRollbackTimeout.
func defaultRollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), defaultRollbackTimeout)
}

// rollbackIndexTransition restores the index from fromOID back to toOID after
// a ref update failed, unless HEAD resolves to fromOID: the update landed after
// all, as when a cancel arrives after git wrote the ref, and the index already
// matches HEAD.
func (s *Store) rollbackIndexTransition(ctx context.Context, gitDir string, fromOID string, toOID string, cause error) error {
	ctx, cancel := s.rollbackContext(ctx)
	defer cancel()
	if headAt(ctx, gitDir, fromOID) {
		return cause
	}
	return s.restoreIndex(ctx, gitDir, fromOID, toOID, cause)
}

// rollbackLandedReadTree restores the index from newOID back to oldOID when
// a read-tree that reported failure moved it after all, as when a cancel
// arrives after git wrote the index. index is the index file before the
// read-tree. Git writes the index by renaming index.lock over it, so a
// read-tree that wrote nothing leaves the same file, even when the user staged
// what it would have written. Otherwise the read-tree moved the index when
// every path that differs between the two commits stands at newOID in the
// index; a staged path it kept differs between neither.
func (s *Store) rollbackLandedReadTree(ctx context.Context, gitDir string, index os.FileInfo, oldOID string, newOID string, cause error) error {
	if oldOID == newOID || sameFile(index, indexFile(gitDir)) {
		return cause
	}
	ctx, cancel := s.rollbackContext(ctx)
	defer cancel()
	// A submodule.<name>.ignore setting would hide a changed gitlink.
	changed, err := s.runNetworkGit(ctx, gitDir, nil, "diff-tree", "-r", "-z", "--name-only", "--ignore-submodules=none", oldOID, newOID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("check index after read-tree failure: %w", err))
	}
	if changed == "" {
		return cause
	}
	notNew, err := s.runNetworkGit(ctx, gitDir, nil, "diff-index", "--cached", "-z", "--name-only", "--ignore-submodules=none", newOID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("check index after read-tree failure: %w", err))
	}
	stale := map[string]bool{}
	for path := range strings.SplitSeq(notNew, "\x00") {
		stale[path] = true
	}
	for path := range strings.SplitSeq(changed, "\x00") {
		if path != "" && stale[path] {
			return cause
		}
	}
	return s.restoreIndex(ctx, gitDir, newOID, oldOID, cause)
}

// indexFile is the index file git reads for gitDir, nil when it is missing.
func indexFile(gitDir string) os.FileInfo {
	path, err := gitIndexPath(gitDir)
	if err != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

func sameFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return os.SameFile(a, b)
}

func headAt(ctx context.Context, gitDir string, oid string) bool {
	head, err := runGit(ctx, gitDir, "rev-parse", "--verify", "HEAD^{commit}")
	return err == nil && strings.TrimSpace(head) == oid
}

func (s *Store) restoreIndex(ctx context.Context, gitDir string, fromOID string, toOID string, cause error) error {
	if err := s.readTreeTransition(ctx, gitDir, fromOID, toOID); err != nil {
		return errors.Join(cause, fmt.Errorf("restore index after failed update: %w", err))
	}
	return cause
}

func restoreRef(ctx context.Context, gitDir string, refName string, oldOID string, newOID string) error {
	if _, err := runGit(ctx, gitDir, "update-ref", refName, oldOID, newOID); err != nil {
		return fmt.Errorf("restore ref after HEAD update failure: %w", err)
	}
	return nil
}

func currentOrEmptyTreeOID(ctx context.Context, gitDir string) (string, error) {
	oid, headErr := runGit(ctx, gitDir, "rev-parse", "--verify", "HEAD^{commit}")
	if headErr == nil {
		return strings.TrimSpace(oid), nil
	}
	if _, err := runGit(ctx, gitDir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return "", headErr
	}
	oid, err := runGit(ctx, gitDir, "hash-object", "-t", "tree", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(oid), nil
}

// readTreeTransition moves the index from oldOID's tree to newOID's. In a git
// dir cloned without trees, read-tree fetches the trees it lacks.
func (s *Store) readTreeTransition(ctx context.Context, gitDir string, oldOID string, newOID string) error {
	_, err := s.runNetworkGit(ctx, gitDir, nil, "read-tree", "-m", oldOID, newOID)
	return err
}

func (s *Store) preparedBranchExpectedOID(ctx context.Context, repo model.RepoConfig, branch string, oid string) (string, error) {
	current, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return zeroOIDForGitDir(ctx, repo.GitDir)
	}
	current = strings.TrimSpace(current)
	if current == oid {
		return current, nil
	}
	if _, err := runGit(ctx, repo.GitDir, "merge-base", "--is-ancestor", current, oid); err != nil {
		return "", fmt.Errorf("prepared git dir branch %s would be overwritten; refusing non-fast-forward update", branch)
	}
	return current, nil
}

func zeroOIDForGitDir(ctx context.Context, gitDir string) (string, error) {
	format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(format) {
	case "sha1":
		return strings.Repeat("0", 40), nil
	case "sha256":
		return strings.Repeat("0", 64), nil
	default:
		return "", fmt.Errorf("unsupported Git object format %q", format)
	}
}

func (s *Store) ValidatePreparedGitDir(ctx context.Context, repo model.RepoConfig) error {
	if strings.TrimSpace(repo.GitDir) == "" {
		return errors.New("git dir is required")
	}
	st, err := os.Stat(repo.GitDir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("git dir %s is not a directory", repo.GitDir)
	}
	if _, err := runGit(ctx, repo.GitDir, "rev-parse", "--git-dir"); err != nil {
		return err
	}
	remoteURL, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
	if err == nil && remoteHasInlineCredentials(remoteURL) {
		return errors.New("prepared git dir origin must use ambient credentials")
	}
	return nil
}

func fetchRefTarget(ref string) (fetchRefInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fetchRefInfo{}, errors.New("fetch ref is required")
	}
	if branch := model.BranchName(ref); branch != "" {
		return fetchRefInfo{
			sourceRef: "refs/heads/" + branch,
			remoteRef: "refs/remotes/origin/" + branch,
			branch:    branch,
		}, nil
	}
	if strings.HasPrefix(ref, "refs/") {
		return fetchRefInfo{sourceRef: ref, remoteRef: fetchedFullRefRemoteTrackingRef}, nil
	}
	return fetchRefInfo{}, errors.New("fetch ref is required")
}
