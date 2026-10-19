package statefile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFrozenFormat(t *testing.T) {
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		data, err := os.ReadFile(filepath.Join("testdata", "v1-"+codec+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(t.Context(), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, sample(t, codec)) {
			t.Fatal("v1 format changed; do not silently regenerate corpus")
		}
	}
}
func TestFormatCandidate(t *testing.T) {
	path := os.Getenv("L2STATE_FORMAT_CANDIDATE")
	if path == "" {
		t.Skip("explicit new candidate directory required")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		if err := os.WriteFile(filepath.Join(path, "v1-"+codec+".bin"), sample(t, codec), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRejectUnsupportedFormatVersion(t *testing.T) {
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		data, err := os.ReadFile(filepath.Join("testdata", "v1-"+codec+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		data[7] = 2
		if _, err := Verify(t.Context(), bytes.NewReader(data)); err == nil {
			t.Fatal("accepted unsupported format version")
		}
	}
}
