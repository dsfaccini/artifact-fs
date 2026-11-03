package auth

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var tokenLike = regexp.MustCompile(`(?i)(access_token|token|password|passwd|secret|key|authorization|x-token-auth)=([^&\s]+)`)

const (
	maxLogStringBytes       = 16 << 10
	logStringTruncatedLabel = "[diagnostic truncated]"
)

func RedactRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	if start, end, ok := userinfoBeforeSeparator(raw); ok {
		raw = raw[:start] + "REDACTED" + raw[end:]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactMalformedURL(raw)
	}
	if u.User == nil && strings.Contains(raw, "@") && (isMalformedHTTPUserinfo(raw, u) || schemeLessUserinfoStart(raw) >= 0 || userinfoAfterLaterSeparator(raw, u)) {
		return redactMalformedURL(raw)
	}
	if u.User != nil {
		username := u.User.Username()
		if _, ok := u.User.Password(); ok || username != "" {
			u.User = url.User("REDACTED")
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery = "REDACTED"
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		u.Fragment = "REDACTED"
	}
	return u.String()
}

func redactMalformedURL(raw string) string {
	redacted := raw
	authorityStart := malformedAuthorityStart(redacted)
	if authorityStart < 0 {
		return redactQueryFragment(tokenLike.ReplaceAllString(redacted, `$1=REDACTED`))
	}
	userinfoEnd := len(redacted)
	if relEnd := strings.IndexAny(redacted[authorityStart:], "?#"); relEnd >= 0 {
		userinfoEnd = authorityStart + relEnd
	}
	if at := strings.LastIndex(redacted[authorityStart:userinfoEnd], "@"); at >= 0 {
		redacted = redacted[:authorityStart] + "REDACTED" + redacted[authorityStart+at:]
	} else if userinfoEnd < len(redacted) && strings.Contains(redacted[userinfoEnd:], "@") && userinfoLikeBeforePath(redacted[authorityStart:userinfoEnd]) {
		redacted = redacted[:authorityStart] + "REDACTED" + redacted[userinfoEnd:]
	}
	redacted = tokenLike.ReplaceAllString(redacted, `$1=REDACTED`)
	return redactQueryFragment(redacted)
}

func redactQueryFragment(raw string) string {
	q := strings.Index(raw, "?")
	f := strings.Index(raw, "#")
	if q >= 0 && (f < 0 || q < f) {
		if f >= 0 {
			return raw[:q+1] + "REDACTED#REDACTED"
		}
		return raw[:q+1] + "REDACTED"
	}
	if f >= 0 {
		return raw[:f+1] + "REDACTED"
	}
	return raw
}

func HasInlineCredentials(raw string) bool {
	if strings.ContainsAny(raw, "?#") {
		return true
	}
	if _, _, ok := userinfoBeforeSeparator(raw); ok {
		return true
	}
	urlLike := strings.Contains(raw, "://")
	u, err := url.Parse(raw)
	if err != nil {
		if malformedAuthorityStart(raw) >= 0 && strings.Contains(raw, "@") {
			return true
		}
		if urlLike && strings.ContainsAny(raw, "@?#") {
			return true
		}
		return tokenLike.MatchString(raw)
	}
	if u.User == nil && strings.Contains(raw, "@") && (isMalformedHTTPUserinfo(raw, u) || schemeLessUserinfoStart(raw) >= 0) {
		return true
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return true
	}
	if tokenLike.MatchString(u.RawQuery) {
		return true
	}
	if u.User == nil && userinfoAfterLaterSeparator(raw, u) {
		// A username alone, as in "codecommit::us-east-1://profile@Repo", names
		// an ambient profile unless the address is http(s).
		separator := strings.Index(raw, "://")
		lower := strings.ToLower(raw[:separator])
		return RawUserinfoHasPassword(raw) || strings.HasSuffix(lower, "http") || strings.HasSuffix(lower, "https")
	}
	if u.User == nil {
		return false
	}
	username := u.User.Username()
	_, hasPassword := u.User.Password()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return username != "" || hasPassword
	case "ssh":
		return hasPassword || tokenLikeUsername(username)
	default:
		return username != "" || hasPassword
	}
}

