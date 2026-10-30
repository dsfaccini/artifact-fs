package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAdoptGitDirRejectsAcquisitionFlags(t *testing.T) {
	for _, flag := range [][]string{
		{"--remote", "https://example.invalid/repository"},
		{"--ref", "refs/heads/main"}, {"--branch", "main"}, {"--fetch-ref", "main"},
		{"--depth", "0"}, {"--require-commit", strings.Repeat("a", 40)},
		{"--async"}, {"--prepared-gitdir"}, {"--refresh", "30s"},
	} {
		t.Run(flag[0], func(t *testing.T) {
			t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
			args := append([]string{"add-repo", "--name", "repo", "--adopt-gitdir", "--git-dir", "/unused"}, flag...)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), args, &stdout, &stderr); code == 0 {
				t.Fatalf("accepted %v", flag)
			}
			if !strings.Contains(stderr.String(), "--adopt-gitdir") {
				t.Fatalf("unexpected diagnostic: %s", stderr.String())
			}
		})
	}
}
