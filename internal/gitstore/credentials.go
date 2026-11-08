package gitstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

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
// the client, so lazyFetchEnv adds BatchMode. It does so only when the command
// would run plain ssh: with neither GIT_SSH_COMMAND nor GIT_SSH set, and no
// core.sshCommand in its effective config. The user's own SSH command, such
// as the one an adopted worktree or a prepared git dir keeps, runs unchanged.
// extraEnv is the environment the command adds, which can change its config.
func lazyFetchEnv(ctx context.Context, gitDir string, extraEnv []string) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never"}
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		if _, ok := os.LookupEnv(key); ok {
			return env, nil
		}
	}
	if gitDir == "" {
		// A clone reads no repository config, but git config would read the
		// repository around the working directory.
		gitDir = os.DevNull
	}
	if _, err := runGitWithEnv(ctx, gitDir, extraEnv, "config", "--get", "core.sshCommand"); err == nil {
		return env, nil
	} else if exitErr, ok := errors.AsType[*gitCommandError](err); !ok || exitErr.exitCode != 1 {
		return nil, fmt.Errorf("read core.sshCommand: %w", err)
	}
	return append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes"), nil
}
