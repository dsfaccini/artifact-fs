package blobcache

import (
	"crypto/sha1"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// BenchmarkInstall installs 5,000 small staged blobs, batch of them per
// Install, and reports the time per blob. The rename case moves each blob
// into place with no counter update, so the difference to it is the cost of
// the counter update.
func BenchmarkInstall(b *testing.B) {
	const blobs = 5000
	for _, bc := range []struct {
		name  string
		batch int
	}{
		{"batch=1", 1},
		{"batch=64", 64},
		{"rename", 0},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				staged := make([]Staged, blobs)
				for i := range staged {
					oid := fmt.Sprintf("%x", sha1.Sum([]byte(strconv.Itoa(i))))
					path, err := Stage(dir, oid)
					if err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("small blob payload\n"), 0o600); err != nil {
						b.Fatal(err)
					}
					staged[i] = Staged{OID: oid, Path: path}
				}
				if _, err := ReadStats(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if bc.batch == 0 {
					for _, s := range staged {
						if err := os.Rename(s.Path, Path(dir, s.OID)); err != nil {
							b.Fatal(err)
						}
					}
					continue
				}
				for i := 0; i < blobs; i += bc.batch {
					if _, err := Install(dir, staged[i:min(i+bc.batch, blobs)]); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*blobs), "us/blob")
		})
	}
}
