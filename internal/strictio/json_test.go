package strictio_test

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/metis-devops/metis-l2state/internal/migration"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

func TestWriteJSONRepresentation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{"text", "out<&>\u2028\u2029", "\"out<&>\u2028\u2029\"\n"},
		{"omitted", struct {
			Flag  bool   `json:"flag,omitempty"`
			Count uint64 `json:"count,omitempty"`
			Data  []byte `json:"data"`
			Empty []int  `json:"empty,omitempty"`
			Zero  int    `json:"zero,omitzero"`
		}{}, `{"flag":false,"count":0,"data":""}` + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := strictio.WriteJSON(&got, tt.value); err != nil {
				t.Fatal(err)
			}
			if got.String() != tt.want {
				t.Fatalf("got %q, want %q", got.String(), tt.want)
			}
		})
	}
}

// Map member order is unspecified; compare canonical JSON while still checking
// the values' wire representations and the report's terminating newline.
func TestWriteJSONMaps(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{"maps", map[string]any{
			"z": []int(nil), "a": []int{}, "emptyMap": map[string]int{},
			"nilMap": map[string]int(nil), "code": hexutil.Bytes{0x60, 0xff}, "bytes": [2]byte{1, 2},
		}, `{"a":[],"bytes":"AQI=","code":"0x60ff","emptyMap":{},"nilMap":{},"z":[]}` + "\n"},
		{"hashKeys", map[common.Hash]common.Hash{
			common.HexToHash("0xff"): common.HexToHash("0x02"),
			common.HexToHash("0x01"): common.HexToHash("0x03"),
		}, `{"` + common.HexToHash("0x01").Hex() + `":"` + common.HexToHash("0x03").Hex() + `","` + common.HexToHash("0xff").Hex() + `":"` + common.HexToHash("0x02").Hex() + `"}` + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := strictio.WriteJSON(&output, tt.value); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(output.Bytes(), []byte("\n")) {
				t.Fatal("missing terminating newline")
			}
			got, want := jsontext.Value(output.Bytes()), jsontext.Value(tt.want)
			if err := got.Canonicalize(); err != nil {
				t.Fatal(err)
			}
			if err := want.Canonicalize(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func TestWriteJSONRejectsInvalidUTF8(t *testing.T) {
	for name, value := range map[string]any{
		"migration": migration.Result{Artifact: "out/\xff"},
		"sync":      ovmevents.Result{Output: "out/\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := strictio.WriteJSON(&output, value); err == nil {
				t.Fatal("accepted invalid UTF-8 output")
			}
		})
	}
}

func TestDecodeJSONTypedSemantics(t *testing.T) {
	type document struct {
		Value string `json:"value"`
		Count uint64 `json:"count"`
		Pair  [2]int `json:"pair"`
	}
	for raw, want := range map[string]document{
		`{}`: {}, `null`: {}, `{"value":null}`: {},
		`{"value":"case","count":18446744073709551615}`: {Value: "case", Count: ^uint64(0)},
		`{"pair":[1,2]}`:           {Pair: [2]int{1, 2}},
		`{"v\u0061lue":"escaped"}`: {Value: "escaped"},
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := strictio.DecodeJSON[document]([]byte(raw), "test")
			if err != nil || got != want {
				t.Fatalf("got %#v, %v; want %#v", got, err, want)
			}
		})
	}
	for _, raw := range []string{
		`{"VALUE":"case"}`, `{"value":"first","value":"last"}`,
		`{"value":"first","v\u0061lue":"last"}`,
		`{"pair":[1]}`, `{"pair":[1,2,3]}`,
		"{\"value\":\"\xff\"}", `{"unknown":1}`, `{"count":1.5}`,
		`{"count":18446744073709551616}`, `{"value":"ok"} {}`, `{"value":`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := strictio.DecodeJSON[document]([]byte(raw), "test")
			if err == nil || !strings.Contains(err.Error(), "decode test:") {
				t.Fatalf("expected labeled decoding error, got %v", err)
			}
		})
	}
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteJSONPropagatesWriterError(t *testing.T) {
	fault := errors.New("output failed")
	if err := strictio.WriteJSON(failedWriter{fault}, struct{ Value int }{1}); !errors.Is(err, fault) {
		t.Fatalf("lost output error: %v", err)
	}
}
