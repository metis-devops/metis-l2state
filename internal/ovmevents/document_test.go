package ovmevents

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestEncodeWriterParity(t *testing.T) {
	frozen, err := os.ReadFile("testdata/v1.json")
	check(t, err)
	for _, shape := range []string{"sample", "empty", "nil"} {
		t.Run(shape, func(t *testing.T) {
			d := sample()
			want := string(frozen)
			if shape != "sample" {
				d.Allowances = d.Allowances[:0]
				d.Members = d.Members[:0]
				if shape == "nil" {
					d.Allowances = nil
					d.Members = nil
				}
				prefix, _, found := strings.Cut(want, `,"allowances":`)
				if !found {
					t.Fatal("frozen fixture is missing allowances")
				}
				want = prefix + ",\"allowances\":[],\"members\":[]}\n"
			}
			var buffer bytes.Buffer
			var builder strings.Builder
			check(t, Encode(&buffer, d))
			check(t, Encode(&builder, d))
			if buffer.String() != want || builder.String() != want {
				t.Fatal("writer type or empty slice representation changed checkpoint bytes")
			}
		})
	}
}

type checkpointLimitWriter struct {
	bytes.Buffer
	remaining int
	fault     error
}

func (w *checkpointLimitWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p[:min(len(p), w.remaining)])
	w.remaining -= n
	if n < len(p) {
		return n, w.fault
	}
	return n, err
}

// Override Buffer.WriteString as framing also uses io.WriteString.
func (w *checkpointLimitWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestEncodePropagatesWriteFailures(t *testing.T) {
	want, err := os.ReadFile("testdata/v1.json")
	check(t, err)
	sentinel := errors.New("checkpoint write failed")
	// Exercise failures inside values, separators, newlines and the final brace.
	for limit := 0; limit <= len(want); limit++ {
		w := &checkpointLimitWriter{remaining: limit, fault: sentinel}
		err := Encode(w, sample())
		if limit < len(want) {
			if !errors.Is(err, sentinel) {
				t.Fatalf("limit %d: lost write error: %v", limit, err)
			}
		} else {
			check(t, err)
		}
		if !bytes.Equal(w.Bytes(), want[:limit]) {
			t.Fatalf("limit %d: changed bytes before write failure", limit)
		}
	}
}
