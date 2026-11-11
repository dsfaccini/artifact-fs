package blobcache

import (
	"crypto/sha1"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// BenchmarkInstall installs 5,000 small staged blobs and reports the time per
// blob. The rename case moves each blob into place with no counter update,
// so the difference to it is the cost of the counter update.
func BenchmarkInstall(b *testing.B) {
	const blobs = 5000
	for _, bc := range []struct {
		name   string
		rename bool
	}{
		{"install", false},
		{"rename", true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				oids := make([]string, blobs)
				staged := make([]string, blobs)
				for i := range blobs {
					oids[i] = fmt.Sprintf("%x", sha1.Sum([]byte(strconv.Itoa(i))))
					path, err := Stage(dir, oids[i])
					if err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("small blob payload\n"), 0o600); err != nil {
						b.Fatal(err)
					}
					staged[i] = path
				}
				if _, err := ReadStats(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for i := range blobs {
					var err error
					if bc.rename {
						err = os.Rename(staged[i], Path(dir, oids[i]))
					} else {
						err = Install(dir, oids[i], staged[i])
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*blobs), "us/blob")
		})
	}
}
