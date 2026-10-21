package migration

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

func TestAllocScalarStreamsOneValue(t *testing.T) {
	for _, tt := range []struct {
		name, raw   string
		value, want any
	}{
		{"balance", `9007199254740993`, new(gethmath.HexOrDecimal256), gethmath.NewHexOrDecimal256(9007199254740993)},
		{"nonce", `18446744073709551615`, new(gethmath.HexOrDecimal64), new(gethmath.HexOrDecimal64(^uint64(0)))},
		{"code", `"0x6000"`, new(hexutil.Bytes), new(hexutil.Bytes{0x60, 0})},
		{"storage", `"\u0030x02"`, new(string), new("0x02")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := iotest.OneByteReader(strings.NewReader("[" + tt.raw + ",true]"))
			l := allocLoader{decoder: jsontext.NewDecoder(strictio.TokenReader(t.Context(), r, 64, 7, "alloc"))}
			_, err := l.decoder.ReadToken()
			must(t, err)
			must(t, l.scalar(tt.value))
			if !reflect.DeepEqual(tt.value, tt.want) {
				t.Fatalf("got %v, want %v", tt.value, tt.want)
			}
			next, err := l.decoder.ReadToken()
			must(t, err)
			if next.Kind() != jsontext.KindTrue {
				t.Fatal("scalar consumed the next array element")
			}
		})
	}
}

func TestAllocRejectsInvalidScalars(t *testing.T) {
	const address = "0x0000000000000000000000000000000000000101"
	for _, field := range []string{"balance", "nonce", "code", "storage"} {
		for _, raw := range []string{`null`, `true`, `false`, `{}`, `[]`} {
			t.Run(field+"/"+raw, func(t *testing.T) {
				value := raw
				if field == "storage" {
					value = `{"0x00":` + raw + `}`
				}
				path := filepath.Join(t.TempDir(), "alloc")
				must(t, os.WriteFile(path, []byte(`{"`+address+`":{"`+field+`":`+value+`}}`), 0600))
				if _, _, err := readAlloc(t.Context(), path); err == nil {
					t.Fatal("accepted non-scalar or null alloc value")
				}
			})
		}
	}
	for _, field := range []string{"balance", "nonce", "code"} {
		for _, raw := range []string{`"\u0030x01"`, `"0x0\u0031"`} {
			t.Run(field+"/"+raw, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "alloc")
				must(t, os.WriteFile(path, []byte(`{"`+address+`":{"`+field+`":`+raw+`}}`), 0600))
				if _, _, err := readAlloc(t.Context(), path); err == nil {
					t.Fatal("normalized escaped scalar before geth decoding")
				}
			})
		}
	}
}

func TestAllocScalarBounds(t *testing.T) {
	for _, raw := range []string{
		`[` + strings.Repeat(`0,`, 10000) + `0]`,
		`{` + strings.Repeat(`"x":0,`, 10000) + `"y":0}`,
	} {
		r := strings.NewReader(raw)
		l := allocLoader{decoder: jsontext.NewDecoder(strictio.TokenReader(t.Context(), r, 64, 7, "alloc"))}
		var value hexutil.Bytes
		if err := l.scalar(&value); err == nil || !strings.Contains(err.Error(), "expected non-null scalar") {
			t.Fatalf("wrong container rejection: %v", err)
		}
		if len(raw)-r.Len() > 7 || l.decoder.InputOffset() != 0 {
			t.Fatal("buffered container before rejecting it")
		}
	}
	for _, raw := range []string{`"0x` + strings.Repeat("00", 32) + `"`, strings.Repeat("1", 65)} {
		l := allocLoader{decoder: jsontext.NewDecoder(strictio.TokenReader(t.Context(), strings.NewReader(raw), 64, 7, "alloc"))}
		var value gethmath.HexOrDecimal256
		if err := l.scalar(&value); err == nil || !strings.Contains(err.Error(), "JSON token exceeds limit 64") {
			t.Fatalf("lost token limit during typed decoding: %v", err)
		}
	}
}
