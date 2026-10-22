package ovmevents

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/metis-devops/metis-l2state/internal/ethjson"
)

type backend interface {
	chainID(context.Context) (string, error)
	header(context.Context, string) (*types.Header, error)
	logs(context.Context, uint64, uint64) ([]types.Log, error)
}
type rpcBackend struct {
	client  *rpc.Client
	timeout time.Duration
}

func (r *rpcBackend) call(ctx context.Context, result any, method string, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.client.CallContext(ctx, result, method, args...); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}
func (r *rpcBackend) chainID(ctx context.Context) (string, error) {
	var id hexutil.Big
	if err := r.call(ctx, &id, "eth_chainId"); err != nil {
		return "", err
	}
	return id.ToInt().String(), nil
}
func (r *rpcBackend) header(ctx context.Context, tag string) (*types.Header, error) {
	var raw jsontext.Value
	if err := r.call(ctx, &raw, "eth_getBlockByNumber", tag, false); err != nil {
		return nil, err
	}
	h, err := ethjson.DecodeHeader(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("invalid RPC header: %w", err)
	}
	if h.Root == (common.Hash{}) {
		return nil, errors.New("missing or invalid RPC header")
	}
	if tag != "latest" && hexutil.EncodeUint64(h.Number.Uint64()) != tag {
		return nil, errors.New("RPC header height mismatch")
	}
	return h, nil
}
func (r *rpcBackend) logs(ctx context.Context, from, to uint64) ([]types.Log, error) {
	var records []rpcLog
	err := r.call(ctx, &records, "eth_getLogs", map[string]any{"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to), "address": Contract, "topics": [][]common.Hash{{ApprovalTopic, TransferTopic}}})
	if err == nil && records == nil {
		return nil, errors.New("null RPC logs response")
	}
	logs := make([]types.Log, len(records))
	for i := range records {
		logs[i] = types.Log(records[i])
	}
	return logs, err
}
func rangeLimited(err error) bool {
	if re, ok := errors.AsType[rpc.Error](err); ok && re.ErrorCode() == -32005 {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "too many results") || strings.Contains(s, "block range") || strings.Contains(s, "response size exceeded") || strings.Contains(s, "query returned more than")
}
func temporary(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if he, ok := errors.AsType[rpc.HTTPError](err); ok {
		return he.StatusCode == 429 || he.StatusCode == 502 || he.StatusCode == 503 || he.StatusCode == 504
	}
	if re, ok := errors.AsType[rpc.Error](err); ok {
		return re.ErrorCode() == -32016 || strings.Contains(strings.ToLower(re.Error()), "rate limit")
	}
	return false
}

// Historical positions must be present even when their valid value is zero.
type rpcLog types.Log

func (l *rpcLog) UnmarshalJSON(raw []byte) error {
	return ethjson.DecodeLog(bytes.NewReader(raw), (*types.Log)(l))
}
