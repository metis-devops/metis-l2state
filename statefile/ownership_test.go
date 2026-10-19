package statefile

import (
	"bytes"
	"errors"
	"io"
	"math/big"
	"reflect"
	"testing"
)

func TestReaderOwnedRecords(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			r, err := NewReader(t.Context(), bytes.NewReader(sample(t, compression)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			var records, saved []Record
			for {
				record, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
				clone := record
				clone.Code = bytes.Clone(record.Code)
				if record.Account.Balance != nil {
					clone.Account.Balance = new(big.Int).Set(record.Account.Balance)
				}
				saved = append(saved, clone)
			}
			if !reflect.DeepEqual(records, saved) {
				t.Fatal("Next overwrote a previously owned record")
			}
			if _, err := r.Result(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReaderBalanceMutationDoesNotAlterValidation(t *testing.T) {
	r, err := NewReader(t.Context(), bytes.NewReader(sample(t, CompressionNone)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	first.Account.Balance.SetUint64(12345)
	for {
		_, err = r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Result(); err != nil {
		t.Fatal(err)
	}
}
