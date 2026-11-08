package auth

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestRedactRemoteURL(t *testing.T) {
	redactors := map[string]func(string) string{"RedactRemoteURL": RedactRemoteURL, "RedactString": RedactString}
	for _, tt := range []struct {
		name, redactor, in, want string
	}{
		{"userinfo and query", "RedactRemoteURL", "https://token123@github.com/org/repo.git?token=abc", "https://REDACTED@github.com/org/repo.git?REDACTED"},
		{"query secret", "RedactRemoteURL", "https://github.com/org/repo.git?sig=s3cr3t", "https://github.com/org/repo.git?REDACTED"},
		{"fragment secret", "RedactRemoteURL", "https://github.com/org/repo.git#access_token=ghp_secret", "https://github.com/org/repo.git#REDACTED"},
		{"malformed userinfo", "RedactRemoteURL", "https://ghp_secret%zz@github.com/org/repo.git", "https://REDACTED@github.com/org/repo.git"},
		{"malformed userinfo with fragment delimiter", "RedactRemoteURL", "https://user:pa#ss@example.com/org/repo.git", "https://REDACTED#REDACTED"},
		{"malformed username-only userinfo", "RedactRemoteURL", "https://ghp_secret#@github.com/org/repo.git", "https://REDACTED#REDACTED"},
		{"malformed userinfo with path delimiter", "RedactRemoteURL", "https://user:pa/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"malformed userinfo with token-like password", "RedactRemoteURL", "https://ghp_secret%zz:password=abc@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"scp-style query secret", "RedactRemoteURL", "git@github.com:org/repo.git?sig=s3cr3t", "git@github.com:org/repo.git?REDACTED"},
		{"malformed https userinfo", "RedactRemoteURL", "https://ghp_secret/@github.com/org/repo.git", "https://REDACTED@github.com/org/repo.git"},
		{"missing-slash https userinfo", "RedactRemoteURL", "https:/user:ghp_secret@github.com/org/repo.git", "REDACTED@github.com/org/repo.git"},
		{"numeric-prefix https userinfo", "RedactRemoteURL", "https://user:123/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"multi-segment https userinfo", "RedactRemoteURL", "https://user:123/dir/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"dotted malformed https userinfo", "RedactRemoteURL", "https://user.name:pa/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"dotted numeric malformed https userinfo", "RedactRemoteURL", "https://user.name:123/dir/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"dotted name numeric malformed https userinfo", "RedactRemoteURL", "https://first.last:123/dir/ss@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"dotted name token malformed https userinfo", "RedactRemoteURL", "https://john.doe:123/ghp_secret@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"path-split username-only malformed https userinfo", "RedactRemoteURL", "https://ghp_secret/part@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"query-split username-only malformed https userinfo", "RedactRemoteURL", "https://ghp_secret/part?x@example.com/org/repo.git", "https://REDACTED?REDACTED"},
		{"single-label path username-only malformed https userinfo", "RedactRemoteURL", "https://ghp_secret/part@example.com", "https://REDACTED@example.com"},
		{"ported single-label path username-only malformed https userinfo", "RedactRemoteURL", "https://ghp_secret:8443/part@example.com", "https://REDACTED@example.com"},
		{"real-host path with at sign and continuation", "RedactRemoteURL", "https://git.example.com/team/repo@2026/archive.git", "https://git.example.com/team/repo@2026/archive.git"},
		{"file URL path with at sign", "RedactRemoteURL", "file:///tmp/repo@2026.git", "file:///tmp/repo@2026.git"},
		{"dotted path-split username-only malformed https userinfo", "RedactRemoteURL", "https://ghp.secret/part@example.com/org/repo.git", "https://REDACTED@example.com/org/repo.git"},
		{"missing-colon https userinfo", "RedactRemoteURL", "https//ghp_secret@github.com/org/repo.git", "https//REDACTED@github.com/org/repo.git"},
		{"malformed userinfo in a line", "RedactString", "fatal: https://ghp_secret%zz:password=abc@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"quoted URL", "RedactString", "fatal: unable to access 'https://user:pass@example.com/org/repo.git/': failed", "fatal: unable to access 'https://REDACTED@example.com/org/repo.git/': failed"},
		{"scp-style query", "RedactString", "fatal: git@github.com:org/repo.git?sig=s3cr3t failed", "fatal: git@github.com:org/repo.git?REDACTED failed"},
		{"malformed ssh userinfo", "RedactRemoteURL", "ssh://git:sec%zzret@example.com/org/repo.git", "ssh://REDACTED@example.com/org/repo.git"},
		{"malformed ssh-style userinfo", "RedactString", "fatal: ssh:/git:pa/ss@github.com/org/repo.git failed", "fatal: REDACTED@github.com/org/repo.git failed"},
		{"prefixed malformed ssh-style userinfo", "RedactString", "fatal: remote=ssh:/git:pa/ss@github.com/org/repo.git failed", "fatal: remote=REDACTED@github.com/org/repo.git failed"},
		{"http-like typo", "RedactString", "fatal: https:/user:ghp_secret@github.com/org/repo.git failed", "fatal: REDACTED@github.com/org/repo.git failed"},
		{"prefixed http-like typo", "RedactString", "fatal: remote=https:/user:ghp_secret@github.com/org/repo.git failed", "fatal: remote=REDACTED@github.com/org/repo.git failed"},
		{"prefixed missing-colon typo", "RedactString", "fatal: remote=https//ghp_secret@github.com/org/repo.git failed", "fatal: remote=https//REDACTED@github.com/org/repo.git failed"},
		{"second URL credential", "RedactString", "remotes=ssh://git@github.com/org/repo.git,https://ghp_secret@example.com/private.git", "remotes=ssh://REDACTED@github.com/org/repo.git,https://REDACTED@example.com/private.git"},
		{"second URL credential after ported URL", "RedactString", "remotes=https://git.example.com:8443/team/repo.git,https://ghp_secret@example.com/private.git", "remotes=https://git.example.com:8443/team/repo.git,https://REDACTED@example.com/private.git"},
		{"second URL credential after single-label ported URL", "RedactString", "remotes=https://git:8443/team/repo.git,https://ghp_secret@example.com/private.git", "remotes=https://git:8443/team/repo.git,https://REDACTED@example.com/private.git"},
		{"scheme-less second URL credential", "RedactString", "remotes=https://git.example.com/repo.git,alice:ghp_secret@github.com:org/repo.git", "remotes=https://git.example.com/repo.git,REDACTED@github.com:org/repo.git"},
		{"scheme-less second URL credential after semicolon", "RedactString", "remotes=https://git.example.com/repo.git;alice:ghp_secret@github.com:org/repo.git", "remotes=https://git.example.com/repo.git;REDACTED@github.com:org/repo.git"},
		{"query value", "RedactString", "fatal: https://github.com/org/repo.git?sig=abc,def failed", "fatal: https://github.com/org/repo.git?REDACTED failed"},
		{"comma-containing userinfo", "RedactString", "fatal: https://user:pa,ss@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"semicolon-containing userinfo", "RedactString", "fatal: https://user:pa;http:ss@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"comma/query malformed userinfo", "RedactString", "fatal: https://user:pa,http:ss?x@example.com/org/repo.git failed", "fatal: https://REDACTED?REDACTED failed"},
		{"comma/url-marker malformed userinfo", "RedactString", "fatal: https://user:pa,https://ss@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"slash comma/url-marker malformed userinfo", "RedactString", "fatal: https://user:pa/ss,https://x@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"slash semicolon/url-marker malformed userinfo", "RedactString", "fatal: https://user:pa/ss;https://x@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"comma-split username", "RedactString", "fatal: https://use,r:pass@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"semicolon-split username", "RedactString", "fatal: https://use;r:pass@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"semicolon/fragment malformed userinfo", "RedactString", "fatal: https://user:pa;http:ss#x@example.com/org/repo.git failed", "fatal: https://REDACTED#REDACTED failed"},
		{"slash query malformed userinfo", "RedactRemoteURL", "https://user:pa/ss?x@example.com/org/repo.git", "https://REDACTED?REDACTED"},
		{"slash fragment malformed userinfo", "RedactRemoteURL", "https://user:pa/ss#x@example.com/org/repo.git", "https://REDACTED#REDACTED"},
		{"numeric slash comma malformed userinfo", "RedactString", "fatal: https://user:8443/ss,https://x@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"numeric slash semicolon malformed userinfo", "RedactString", "fatal: https://user:8443/ss;https://x@example.com/org/repo.git failed", "fatal: https://REDACTED@example.com/org/repo.git failed"},
		{"ordinary punctuation", "RedactString", "fatal: what? failed issue#123", "fatal: what? failed issue#123"},
		{"ordinary email punctuation", "RedactString", "contact dev@example.com? issue#123", "contact dev@example.com? issue#123"},
		{"missing-colon comma/query malformed userinfo", "RedactString", "fatal: https//ghp_secret,http:ss?x@example.com/org/repo.git failed", "fatal: https//REDACTED?REDACTED failed"},
		{"missing-colon semicolon/fragment malformed userinfo", "RedactString", "fatal: https//ghp_secret;http:ss#x@example.com/org/repo.git failed", "fatal: https//REDACTED#REDACTED failed"},
		{"malformed query with at sign", "RedactRemoteURL", "https://github.com/%zz?access_token=abc@def", "https://github.com/%zz?REDACTED"},
		{"scheme-less malformed userinfo", "RedactRemoteURL", "alice:ghp_secret@github.com:org/repo.git", "REDACTED@github.com:org/repo.git"},
		{"scheme-less token userinfo", "RedactRemoteURL", "x-token-auth:secret@bitbucket.org/org/repo.git", "REDACTED@bitbucket.org/org/repo.git"},
		{"scheme-less delimiter-split userinfo", "RedactRemoteURL", "alice:ghp_secret#@github.com:org/repo.git", "REDACTED#REDACTED"},
		{"typo-scheme userinfo after an at-sign prefix", "RedactString", "fatal: unable to access 'jane@example.com:https:user:ghp_secret@example.com/org/repo.git': HTTP 401", "fatal: unable to access 'jane@example.com:REDACTED@example.com/org/repo.git': HTTP 401"},
		{"userinfo ending at a query after a later scheme separator", "RedactString", "fatal: unable to access 'https:/example.com/a://user:ghp_secret?q@z': HTTP 401", "fatal: unable to access 'https:/example.com/a://REDACTED?REDACTED': HTTP 401"},
		{"remote helper URL without userinfo", "RedactRemoteURL", "gcrypt::https://github.com/org/repo@v1.git", "gcrypt::https://github.com/org/repo@v1.git"},
		{"userinfo after a further separator behind a remote helper", "RedactRemoteURL", "gcrypt::https://host.example.com:8443/a://u:pwzq@h", "gcrypt::https://REDACTED@h"},
	} {
		if got := redactors[tt.redactor](tt.in); got != tt.want {
			t.Errorf("%s: %s(%q) = %q, want %q", tt.name, tt.redactor, tt.in, got, tt.want)
		}
	}
}

func TestHasInlineCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "https userinfo",
			raw:  "https://token@example.com/org/repo.git",
			want: true,
		},
		{
			name: "token query parameter",
			raw:  "https://github.com/org/repo.git?access_token=secret",
			want: true,
		},
		{
			name: "token-like query parameter",
			raw:  "https://github.com/org/repo.git?x-token-auth=secret",
			want: true,
		},
		{
			name: "unrecognized query secret",
			raw:  "https://github.com/org/repo.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "benign-looking query",
			raw:  "https://github.com/org/repo.git?ref=main",
			want: true,
		},
		{
			name: "fragment secret",
			raw:  "https://github.com/org/repo.git#access_token=secret",
			want: true,
		},
		{
			name: "empty fragment marker",
			raw:  "https://github.com/org/repo.git#",
			want: true,
		},
		{
			name: "malformed https userinfo path split",
			raw:  "https://ghp_secret/@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https username path continuation",
			raw:  "https://ghp_secret/part@example.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https username query continuation",
			raw:  "https://ghp_secret/part?x@example.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https username single-label path",
			raw:  "https://ghp_secret/part@example.com",
			want: true,
		},
		{
			name: "malformed https username ported single-label path",
			raw:  "https://ghp_secret:8443/part@example.com",
			want: true,
		},
		{
			name: "malformed https userinfo missing slash",
			raw:  "https:/user:ghp_secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https userinfo numeric prefix",
			raw:  "https://user:123/ss@example.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https userinfo missing colon",
			raw:  "https//ghp_secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https parse error missing colon",
			raw:  "https//ghp_secret%zz@github.com/org/repo.git",
			want: true,
		},
		{
			name: "empty query marker",
			raw:  "https://github.com/org/repo.git?",
			want: true,
		},
		{
			name: "malformed userinfo",
			raw:  "https://user%zz@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed query secret",
			raw:  "https://github.com/org/%zz.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "scp-style ssh",
			raw:  "git@github.com:org/repo.git",
			want: false,
		},
		{
			name: "scheme-less malformed userinfo",
			raw:  "alice:ghp_secret@github.com:org/repo.git",
			want: true,
		},
		{
			name: "scheme-less bitbucket token userinfo",
			raw:  "x-token-auth:secret@bitbucket.org/org/repo.git",
			want: true,
		},
		{
			name: "scp-style ssh with query",
			raw:  "git@github.com:org/repo.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "scp-style ssh with fragment",
			raw:  "git@github.com:org/repo.git#access_token=secret",
			want: true,
		},
		{
			name: "scp-style root path with at sign",
			raw:  "git@example.com:repo:v1@2026.git",
			want: false,
		},
		{
			name: "ssh username only",
			raw:  "ssh://git@github.com/org/repo.git",
			want: false,
		},
		{
			name: "ssh token username",
			raw:  "ssh://ghp_abcdefghijklmnopqrstuvwxyz@github.com/org/repo.git",
			want: true,
		},
		{
			name: "ssh username containing token",
			raw:  "ssh://token-admin@git.example.com/org/repo.git",
			want: false,
		},
		{
			name: "git protocol username only",
			raw:  "git://ghp_secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "ssh username with query",
			raw:  "ssh://git@github.com/org/repo.git?ref=main",
			want: true,
		},
		{
			name: "ssh password",
			raw:  "ssh://git:secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "plain https remote",
			raw:  "https://github.com/org/repo.git",
			want: false,
		},
		{
			name: "file URL path with at sign",
			raw:  "file:///tmp/repo@2026.git",
			want: false,
		},
		{
			name: "https path with at sign",
			raw:  "https://git.example.com/team/repo@2026.git",
			want: false,
		},
		{
			name: "https ported path with at sign",
			raw:  "https://git.example.com:8443/team/repo@2026.git",
			want: false,
		},
		{
			name: "https path with colon and at sign",
			raw:  "https://git.example.com/team/repo:v1@2026.git",
			want: false,
		},
		{
			name: "user subdomain path with colon and at sign",
			raw:  "https://user.example.com/team/repo:v1@2026.git",
			want: false,
		},
		{
			name: "root https path with colon and at sign",
			raw:  "https://git.example.com/repo:v1@2026.git",
			want: false,
		},
		{
			name: "single label https path with colon and at sign",
			raw:  "https://git/repo:v1@2026.git",
			want: false,
		},
		{
			name: "single label ported https path with at sign",
			raw:  "https://git:8443/team/repo@2026.git",
			want: false,
		},
		{
			name: "single label ported root https path with at sign",
			raw:  "https://git:8443/repo@2026.git",
			want: false,
		},
		{
			name: "ghe ported https path with at sign",
			raw:  "https://ghe:8443/team/repo@2026.git",
			want: false,
		},
		{
			name: "real host path with at sign continuation",
			raw:  "https://git.example.com/team/repo@2026/archive.git",
			want: false,
		},
		{
			name: "real host ported path with at sign continuation",
			raw:  "https://git.example.com:8443/team/repo@2026/archive.git",
			want: false,
		},
		{
			name: "localhost path with colon and at sign",
			raw:  "https://localhost/team/repo:v1@2026.git",
			want: false,
		},
		{
			name: "remote helper with ambient profile",
			raw:  "codecommit::us-east-1://demo-profile@MyRepositoryName",
			want: false,
		},
		{
			name: "remote helper over ssh username",
			raw:  "gcrypt::ssh://git@github.com/org/repo.git",
			want: false,
		},
		{
			name: "userinfo before a trailing scheme separator",
			raw:  "A:0@://",
			want: true,
		},
		{
			name: "username before a later scheme separator",
			raw:  "ssh:/git@host/x://repo",
			want: true,
		},
		{
			name: "userinfo before a later separator that an at sign follows",
			raw:  "alice:s3cret@host/a://b@c",
			want: true,
		},
		{
			name: "remote helper over https with password",
			raw:  "gcrypt::https://user:token@example.com/repo",
			want: true,
		},
		{
			name: "remote helper over https with token username",
			raw:  "gcrypt::https://ghp_token@github.com/org/repo.git",
			want: true,
		},
		{
			name: "scheme then helper separator",
			raw:  "B:://u:pw@h",
			want: true,
		},
		{
			name: "userinfo after a separator in an ssh path",
			raw:  "ssh:/x://u:pw@h",
			want: true,
		},
		{
			name: "remote helper over https with an at sign in the path",
			raw:  "gcrypt::https://github.com/org/repo@v1.git",
			want: false,
		},
		{
			name: "remote helper over ssh with a port and an at sign in the path",
			raw:  "gcrypt::ssh://git@host.example.com:22/org/repo@v1.git",
			want: false,
		},
		{
			name: "remote helper over https with an empty username",
			raw:  "gcrypt::https://@github.com/org/repo.git",
			want: false,
		},
		{
			name: "remote helper prefix before a username",
			raw:  "codecommit::alice@host/a://b@c",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasInlineCredentials(tt.raw); got != tt.want {
				t.Fatalf("HasInlineCredentials(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRedactionHidesLeftoverPasswords(t *testing.T) {
	for _, tt := range []struct {
		raw, password string
	}{
		{"alice:s3cret@host/a://b@c", "s3cret"},
		{"gcrypt::https://user:token@example.com/repo", "token"},
		{"B:://u:pwzq@h", "pwzq"},
		{"ssh:/x://u:pwzq@h", "pwzq"},
		{"0:1?git:@", "0:1"},
		{"remotes=https://example.com/a.git,remote=https//user:s3cret@host/x", "s3cret"},
		{"remotes=https://example.com/a.git,remote=1:pa?ssqgit:/repo", "ssqgit"},
		{"remotes=https://example.com/a.git,ghp_tokq:pa?ssqgit:22:org/repo.git,", "ssqgit"},
		{"gcrypt::ssh://a.b:s3cretq[::1]:8443:/a://u:pw@h", "s3cretq"},
		{"ssh:alice:pwzq@host.example.com:22:/a://u:pw#f@z", "pw#"},
		{"http::pwzq@host.example.com:22/a://u:pw@h", "pwzq"},
		// "Ⱥ" is 2 bytes and lowercases to 3.
		{"x:ȺȺȺȺȺȺȺȺ://alice:s3cret@host/repo", "alice:s3"},
	} {
		for name, redact := range map[string]func(string) string{"RedactRemoteURL": RedactRemoteURL, "RedactString": RedactString, "RedactLogString": func(s string) string { return RedactLogString(s) }} {
			if got := redact(tt.raw); strings.Contains(got, tt.password) {
				t.Errorf("%s(%q) = %q leaks %q", name, tt.raw, got, tt.password)
			}
		}
	}
}

func TestRedactionHandlesSeparatorBeforeRemote(t *testing.T) {
	for _, raw := range []string{"a,https://github.com/org/repo.git", "a;git@host:repo", "x;https://u@h"} {
		if got := RedactString(raw); strings.Contains(got, "u@h") {
			t.Errorf("RedactString(%q) = %q keeps the username", raw, got)
		}
		if got := RedactLogString(raw); !strings.Contains(got, "REDACTED_REMOTE") {
			t.Errorf("RedactLogString(%q) = %q keeps the remote", raw, got)
		}
	}
}

func TestRedactionKeepsTrailingRunesWhole(t *testing.T) {
	// "Ѡ" is 2 bytes, so the remote must end after both.
	const line = "clone http://u:pw@hostѠ failed"
	for name, redact := range map[string]func(string) string{"RedactString": RedactString, "RedactLogString": func(s string) string { return RedactLogString(s) }} {
		if got := redact(line); !utf8.ValidString(got) {
			t.Errorf("%s(%q) = %q splits a rune", name, line, got)
		}
	}
}

func TestRedactionHandlesCaseFoldingTextBeforeRemote(t *testing.T) {
	// "Ⱥ" lowercases from 2 bytes to 3, so an index into a lowercased copy
	// points past the input: the first line's in remoteStartIndex, the
	// second's in authorityStartInCore.
	for _, line := range []string{"denied " + strings.Repeat("Ⱥ", 15) + "https://u:pw@h here", "denied git:ȺȺȺȺ://x,y here"} {
		if got := RedactString(line); strings.Contains(got, ":pw@") {
			t.Errorf("RedactString(%q) = %q keeps the password", line, got)
		}
		if got := RedactLogString(line); !strings.Contains(got, "REDACTED_REMOTE") {
			t.Errorf("RedactLogString(%q) = %q keeps the remote", line, got)
		}
	}
}

func TestRedactLogStringDropsLocationBeforeRedactedUserinfo(t *testing.T) {
	// RedactString replaces the userinfo before "@", so only the token as it
	// came in shows that the location before it belongs to the remote.
	const line = "fetch private.example.com/team/repo?u:pw@,ssh://private.example.com/team/repo failed"
	if got := RedactLogString(line); strings.Contains(got, "private.example.com") {
		t.Errorf("RedactLogString(%q) = %q keeps the location", line, got)
	}
}

func TestRawUserinfoHasPassword(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want bool
	}{
		// The authority check finds these. A password can hold a "/" when no
		// real host precedes it.
		{"https:user:pw@host", true},
		{"ssh://user:1234/secret@host/repo", true},
		{"git://alice:22/s3cret@host", true},
		// The delimited check finds userinfo behind a path or a "?".
		{"x/alice:pw@[::1]/a@b", true},
		{"a//1:pw@examplehost.com:22:repo", true},
		{"#::@", true},
		{"git@github.com:org/repo.git", false},
		{"https://example.com/org/repo.git", false},
		{"gcrypt::ssh://git@github.com/org/repo.git", false},
		// An "@" after a real host and port, or after the authority's own "@",
		// is in the path.
		{"ssh://host.example.com:22/org/repo@v1.git", false},
		{"gcrypt::ssh://git@host.example.com:22/org/repo@v1.git", false},
		{"https://user@example.com/a:b@c", false},
	} {
		if got := RawUserinfoHasPassword(tt.raw); got != tt.want {
			t.Errorf("RawUserinfoHasPassword(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestRedactLogStringRemovesRemoteReferences(t *testing.T) {
	for _, input := range []string{
		"fatal: repository 'https://github.example.com/private/repo.git' not found",
		"clone https://user:secret@example.com/org/repo.git failed",
		"fetch git@example.com:private/repo.git failed",
		"fatal: path/x://user:ghp_secret@example.com failed",
		"fatal: repository 'codecommit::us-east-1://demo-profile@MyRepositoryName' not found",
		"fatal: repository 'gcrypt::rsync://user@example.com/repo' not found",
		"fetch remotes=alice:secret@example.com/org/repo.git,https://github.example.com/x.git failed",
		"fatal: unable to access 'https:/user:pa?ss@host.example.com/a://b;git@mirror.example.com:org/repo.git': HTTP 401",
		"clone x:y@git:a@b,remote=https//user:secret@mirror.example.com failed",
	} {
		got := RedactLogString(input)
		if !strings.Contains(got, "REDACTED_REMOTE") {
			t.Fatalf("RedactLogString(%q) = %q, want remote marker", input, got)
		}
		for _, secret := range []string{"github.example.com", "private/repo.git", "user", "secret", "example.com", "org/repo.git", "MyRepositoryName"} {
			if strings.Contains(got, secret) {
				t.Fatalf("RedactLogString(%q) leaked %q in %q", input, secret, got)
			}
		}
	}
	for _, remote := range []string{"/private/customer/repo.git", "github.example.com:private/repo.git"} {
		got := RedactLogString("fatal: repository "+remote+" not found", remote)
		if strings.Contains(got, remote) || !strings.Contains(got, "REDACTED_REMOTE") {
			t.Fatalf("RedactLogString leaked exact remote %q in %q", remote, got)
		}
		partial := remote[:len(remote)-4]
		got = RedactLogString("fatal: repository "+partial+"\n[git stderr truncated]", remote)
		if strings.Contains(got, partial) || !strings.Contains(got, "REDACTED_REMOTE") {
			t.Fatalf("RedactLogString leaked partial remote %q in %q", partial, got)
		}
	}
}

func TestRedactLogStringBoundsDiagnostics(t *testing.T) {
	got := RedactLogString(strings.Repeat("x", maxLogStringBytes*2))
	if len(got) > maxLogStringBytes+len(logStringTruncatedLabel)+1 || !strings.Contains(got, logStringTruncatedLabel) {
		t.Fatalf("RedactLogString returned %d bytes without truncation marker", len(got))
	}
}

// remoteForms are the remote prefixes the redaction heuristics recognize,
// including the typos and the scheme-less form.
var remoteForms = []string{"https://", "http://", "ssh://", "git://", "https:/", "https//", "https:", "ssh:/", ""}

// tokenPrefixes are text that a log token can carry in front of a remote.
var tokenPrefixes = []string{"", "jane@example.com:", "HEAD@{1}:", "remote="}

// userinfoEnds are the characters that can end the userinfo of a remote.
var userinfoEnds = []string{"@", "?", "#"}

func FuzzRedactionHidesPassword(f *testing.F) {
	for _, seed := range []struct {
		prefix, form, end    int
		lead, user, password string
		rest                 string
	}{
		{0, 0, 0, "", "user", "pass", "example.com/org/repo.git/"},
		{0, 0, 0, "", "ghp_secret%zz", "password", "example.com/org/repo.git"},
		{0, 0, 0, "", "user.name", "secret", "example.com/org/repo.git"},
		{0, 2, 0, "", "git", "secret", "github.com/org/repo.git"},
		{0, 2, 0, "", "git", "sec%zzret", "example.com/org/repo.git"},
		{0, 4, 0, "", "user", "ghp_secret", "github.com/org/repo.git"},
		{0, 7, 0, "", "git", "pa", "github.com/org/repo.git"},
		{0, 8, 0, "", "alice", "ghp_secret", "github.com:org/repo.git"},
		{0, 8, 0, "", "x-token-auth", "secret", "bitbucket.org/org/repo.git"},
		{0, 8, 0, "", "1", "000000", "0://"},
		{0, 8, 0, "", "A", "0", "://"},
		{0, 8, 0, "", "alice", "ghp_secret", "git:0"},
		{0, 5, 0, "", "0", "00", "://"},
		{0, 8, 0, "", "alice", "ghp_secret", "corpgit:org/repo.git"},
		{1, 6, 0, "", "user", "ghp_secret", "example.com/org/repo.git"},
		{2, 4, 0, "", "alice", "ghp_secret", "example.com"},
		{0, 4, 1, "example.com/a://", "u", "pwzq", "q@z"},
		{0, 6, 2, "example.com/a://", "u", "pwzq", "f@z"},
		{0, 8, 0, "", "alice", "s3cret", "host/a://b@c"},
		{0, 4, 0, "", "alice", "pwzq", "host/a://b@c"},
		{0, 7, 0, "x://", "u", "pwzq", "h"},
		{0, 8, 0, "B:://", "u", "pwzq", "h"},
		{0, 8, 1, "", "0", "1", "git:@"},
		{0, 8, 1, "", "0", "1", "://@"},
		{0, 8, 1, "", "0", "1", "/@://@"},
		{3, 8, 0, "", "0", "1", "/@://@"},
		{3, 5, 0, "", "user", "s3cretzq", "host/x"},
		// "Ⱥ" is 2 bytes and lowercases to 3, "İ" is 2 and lowercases to 1, and
		// "K" (Kelvin) is 3 and lowercases to 1.
		{0, 8, 0, "x:ȺȺȺȺȺȺȺȺ://", "alice", "s3cret", "host/repo"},
		{0, 0, 0, "", "ȺȺȺȺ", "pwzq", "example.com/org/repo.git"},
		{0, 8, 0, "", "İİİİ", "pwzq", "host/a://b@c"},
		{2, 4, 1, "", "u", "pwKKzq", "x@example.com"},
		// RedactRemoteURL percent-escapes non-ASCII text it keeps.
		{3, 5, 0, "", "خ", "1", "0"},
		{3, 5, 0, "", "Л", "1", "%00"},
	} {
		f.Add(uint8(seed.prefix), uint8(seed.form), uint8(seed.end), seed.lead, seed.user, seed.password, seed.rest)
	}
	f.Fuzz(func(t *testing.T, prefixIndex, formIndex, endIndex uint8, lead, user, password, rest string) {
		if !onlyText(user, "-._~%") || password == "" || !onlyText(password, "-._~%") || !onlyText(rest, "-._~%:/@") {
			t.Skip()
		}
		prefix := tokenPrefixes[int(prefixIndex)%len(tokenPrefixes)]
		form := remoteForms[int(formIndex)%len(remoteForms)]
		end := userinfoEnds[int(endIndex)%len(userinfoEnds)]
		// A lead puts the userinfo after a later "://", as in "https:/example.com/a://u:pw@z".
		// The authority follows the first "://", so the lead holds one, at its end.
		if !onlyText(lead, "-._~%:/") || lead != "" && (!strings.HasSuffix(lead, "://") || strings.Index(lead, "://") != len(lead)-len("://")) {
			t.Skip()
		}
		// A "://" form, a form and lead that join into a "://" before the lead's
		// own, as in "https:" + "//x://", or a leading "//", as in "//./://",
		// starts the authority there. The lead is then a host and path, and the
		// userinfo slot sits in the path.
		if lead != "" && (strings.Index(form+lead, "://") < len(form+lead)-len("://") || strings.HasPrefix(form+lead, "//")) {
			t.Skip()
		}
		// The heuristics take the text before a "?" or "#" as userinfo only when an
		// "@" follows, as in "u:pw?q@z". A non-http URL ends its authority at the
		// "?" or "#", so "u:pw" there is a host and port. That URL comes from an
		// ssh:// or git:// form, or, without a form, from a lead that is a non-http
		// scheme and its "://", as in "A://0:1?@".
		beforeSeparator, _, _ := strings.Cut(lead, "://")
		schemeLead := lead != "" && beforeSeparator != "" && asciiLetter(beforeSeparator[0]) && onlyRunes(beforeSeparator, "+-.")
		httpLead := strings.EqualFold(beforeSeparator, "http") || strings.EqualFold(beforeSeparator, "https")
		if end != "@" && (!strings.Contains(rest, "@") || form == "ssh://" || form == "git://" || form == "" && schemeLead && !httpLead) {
			t.Skip()
		}
		// "jane@example.com:alice:pw@host" is an scp remote whose path holds "alice:pw@host".
		if form == "" && strings.Contains(prefix, "@") {
			t.Skip()
		}
		remote := form + lead + user + ":" + password + end + rest
		if !HasInlineCredentials(remote) {
			t.Fatalf("HasInlineCredentials(%q) = false", remote)
		}
		lines := []string{
			prefix + remote,
			"fatal: unable to access '" + prefix + remote + "': HTTP 401",
			"clone " + prefix + remote + " failed",
		}
		lines = append(lines, "remotes=https://example.com/a.git,"+prefix+remote)
		lowerPassword := strings.ToLower(password)
		for _, line := range lines {
			if strings.Count(strings.ToLower(line), lowerPassword) != 1 || strings.Contains(lowerPassword, "redacted") {
				continue
			}
			if got := RedactString(line); leaksPassword(got, password) {
				t.Fatalf("RedactString(%q) = %q leaks the password", line, got)
			}
			if got := RedactLogString(line); leaksPassword(got, password) {
				t.Fatalf("RedactLogString(%q) = %q leaks the password", line, got)
			}
		}
	})
}

var redactionMarkers = strings.NewReplacer("REDACTED_REMOTE", "\x00", "REDACTED", "\x00")

// leaksPassword masks the redaction markers, which can join with neighboring
// text into the password, and ignores case, because redaction uppercases
// percent-encodings.
// nonASCIIEscape matches the escape url.URL.String writes for a non-ASCII byte.
var nonASCIIEscape = regexp.MustCompile(`%[89A-Fa-f][0-9A-Fa-f]`)

func leaksPassword(redacted string, password string) bool {
	shown := redactionMarkers.Replace(redacted)
	// The "1" in "%D1" is not the password "1".
	if !strings.Contains(password, "%") {
		shown = nonASCIIEscape.ReplaceAllString(shown, "\x00")
	}
	return strings.Contains(strings.ToLower(shown), strings.ToLower(password))
}

func asciiLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// onlyText reports whether s holds only letters, ASCII digits and extra.
func onlyText(s string, extra string) bool {
	for _, r := range s {
		if !(r > unicode.MaxASCII && unicode.IsLetter(r) || onlyRunes(string(r), extra)) {
			return false
		}
	}
	return true
}

// onlyRunes reports whether s holds only ASCII letters, digits and extra.
func onlyRunes(s string, extra string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(extra, r)) {
			return false
		}
	}
	return true
}
