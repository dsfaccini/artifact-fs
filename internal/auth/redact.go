package auth

import (
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// remoteHelperPrefix matches Git's "<transport>::" remote helper syntax, as in "gcrypt::".
var remoteHelperPrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

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
		// Keep a "?" or "#" inside the userinfo: the query and fragment
		// redaction then still hides the text after it.
		if query := strings.IndexAny(raw[start:end], "?#"); query >= 0 {
			end = start + query
		}
		raw = raw[:start] + "REDACTED" + raw[end:]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactMalformedURL(raw)
	}
	if HasMisparsedCredentials(raw, u) || u.User == nil && strings.Contains(raw, "@") && (isMalformedHTTPUserinfo(raw, u) || schemeLessUserinfoStart(raw) >= 0 || userinfoAfterLaterSeparator(raw, u)) {
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
	if HasMisparsedCredentials(raw, u) || u.User == nil && strings.Contains(raw, "@") && (isMalformedHTTPUserinfo(raw, u) || schemeLessUserinfoStart(raw) >= 0) {
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
		before, address, _ := strings.Cut(raw, "://")
		lower := lowerASCII(before)
		username, _ := addressUserinfo(address)
		return RawUserinfoHasPassword(raw) || username != "" && (strings.HasSuffix(lower, "http") || strings.HasSuffix(lower, "https"))
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
	// Userinfo follows a "://" unless every "@" precedes it, as in "user:pass@host://".
	if i := strings.Index(raw, "://"); i >= 0 && strings.Contains(raw[i:], "@") {
		return i + len("://")
	}
	if schemeLessUserinfoStart(raw) >= 0 {
		return 0
	}
	trimmed := lowerASCII(strings.TrimLeftFunc(raw, unicode.IsSpace))
	leading := len(raw) - len(trimmed)
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return leading + len(prefix)
		}
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		return i + len("://")
	}
	return -1
}

// lowerASCII lowercases the ASCII letters in s and keeps every other byte, so
// an index into the result is an index into s. The remote markers are ASCII.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// userinfoBeforeSeparator returns the bounds of "user:pass" text before the
// first "://" when an "@" follows that "://", as in "alice:pw@host/a://b@c".
// The heuristics take the userinfo to follow the "://", but either side can
// hold it. Before the "://", userinfo can end at any "@" or at the first "?" or
// "#", as in "u:pw?x://y@z". It starts after the last "/", a remote helper's
// "<transport>::" or a typo scheme. The bounds cover every candidate that holds
// a ":".
func userinfoBeforeSeparator(raw string) (start int, end int, ok bool) {
	separator := strings.Index(raw, "://")
	if separator < 0 || !strings.Contains(raw[separator:], "@") {
		return 0, 0, false
	}
	head := raw[:separator]
	lower := lowerASCII(raw)
	candidateEnds := []int{strings.IndexAny(head, "?#")}
	for i := range len(head) {
		if head[i] == '@' {
			candidateEnds = append(candidateEnds, i)
		}
	}
	for _, candidateEnd := range candidateEnds {
		if candidateEnd < 0 {
			continue
		}
		candidateStart := strings.LastIndex(raw[:candidateEnd], "/") + 1
		for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https:", "http:", "ssh:", "git:"} {
			if strings.HasPrefix(lower, marker) && len(marker) > candidateStart && len(marker) <= candidateEnd {
				candidateStart = len(marker)
			}
		}
		if helper := remoteHelperPrefix.FindStringIndex(raw); candidateStart == 0 && helper != nil && helper[1] <= candidateEnd {
			candidateStart = helper[1]
		}
		if !strings.Contains(raw[candidateStart:candidateEnd], ":") {
			continue
		}
		if !ok || candidateStart < start {
			start = candidateStart
		}
		if !ok || candidateEnd > end {
			end = candidateEnd
		}
		ok = true
	}
	return start, end, ok
}

// userinfoAfterLaterSeparator reports userinfo after a "://" that does not end
// the parsed scheme, as in "ssh:/x://u:pw@h" or "gcrypt::https://u:pw@h". The
// parser keeps that userinfo in the path or opaque part.
func userinfoAfterLaterSeparator(raw string, u *url.URL) bool {
	separator := strings.Index(raw, "://")
	if u.Scheme == "" || separator < 0 || separator == len(u.Scheme) {
		return false
	}
	// A real host after this "://" can precede a further "://" and its userinfo,
	// as in "gcrypt::https://host:8443/a://u:pw@h".
	for address := raw[separator+len("://"):]; ; {
		if hasAddressUserinfo(address) {
			return true
		}
		next := strings.Index(address, "://")
		if next < 0 {
			return false
		}
		address = address[next+len("://"):]
	}
}