func malformedAuthorityStart(raw string) int {
	lower := strings.ToLower(strings.TrimSpace(raw))
	// Userinfo follows a "://" unless every "@" precedes it, as in "user:pass@host://".
	if i := strings.Index(lower, "://"); i >= 0 && strings.Contains(lower[i:], "@") {
		return i + len("://")
	}
	if schemeLessUserinfoStart(raw) >= 0 {
		return 0
	}
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, prefix) {
			return len(prefix)
		}
	}
	if i := strings.Index(lower, "://"); i >= 0 {
		return i + len("://")
	}
	return -1
}

// userinfoBeforeSeparator returns the bounds of "user:pass" text that ends at
// an "@" before the first "://" when an "@" also follows that "://", as in
// "alice:pw@host/a://b@c". The heuristics take the userinfo to follow the
// "://", but either side can hold it.
func userinfoBeforeSeparator(raw string) (start int, end int, ok bool) {
	separator := strings.Index(raw, "://")
	if separator < 0 || !strings.Contains(raw[separator:], "@") {
		return 0, 0, false
	}
	end = strings.LastIndex(raw[:separator], "@")
	if end < 0 {
		return 0, 0, false
	}
	start = strings.LastIndex(raw[:end], "/") + 1
	lower := strings.ToLower(raw)
	for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, marker) && len(marker) > start {
			start = len(marker)
		}
	}
	if start > end || !strings.Contains(raw[start:end], ":") {
		return 0, 0, false
	}
	return start, end, true
}

// userinfoAfterLaterSeparator reports an "@" after a "://" that does not end
// the parsed scheme, as in "ssh:/x://u:pw@h" or "gcrypt::https://u:pw@h". The
// parser keeps that userinfo in the path or opaque part.
func userinfoAfterLaterSeparator(raw string, u *url.URL) bool {
	separator := strings.Index(raw, "://")
	return u.Scheme != "" && separator >= 0 && separator != len(u.Scheme) && strings.Contains(raw[separator:], "@")
}

func isHTTPRemoteLike(raw string, scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	case "":
		return malformedAuthorityStart(raw) >= 0
	default:
		return false
	}
}

func isMalformedHTTPUserinfo(raw string, u *url.URL) bool {
	if !isHTTPRemoteLike(raw, u.Scheme) {
		return false
	}
	if u.Host == "" {
		return true
	}
	if strings.HasPrefix(u.Path, "/@") {
		return true
	}
	if rawUsernameBeforeDelimiter(raw) {
		return true
	}
	if strings.Contains(u.Path, "@") {
		if slashAfterPathAt(u.Path) {
			host := u.Hostname()
			return !(isRealHost(host) && !isUserLikeHost(host) && pathBeforeAtSegmentCount(u.Path) > 1)
		}
		return !validParsedHostPort(u)
	}
	if !RawUserinfoHasPassword(raw) {
		return false
	}
	pathBeforeAt := u.Path
	if at := strings.Index(pathBeforeAt, "@"); at >= 0 {
		pathBeforeAt = pathBeforeAt[:at]
	}
	if strings.Count(strings.Trim(pathBeforeAt, "/"), "/") > 0 {
		host := u.Hostname()
		if isRealHost(host) && !isUserLikeHost(host) && !slashAfterPathAt(u.Path) {
			return false
		}
	}
	return true
}

func isRealHost(host string) bool {
	return strings.Contains(host, ".") || host == "localhost" || net.ParseIP(host) != nil
}

func validParsedHostPort(u *url.URL) bool {
	if u.Host == "" {
		return false
	}
	if strings.Contains(u.Path, "@") && slashAfterPathAt(u.Path) {
		return false
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		return true
	}
	if isUserLikeHost(host) {
		return false
	}
	if u.Port() != "" {
		return isRealHost(host) || isKnownSingleLabelGitHost(host)
	}
	if strings.Contains(u.Path, "@") && !isRealHost(host) && !isKnownSingleLabelGitHost(host) {
		return false
	}
	return !strings.Contains(u.Host, ":") || u.Port() != ""
}

func isUserLikeHost(host string) bool {
	return host == "user" || host == "user.name" || host == "first.last"
}

