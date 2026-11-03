package model

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// FileMatchesBlobOID reports whether the file's raw bytes hash to the git blob
// oid, as `git hash-object --no-filters` would.
func FileMatchesBlobOID(path string, oid string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	return ReaderMatchesBlobOID(f, st.Size(), oid)
}

// ReaderMatchesBlobOID hashes size bytes from r as a git blob. The oid's length
// selects SHA-1 or SHA-256.
func ReaderMatchesBlobOID(r io.Reader, size int64, oid string) (bool, error) {
	var h hash.Hash
	switch len(oid) {
	case sha1.Size * 2:
		h = sha1.New()
	case sha256.Size * 2:
		h = sha256.New()
	default:
		return false, nil
	}
	if _, err := fmt.Fprintf(h, "blob %d\x00", size); err != nil {
		return false, err
	}
	if _, err := io.Copy(h, r); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), oid), nil
}