// addressUserinfo returns the userinfo of the address after a "://": the
// text before the last "@" of the authority, which ends at the first "/".
// Without such an "@", it is the text before the last "@" in the address,
// because a password can hold a "/", as in "user:pa/ss@host". A real host
// before the "/", as in "host.example.com:22/org/repo@v1.git", rules that out:
// the "@" is then in the path.
func addressUserinfo(address string) (userinfo string, ok bool) {
	authority, _, _ := strings.Cut(address, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		return authority[:at], true
	}
	if isRealHostPort(authority) {
		return "", false
	}
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return "", false
	}
	return address[:at], true
}

func hasAddressUserinfo(address string) bool {
	_, ok := addressUserinfo(address)
	return ok
}

// isRealHostPort reports whether authority is a real host with an optional
// numeric port, as validParsedHostPort judges a parsed one.
func isRealHostPort(authority string) bool {
	host, port := authority, ""
	if rest, ok := strings.CutPrefix(authority, "["); ok {
		bracketed, after, closed := strings.Cut(rest, "]")
		if !closed || after != "" && !strings.HasPrefix(after, ":") {
			return false
		}
		host, port = bracketed, strings.TrimPrefix(after, ":")
		// An IPv6 zone is percent-escaped, as in "[fe80::1%25en0]".
		if literal, err := url.PathUnescape(host); err == nil && isIPAddress(literal) {
			return strings.Trim(port, "0123456789") == ""
		}
	} else if before, after, ok := strings.Cut(authority, ":"); ok {
		host, port = before, after
	}
	if strings.ContainsAny(host, "?#%[]") || strings.Trim(port, "0123456789") != "" {
		return false
	}
	return !isUserLikeHost(host) && (isRealHost(host) || isKnownSingleLabelGitHost(host))
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
	return strings.Contains(host, ".") || host == "localhost" || isIPAddress(host)
}

// isIPAddress reports an IP address, including an IPv6 one with a zone, as in
// "fe80::1%en0", which net.ParseIP rejects.
func isIPAddress(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
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

// HasMisparsedCredentials reports whether raw, which url.Parse read as u,
// holds credentials outside u's userinfo. Without userinfo, that is a password
// in text url.Parse took for the host or path, as in
// "ssh://user:1234/secret@host/repo". With userinfo, it is credentials in the
// URL left once the userinfo is removed, as when a password holds an
// unescaped "@": "https://alice:p@ss/w0rd@github.com/org/repo.git". Every
// check that decides whether a parsed remote holds credentials uses it, so
// they agree.
func HasMisparsedCredentials(raw string, u *url.URL) bool {
	if u.User != nil {
		return HasInlineCredentials(withoutUserinfo(raw))
	}
	return strings.Contains(raw, "@") && RawUserinfoHasPassword(raw)
}

// withoutUserinfo returns raw, which url.Parse read with userinfo, without
// that userinfo, query and fragment. It keeps raw's own text, because
// url.URL.String percent-escapes a non-ASCII host, which then no longer reads
// as a real host.
func withoutUserinfo(raw string) string {
	if end := strings.IndexAny(raw, "?#"); end >= 0 {
		raw = raw[:end]
	}
	// The authority follows the first "//" and ends at the next "/".
	authorityStart := strings.Index(raw, "//") + len("//")
	authority, _, _ := strings.Cut(raw[authorityStart:], "/")
	return raw[:authorityStart] + raw[authorityStart+strings.LastIndex(authority, "@")+1:]
}

// RawUserinfoHasPassword reports whether a remote that url.Parse misreads or
// rejects, such as "https:user:pw@host", holds "user:pass@" userinfo. It checks
// two candidate authorities and reports a password when either holds one.
func RawUserinfoHasPassword(raw string) bool {
	return authorityUserinfoHasPassword(raw) || delimitedUserinfoHasPassword(raw)
}

// authorityUserinfoHasPassword checks the userinfo of the address that starts
// where malformedAuthorityStart finds the authority, before any "?" or "#".
func authorityUserinfoHasPassword(raw string) bool {
	authorityStart := malformedAuthorityStart(raw)
	if authorityStart < 0 {
		return false
	}
	address := raw[authorityStart:]
	if end := strings.IndexAny(address, "?#"); end >= 0 {
		address = address[:end]
	}
	userinfo, ok := addressUserinfo(address)
	return ok && strings.Contains(userinfo, ":")
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
			start, end, ok := remoteTokenCore(token)
			if !ok {
				continue
			}
			core := token[start:end]
			remoteStart := remoteStartIndex(core)
			// Text with an "@" before the marker is redacted userinfo or an scp
			// remote, as in "REDACTED@host/x,https://...", so it goes too.
			// RedactString can have removed that "@", so check the token as it
			// came in as well.
			if strings.Contains(core[:remoteStart], "@") || i < len(unredacted) && atBeforeRemote(unredacted[i]) {
				remoteStart = 0
			}
			parts[i] = token[:start] + core[:remoteStart] + "REDACTED_REMOTE" + token[end:]
		}
		s = strings.Join(parts, " ")
	}
	if len(s) > maxLogStringBytes {
		s = strings.ToValidUTF8(s[:maxLogStringBytes], "") + "\n" + logStringTruncatedLabel
	}
	return s
}

