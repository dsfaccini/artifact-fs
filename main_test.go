package main

import (
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/cli"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
