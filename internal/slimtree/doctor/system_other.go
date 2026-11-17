//go:build !darwin

package doctor

import "errors"

func osVersion() (string, error) { return "", errors.New("not macOS") }

func apfs(string) error { return errors.New("APFS exists only on macOS") }
