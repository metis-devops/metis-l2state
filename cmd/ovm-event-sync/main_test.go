package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestCLI(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}, {"--help"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if args[0] == "version" && !jsontext.Value(out.Bytes()).IsValid() {
			t.Fatal("invalid result")
		}
	}
	for _, args := range [][]string{{"bad"}, {"version", "bad"}, {"--rpc-url", "x"}, {"--to-block", "-1"}, {"--rpc-url", "x", "--out", "x", "--batch-size", "0"}, {"--rpc-url", "x", "--out", "x", "--poll-interval", "0s"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, &out, &stderr); err == nil {
			t.Fatal("accepted", args)
		}
		if out.Len() != 0 {
			t.Fatal("error leaked to stdout")
		}
	}
}

func TestSyncProgressAndQuiet(t *testing.T) {
	header := &types.Header{Number: big.NewInt(0), Difficulty: big.NewInt(1), Root: common.HexToHash("0x01")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
		}
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "eth_chainId":
			result = "0x440"
		case "eth_getBlockByNumber":
			result = header
		case "eth_getLogs":
			result = []any{}
		default:
			t.Error("unexpected method", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	for _, quiet := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"--rpc-url", server.URL, "--out", filepath.Join(t.TempDir(), "events.json"), "--to-block", "0"}
		if quiet {
			args = append(args, "--quiet")
		}
		if err := run(t.Context(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if !jsontext.Value(out.Bytes()).IsValid() {
			t.Fatal("progress contaminated stdout:", out.String())
		}
		if quiet {
			if stderr.Len() != 0 {
				t.Fatal("quiet mode emitted progress:", stderr.String())
			}
		} else if !strings.Contains(stderr.String(), "Starting OVM event sync") || !strings.Contains(stderr.String(), "Persisted OVM events") {
			t.Fatal("missing progress:", stderr.String())
		}
	}
}

func TestStartupProgressBeforeConnectionTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // Never complete the WebSocket handshake.
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := run(ctx, []string{"--rpc-url", "ws" + strings.TrimPrefix(server.URL, "http"), "--out", filepath.Join(t.TempDir(), "events.json"), "--rpc-timeout", "20ms"}, &out, &stderr)
	var timeout net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
	if !timedOut || ctx.Err() != nil {
		t.Fatal("connection was not bounded by rpc-timeout:", err)
	}
	if out.Len() != 0 || !strings.Contains(stderr.String(), "Connecting to RPC") {
		t.Fatal("startup was silent or contaminated stdout:", out.String(), stderr.String())
	}
}
