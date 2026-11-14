package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// ReadTreeHEAD initializes the index during controlled clone or preparation.
// Do not call it from delayed HEAD watchers: it can discard newer staged work.
func (s *Store) ReadTreeHEAD(ctx context.Context, repo model.RepoConfig) error {
	_, err := s.runNetworkGit(ctx, repo.GitDir, nil, "read-tree", "HEAD")
	return err
}

// gitIndexPath is the index file git reads for gitDir: GIT_INDEX_FILE when it is
// set.
func gitIndexPath(gitDir string) (string, error) {
	if alternateIndex, ok := os.LookupEnv("GIT_INDEX_FILE"); ok {
		if alternateIndex == "" {
			return "", errors.New("GIT_INDEX_FILE is empty")
		}
		return alternateIndex, nil
	}
	return filepath.Join(gitDir, "index"), nil
}

// EnsureIndexInitialized creates a missing index without replacing staged work.
func (s *Store) EnsureIndexInitialized(ctx context.Context, repo model.RepoConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := gitIndexPath(repo.GitDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		// Validate the effective index without refreshing it. Unlike read-tree,
		// ls-files does not replace staged state or resolve missing blobs.
		return runGitDiscardOutputWithEnv(
			ctx,
			repo.GitDir,
			[]string{"GIT_NO_LAZY_FETCH=1"},
			"-c",
			"core.fsmonitor=false",
			"ls-files",
			"--stage",
			"--",
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.ReadTreeHEAD(ctx, repo)
}

// WarnAdoptedRemoteCredentials logs one warning when an adopted worktree's
// origin or one of its promisor remotes embeds credentials. The warning leaves
// the remote out: redaction is heuristic, and this remote is known to hold
// credentials.
//
// add-repo never persists inline credentials: a sync clone uses them once
// and stores the URL without them, and async and prepared repos reject them,
// because the daemon would store the URL and fetch with it on its own
// schedule. Adoption only warns. It stores no URL and never clones, fetches or
// refreshes. The remotes stay the user's existing Git config. Git's lazy
// fetches and the hydrator's batch fetches of missing blobs and trees use the
// promisor remotes, as any Git command in the worktree would. ArtifactFS
// redacts all Git output it logs.
func (s *Store) WarnAdoptedRemoteCredentials(ctx context.Context, repo model.RepoConfig) {
	names := []string{"origin"}
	if promisors, err := promisorRemotes(ctx, repo.GitDir); err == nil {
		names = append(names, promisors...)
	}
	for _, name := range names {
		if remote, err := runGit(ctx, repo.GitDir, "remote", "get-url", "--", name); err == nil && remoteHasInlineCredentials(remote) {
			s.logger.WarnContext(ctx, "adopted worktree remote embeds credentials; fetches of missing objects use them", "repo", repo.Name)
			return
		}
	}
}

// ConfigureAdoptedWorktree preserves Git's refs, remotes, tracking and index.
// Filesystem settings belong to the adopted worktree, never its native siblings.
func (s *Store) ConfigureAdoptedWorktree(ctx context.Context, repo model.RepoConfig) error {
	if strings.TrimSpace(repo.GitDir) == "" || strings.TrimSpace(repo.MountPath) == "" {
		return errors.New("adoption requires a git dir and mount path")
	}
	if _, _, err := s.ResolveHEAD(ctx, repo); err != nil {
		return err
	}
	// The HEAD watcher polls loose refs and packed-refs only.
	if format, err := runGit(ctx, repo.GitDir, "config", "--get", "extensions.refStorage"); err == nil && format != "files" {
		return fmt.Errorf("adoption requires the files ref storage format, not %s", format)
	} else if err != nil {
		var exitErr *gitCommandError
		if !errors.As(err, &exitErr) || exitErr.exitCode != 1 {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(repo.GitDir, "index")); err != nil {
		return fmt.Errorf("adoption requires an initialized index: %w", err)
	}
	commonDir, err := runGit(ctx, repo.GitDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	commonDir, err = filepath.EvalSymlinks(strings.TrimSpace(commonDir))
	if err != nil {
		return err
	}
	gitDir, err := filepath.Abs(repo.GitDir)
	if err != nil {
		return err
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return err
	}
	registeredDir, err := filepath.EvalSymlinks(filepath.Join(commonDir, "worktrees"))
	if err != nil || gitDir == commonDir || filepath.Dir(gitDir) != registeredDir {
		return errors.New("adoption requires a registered linked worktree git dir")
	}
	gitfile, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return err
	}
	registeredGitfile := strings.TrimSpace(string(gitfile))
	if !filepath.IsAbs(registeredGitfile) {
		registeredGitfile = filepath.Join(gitDir, registeredGitfile)
	}
	registeredMount, err := filepath.EvalSymlinks(filepath.Dir(registeredGitfile))
	if err != nil {
		return err
	}
	requestedMount, err := filepath.Abs(repo.MountPath)
	if err != nil {
		return err
	}
	requestedMount, err = filepath.EvalSymlinks(requestedMount)
	if err != nil {
		return err
	}
	if filepath.Base(registeredGitfile) != ".git" || registeredMount != requestedMount {
		return errors.New("adopted mount path must match the registered worktree path")
	}
	commonConfig := filepath.Join(commonDir, "config")
	mainConfig := filepath.Join(commonDir, "config.worktree")
	// Git's migration moves shared core.worktree and core.bare=true into the
	// primary worktree's config.worktree. Linked worktrees ignore those shared
	// keys until the extension is enabled, so enabling it last means an
	// interrupted migration never repoints them. Keep an existing primary
	// override: it wins once the extension is on, and Git ignores it before.
	for _, key := range []string{"core.worktree", "core.bare"} {
		args := []string{"config", "--file", commonConfig}
		if key == "core.bare" {
			args = append(args, "--bool", "--get", key)
		} else {
			// Keep path whitespace across runGit's textual output trimming.
			args = append(args, "--null", "--get-regexp", "^core\\.worktree$")
		}
		value, readErr := runGit(ctx, repo.GitDir, args...)
		if readErr != nil {
			var exitErr *gitCommandError
			if errors.As(readErr, &exitErr) && exitErr.exitCode == 1 {
				continue
			}
			return readErr
		}
		if key == "core.worktree" {
			_, path, found := strings.Cut(value, "\n")
			if !found || !strings.HasSuffix(path, "\x00") {
				return errors.New("invalid core.worktree config output")
			}
			value = strings.TrimSuffix(path, "\x00")
		} else if value != "true" {
			continue
		}
		if _, readErr := runGit(ctx, repo.GitDir, "config", "--file", mainConfig, "--get", key); readErr != nil {
			var exitErr *gitCommandError
			if !errors.As(readErr, &exitErr) || exitErr.exitCode != 1 {
				return readErr
			}
			if _, err := runGit(ctx, repo.GitDir, "config", "--file", mainConfig, key, value); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"core.worktree", "core.bare"} {
		if key == "core.bare" {
			value, err := runGit(ctx, repo.GitDir, "config", "--file", commonConfig, "--bool", "--get", key)
			if err != nil {
				var exitErr *gitCommandError
				if errors.As(err, &exitErr) && exitErr.exitCode == 1 {
					continue
				}
				return err
			}
			if strings.TrimSpace(value) != "true" {
				continue
			}
		}
		if _, err := runGit(ctx, repo.GitDir, "config", "--file", commonConfig, "--unset-all", key); err != nil {
			var exitErr *gitCommandError
			if !errors.As(err, &exitErr) || exitErr.exitCode != 5 {
				return err
			}
		}
	}
	if _, err := runGit(ctx, repo.GitDir, "config", "--file", commonConfig, "extensions.worktreeConfig", "true"); err != nil {
		return err
	}
	for _, setting := range [][2]string{
		{"core.bare", "false"},
		{"core.worktree", requestedMount},
		{"core.filemode", "true"},
		{"core.ignorecase", "false"},
		{"core.symlinks", "true"},
	} {
		if _, err := runGit(ctx, repo.GitDir, "config", "--worktree", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ConfigureStatusOptimization(ctx context.Context, repo model.RepoConfig, stateRoot string) error {
	if repo.RequiredCommit != "" && strings.TrimSpace(repo.MountPath) != "" {
		if _, err := os.Stat(repo.MountPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		// Verified acquisitions use a standalone Git directory. Pin it only
		// after FUSE mounts so index refreshes cannot infer the caller's cwd.
		if _, err := runGit(ctx, repo.GitDir, "config", "core.worktree", repo.MountPath); err != nil {
			return err
		}
	}
	return s.ConfigureFSMonitorHook(ctx, repo, stateRoot)
}

// ConfigureFSMonitorHook installs ArtifactFS's hook without probing the
// worktree, which may still be gated during startup. It writes only the hook
// config and core.untrackedCache=true, which turns on Git's untracked cache
// beside the hook. An adopted worktree writes them to config.worktree, which
// ConfigureAdoptedWorktree must have enabled.
//
// The hook is the link FSMonitorHookLink returns, which Git runs without a
// shell. Git runs a path with a shell metacharacter through sh -c unquoted,
// and sh splits it, so for such a link path the hook is the script at
// <git dir>/hooks/artifact-fs-fsmonitor, which quotes the state root. The
// hook is the script also when privateAncestors finds a directory above the
// state root that another user can rename. Git then runs a file in the git
// dir, and the hook refuses a state root that another user put in its place.
// A git dir under the state root, the default for a clone, lies under the
// same directories, so there the script protects nothing more.
func (s *Store) ConfigureFSMonitorHook(ctx context.Context, repo model.RepoConfig, stateRoot string) error {
	configArgs := []string{"config"}
	if repo.AdoptGitDir {
		configArgs = append(configArgs, "--worktree")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	hookPath := FSMonitorHookLink(stateRoot, repo.ID)
	if strings.ContainsAny(hookPath, gitShellMeta) || !privateAncestors(stateRoot) {
		hookPath = fsmonitorHookPath(repo.GitDir)
		if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
			return err
		}
		if err := s.replaceExecutable(hookPath, fsmonitorHookScript(stateRoot, exe, repo.Name)); err != nil {
			return err
		}
	} else if err := s.replaceLink(hookPath, exe); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, append(configArgs, "core.fsmonitor", hookPath)...); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, append(configArgs, "fsmonitor.allowRemote", "true")...); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, append(configArgs, "core.untrackedCache", "true")...); err != nil {
		return err
	}
	// Git adds the fsmonitor extension on the next porcelain index read. The
	// daemon must not write the shared index behind a concurrent Git process.
	return nil
}

// replaceExecutable writes script to a new file beside path, then renames it
// over path. Linux refuses to exec a file that is open for writing, so
// rewriting the hook in place would fail a Git that runs it meanwhile, and Git
// would then rescan the worktree.
func (s *Store) replaceExecutable(path, script string) error {
	// A fork in another goroutine while the new file is open for writing
	// would hand the child its write descriptor until the child execs
	// (golang/go#22315), and the rename would install that file as the hook.
	syscall.ForkLock.RLock()
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		syscall.ForkLock.RUnlock()
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(script)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	syscall.ForkLock.RUnlock()
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err == nil && s.testHookBeforeHookRename != nil {
		s.testHookBeforeHookRename(path, tmp)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// replaceLink makes a symlink to target in a new directory beside path, then
// renames it over path, so a Git that runs the hook meanwhile finds the old
// link or the new one.
func (s *Store) replaceLink(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	link := filepath.Join(tmp, filepath.Base(path))
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	if s.testHookBeforeHookRename != nil {
		s.testHookBeforeHookRename(path, link)
	}
	return os.Rename(link, path)
}

func fsmonitorHookPath(gitDir string) string {
	return filepath.Join(gitDir, "hooks", "artifact-fs-fsmonitor")
}

// gitShellMeta holds the characters that make Git run a command through
// sh -c, from prepare_shell_cmd in Git's run-command.c.
const gitShellMeta = "|&;<>()$`\\\"' \t\n*?[#~=%"

// fsmonitorHookLinkName names the link FSMonitorHookLink returns.
const fsmonitorHookLinkName = "fsmonitor-hook"

// FSMonitorHookLink returns the path of the symlink to the artifact-fs
// executable that Git runs as repo id's FSMonitor hook. The hook reads the
// state root and the repo from it with ParseFSMonitorHookLink.
func FSMonitorHookLink(stateRoot string, id model.RepoID) string {
	return filepath.Join(stateRoot, "overlays", string(id), fsmonitorHookLinkName)
}

// fsmonitorHookLinkPattern returns an extended regular expression that
// matches every path FSMonitorHookLink returns for id, under any state root.
func fsmonitorHookLinkPattern(id model.RepoID) string {
	return "/(.*/)?overlays/" + regexp.QuoteMeta(string(id)) + "/" + fsmonitorHookLinkName
}

// ParseFSMonitorHookLink returns the state root and the repo ID of the hook
// link at path, the program name Git runs it with. It returns false for any
// other path.
func ParseFSMonitorHookLink(path string) (stateRoot string, id model.RepoID, ok bool) {
	overlayDir := filepath.Dir(path)
	overlays := filepath.Dir(overlayDir)
	if !filepath.IsAbs(path) || filepath.Base(path) != fsmonitorHookLinkName || filepath.Base(overlays) != "overlays" {
		return "", "", false
	}
	return filepath.Dir(overlays), model.RepoID(filepath.Base(overlayDir)), true
}

// ReleaseAdoptedWorktree hands a worktree back to native Git once remove-repo
// releases it. It unsets the FSMonitor hook, core.untrackedCache, and the
// filesystem settings that ConfigureAdoptedWorktree pinned for the mount, so
// the worktree inherits its repository's settings again. It unsets a key only
// while it holds the value adoption writes, so an adoption that failed before
// writing it leaves the worktree's own value. A value the worktree's own config
// held before a successful adoption does not come back, because adoption
// overwrote it. The core.worktree and core.bare pins stay, because they match
// the registered worktree. extensions.worktreeConfig stays on because sibling
// worktrees may now keep their own settings in config.worktree. It unsets the
// hook script in the git dir and a hook link for the repo's ID under any state
// root, so it also unsets a link the mount made before the state root moved.
func (s *Store) ReleaseAdoptedWorktree(ctx context.Context, repo model.RepoConfig) error {
	// --worktree resolves to this file, but fails once the extension is off.
	worktreeConfig := filepath.Join(repo.GitDir, "config.worktree")
	if _, err := os.Stat(worktreeConfig); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	for _, pin := range [][2]string{
		{"core.fsmonitor", "^(" + fsmonitorHookLinkPattern(repo.ID) + "|" + regexp.QuoteMeta(fsmonitorHookPath(repo.GitDir)) + ")$"},
		{"fsmonitor.allowRemote", "^true$"},
		{"core.untrackedCache", "^true$"},
		{"core.filemode", "^true$"},
		{"core.ignorecase", "^false$"},
		{"core.symlinks", "^true$"},
	} {
		// Git matches a value pattern in its locale. Under a UTF-8 locale "."
		// matches no byte that is not valid UTF-8, which a path can hold, and
		// the C locale matches each byte.
		if _, err := runGitWithEnv(ctx, repo.GitDir, []string{"LC_ALL=C"}, "config", "--file", worktreeConfig, "--unset-all", pin[0], pin[1]); err != nil {
			var exitErr *gitCommandError
			if !errors.As(err, &exitErr) || exitErr.exitCode != 5 {
				return err
			}
		}
	}
	return nil
}

// FSMonitorBootstrapEnv marks the FSMonitor hook query that
// BootstrapCleanIndexForFSMonitor's update-index makes, which may run before
// the worktree is mounted.
const FSMonitorBootstrapEnv = "ARTIFACT_FS_FSMONITOR_BOOTSTRAP"

// BootstrapCleanIndexForFSMonitor marks snapshot paths valid only while the
// real index is locked and proven to match the immutable snapshot commit.
func (s *Store) BootstrapCleanIndexForFSMonitor(ctx context.Context, repo model.RepoConfig, snapshotOID string, validate func() (bool, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if snapshotOID == "" {
		return false, nil
	}
	if _, ok := os.LookupEnv("GIT_INDEX_FILE"); ok {
		return false, nil
	}
	skip := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	indexPath := filepath.Join(repo.GitDir, "index")
	lockPath := indexPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	lockOwned := true
	defer func() {
		_ = lock.Close()
		if lockOwned {
			_ = os.Remove(lockPath)
		}
	}()

	info, err := os.Stat(indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := lock.Chmod(info.Mode().Perm()); err != nil {
		return false, err
	}
	privateIndex, err := os.CreateTemp(filepath.Dir(indexPath), ".artifact-fs-index-*")
	if err != nil {
		return false, err
	}
	privatePath := privateIndex.Name()
	defer os.Remove(privatePath)
	if err := privateIndex.Chmod(info.Mode().Perm()); err != nil {
		privateIndex.Close()
		return false, err
	}
	if err := privateIndex.Close(); err != nil {
		return false, err
	}
	if err := copyFile(indexPath, privatePath); err != nil {
		return false, err
	}
	privateWorktree, err := os.MkdirTemp(s.tempDir, "artifact-fs-index-worktree-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(privateWorktree)
	privateHooks := filepath.Join(privateWorktree, "hooks")
	if err := os.Mkdir(privateHooks, 0o700); err != nil {
		return false, err
	}
	env := []string{"GIT_INDEX_FILE=" + privatePath, "GIT_WORK_TREE=" + privateWorktree, "GIT_NO_LAZY_FETCH=1"}
	run := func(args ...string) (string, error) {
		return runGitWithEnv(ctx, repo.GitDir, env, args...)
	}
	if err := runGitDiscardOutputWithEnv(ctx, repo.GitDir, env, "-c", "core.fsmonitor=false", "ls-files", "--stage", "--", "."); err != nil {
		return skip()
	}
	if out, err := run("-c", "core.fsmonitor=false", "diff-index", "--cached", "--quiet", "--no-renames", "--ita-visible-in-index", snapshotOID, "--"); err != nil || strings.TrimSpace(out) != "" {
		return skip()
	}
	if out, err := run("-c", "core.fsmonitor=false", "diff-index", "--cached", "--quiet", "--no-renames", "--ita-invisible-in-index", snapshotOID, "--"); err != nil || strings.TrimSpace(out) != "" {
		return skip()
	}
	if head, err := run("rev-parse", "--verify", "HEAD"); err != nil || strings.TrimSpace(head) != snapshotOID {
		return skip()
	}
	entries, err := run("-c", "core.fsmonitor=false", "ls-files", "--stage", "-z", "--", ".")
	if err != nil {
		return skip()
	}
	paths := make([]string, 0)
	for entry := range strings.SplitSeq(entries, "\x00") {
		if entry == "" {
			continue
		}
		meta, path, ok := strings.Cut(entry, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 || parts[2] != "0" {
			return false, nil
		}
		if parts[0] != "160000" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return false, nil
	}
	if out, err := run("config", "--type=bool", "--default=false", "--get", "core.autocrlf"); err != nil || strings.EqualFold(strings.TrimSpace(out), "true") {
		return skip()
	}
	configs, err := run("config", "--null", "--list")
	if err != nil {
		return skip()
	}
	for entry := range strings.SplitSeq(configs, "\x00") {
		key, _, ok := strings.Cut(entry, "\n")
		if ok && strings.EqualFold(key, "core.attributesfile") {
			return false, nil
		}
	}
	pathInput := []byte(strings.Join(paths, "\x00") + "\x00")
	attrs, err := runGitInput(ctx, repo.GitDir, env, pathInput, "-c", "core.fsmonitor=false", "check-attr", "--cached", "--all", "-z", "--stdin")
	if err != nil {
		return skip()
	}
	textAttrs := map[string]string{}
	eolLF := map[string]bool{}
	attrFields := strings.Split(attrs, "\x00")
	for i := 0; i+2 < len(attrFields); i += 3 {
		path, name, value := attrFields[i], attrFields[i+1], attrFields[i+2]
		switch name {
		case "crlf", "ident", "filter", "working-tree-encoding":
			return false, nil
		case "eol":
			if value == "crlf" {
				return false, nil
			}
			eolLF[path] = value == "lf"
		case "text":
			textAttrs[path] = value
		}
	}
	// Status compares Git's check-in conversion of the worktree file, which
	// holds the blob's bytes, with the blob. -text and text=auto keep those
	// bytes. text, text=input and eol=lf without text=auto turn CRLF into LF,
	// so leave those paths for Git to compare. check-attr prints -text and the
	// literal text=unset alike, and Git converts the second under eol=lf.
	validPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		text := textAttrs[path]
		if text == "set" || text == "input" || (eolLF[path] && text != "auto") {
			continue
		}
		validPaths = append(validPaths, path)
	}
	if len(validPaths) == 0 {
		return false, nil
	}
	if err := verifyGitAttributesPresent(ctx, repo, env); err != nil {
		return skip()
	}
	validInput := []byte(strings.Join(validPaths, "\x00") + "\x00")
	updateEnv := append([]string{FSMonitorBootstrapEnv + "=1"}, env...)
	// An untracked cache this Git wrote would name the private worktree. Once
	// remove-repo unsets core.untrackedCache, native Git keeps such a cache
	// and warns on every status that it cannot use it, so this Git drops the
	// cache. The mount's next status builds it again.
	if _, err := runGitInput(ctx, repo.GitDir, updateEnv, validInput, "-c", "core.hooksPath="+privateHooks, "-c", "core.untrackedCache=false", "update-index", "--fsmonitor", "--fsmonitor-valid", "-z", "--stdin"); err != nil {
		return skip()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if head, err := run("rev-parse", "--verify", "HEAD"); err != nil || strings.TrimSpace(head) != snapshotOID {
		return skip()
	}
	updated, err := os.ReadFile(privatePath)
	if err != nil {
		return false, err
	}
	if validate != nil {
		valid, err := validate()
		if err != nil {
			return false, err
		}
		if !valid {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := lock.Write(updated); err != nil {
		return false, err
	}
	if err := lock.Sync(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := lock.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(lockPath, indexPath); err != nil {
		return false, err
	}
	lockOwned = false
	return true, nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func verifyGitAttributesPresent(ctx context.Context, repo model.RepoConfig, env []string) error {
	entries, err := runGitWithEnv(ctx, repo.GitDir, env, "-c", "core.fsmonitor=false", "ls-files", "--stage", "-z", "--", ".")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(entries, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || filepath.Base(path) != ".gitattributes" {
			continue
		}
		parts := strings.Fields(meta)
		if len(parts) != 3 || parts[2] != "0" {
			return errors.New("unmerged .gitattributes entry")
		}
		if err := runGitDiscardOutputWithEnv(ctx, repo.GitDir, env, "cat-file", "-e", parts[1]+"^{blob}"); err != nil {
			return err
		}
	}
	return nil
}

func fsmonitorHookScript(stateRoot, exe, repoName string) string {
	return fmt.Sprintf("#!/bin/sh\nARTIFACT_FS_ROOT=%s exec %s fsmonitor-hook --name %s \"$@\"\n", shellScriptQuote(stateRoot), shellScriptQuote(exe), shellScriptQuote(repoName))
}

func shellScriptQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafeScriptValue(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func isShellSafeScriptValue(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune("@%_+=:,./-~", r) {
			continue
		}
		return false
	}
	return true
}
