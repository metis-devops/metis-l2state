package ethjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Reuse geth's RPC quantity and byte encodings on the current decoder. These
// callbacks are stateless; field validation stays local to each object decode.
var hexUnmarshalers = json.JoinUnmarshalers(
	json.UnmarshalFromFunc(func(d *jsontext.Decoder, value *big.Int) error {
		return json.UnmarshalDecode(d, (*hexutil.Big)(value))
	}),
	json.UnmarshalFromFunc(func(d *jsontext.Decoder, value *uint64) error {
		return json.UnmarshalDecode(d, (*hexutil.Uint64)(value))
	}),
	json.UnmarshalFromFunc(func(d *jsontext.Decoder, value *uint) error {
		return json.UnmarshalDecode(d, (*hexutil.Uint)(value))
	}),
	json.UnmarshalFromFunc(func(d *jsontext.Decoder, value *[]byte) error {
		return json.UnmarshalDecode(d, (*hexutil.Bytes)(value))
	}),
)