func tokenLikeUsername(username string) bool {
	lower := strings.ToLower(username)
	return strings.HasPrefix(lower, "ghp_") ||
		strings.HasPrefix(lower, "github_pat_") ||
		strings.HasPrefix(lower, "glpat-") ||
		strings.HasPrefix(lower, "gho_") ||
		strings.HasPrefix(lower, "ghu_") ||
		strings.HasPrefix(lower, "ghs_") ||
		strings.HasPrefix(lower, "ghr_") ||
		lower == "x-token-auth" ||
		lower == "oauth2"
}

func isKnownSingleLabelGitHost(host string) bool {
	return host == "git" || host == "ghe"
}

func slashAfterPathAt(path string) bool {
	at := strings.Index(path, "@")
	return at >= 0 && strings.Contains(path[at:], "/")
}

func pathBeforeAtSegmentCount(path string) int {
	if at := strings.Index(path, "@"); at >= 0 {
		path = path[:at]
	}
	count := 0
	for segment := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if segment != "" {
			count++
		}
	}
	return count
}

func rawUsernameBeforeDelimiter(raw string) bool {
	authorityStart := malformedAuthorityStart(raw)
	if authorityStart < 0 {
		return false
	}
	at := strings.LastIndex(raw[authorityStart:], "@")
	if at < 0 {
		return false
	}
	at += authorityStart
	if relEnd := strings.IndexAny(raw[authorityStart:at], "?#"); relEnd >= 0 {
		candidate := raw[authorityStart : authorityStart+relEnd]
		return userinfoLikeBeforePath(candidate)
	}
	return false
}

func userinfoLikeBeforePath(candidate string) bool {
	if candidate == "" {
		return false
	}
	if before, _, ok := strings.Cut(candidate, "/"); ok {
		prefix := before
		return strings.Contains(prefix, ":") || !isRealHost(prefix)
	}
	return true
}

