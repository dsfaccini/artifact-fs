package overlay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func benchStore(b *testing.B) *Store {
	dir := b.TempDir()
	s, err := New(context.Background(), model.RepoConfig{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func benchWriteChunks(b *testing.B, size int) {
	s := benchStore(b)
	ctx := context.Background()
	if _, err := s.CreateFile(ctx, "out.bin", 0o644); err != nil {
		b.Fatal(err)
	}
	chunk := make([]byte, size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := range b.N {
		if _, err := s.WriteFile(ctx, "out.bin", int64(i)*int64(size), chunk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOverlayWriteFile4KiBChunks(b *testing.B)   { benchWriteChunks(b, 4096) }
func BenchmarkOverlayWriteFile128KiBChunks(b *testing.B) { benchWriteChunks(b, 128*1024) }

func BenchmarkOverlayUpsert(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		e := model.OverlayEntry{Path: "x.txt", Kind: model.OverlayKindCreate, Mode: 0o644, SizeBytes: int64(i), MtimeUnixNs: int64(i), CtimeUnixNs: int64(i)}
		if err := s.upsertEntry(ctx, e); err != nil {
			b.Fatal(err)
		}
	}
}
