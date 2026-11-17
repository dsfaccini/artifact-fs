package doctor

import (
	"errors"

	"github.com/cloudflare/artifact-fs/internal/slimtree/mounts"
)

func osVersion() (string, error) { return "", errors.New("not macOS") }

func apfs(string) error { return errors.New("APFS exists only on macOS") }

func mountIDs() (bool, error) { return mounts.UniqueMountIDs() }