func schemeLessUserinfoStart(raw string) int {
	// Userinfo follows a "://" unless every "@" precedes it, as in "user:pass@host://".
	if _, after, ok := strings.Cut(raw, "://"); ok && strings.Contains(after, "@") {
		return -1
	}
	if isSCPStyleRemote(raw) {
		return -1
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw, "/?#"); relEnd >= 0 {
		end = relEnd
	}
	if end == 0 {
		return -1
	}
	prefix := raw[:end]
	at := strings.LastIndex(prefix, "@")
	colon := strings.Index(prefix, ":")
	if colon >= 0 && (at > colon || strings.Contains(raw[end:], "@")) {
		return 0
	}
	return -1
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

// RawUserinfoHasPassword reports whether a remote that url.Parse misreads or
// rejects, such as "https:user:pw@host", holds "user:pass@" userinfo. It checks
// two candidate authorities and reports a password when either holds one.
func RawUserinfoHasPassword(raw string) bool {
	return authorityUserinfoHasPassword(raw) || delimitedUserinfoHasPassword(raw)
}

// authorityUserinfoHasPassword checks the authority malformedAuthorityStart
// finds, up to the last "@" before any "?" or "#".
func authorityUserinfoHasPassword(raw string) bool {
	authorityStart := malformedAuthorityStart(raw)
	if authorityStart < 0 {
		return false
	}
	userinfoEnd := len(raw)
	if relEnd := strings.IndexAny(raw[authorityStart:], "?#"); relEnd >= 0 {
		userinfoEnd = authorityStart + relEnd
	}
	at := strings.LastIndex(raw[authorityStart:userinfoEnd], "@")
	if at < 0 {
		return false
	}
	candidate := raw[authorityStart : authorityStart+at]
	return strings.Contains(candidate, ":")
}

// delimitedUserinfoHasPassword checks the text after the last "://", or else
// after the first ":/", "//" or ":", up to the last "@" before the next "/" (or
// "?" or "#" when there is no "://"). It finds userinfo behind a path, as in
// "x/alice:pw@host/a@b", which the authority check reads differently.
func delimitedUserinfoHasPassword(raw string) bool {
	if isSCPStyleRemote(raw) {
		return false
	}
	start := -1
	if i := strings.LastIndex(raw, "://"); i >= 0 {
		start = i + len("://")
	} else if i := strings.Index(raw, ":/"); i >= 0 {
		start = i + len(":/")
	} else if i := strings.Index(raw, "//"); i >= 0 {
		start = i + len("//")
	} else if i := strings.Index(raw, ":"); i >= 0 {
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
	return at >= 0 && strings.Contains(raw[start:start+at], ":")
}

func RedactString(s string) string {
	if s == "" {
		return ""
	}
	// Redact any URL-shaped substring with credentials (not just those with @)
	if strings.Contains(s, "://") || strings.ContainsAny(s, "?#@") {
		parts := strings.Split(s, " ")
		for i := range parts {
			if shouldRedactRemoteToken(parts[i]) {
				parts[i] = redactRemoteToken(parts[i])
			}
		}
		s = strings.Join(parts, " ")
	}
	s = tokenLike.ReplaceAllString(s, `$1=REDACTED`)
	return s
}

// RedactLogString removes complete remote references in addition to credentials.
// Logs need the transport error, not repository locations that may be private.
func RedactLogString(s string, sensitiveValues ...string) string {
	values := append([]string(nil), sensitiveValues...)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		if value != "" {
			s = strings.ReplaceAll(s, value, "REDACTED_REMOTE")
			s = redactTruncatedSensitivePrefix(s, value)
		}
	}
	// RedactString can strip every remote marker from a token, so the token
	// before it also decides whether a remote goes. Both split on " ".
	unredacted := strings.Split(s, " ")
	s = RedactString(s)
	if s != "" && (strings.Contains(s, "://") || strings.ContainsAny(s, "@?#")) {
		parts := strings.Split(s, " ")
		for i, token := range parts {
			if !shouldRedactRemoteToken(token) && (i >= len(unredacted) || !shouldRedactRemoteToken(unredacted[i])) {
				continue
			}
			start := strings.IndexFunc(token, func(r rune) bool {
				return !strings.ContainsRune("'\"([{<", r)
			})
			end := strings.LastIndexFunc(token, func(r rune) bool {
				return !strings.ContainsRune("'\")]}>,.:", r)
			})
			if start < 0 || end < start {
				continue
			}
			core := token[start : end+1]
			remoteStart := remoteStartIndex(core)
			// Text with an "@" before the marker is redacted userinfo or an scp
			// remote, as in "REDACTED@host/x,https://...", so it goes too.
			if strings.Contains(core[:remoteStart], "@") {
				remoteStart = 0
			}
			parts[i] = token[:start] + core[:remoteStart] + "REDACTED_REMOTE" + token[end+1:]
		}
		s = strings.Join(parts, " ")
	}
	if len(s) > maxLogStringBytes {
		s = strings.ToValidUTF8(s[:maxLogStringBytes], "") + "\n" + logStringTruncatedLabel
	}
	return s
}

func redactTruncatedSensitivePrefix(s string, sensitive string) string {
	body := s
	suffix := ""
	if suffixStart := strings.LastIndex(s, "\n["); suffixStart >= 0 && strings.HasSuffix(s, "]") {
		body = s[:suffixStart]
		suffix = s[suffixStart:]
	}
	for prefixLen := len(sensitive) - 1; prefixLen > 0; prefixLen-- {
		if strings.HasSuffix(body, sensitive[:prefixLen]) {
			return body[:len(body)-prefixLen] + "REDACTED_REMOTE" + suffix
		}
	}
	return s
}

func shouldRedactRemoteToken(token string) bool {
	return strings.Contains(token, "://") || containsRemoteMarker(token) || schemeLessUserinfoStart(token) >= 0 || isSCPStyleRemote(token)
}

func redactRemoteToken(token string) string {
	start := strings.IndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\"([{<", r)
	})
	if start < 0 {
		return token
	}
	end := strings.LastIndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\")]}>,.:", r)
	})
	if end < start {
		return token
	}
	core := token[start : end+1]
	return token[:start] + redactRemoteCore(core) + token[end+1:]
}

