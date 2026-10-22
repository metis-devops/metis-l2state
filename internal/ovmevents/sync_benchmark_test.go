package ovmevents

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// This harness is also compiled unchanged against the pre-change production code.
// HTTP requests are local, without injected delay; this is not a public RPC benchmark.
func BenchmarkSyncBoundary(b *testing.B) {
	for _, density := range []string{"empty", "sparse", "dense"} {
		b.Run(density, func(b *testing.B) {
			const blocks = 2000
			chain := &fakeBackend{id: "1088", head: blocks - 1}
			expected := New("1088", common.Hash{})
			for i := range blocks {
				h := &types.Header{Number: big.NewInt(int64(i)), Difficulty: big.NewInt(1), Root: common.HexToHash("0x01"), Time: uint64(i)}
				if i > 0 {
					h.ParentHash = chain.headers[i-1].Hash()
				}
				chain.headers = append(chain.headers, h)
				if density == "dense" || (density == "sparse" && i%200 == 0) {
					address := common.BigToAddress(big.NewInt(int64(i + 1)))
					chain.events = append(chain.events, chain.event(uint64(i), 0, ApprovalTopic, address, common.Address{}, 0), chain.event(uint64(i), 1, TransferTopic, address, common.Address{}, 1))
					expected.Allowances = append(expected.Allowances, Allowance{address, common.Address{}})
					expected.Members = append(expected.Members, address)
				}
			}
			expected.Genesis = chain.headers[0].Hash()
			expected.Progress = Position{blocks - 1, chain.headers[blocks-1].Hash()}
			var headerCalls, logCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage   `json:"id"`
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					b.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				var result any
				switch req.Method {
				case "eth_chainId":
					result = "0x440"
				case "eth_getBlockByNumber":
					headerCalls.Add(1)
					var tag string
					if err := json.Unmarshal(req.Params[0], &tag); err != nil {
						b.Error(err)
						return
					}
					var err error
					result, err = chain.header(r.Context(), tag)
					if err != nil {
						b.Error(err)
						return
					}
				case "eth_getLogs":
					logCalls.Add(1)
					var filter struct {
						From string `json:"fromBlock"`
						To   string `json:"toBlock"`
					}
					if err := json.Unmarshal(req.Params[0], &filter); err != nil {
						b.Error(err)
						return
					}
					from, e1 := hexutil.DecodeUint64(filter.From)
					to, e2 := hexutil.DecodeUint64(filter.To)
					if e1 != nil || e2 != nil {
						b.Error("bad range")
						return
					}
					logs := []types.Log{}
					for _, l := range chain.events {
						if l.BlockNumber >= from && l.BlockNumber <= to {
							logs = append(logs, l)
						}
					}
					result = logs
				default:
					b.Error("unexpected method", req.Method)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
					b.Error(err)
				}
			}))
			defer server.Close()
			client, err := rpc.DialContext(b.Context(), server.URL)
			if err != nil {
				b.Fatal(err)
			}
			defer client.Close()
			backend := &rpcBackend{client: client, timeout: time.Second * 30}
			directory := b.TempDir()
			target := uint64(blocks - 1)
			outputs := make([]string, b.N)
			for i := range outputs {
				outputs[i] = filepath.Join(directory, fmt.Sprintf("%d.json", i))
			}
			b.ResetTimer()
			for _, output := range outputs {
				_, err := synchronize(b.Context(), Options{Output: output, ToBlock: &target, BatchSize: 1000, PollInterval: time.Millisecond, FlushInterval: time.Hour}, backend, Save)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(headerCalls.Load())/float64(b.N), "headers/op")
			b.ReportMetric(float64(logCalls.Load())/float64(b.N), "logs/op")
			var want bytes.Buffer
			if err := Encode(&want, expected); err != nil {
				b.Fatal(err)
			}
			for _, output := range outputs {
				raw, err := os.ReadFile(output)
				if err != nil {
					b.Fatal(err)
				}
				if !bytes.Equal(raw, want.Bytes()) {
					b.Fatal("checkpoint differs from independently constructed expected records")
				}
				if _, _, err := ReadFile(b.Context(), output); err != nil {
					b.Fatal(err)
				}
			}
			b.Logf("CHECKPOINT sha256=%x blocks=%d allowances=%d members=%d", sha256.Sum256(want.Bytes()), blocks, len(expected.Allowances), len(expected.Members))
		})
	}
}