// atBeforeRemote reports an "@" in a token's core before its remote starts.
func atBeforeRemote(token string) bool {
	start, end, ok := remoteTokenCore(token)
	if !ok {
		return false
	}
	core := token[start:end]
	return strings.Contains(core[:remoteStartIndex(core)], "@")
}

// remoteTokenCore returns the bounds of token without the quotes, brackets and
// punctuation around a remote. The end is exclusive and on a rune boundary.
func remoteTokenCore(token string) (start int, end int, ok bool) {
	start = strings.IndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\"([{<", r)
	})
	last := strings.LastIndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\")]}>,.:", r)
	})
	if start < 0 || last < start {
		return 0, 0, false
	}
	_, size := utf8.DecodeRuneInString(token[last:])
	return start, last + size, true
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
	start, end, ok := remoteTokenCore(token)
	if !ok {
		return token
	}
	return token[:start] + redactRemoteCore(token[start:end]) + token[end:]
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
			// A later segment can hold a remote behind a prefix, as in
			// "remote=https//u:pw@h", which the whole core's redaction misses.
			if !insideUserinfo && containsRemoteMarker(core[sep+1:]) {
				whole = redactTailBySegment(whole, core, sep)
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

// redactTailBySegment redacts core's text after the separator at sep segment
// by segment too, where whole, the whole core's redaction, kept that text: as
// it was or percent-escaped, or apart from a query or fragment that whole
// replaced. The result shows no more than whole.
func redactTailBySegment(whole string, core string, sep int) string {
	tail := core[sep+1:]
	for _, kept := range keptForms(core[sep:]) {
		if strings.HasSuffix(whole, kept) {
			return whole[:len(whole)-len(kept)] + core[sep:sep+1] + redactRemoteCore(tail)
		}
	}
	query := strings.IndexAny(tail, "?#")
	if query < 0 || strings.ContainsAny(tail[query:], ",;") {
		return whole
	}
	for _, kept := range keptQueryForms(core[sep : sep+1+query]) {
		if !strings.HasSuffix(whole, kept) {
			continue
		}
		// Keep whole's query redaction. The segment's redaction can start a
		// remote inside the query and keep the text before it.
		bySegment := redactRemoteCore(tail)
		if cut := strings.IndexAny(bySegment, "?#"); cut >= 0 {
			bySegment = bySegment[:cut]
		}
		if !shownWithin(bySegment, tail[:query]) {
			return whole
		}
		return whole[:len(whole)-len(kept)] + core[sep:sep+1] + bySegment + kept[strings.IndexAny(kept, "?#"):]
	}
	return whole
}

// keptForms returns s and s as url.URL.String writes it back after parsing it
// in a path, which is where RedactRemoteURL leaves text it keeps.
func keptForms(s string) []string {
	forms := []string{s}
	if path, err := url.PathUnescape(s); err == nil {
		forms = append(forms, (&url.URL{Path: path}).EscapedPath())
	}
	return forms
}

// keptQueryForms returns each form of s followed by a query or fragment that
// RedactRemoteURL replaced.
func keptQueryForms(s string) []string {
	var forms []string
	for _, kept := range keptForms(s) {
		for _, redacted := range []string{"?REDACTED", "?REDACTED#REDACTED", "#REDACTED"} {
			forms = append(forms, kept+redacted)
		}
	}
	return forms
}

// shownWithin reports whether each run of redacted text between "REDACTED"
// markers occurs in original, in order.
func shownWithin(redacted string, original string) bool {
	for run := range strings.SplitSeq(redacted, "REDACTED") {
		i := strings.Index(original, run)
		if i < 0 {
			return false
		}
		original = original[i+len(run):]
	}
	return true
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
	rest := core[remoteStart:]
	for _, marker := range []string{"://", ":/", "//", ":"} {
		if i := strings.Index(rest, marker); i >= 0 {
			return remoteStart + i + len(marker)
		}
	}
	return -1
}

// markerPrefix lowercases only as much of s as the longest remote marker, so a
// check per comma-list segment stays linear in the segment.
func markerPrefix(s string) string {
	return lowerASCII(s[:min(len(s), len("https://"))])
}

func hasSeparateRemoteMarker(s string) bool {
	lower := markerPrefix(s)
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
	lower := markerPrefix(s)
	for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func containsRemoteMarker(s string) bool {
	lower := lowerASCII(s)
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
	lower := lowerASCII(s)
	for _, marker := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if i := strings.Index(lower, marker); i >= 0 && (best < 0 || i < best) {
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
