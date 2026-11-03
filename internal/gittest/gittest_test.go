package gittest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExpireOnFileExpiresWhenFileAppears(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	ctx, cancel := ExpireOnFile(context.Background(), marker)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("Err before marker = %v, want nil", err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want deadline exceeded", err)
	}
}

func TestExpireOnFileEndsWithParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := ExpireOnFile(parent, filepath.Join(t.TempDir(), "never"))
	defer cancel()
	cancelParent()
	<-ctx.Done()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want canceled", err)
	}
}

func TestExpireOnFileEndsOnCancel(t *testing.T) {
	ctx, cancel := ExpireOnFile(context.Background(), filepath.Join(t.TempDir(), "never"))
	cancel()
	<-ctx.Done()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want canceled", err)
	}
}
