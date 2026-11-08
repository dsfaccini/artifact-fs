package gitstore

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

var errStoreClosed = errors.New("git store closed")

// ValidateRemoteNotOption refuses a remote that git would parse as an option,
// such as --upload-pack=<command>.
func ValidateRemoteNotOption(rawURL string) error {
	if strings.HasPrefix(rawURL, "-") {
		return errors.New("remote URL must not start with '-'")
	}
	return nil
}

// credentialEnv returns a sanitized URL (safe for ps) and env vars that
// configure a one-shot git credential helper to supply the real credentials.
func credentialEnv(rawURL string) (safeURL string, env []string, err error) {
	if rawURL == "" {
		return "", nil, nil
	}
	if err := ValidateRemoteNotOption(rawURL); err != nil {
		return "", nil, err
	}
	if strings.ContainsAny(rawURL, "?#") {
		return "", nil, errors.New("remote URL must not include query or fragment")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		if remoteHasInlineCredentials(rawURL) {
			return "", nil, errors.New("malformed remote URL")
		}
		if auth.RawUserinfoHasPassword(rawURL) {
			return "", nil, errors.New("malformed remote URL")
		}
		if strings.Contains(rawURL, "://") {
			return "", nil, errors.New("malformed remote URL")
		}
		return rawURL, nil, nil
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURL, "#") {
		return "", nil, errors.New("remote URL must not include query or fragment")
	}
	if u.User == nil && strings.Contains(rawURL, "@") && (auth.HasInlineCredentials(rawURL) || malformedUserinfoInRemote(rawURL, u)) {
		return "", nil, errors.New("malformed remote URL")
	}
	if u.User == nil {
		return rawURL, nil, nil
	}
	if auth.HasMisparsedCredentials(rawURL, u) {
		return "", nil, errors.New("malformed remote URL")
	}
	if !isHTTPRemote(rawURL, u.Scheme) {
		if strings.ToLower(u.Scheme) != "ssh" {
			return "", nil, errors.New("remote URL includes unsupported inline credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword || auth.HasInlineCredentials(rawURL) {
			return "", nil, errors.New("remote URL includes unsupported inline credentials")
		}
		return rawURL, nil, nil
	}
	if u.Hostname() == "" {
		return "", nil, errors.New("malformed remote URL")
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	if username == "" && !hasPassword {
		return rawURL, nil, nil
	}

	credentialUsername := username
	credentialPassword := password
	if hasPassword {
		credentialPassword = password
	} else if username != "" {
		// Token-as-username pattern (e.g., https://ghp_xxx@github.com)
		credentialPassword = username
	}
	helper := "!f() { printf '%s\\n' \"username=$ARTIFACT_FS_GIT_USERNAME\" \"password=$ARTIFACT_FS_GIT_PASSWORD\"; }; f"

	u.User = nil
	credentialScope := strings.ToLower(u.Scheme) + "://" + u.Host
	return u.String(), []string{
		"GIT_TERMINAL_PROMPT=0",
		"ARTIFACT_FS_GIT_USERNAME=" + credentialUsername,
		"ARTIFACT_FS_GIT_PASSWORD=" + credentialPassword,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential." + credentialScope + ".helper",
		"GIT_CONFIG_VALUE_1=" + helper,
	}, nil
}

func isHTTPRemote(rawURL string, scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	return strings.HasPrefix(lower, "http:/") || strings.HasPrefix(lower, "https:/") ||
		strings.HasPrefix(lower, "http//") || strings.HasPrefix(lower, "https//") ||
		strings.HasPrefix(lower, "http:") || strings.HasPrefix(lower, "https:")
}

func isMalformedHTTPUserinfo(rawURL string, u *url.URL) bool {
	if !isHTTPRemote(rawURL, u.Scheme) {
		return false
	}
	if u.Host == "" {
		return true
	}
	return strings.HasPrefix(u.Path, "/@")
}

func remoteHasInlineCredentials(rawURL string) bool {
	if strings.ContainsAny(rawURL, "?#") {
		return true
	}
	if schemeLessUserinfoHasPassword(rawURL) {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return auth.HasInlineCredentials(rawURL) || auth.RawUserinfoHasPassword(rawURL)
	}
	if u.User != nil {
		_, hasPassword := u.User.Password()
		return isHTTPRemote(rawURL, u.Scheme) || strings.ToLower(u.Scheme) != "ssh" || hasPassword || auth.HasInlineCredentials(rawURL)
	}
	return strings.Contains(rawURL, "@") && malformedUserinfoInRemote(rawURL, u)
}

func malformedUserinfoInRemote(rawURL string, u *url.URL) bool {
	if isHTTPRemote(rawURL, u.Scheme) {
		return auth.HasInlineCredentials(rawURL)
	}
	if isMalformedHTTPUserinfo(rawURL, u) {
		return true
	}
	if isHTTPRemote(rawURL, u.Scheme) && strings.Contains(u.Hostname(), ".") {
		return false
	}
	return auth.HasMisparsedCredentials(rawURL, u)
}

func schemeLessUserinfoHasPassword(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	if isSCPStyleRemote(raw) {
		return false
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw, "/?#"); relEnd >= 0 {
		end = relEnd
	}
	if end == 0 {
		return false
	}
	prefix := raw[:end]
	at := strings.LastIndex(prefix, "@")
	colon := strings.Index(prefix, ":")
	return colon >= 0 && (at > colon || strings.Contains(raw[end:], "@"))
}

func isSCPStyleRemote(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw, "/?#"); relEnd >= 0 {
		end = relEnd
	}
	prefix := raw[:end]
	at := strings.Index(prefix, "@")
	colon := strings.Index(prefix, ":")
	return at > 0 && colon > at
}

// lazyFetchEnv returns the environment that keeps a Git command that contacts
// a remote from prompting. Run the command with configureCommandSession too:
// without a controlling terminal SSH cannot prompt, and SSH_ASKPASS_REQUIRE
// keeps it from opening an askpass dialog instead. SSH still tries empty
// passwords, and a retried command repeats them against a server that may ban
// the client, so GIT_SSH names a wrapper that runs ssh with BatchMode. Git
// runs GIT_SSH only when neither GIT_SSH_COMMAND nor an effective
// core.sshCommand names an SSH command, so the user's own command, such as the
// one an adopted worktree, a prepared git dir or a conditional include sets,
// runs unchanged. A GIT_SSH the user set stays.
func (s *Store) lazyFetchEnv() ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never"}
	if _, ok := os.LookupEnv("GIT_SSH"); ok {
		return env, nil
	}
	wrapper, err := s.batchModeSSH()
	if err != nil || wrapper == "" {
		return env, err
	}
	return append(env, "GIT_SSH="+wrapper), nil
}

