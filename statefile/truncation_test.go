package statefile

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRecordPayloadEOFIsTruncation(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			r, err := NewReader(t.Context(), bytes.NewReader(sample(t, compression)))
			if err != nil {
				t.Fatal(err)
			}
			h := r.Header()
			r.Close()
			var out bytes.Buffer
			w, err := NewWriter(t.Context(), &out, h.HeaderRLP, h.Metadata, compression)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Abort()
			recordHeader := []byte{byte(AccountStart), 23}
			if err := writeAll(w.blocks, recordHeader[:]); err != nil {
				t.Fatal(err)
			}
			// A valid physical terminator cannot stand in for the missing payload.
			if err := w.blocks.close(); err != nil {
				t.Fatal(err)
			}
			r, err = NewReader(t.Context(), bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			for range 2 {
				if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("missing payload must be a sticky truncation error, got %v", err)
				}
			}
			if _, err := r.Result(); err == nil {
				t.Fatal("truncated file has a verified result")
			}
		})
	}
}
