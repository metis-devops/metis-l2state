package migration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/metis-devops/metis-l2state/statefile"
)

// Fixture generation, independent verification and cleanup are untimed.
// The suite selects the current cache mode through L2STATE_BENCH_CODE_CACHE.
func BenchmarkDump(b *testing.B) {
	f := newFixture(b, 10000, 0)
	dir := b.TempDir()
	cache := os.Getenv("L2STATE_BENCH_CODE_CACHE")
	b.ReportAllocs()
	for b.Loop() {
		out := filepath.Join(dir, "state")
		result, err := Dump(b.Context(), DumpOptions{CodeCache: cache, Source: f.source, Output: out, Workers: 2, CacheMB: 16, Handles: 16})
		must(b, err)
		b.StopTimer()
		file, err := os.Open(out)
		must(b, err)
		verified, err := statefile.Verify(b.Context(), file)
		closeErr := file.Close()
		must(b, err)
		must(b, closeErr)
		if verified != result.Summary {
			b.Fatal("independent benchmark verification disagrees with dump")
		}
		must(b, os.Remove(out))
		b.StartTimer()
	}
}