// batchModeSSH returns a program named ssh, so that Git passes it OpenSSH's
// options, that runs the ssh on PATH with BatchMode. It returns "" when PATH
// has no ssh, because Git then reaches no SSH remote either. The Store writes
// the program in a directory of its own that Close removes. It reuses the
// program only while the program and its directory have mode 0700, belong to
// the effective uid, and are no symlinks. Otherwise it writes the program in
// a new directory: a temp cleaner can remove the old one, and another local
// user can then make one at its path. s.mu guards only sshWrapperDir and
// closed, so a slow temp directory stalls no other Store operation.
func (s *Store) batchModeSSH() (string, error) {
	s.mu.Lock()
	closed, dir := s.closed, s.sshWrapperDir
	s.mu.Unlock()
	if closed {
		return "", errStoreClosed
	}
	if dir != "" && lstatPrivate(dir, fs.ModeDir) && lstatPrivate(filepath.Join(dir, "ssh"), 0) {
		return filepath.Join(dir, "ssh"), nil
	}
	// LookPath refuses an ssh that a relative PATH entry finds, so the
	// wrapper runs an absolute path, and never itself.
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return "", nil
	}
	created, err := os.MkdirTemp("", "artifact-fs-ssh-")
	if err != nil {
		return "", fmt.Errorf("create SSH wrapper: %w", err)
	}
	if s.testHookSSHWrapperDirMade != nil {
		s.testHookSSHWrapperDirMade(created)
	}
	wrapper := filepath.Join(created, "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellScriptQuote(ssh)+" -o BatchMode=yes \"$@\"\n"), 0o700); err != nil {
		_ = os.RemoveAll(created)
		return "", fmt.Errorf("create SSH wrapper: %w", err)
	}
	s.mu.Lock()
	closed, current := s.closed, s.sshWrapperDir
	if !closed && current == dir {
		s.sshWrapperDir = created
	}
	s.mu.Unlock()
	switch {
	case closed:
		_ = os.RemoveAll(created)
		return "", errStoreClosed
	case current != dir:
		// A concurrent call replaced dir first.
		_ = os.RemoveAll(created)
		return filepath.Join(current, "ssh"), nil
	}
	if dir != "" {
		// RemoveAll follows no symlink, so it removes nothing outside the old
		// path.
		_ = os.RemoveAll(dir)
	}
	return wrapper, nil
}

// lstatPrivate reports whether path, unfollowed, has type typ and is private
// to the effective uid, as privateToEUID decides.
func lstatPrivate(path string, typ fs.FileMode) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().Type() == typ && privateToEUID(info)
}
