package ethjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Keep geth's fields and tags without invoking its whole-object JSON method.
type headerFields types.Header

// Only these fixture fields permit decimal JSON numbers. For all other values,
// leave the decoder untouched and fall through to the shared RPC hex decoding.
var headerDecimalUnmarshaler = json.UnmarshalFromFunc(func(d *jsontext.Decoder, value any) error {
	if d.PeekKind() == jsontext.KindNumber {
		switch d.StackPointer() {
		case "/number", "/difficulty", "/gasLimit", "/gasUsed", "/timestamp":
			return json.UnmarshalDecode(d, value, json.WithUnmarshalers(nil))
		}
	}
	return errors.ErrUnsupported
})

// DecodeHeader reads one header, checking required fields, quantity ranges and
// the declared hash. decimalQuantities also accepts the legacy fixture's numeric
// number, difficulty, gasLimit, gasUsed and timestamp fields.
func DecodeHeader(r io.Reader, decimalQuantities bool) (*types.Header, error) {
	fields := objectFields{
		"parentHash": true, "sha3Uncles": true, "stateRoot": true, "transactionsRoot": true,
		"receiptsRoot": true, "logsBloom": true, "difficulty": true, "number": true,
		"gasLimit": true, "gasUsed": true, "timestamp": true, "extraData": true, "hash": true,
		"miner": false, "mixHash": false, "nonce": false, "baseFeePerGas": false,
		"withdrawalsRoot": false, "blobGasUsed": false, "excessBlobGas": false,
		"parentBeaconBlockRoot": false, "requestsHash": false, "blockAccessListHash": false, "slotNumber": false,
	}
	var decimal *json.Unmarshalers
	if decimalQuantities {
		decimal = headerDecimalUnmarshaler
	}
	unmarshalers := json.JoinUnmarshalers(fields.unmarshalers(), decimal, hexUnmarshalers)
	var decoded struct {
		Header headerFields `json:",embed"`
		// Match geth's optional-field null handling for these value fields.
		Miner *common.Address           `json:"miner"`
		Mix   *common.Hash              `json:"mixHash"`
		Nonce *types.BlockNonce         `json:"nonce"`
		Hash  common.Hash               `json:"hash"`
		Extra map[string]discardedField `json:",embed"`
	}
	if err := json.UnmarshalRead(r, &decoded, json.WithUnmarshalers(unmarshalers)); err != nil {
		return nil, err
	}
	if err := fields.check(); err != nil {
		return nil, err
	}
	h := types.Header(decoded.Header)
	if decoded.Miner != nil {
		h.Coinbase = *decoded.Miner
	}
	if decoded.Mix != nil {
		h.MixDigest = *decoded.Mix
	}
	if decoded.Nonce != nil {
		h.Nonce = *decoded.Nonce
	}
	if h.Number == nil || !h.Number.IsUint64() {
		return nil, errors.New("header number outside uint64")
	}
	if h.Difficulty == nil || h.Difficulty.Sign() < 0 || h.Difficulty.BitLen() > 256 {
		return nil, errors.New("header difficulty outside uint256")
	}
	if h.Hash() != decoded.Hash {
		return nil, errors.New("block hash mismatch")
	}
	return &h, nil
}
