package auth

import (
	"strings"
	"testing"
)

func TestRedactRemoteURL(t *testing.T) {
	in := "https://token123@github.com/org/repo.git?token=abc"
	out := RedactRemoteURL(in)
	if out == in {
		t.Fatalf("expected redaction")
	}
	if containsAny(out, []string{"token123", "abc"}) {
		t.Fatalf("token leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://github.com/org/repo.git?sig=s3cr3t")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("query secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://github.com/org/repo.git#access_token=ghp_secret")
	if containsAny(out, []string{"access_token", "ghp_secret"}) {
		t.Fatalf("fragment secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret%zz@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "%zz"}) {
		t.Fatalf("malformed userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:pa#ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("malformed userinfo with fragment delimiter leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret#@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("malformed username-only userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:pa/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("malformed userinfo with path delimiter leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret%zz:password=abc@example.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "%zz", "abc"}) {
		t.Fatalf("malformed userinfo with token-like password leaked in output: %s", out)
	}

	out = RedactRemoteURL("git@github.com:org/repo.git?sig=s3cr3t")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("scp-style query secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret/@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https:/user:ghp_secret@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("missing-slash https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:123/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "123", "ss"}) {
		t.Fatalf("numeric-prefix https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:123/dir/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "123", "ss"}) {
		t.Fatalf("multi-segment https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user.name:pa/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user.name", "pa", "ss"}) {
		t.Fatalf("dotted malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user.name:123/dir/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user.name", "123", "ss"}) {
		t.Fatalf("dotted numeric malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://first.last:123/dir/ss@example.com/org/repo.git")
	if containsAny(out, []string{"first.last", "123", "ss"}) {
		t.Fatalf("dotted name numeric malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://john.doe:123/ghp_secret@example.com/org/repo.git")
	if containsAny(out, []string{"john.doe", "123", "ghp_secret"}) {
		t.Fatalf("dotted name token malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret/part@example.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "part"}) {
		t.Fatalf("path-split username-only malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret/part?x@example.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "part"}) {
		t.Fatalf("query-split username-only malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret/part@example.com")
	if containsAny(out, []string{"ghp_secret", "part"}) {
		t.Fatalf("single-label path username-only malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret:8443/part@example.com")
	if containsAny(out, []string{"ghp_secret", "8443", "part"}) {
		t.Fatalf("ported single-label path username-only malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://git.example.com/team/repo@2026/archive.git")
	if out != "https://git.example.com/team/repo@2026/archive.git" {
		t.Fatalf("real-host path with at sign and continuation was redacted: %s", out)
	}

	out = RedactRemoteURL("file:///tmp/repo@2026.git")
	if out != "file:///tmp/repo@2026.git" {
		t.Fatalf("file URL path with at sign was redacted: %s", out)
	}

	out = RedactRemoteURL("https://ghp.secret/part@example.com/org/repo.git")
	if containsAny(out, []string{"ghp.secret", "part"}) {
		t.Fatalf("dotted path-split username-only malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https//ghp_secret@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("missing-colon https userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: https://ghp_secret%zz:password=abc@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "%zz", "abc"}) {
		t.Fatalf("redacted string leaked malformed credentials: %s", out)
	}

	out = RedactString("fatal: unable to access 'https://user:pass@example.com/org/repo.git/': failed")
	if containsAny(out, []string{"user", "pass"}) {
		t.Fatalf("quoted URL leaked credentials: %s", out)
	}

	out = RedactString("fatal: git@github.com:org/repo.git?sig=s3cr3t failed")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("scp-style query leaked in redacted string: %s", out)
	}

	out = RedactRemoteURL("ssh://git:sec%zzret@example.com/org/repo.git")
	if containsAny(out, []string{"sec", "%zz", "ret"}) {
		t.Fatalf("malformed ssh userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: ssh:/git:pa/ss@github.com/org/repo.git failed")
	if containsAny(out, []string{"pa/ss"}) {
		t.Fatalf("malformed ssh-style userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: remote=ssh:/git:pa/ss@github.com/org/repo.git failed")
	if containsAny(out, []string{"pa/ss"}) {
		t.Fatalf("prefixed malformed ssh-style userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https:/user:ghp_secret@github.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("http-like typo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: remote=https:/user:ghp_secret@github.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("prefixed http-like typo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: remote=https//ghp_secret@github.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("prefixed missing-colon typo leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=ssh://git@github.com/org/repo.git,https://ghp_secret@example.com/private.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("second URL credential leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=https://git.example.com:8443/team/repo.git,https://ghp_secret@example.com/private.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("second URL credential after ported URL leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=https://git:8443/team/repo.git,https://ghp_secret@example.com/private.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("second URL credential after single-label ported URL leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=https://git.example.com/repo.git,alice:ghp_secret@github.com:org/repo.git")
	if containsAny(out, []string{"alice", "ghp_secret"}) {
		t.Fatalf("scheme-less second URL credential leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=https://git.example.com/repo.git;alice:ghp_secret@github.com:org/repo.git")
	if containsAny(out, []string{"alice", "ghp_secret"}) {
		t.Fatalf("scheme-less second URL credential after semicolon leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://github.com/org/repo.git?sig=abc,def failed")
	if containsAny(out, []string{"abc", "def"}) {
		t.Fatalf("query value leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa,ss@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("comma-containing userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa;http:ss@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("semicolon-containing userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa,http:ss?x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("comma/query malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa,https://ss@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("comma/url-marker malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa/ss,https://x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa/ss"}) {
		t.Fatalf("slash comma/url-marker malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa/ss;https://x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa/ss"}) {
		t.Fatalf("slash semicolon/url-marker malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://use,r:pass@example.com/org/repo.git failed")
	if containsAny(out, []string{"use", "pass"}) {
		t.Fatalf("comma-split username leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://use;r:pass@example.com/org/repo.git failed")
	if containsAny(out, []string{"use", "pass"}) {
		t.Fatalf("semicolon-split username leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa;http:ss#x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("semicolon/fragment malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactRemoteURL("https://user:pa/ss?x@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa/ss"}) {
		t.Fatalf("slash query malformed userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:pa/ss#x@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa/ss"}) {
		t.Fatalf("slash fragment malformed userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: https://user:8443/ss,https://x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "8443/ss"}) {
		t.Fatalf("numeric slash comma malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:8443/ss;https://x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "8443/ss"}) {
		t.Fatalf("numeric slash semicolon malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: what? failed issue#123")
	if out != "fatal: what? failed issue#123" {
		t.Fatalf("ordinary punctuation was redacted: %s", out)
	}

	out = RedactString("contact dev@example.com? issue#123")
	if out != "contact dev@example.com? issue#123" {
		t.Fatalf("ordinary email punctuation was redacted: %s", out)
	}

	out = RedactString("fatal: https//ghp_secret,http:ss?x@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "ss"}) {
		t.Fatalf("missing-colon comma/query malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https//ghp_secret;http:ss#x@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "ss"}) {
		t.Fatalf("missing-colon semicolon/fragment malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactRemoteURL("https://github.com/%zz?access_token=abc@def")
	if containsAny(out, []string{"abc", "def"}) {
		t.Fatalf("malformed query with at sign leaked in output: %s", out)
	}

	out = RedactRemoteURL("alice:ghp_secret@github.com:org/repo.git")
	if containsAny(out, []string{"alice", "ghp_secret"}) {
		t.Fatalf("scheme-less malformed userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("x-token-auth:secret@bitbucket.org/org/repo.git")
	if containsAny(out, []string{"x-token-auth", "secret"}) {
		t.Fatalf("scheme-less token userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("alice:ghp_secret#@github.com:org/repo.git")
	if containsAny(out, []string{"alice", "ghp_secret"}) {
		t.Fatalf("scheme-less delimiter-split userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: unable to access 'jane@example.com:https:user:ghp_secret@example.com/org/repo.git': HTTP 401")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("typo-scheme userinfo after an at-sign prefix leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: unable to access 'https:/example.com/a://user:ghp_secret?q@z': HTTP 401")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("userinfo ending at a query after a later scheme separator leaked in redacted string: %s", out)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasInlineCredentials(tt.raw); got != tt.want {
				t.Fatalf("HasInlineCredentials(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
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
	} {
		f.Add(uint8(seed.prefix), uint8(seed.form), uint8(seed.end), seed.lead, seed.user, seed.password, seed.rest)
	}
	f.Fuzz(func(t *testing.T, prefixIndex, formIndex, endIndex uint8, lead, user, password, rest string) {
		if !onlyRunes(user, "-._~%") || password == "" || !onlyRunes(password, "-._~%") || !onlyRunes(rest, "-._~%:/@") {
			t.Skip()
		}
		// An "@" after a later "://", as in "alice:pw@host/a://b@c", puts the
		// userinfo after that "://", and the userinfo before it is left as is.
		if _, after, ok := strings.Cut(rest, "://"); ok && strings.Contains(after, "@") {
			t.Skip()
		}
		prefix := tokenPrefixes[int(prefixIndex)%len(tokenPrefixes)]
		form := remoteForms[int(formIndex)%len(remoteForms)]
		end := userinfoEnds[int(endIndex)%len(userinfoEnds)]
		// A lead puts the userinfo after a later "://", as in "https:/example.com/a://u:pw@z".
		// The authority follows the first "://", so the lead holds one, at its end.
		if !onlyRunes(lead, "-._~%:/") || lead != "" && strings.Index(lead, "://") != len(lead)-len("://") {
			t.Skip()
		}
		// After a "://" form, or one that a lead's leading ":" or "/" turns into
		// "://" ("https:" + "//x://"), the lead is a host and path, and the
		// userinfo slot sits in the path.
		if lead != "" && (strings.HasSuffix(form, "://") || strings.IndexAny(lead, ":/") == 0) {
			t.Skip()
		}
		// Without an http form the lead can parse as a scheme or an ssh URL path,
		// as in "B:://u:pw@h" and "ssh:/x://u:pw@h", and the redaction leaves the
		// userinfo as is.
		if lead != "" && (form == "" || form == "ssh:/") {
			t.Skip()
		}
		// "?" and "#" end the userinfo only when an "@" follows, as in "u:pw?q@z".
		// An ssh or git URL reads "u:pw?q@z" as a host and port instead.
		if end != "@" && (!strings.Contains(rest, "@") || form == "ssh://" || form == "git://") {
			t.Skip()
		}
		// In "0:1?git:@", a remote marker between the "?" and the "@" starts the
		// remote, and the userinfo before it is left as is.
		if form == "" && end != "@" && containsRemoteMarker(rest[:strings.Index(rest, "@")]) {
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
		// A comma list splits only before a remote marker, so a prefixed
		// second remote stays joined to the first.
		if prefix == "" {
			lines = append(lines, "remotes=https://example.com/a.git,"+remote)
		}
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
func leaksPassword(redacted string, password string) bool {
	return strings.Contains(strings.ToLower(redactionMarkers.Replace(redacted)), strings.ToLower(password))
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

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if n != "" && len(s) >= len(n) && stringContains(s, n) {
			return true
		}
	}
	return false
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
