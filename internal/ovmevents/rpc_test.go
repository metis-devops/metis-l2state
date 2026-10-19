package ovmevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestRPCLogRejectsAmbiguousFields(t *testing.T) {
	b := fakeChain()
	l := b.event(0, 0, TransferTopic, common.Address{}, common.Address{}, 1)
	raw, err := json.Marshal(l)
	check(t, err)
	var fields map[string]json.RawMessage
	check(t, json.Unmarshal(raw, &fields))
	for key, value := range fields {
		quoted := fmt.Sprintf("%q", key)
		alias := fmt.Sprintf("%q", strings.ToUpper(key))
		escaped := fmt.Sprintf(`"\u%04x%s"`, key[0], key[1:])
		for name, input := range map[string]string{
			"duplicate":         `{` + quoted + `:` + string(value) + `,` + string(raw[1:]),
			"escaped-duplicate": `{` + escaped + `:` + string(value) + `,` + string(raw[1:]),
			"alias-before":      `{` + alias + `:` + string(value) + `,` + string(raw[1:]),
			"alias-after":       string(raw[:len(raw)-1]) + `,` + alias + `:` + string(value) + `}`,
			"alias-only":        strings.Replace(string(raw), quoted+":", alias+":", 1),
		} {
			t.Run(key+"/"+name, func(t *testing.T) {
				var got rpcLog
				if err := json.Unmarshal([]byte(input), &got); err == nil {
					t.Fatal("accepted ambiguous RPC log field")
				}
			})
		}
	}
	// encoding/json folds these Unicode characters to ASCII K and S as well.
	for _, alias := range []string{"blocKHash", "blockHaſh"} {
		input := string(raw[:len(raw)-1]) + fmt.Sprintf(",%q:%q}", alias, common.HexToHash("0x99"))
		var got rpcLog
		if err := json.Unmarshal([]byte(input), &got); err == nil {
			t.Fatalf("accepted Unicode case alias %q", alias)
		}
	}
	// Unique provider extensions remain compatible and do not affect log fields.
	input := string(raw[:len(raw)-1]) + `,"providerExtension":{"removed":true}}`
	var got rpcLog
	check(t, json.Unmarshal([]byte(input), &got))
	if got.Removed || got.BlockHash != l.BlockHash {
		t.Fatal("provider extension affected log identity")
	}
}

func TestAmbiguousRPCLogsPreserveCheckpoint(t *testing.T) {
	for _, conflicting := range []string{`"removed":true`, `"Removed":true`, `"remov\u0065d":true`} {
		t.Run(conflicting, func(t *testing.T) {
			b := fakeChain()
			o := syncOptions(t, 1)
			d := New(b.id, b.headers[0].Hash())
			d.Progress = Position{0, b.headers[0].Hash()}
			check(t, Save(o.Output, d))
			before, err := os.ReadFile(o.Output)
			check(t, err)
			l := b.event(1, 0, TransferTopic, common.HexToAddress("0x01"), common.Address{}, 1)
			raw, err := json.Marshal(l)
			check(t, err)
			// The canonical removed:false would overwrite the conflicting value.
			response := json.RawMessage(`[{` + conflicting + `,` + string(raw[1:]) + `]`)
			var calls atomic.Int32
			r := rpcFixture(t, func(method string, params json.RawMessage) any {
				switch method {
				case "eth_chainId":
					return "0x440"
				case "eth_getBlockByNumber":
					var args []json.RawMessage
					check(t, json.Unmarshal(params, &args))
					if string(args[0]) == `"0x0"` {
						return b.headers[0]
					}
					return b.headers[1]
				case "eth_getLogs":
					calls.Add(1)
					return response
				default:
					t.Error("unexpected RPC method", method)
					return nil
				}
			})
			_, err = synchronize(t.Context(), o, r, Save)
			if err == nil || temporary(err) || calls.Load() != 1 {
				t.Fatalf("ambiguous log must fail permanently: calls=%d err=%v", calls.Load(), err)
			}
			after, err := os.ReadFile(o.Output)
			check(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("ambiguous RPC log advanced or changed checkpoint")
			}
		})
	}
}

