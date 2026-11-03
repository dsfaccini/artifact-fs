package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const fetchedFullRefRemoteTrackingRef = "refs/remotes/artifact-fs/fetch-ref"

const verifiedSourceTrackingRef = "refs/remotes/artifact-fs/source"

type fetchRefInfo struct {
	sourceRef string
	remoteRef string
	branch    string
}

func (s *Store) CloneBlobless(ctx context.Context, cfg model.RepoConfig) error {
	return s.cloneBlobless(ctx, cfg, nil)
}

func (s *Store) CloneBloblessNonInteractive(ctx context.Context, cfg model.RepoConfig) error {
	return s.cloneBlobless(ctx, cfg, nonInteractiveGitEnv())
}

func (s *Store) cloneBlobless(ctx context.Context, cfg model.RepoConfig, extraEnv []string) error {
	if _, err := os.Stat(cfg.GitDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	// Strip credentials from the CLI-visible URL; pass them via a credential helper
	// so they don't appear in ps output.
	safeURL, credHelper, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return err
	}
	env := append([]string{}, extraEnv...)
	env = append(env, credHelper...)

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
		branch := sourceRef
		if after, ok := strings.CutPrefix(sourceRef, "refs/heads/"); ok {
			branch = after
		} else if strings.HasPrefix(sourceRef, "refs/") {
			branch = ""
		}
		if branch != "" {
			args := []string{"clone", "--filter=blob:none"}
			if cfg.HistoryDepth > 0 {
				args = append(args, fmt.Sprintf("--depth=%d", cfg.HistoryDepth))
			}
			args = append(args, "--no-checkout", "--single-branch", "--no-tags", "--branch", branch, safeURL, target)
			_, err = runGitWithEnv(ctx, "", env, args...)
		} else {
			err = cloneExactRef(ctx, target, safeURL, env, sourceRef, cfg.HistoryDepth)
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
	if _, err := runGit(ctx, targetGitDir, "read-tree", "HEAD"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(target, ".git"), cfg.GitDir); err != nil {
		return err
	}
	s.invalidateBatchPool(cfg.GitDir)
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
			if err := prepareFixedHEAD(ctx, cfg.GitDir, requirement.RequiredCommit, false); err != nil {
				return model.PreparedSource{}, err
			}
			return prepared, nil
		}
	}

	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return model.PreparedSource{}, err
	}
	safeURL, credHelper, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return model.PreparedSource{}, err
	}
	env := append(nonInteractiveGitEnv(), credHelper...)
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
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "remote", "add", "origin", safeURL); err != nil {
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
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, args...); err != nil {
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
		if err := prepareFixedHEAD(ctx, candidateGitDir, requirement.RequiredCommit, true); err != nil {
			return err
		}
		return nil
	}
	if err := s.retryGitOperation(ctx, GitOperationClone, cfg.Name, attempt); err != nil {
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
	s.invalidateBatchPool(cfg.GitDir)
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

func cloneExactRef(ctx context.Context, target string, safeURL string, env []string, ref string, depth int) error {
	advertisement, err := runGitWithEnv(ctx, "", env, "ls-remote", safeURL, ref)
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
	if _, err := runGitWithEnv(ctx, gitDir, env, "remote", "add", "origin", safeURL); err != nil {
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
	if _, err := runGitWithEnv(ctx, gitDir, env, fetchArgs...); err != nil {
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

func prepareFixedHEAD(ctx context.Context, gitDir string, commit string, resetIndex bool) error {
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
	_, err = runGit(ctx, gitDir, "read-tree", commit)
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

func (s *Store) Fetch(ctx context.Context, repo model.RepoConfig) error {
	remotesForLogging, cancelLookup := s.startRemotesForLogging(ctx, repo)
	defer cancelLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		args := []string{"fetch", "--no-tags"}
		if repo.HistoryDepth > 0 {
			args = append(args, fmt.Sprintf("--depth=%d", repo.HistoryDepth))
		}
		args = append(args, "origin")
		_, err := runGit(ctx, repo.GitDir, args...)
		return err
	})
}

func (s *Store) FetchRefNonInteractive(ctx context.Context, repo model.RepoConfig, ref string) error {
	return s.fetchRef(ctx, repo, ref, nonInteractiveGitEnv())
}

func (s *Store) FetchRefWithCredentials(ctx context.Context, repo model.RepoConfig, ref string) error {
	_, env, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	return s.fetchRef(ctx, repo, ref, env)
}

func (s *Store) fetchRef(ctx context.Context, repo model.RepoConfig, ref string, env []string) error {
	target, err := fetchRefTarget(repo, ref)
	if err != nil {
		return err
	}
	refspec := "+" + target.sourceRef + ":" + target.remoteRef
	if target.branch != "" {
		refspec = fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", target.branch, target.branch)
	}
	remotesForLogging, cancelLookup := s.startRemotesForLogging(ctx, repo)
	defer cancelLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		_, err := runGitWithEnv(ctx, repo.GitDir, env, "fetch", "--filter=blob:none", "--no-tags", "origin", refspec)
		return err
	})
}

func (s *Store) startRemotesForLogging(ctx context.Context, repo model.RepoConfig) (func() ([]string, bool), context.CancelFunc) {
	type lookupResult struct {
		remote string
		err    error
	}
	lookupCtx, cancel := context.WithCancel(ctx)
	result := make(chan lookupResult, 1)
	go func() {
		remote, err := runGit(lookupCtx, repo.GitDir, "remote", "get-url", "origin")
		result <- lookupResult{remote: strings.TrimSpace(remote), err: err}
	}()

	finished := false
	known := false
	actualRemote := ""
	return func() ([]string, bool) {
		if !finished {
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
	}, cancel
}

func (s *Store) PrepareExistingCloneNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	if err := s.ValidateAmbientRemote(repo); err != nil {
		return err
	}
	if err := s.ConfigureRemoteNonInteractive(ctx, repo); err != nil {
		return err
	}
	if err := s.FetchRefNonInteractive(ctx, repo, repo.FetchRef); err != nil {
		return err
	}
	return s.PrepareFetchedBranch(ctx, repo, repo.FetchRef)
}

func (s *Store) ConfigureRemoteNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	return s.configureRemote(ctx, repo, repo.RemoteURL, nonInteractiveGitEnv())
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
		if _, err := runGitWithEnv(ctx, repo.GitDir, env, "remote", "set-url", "origin", targetRemote); err != nil {
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
	target, err := fetchRefTarget(repo, ref)
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
		if err := readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
			return err
		}
		oldHeadOID, err := refOIDOrZero(ctx, repo.GitDir, "HEAD")
		if err != nil {
			return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		}
		updateArgs := []string{"update-ref", "--no-deref", "HEAD", oid}
		if strings.Trim(oldHeadOID, "0") != "" {
			updateArgs = append(updateArgs, oldHeadOID)
		}
		if _, err := runGit(ctx, repo.GitDir, updateArgs...); err != nil {
			return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
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
	if err := readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "update-ref", refName, oid, oldBranchOID); err != nil {
		return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
	}
	if _, err := runGit(ctx, repo.GitDir, "symbolic-ref", "HEAD", refName); err != nil {
		refErr := restoreRef(ctx, repo.GitDir, refName, oldBranchOID, oid)
		indexErr := rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		return errors.Join(indexErr, refErr)
	}
	if _, err := runGit(ctx, repo.GitDir, "branch", "--set-upstream-to", "origin/"+target.branch, target.branch); err != nil {
		s.logger.WarnContext(ctx, "set upstream failed", "repo", repo.Name, "error", err)
	}
	return nil
}

func refOIDOrZero(ctx context.Context, gitDir string, refName string) (string, error) {
	oid, err := runGit(ctx, gitDir, "rev-parse", "--verify", refName+"^{commit}")
	if err == nil {
		return strings.TrimSpace(oid), nil
	}
	return zeroOIDForGitDir(ctx, gitDir)
}

func rollbackIndexTransition(ctx context.Context, gitDir string, fromOID string, toOID string, cause error) error {
	if err := readTreeTransition(ctx, gitDir, fromOID, toOID); err != nil {
		return errors.Join(cause, fmt.Errorf("restore index after ref update failure: %w", err))
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

func readTreeTransition(ctx context.Context, gitDir string, oldOID string, newOID string) error {
	_, err := runGit(ctx, gitDir, "read-tree", "-m", oldOID, newOID)
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

func fetchRefTarget(repo model.RepoConfig, ref string) (fetchRefInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = strings.TrimSpace(repo.Branch)
	}
	if ref == "" {
		return fetchRefInfo{}, errors.New("fetch ref is required")
	}
	if branch := branchName(ref); branch != "" {
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

func branchName(ref string) string {
	ref = strings.TrimSpace(ref)
	if after, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(ref, "origin/"); ok {
		return after
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
	return ref
}
