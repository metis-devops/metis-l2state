package strictio_test

import (
	"bytes"
	jsonv1 "encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/metis-devops/metis-l2state/internal/migration"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

// These options are intentional wire behavior, not just equivalent JSON values:
// tools consume the output and callers may use paths with HTML/JS characters.
func TestWriteJSONMatchesExistingBytes(t *testing.T) {
	path := "out<&>\u2028\u2029/\xff"
	for name, value := range map[string]any{
		"migration": migration.Result{Artifact: path},
		"sync":      ovmevents.Result{Output: path, Progress: ovmevents.Position{BlockNumber: ^uint64(0)}},
		"maps": map[string]any{
			"z": []int(nil), "a": []int{}, "emptyMap": map[string]int{},
			"nilMap": map[string]int(nil), "path": path,
			"code": hexutil.Bytes{0x60, 0xff}, "bytes": [2]byte{1, 2},
		},
		"hashKeys": map[common.Hash]common.Hash{
			common.HexToHash("0xff"): common.HexToHash("0x02"),
			common.HexToHash("0x01"): common.HexToHash("0x03"),
		},
		"omitted": struct {
			Flag  bool   `json:"flag,omitempty"`
			Count uint64 `json:"count,omitempty"`
			Data  []byte `json:"data"`
		}{},
	} {
		t.Run(name, func(t *testing.T) {
			var want, got bytes.Buffer
			if err := jsonv1.NewEncoder(&want).Encode(value); err != nil {
				t.Fatal(err)
			}
			if err := strictio.WriteJSON(&got, value); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("output changed:\ngot  %q\nwant %q", got.Bytes(), want.Bytes())
			}
		})
	}
}

func TestDecodeJSONPreservesTypedSemantics(t *testing.T) {
	type document struct {
		Value string `json:"value"`
		Count uint64 `json:"count"`
		Pair  [2]int `json:"pair"`
	}
	for _, raw := range []string{
		`{}`, `null`, `{"VALUE":"case","count":18446744073709551615}`,
		`{"value":"first","value":"last"}`, `{"value":null}`,
		`{"pair":[1]}`, `{"pair":[1,2,3]}`, `{"v\u0061lue":"escaped"}`,
		"{\"value\":\"\xff\"}", `{"unknown":1}`, `{"count":1.5}`,
		`{"count":18446744073709551616}`, `{"value":"ok"} {}`, `{"value":`,
	} {
		t.Run(raw, func(t *testing.T) {
			var want document
			reference := jsonv1.NewDecoder(strings.NewReader(raw))
			reference.DisallowUnknownFields()
			wantErr := reference.Decode(&want)
			if wantErr == nil {
				var trailing any
				if err := reference.Decode(&trailing); !errors.Is(err, io.EOF) {
					wantErr = errors.New("trailing data")
				}
			}
			got, gotErr := strictio.DecodeJSON[document]([]byte(raw), "test")
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("changed acceptance: got %v, want %v", gotErr, wantErr)
			}
			if gotErr == nil && !reflect.DeepEqual(got, want) {
				t.Fatalf("changed decoded value: got %#v, want %#v", got, want)
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