func rpcFixture(t *testing.T, respond func(string, json.RawMessage) any) *rpcBackend {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": respond(req.Method, req.Params)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := rpc.DialContext(t.Context(), server.URL)
	check(t, err)
	t.Cleanup(client.Close)
	return &rpcBackend{client: client, timeout: time.Second}
}
func TestRPCValidation(t *testing.T) {
	b := fakeChain()
	h := b.headers[0]
	l := b.event(0, 0, ApprovalTopic, common.Address{}, common.Address{}, 0)
	for _, name := range []string{"valid", "null-header", "wrong-hash", "wrong-height", "null-logs", "missing-position", "null-position", "malformed-logs"} {
		t.Run(name, func(t *testing.T) {
			r := rpcFixture(t, func(method string, params json.RawMessage) any {
				switch method {
				case "eth_chainId":
					return "0x440"
				case "eth_getBlockByNumber":
					if name == "null-header" {
						return nil
					}
					if name == "wrong-height" {
						return b.headers[1]
					}
					raw, err := json.Marshal(h)
					check(t, err)
					var obj map[string]any
					check(t, json.Unmarshal(raw, &obj))
					if name == "wrong-hash" {
						obj["hash"] = common.Hash{}
					}
					return obj
				case "eth_getLogs":
					var filter []struct {
						From    string          `json:"fromBlock"`
						To      string          `json:"toBlock"`
						Address common.Address  `json:"address"`
						Topics  [][]common.Hash `json:"topics"`
					}
					check(t, json.Unmarshal(params, &filter))
					if len(filter) != 1 || filter[0].From != "0x0" || filter[0].To != "0x0" || filter[0].Address != Contract || len(filter[0].Topics[0]) != 2 {
						t.Error("incorrect RPC filter")
					}
					if name == "null-logs" {
						return nil
					}
					if name == "malformed-logs" {
						return "invalid"
					}
					raw, err := json.Marshal(l)
					check(t, err)
					var obj map[string]any
					check(t, json.Unmarshal(raw, &obj))
					if name == "missing-position" {
						delete(obj, "logIndex")
					}
					if name == "null-position" {
						obj["blockNumber"] = nil
					}
					return []any{obj}
				}
				t.Error("unknown method")
				return nil
			})
			id, err := r.chainID(t.Context())
			check(t, err)
			if id != "1088" {
				t.Fatal(id)
			}
			_, err = r.header(t.Context(), "0x0")
			badHeader := name == "null-header" || name == "wrong-hash" || name == "wrong-height"
			if (err != nil) != badHeader {
				t.Fatal(name, err)
			}
			_, err = r.logs(t.Context(), 0, 0)
			badLogs := strings.Contains(name, "logs") || strings.Contains(name, "position")
			if (err != nil) != badLogs {
				t.Fatal(name, err)
			}
		})
	}
}
func TestRPCFailureClassification(t *testing.T) {
	for _, e := range []error{context.DeadlineExceeded, rpc.HTTPError{StatusCode: 429}, rpc.HTTPError{StatusCode: 503}, rpcFault{-32000, "rate limit exceeded"}} {
		if !temporary(e) {
			t.Fatal("not retried", e)
		}
	}
	for _, e := range []error{context.Canceled, rpc.HTTPError{StatusCode: 401}, rpcFault{-32601, "method not found"}, errors.New("invalid header")} {
		if temporary(e) {
			t.Fatal("retried permanent error", e)
		}
	}
	if !rangeLimited(rpcFault{-32005, "limit"}) || !rangeLimited(errors.New("query returned more than 10000 results")) {
		t.Fatal("range limit not recognized")
	}
}
func TestPendingProgressFlushesDuringRetry(t *testing.T) {
	b := fakeChain()
	o := syncOptions(t, 2)
	o.BatchSize = 1
	o.FlushInterval = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.onLogs = func(from, to uint64) error {
		if from == 1 {
			cancel()
			return context.DeadlineExceeded
		}
		return nil
	}
	_, err := synchronize(ctx, o, b, Save)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	d, _, err := ReadFile(t.Context(), o.Output)
	check(t, err)
	if d.Progress.BlockNumber != 0 {
		t.Fatal("lost complete prefix")
	}
}
