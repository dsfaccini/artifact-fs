package slimtree

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// Home returns slimtree's home directory: $SLIMTREE_HOME, else
// $XDG_DATA_HOME/slimtree, else ~/.local/share/slimtree. It neither creates
// nor checks the directory.
func Home() (string, error) {
	if h := os.Getenv("SLIMTREE_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return "", fmt.Errorf("SLIMTREE_HOME %q is not an absolute path", h)
		}
		return filepath.Clean(h), nil
	}
	// The XDG base directory spec ignores a relative path.
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "slimtree"), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".local", "share", "slimtree"), nil
}

// openHome opens the home Home names.
func openHome() (*state.Home, error) {
	dir, err := Home()
	if err != nil {
		return nil, err
	}
	return state.Open(dir)
}
