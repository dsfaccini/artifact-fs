//go:build !darwin && !linux

package doctor

import "errors"

func osVersion() (string, error) { return "", errors.New("not macOS") }

func apfs(string) error { return errors.New("APFS exists only on macOS") }

func mountIDs() (bool, error) { return false, errors.New("mount IDs are read only on Linux") }
