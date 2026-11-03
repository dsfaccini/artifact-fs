package gitstore

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// credentialEnv returns a sanitized URL (safe for ps) and env vars that
// configure a one-shot git credential helper to supply the real credentials.
func credentialEnv(rawURL string) (safeURL string, env []string, err error) {
	if rawURL == "" {
		return "", nil, nil
	}
	if strings.ContainsAny(rawURL, "?#") {
		return "", nil, errors.New("remote URL must not include query or fragment")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		if remoteHasInlineCredentials(rawURL) {
			return "", nil, errors.New("malformed remote URL")
		}
		if rawUserinfoCandidateHasPassword(rawURL) {
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
		return auth.HasInlineCredentials(rawURL) || rawUserinfoCandidateHasPassword(rawURL)
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
	return rawUserinfoCandidateHasPassword(rawURL)
}

func rawUserinfoCandidateHasPassword(raw string) bool {
	if isSCPStyleRemote(raw) {
		return false
	}
	if schemeLessUserinfoHasPassword(raw) {
		return true
	}
	prefix := raw
	start := -1
	if i := strings.LastIndex(prefix, "://"); i >= 0 {
		start = i + len("://")
	} else if i := strings.Index(prefix, ":/"); i >= 0 {
		start = i + len(":/")
	} else if i := strings.Index(prefix, "//"); i >= 0 {
		start = i + len("//")
	} else if i := strings.Index(prefix, ":"); i >= 0 {
		start = i + len(":")
	}
	if start < 0 || start >= len(raw) {
		return false
	}
	endChars := "?#"
	if strings.Contains(raw, "://") {
		endChars = "/?#"
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw[start:], endChars); relEnd >= 0 {
		end = start + relEnd
	}
	at := strings.LastIndex(raw[start:end], "@")
	if at < 0 {
		return false
	}
	at += start
	return strings.Contains(raw[start:at], ":")
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

func nonInteractiveGitEnv() []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + sshBatchModeCommand(os.Getenv("GIT_SSH_COMMAND"))}
}

func sshBatchModeCommand(existing string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return "ssh -o BatchMode=yes"
	}
	tokens := splitShellFields(existing)
	filtered := make([]string, 0, len(tokens)+2)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		lower := strings.ToLower(tok)
		if lower == "-o" && i+1 < len(tokens) && isBatchModeOption(tokens[i+1]) {
			i++
			continue
		}
		if strings.HasPrefix(lower, "-obatchmode=") {
			continue
		}
		filtered = append(filtered, tok)
	}
	if len(filtered) == 0 {
		filtered = append(filtered, "ssh")
	}
	filtered = append(filtered, "-o", "BatchMode=yes")
	for i, tok := range filtered {
		filtered[i] = shellQuote(tok)
	}
	return strings.Join(filtered, " ")
}

func splitShellFields(s string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range s {
		if escaped {
			if r == '$' {
				b.WriteString(`\$`)
			} else {
				b.WriteRune(r)
			}
			escaped = false
			continue
		}
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '$' && quote == '\'' {
				b.WriteString(`\$`)
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '\\':
			escaped = true
		case r == ' ' || r == '\t' || r == '\n':
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

func isBatchModeOption(opt string) bool {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(opt)))
	if len(parts) == 0 {
		return false
	}
	return parts[0] == "batchmode" || strings.HasPrefix(parts[0], "batchmode=")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.Contains(s, "$") {
		return doubleQuote(s)
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func doubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '$' {
			b.WriteString(`\$`)
			i++
			continue
		}
		switch s[i] {
		case '\\', '"', '`':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func isShellSafe(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune("@%_+=:,./-~$", r) {
			continue
		}
		return false
	}
	return true
}
