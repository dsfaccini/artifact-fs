package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/slimtree"
)

// main cancels the command's context on SIGINT, SIGTERM and SIGHUP, the
// signal a closed terminal sends, so a command can roll back what it made.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	os.Exit(slimtree.Main(ctx, os.Args, os.Stdout, os.Stderr))
}
