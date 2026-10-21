package ethjson

import (
	"encoding/json/v2"
	"io"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type logFields types.Log

// DecodeLog reads one historical log, requiring even zero-valued positions and
// removed:false. Provider extensions are validated but do not affect the log.
func DecodeLog(r io.Reader, log *types.Log) error {
	fields := objectFields{
		"address": true, "topics": true, "data": true, "blockNumber": true,
		"blockHash": true, "transactionHash": true, "transactionIndex": true,
		"logIndex": true, "removed": true, "blockTimestamp": false,
	}
	unmarshalers := json.JoinUnmarshalers(fields.unmarshalers(), hexUnmarshalers)
	decoded := struct {
		Log       logFields                 `json:",embed"`
		Timestamp *hexutil.Uint64           `json:"blockTimestamp"`
		Extra     map[string]discardedField `json:",embed"`
	}{Log: logFields(*log)}
	if err := json.UnmarshalRead(r, &decoded, json.WithUnmarshalers(unmarshalers)); err != nil {
		return err
	}
	if err := fields.check(); err != nil {
		return err
	}
	if decoded.Timestamp != nil {
		decoded.Log.BlockTimestamp = uint64(*decoded.Timestamp)
	}
	*log = types.Log(decoded.Log)
	return nil
}
