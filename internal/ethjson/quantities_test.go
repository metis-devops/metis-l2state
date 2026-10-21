package ethjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math/big"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestHeaderQuantityFormats(t *testing.T) {
	t.Parallel()
	h := &types.Header{
		Number: big.NewInt(42), Difficulty: big.NewInt(1), GasLimit: 9, GasUsed: 8, Time: 7,
		BaseFee: big.NewInt(6), BlobGasUsed: new(uint64(5)), ExcessBlobGas: new(uint64(4)),
		SlotNumber: new(uint64(3)), Extra: []byte{0},
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, decimal := range []bool{false, true} {
		if _, err := DecodeHeader(iotest.OneByteReader(bytes.NewReader(raw)), decimal); err != nil {
			t.Fatalf("hex header with decimal=%t: %v", decimal, err)
		}
	}
	for _, tc := range []struct {
		field, number  string
		fixtureDecimal bool
	}{
		{"number", "42", true}, {"difficulty", "1", true}, {"gasLimit", "9", true},
		{"gasUsed", "8", true}, {"timestamp", "7", true}, {"baseFeePerGas", "6", false},
		{"blobGasUsed", "5", false}, {"excessBlobGas", "4", false}, {"slotNumber", "3", false},
	} {
		t.Run(tc.field, func(t *testing.T) {
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			wire := fields[tc.field]
			escaped := bytes.Replace(wire, []byte("0x"), []byte(`\u0030x`), 1)
			escapedInput := bytes.Replace(raw, []byte(`"`+tc.field+`":`+string(wire)), []byte(`"`+tc.field+`":`+string(escaped)), 1)
			fields[tc.field] = jsontext.Value(tc.number)
			input, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			for _, decimal := range []bool{false, true} {
				_, err := DecodeHeader(iotest.OneByteReader(bytes.NewReader(input)), decimal)
				if wantAccepted := decimal && tc.fixtureDecimal; (err == nil) != wantAccepted {
					t.Fatalf("decimal=%t: got %v, want accepted=%t", decimal, err, wantAccepted)
				}
				if _, err := DecodeHeader(iotest.OneByteReader(bytes.NewReader(escapedInput)), decimal); err == nil {
					t.Fatalf("decimal=%t: accepted escaped hex quantity", decimal)
				}
			}
		})
	}
}

func TestLogQuantityFormats(t *testing.T) {
	t.Parallel()
	log := types.Log{Topics: []common.Hash{}, Data: []byte{0}, BlockNumber: 1, TxIndex: 2, Index: 3, BlockTimestamp: 4}
	raw, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeLog(iotest.OneByteReader(bytes.NewReader(raw)), new(types.Log)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"blockNumber", "transactionIndex", "logIndex", "blockTimestamp", "data"} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			invalid := []string{`0`, `"\u0030x1"`, `[]`, `{}`}
			if name == "data" {
				invalid = append(invalid, `"0x0"`)
			} else {
				invalid = append(invalid, `"0x00"`, `"0x10000000000000000"`)
			}
			for _, value := range invalid {
				// Splice the wire value so marshaling cannot normalize away
				// the escapes that geth must reject inside quantity strings.
				input := bytes.Replace(raw, []byte(`"`+name+`":`+string(fields[name])), []byte(`"`+name+`":`+value), 1)
				if err := DecodeLog(iotest.OneByteReader(bytes.NewReader(input)), new(types.Log)); err == nil {
					t.Fatalf("accepted invalid value %s", value)
				}
			}
		})
	}
}