func redactRemoteCore(core string) string {
	var b strings.Builder
	for len(core) > 0 {
		sep := strings.IndexAny(core, ",;")
		if sep < 0 {
			b.WriteString(redactSingleRemoteCore(core))
			break
		}
		next := strings.TrimLeft(core[sep+1:], " ")
		if insideUserinfo := separatorInsideUserinfo(core, sep); insideUserinfo || !hasSplitRemoteMarker(next) {
			whole := redactSingleRemoteCore(core)
			nextSegment := next
			if end := strings.IndexAny(next, ",;"); end >= 0 {
				nextSegment = next[:end]
			}
			// A later segment can hold a remote behind a prefix, as in
			// "remote=https//u:pw@h", which the whole core's redaction misses.
			// Redacting each segment of the result as well removes only more.
			if wholeSep := strings.IndexAny(whole, ",;"); !insideUserinfo && containsRemoteMarker(nextSegment) && wholeSep >= 0 {
				whole = redactSingleRemoteCore(whole[:wholeSep]) + whole[wholeSep:wholeSep+1] + redactRemoteCore(whole[wholeSep+1:])
			}
			b.WriteString(whole)
			break
		}
		b.WriteString(redactSingleRemoteCore(core[:sep]))
		b.WriteByte(core[sep])
		core = core[sep+1:]
	}
	return b.String()
}

func separatorInsideUserinfo(core string, sep int) bool {
	remoteStart := remoteStartIndex(core[:sep])
	if strings.Contains(core[remoteStart:sep], "@") {
		return false
	}
	authorityStart := authorityStartInCore(core[:sep], remoteStart)
	if authorityStart < 0 {
		return false
	}
	candidate := core[authorityStart:sep]
	if !userinfoLikeBeforePath(candidate) {
		return false
	}
	if completeURLBeforeSeparator(core[remoteStart:sep]) {
		return false
	}
	nextBoundary := len(core)
	if relEnd := strings.IndexAny(core[sep+1:], " "); relEnd >= 0 {
		nextBoundary = sep + 1 + relEnd
	}
	if at := strings.Index(core[sep+1:nextBoundary], "@"); at >= 0 && strings.Contains(core[sep+1:sep+1+at], ":") {
		return true
	}
	return strings.Contains(core[sep+1:nextBoundary], "@")
}

func completeURLBeforeSeparator(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return false
	}
	if strings.HasPrefix(u.Host, "[") {
		return true
	}
	host := u.Hostname()
	return isRealHost(host) || isKnownSingleLabelGitHost(host)
}

func authorityStartInCore(core string, remoteStart int) int {
	rest := strings.ToLower(core[remoteStart:])
	for _, marker := range []string{"://", ":/", "//", ":"} {
		if i := strings.Index(rest, marker); i >= 0 {
			return remoteStart + i + len(marker)
		}
	}
	return -1
}

func hasSeparateRemoteMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func hasSplitRemoteMarker(s string) bool {
	return hasSeparateRemoteMarker(s) || hasRemoteMarker(s) || schemeLessUserinfoStart(s) >= 0 || isSCPStyleRemote(s)
}

func hasRemoteMarker(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	lower := strings.ToLower(s)
	for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func containsRemoteMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func redactSingleRemoteCore(core string) string {
	remoteStart := remoteStartIndex(core)
	prefix := core[:remoteStart]
	// Userinfo can precede a marker inside the host, as in "user:pass@corpgit:repo".
	if at := strings.LastIndex(prefix, "@"); at >= 0 && strings.Contains(prefix[:at], ":") {
		prefix = "REDACTED" + prefix[at:]
	}
	// Userinfo can end at a "?" or "#" before the marker, as in "u:pw?git:@".
	if end := strings.IndexAny(prefix, "?#"); end >= 0 && strings.Contains(prefix[:end], ":") && strings.Contains(core[remoteStart:], "@") {
		prefix = "REDACTED" + prefix[end:]
	}
	return prefix + RedactRemoteURL(core[remoteStart:])
}

func remoteStartIndex(s string) int {
	best := -1
	for _, marker := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if i := strings.Index(strings.ToLower(s), marker); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	if at := strings.Index(s, "@"); at >= 0 && strings.ContainsAny(s[at:], "?#") {
		if eq := strings.LastIndex(s[:at], "="); eq >= 0 {
			return eq + 1
		}
	}
	return 0
}
