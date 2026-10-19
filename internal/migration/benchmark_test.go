package migration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/metis-devops/metis-l2state/statefile"
)

// Run in fresh processes; fixture creation and output cleanup are not timed.
func BenchmarkMigrate(b *testing.B) {
	f := newFixture(b, 10000, 0)
	dir := b.TempDir()
	cache := os.Getenv("L2STATE_BENCH_CODE_CACHE")
	b.ReportAllocs()

	for b.Loop() {
		out := filepath.Join(dir, "state")
		options := f.options(out)
		options.CodeCache = cache
		result, err := Migrate(b.Context(), options)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		file, err := os.Open(out)
		must(b, err)
		verified, err := statefile.Verify(b.Context(), file)
		closeErr := file.Close()
		must(b, err)
		must(b, closeErr)
		if verified != result.Summary {
			b.Fatal("independent benchmark verification disagrees with migration")
		}
		if err := os.Remove(out); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}
