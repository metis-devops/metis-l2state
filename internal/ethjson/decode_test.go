package ethjson

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestStreamingDecode(t *testing.T) {
	header := &types.Header{
		Number: big.NewInt(42), Difficulty: big.NewInt(1), Root: common.HexToHash("0x1234"),
		Extra: []byte{0x60, 0xff}, GasLimit: ^uint64(0),
	}
	log := types.Log{
		Address: common.HexToAddress("0x1234"), Topics: []common.Hash{common.HexToHash("0xabcd")},
		Data: []byte{0x60, 0xff}, BlockNumber: ^uint64(0), TxHash: common.HexToHash("0x01"),
		TxIndex: 2, BlockHash: header.Hash(), BlockTimestamp: ^uint64(0), Index: 3, Removed: true,
	}
	for _, tc := range []struct {
		name   string
		value  any
		decode func(io.Reader) error
	}{
		{"header", header, func(r io.Reader) error {
			got, err := DecodeHeader(r, false)
			if err == nil {
				gotRLP, _ := rlp.EncodeToBytes(got)
				wantRLP, _ := rlp.EncodeToBytes(header)
				if !bytes.Equal(gotRLP, wantRLP) {
					t.Fatal("header changed during decoding")
				}
			}
			return err
		}},
		{"log", log, func(r io.Reader) error {
			var got types.Log
			err := DecodeLog(r, &got)
			if err == nil && !reflect.DeepEqual(got, log) {
				t.Fatalf("log changed: got %+v, want %+v", got, log)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			// Extensions stay opaque, including schema-like names, arbitrary
			// precision numbers, nulls and escaped pointer characters in keys.
			body := string(raw[:len(raw)-1]) + `,"provider/~":{"number":18446744073709551616,"removed":null,"Number":[]}}`
			if err := tc.decode(iotest.OneByteReader(strings.NewReader(body))); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{` {}`, ` null`, ` x`} {
				if err := tc.decode(iotest.OneByteReader(strings.NewReader(body + suffix))); err == nil {
					t.Fatalf("accepted trailing input %q", suffix)
				}
			}
			for _, prefix := range []string{body[:len(body)/2], body} {
				failure := errors.New("reader failed")
				r := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(failure))
				if err := tc.decode(r); !errors.Is(err, failure) {
					t.Fatalf("lost reader error: %v", err)
				}
			}
		})
	}
}

func TestRequiredNullStopsBeforeReadingRemainder(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, field string
		decode              func(io.Reader) error
	}{
		{"header", `{"number":null,`, "number", func(r io.Reader) error {
			_, err := DecodeHeader(r, false)
			return err
		}},
		{"log", `{"removed":null,`, "removed", func(r io.Reader) error {
			return DecodeLog(r, new(types.Log))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := errors.New("read past null")
			r := io.MultiReader(strings.NewReader(tc.prefix), iotest.ErrReader(failure))
			err := tc.decode(r)
			if err == nil || !strings.Contains(err.Error(), "null field "+tc.field) || errors.Is(err, failure) {
				t.Fatalf("expected immediate null rejection, got %v", err)
			}
		})
	}
}

func TestLogOptionalTimestamp(t *testing.T) {
	const prefix = `{"address":"0x0000000000000000000000000000000000000000","topics":[],"data":"0x","blockNumber":"0x0","blockHash":"0x0000000000000000000000000000000000000000000000000000000000000000","transactionHash":"0x0000000000000000000000000000000000000000000000000000000000000000","transactionIndex":"0x0","logIndex":"0x0","removed":false`
	for _, suffix := range []string{`}`, `,"blockTimestamp":null}`, `,"blockTimestamp":"0x7"}`} {
		log := types.Log{BlockTimestamp: 7}
		if err := DecodeLog(strings.NewReader(prefix+suffix), &log); err != nil {
			t.Fatal(err)
		}
		if log.BlockTimestamp != 7 || len(log.Topics) != 0 || len(log.Data) != 0 || log.Removed {
			t.Fatalf("changed optional timestamp or zero values: %+v", log)
		}
	}
	for _, value := range []string{`7`, `"0x07"`, `"\u0030x7"`, `[]`, `{}`} {
		if err := DecodeLog(strings.NewReader(prefix+`,"blockTimestamp":`+value+`}`), new(types.Log)); err == nil {
			t.Fatalf("accepted invalid timestamp %s", value)
		}
	}
}
