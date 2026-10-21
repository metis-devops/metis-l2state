// Package ovmevents synchronizes and validates RPC-derived OVM event indexes.
package ovmevents

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Contract and topics identify the only supported legacy token.
var (
	Contract      = common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")
	TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	ApprovalTopic = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
)

// Position is the end of a fully processed canonical prefix.
type Position struct {
	BlockNumber uint64      `json:"blockNumber"`
	BlockHash   common.Hash `json:"blockHash"`
}

// Allowance identifies a nested mapping relationship, not its current value.
type Allowance struct {
	Owner   common.Address
	Spender common.Address
}

// MarshalJSONTo streams the fixed [owner, spender] checkpoint tuple.
func (a Allowance) MarshalJSONTo(e *jsontext.Encoder) error {
	return json.MarshalEncode(e, [2]common.Address{a.Owner, a.Spender})
}

// Document is a complete prefix, including empty blocks, starting at genesis.
type Document struct {
	Version  uint64      `json:"version"`
	ChainID  string      `json:"chainId"`
	Genesis  common.Hash `json:"genesis"`
	Progress Position    `json:"progress"`
	// Allowances and Members retain first occurrence in canonical event order.
	Allowances []Allowance `json:"allowances"`
	// Members contains unique senders of nonzero Transfers in the prefix.
	Members []common.Address `json:"members"`
}

// New constructs an empty index; progress must be set after processing genesis.
func New(chainID string, genesis common.Hash) *Document {
	return &Document{Version: 1, ChainID: chainID, Genesis: genesis, Allowances: []Allowance{}, Members: []common.Address{}}
}

// AllowanceKey returns the already-hashed secure trie key at mapping slot 1.
func AllowanceKey(owner, spender common.Address) common.Hash {
	base := make([]byte, 32)
	base[31] = 1
	inner := crypto.Keccak256(common.LeftPadBytes(owner[:], 32), base)
	return crypto.Keccak256Hash(crypto.Keccak256(common.LeftPadBytes(spender[:], 32), inner))
}

// Validate rejects inconsistent or duplicate index evidence without reordering it.
func (d *Document) Validate() error { return d.ValidateContext(context.Background()) }

// ValidateContext checks mutable checkpoint evidence with cancellable scans.
func (d *Document) ValidateContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil {
		return errors.New("missing OVM events")
	}
	id, ok := new(big.Int).SetString(d.ChainID, 10)
	if !ok || id.Sign() < 0 || id.BitLen() > 256 || id.String() != d.ChainID || d.Version != 1 || d.Genesis == (common.Hash{}) {
		return errors.New("invalid OVM event identity")
	}
	if d.Progress.BlockHash == (common.Hash{}) || (d.Progress.BlockNumber == 0 && d.Progress.BlockHash != d.Genesis) {
		return errors.New("invalid OVM event progress")
	}
	allowances := make(map[common.Hash]struct{}, len(d.Allowances))
	for _, a := range d.Allowances {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := AllowanceKey(a.Owner, a.Spender)
		if _, exists := allowances[key]; exists {
			return errors.New("duplicate or conflicting allowance key")
		}
		allowances[key] = struct{}{}
	}
	members := make(map[common.Address]struct{}, len(d.Members))
	for _, address := range d.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := members[address]; exists {
			return errors.New("duplicate member")
		}
		members[address] = struct{}{}
	}
	return nil
}

// ReadFile parses and hashes the original bytes once, using bounded JSON tokens.
func ReadFile(ctx context.Context, path string) (d *Document, digest common.Hash, retErr error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, digest, err
	}
	if !before.Mode().IsRegular() {
		return nil, digest, errors.New("events input must be a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	after, err := f.Stat()
	if err != nil {
		return nil, digest, err
	}
	if !os.SameFile(before, after) {
		return nil, digest, errors.New("events input changed while opening")
	}
	h := sha256.New()
	d, err = Decode(ctx, io.TeeReader(f, h))
	if err != nil {
		return nil, digest, err
	}
	return d, common.BytesToHash(h.Sum(nil)), nil
}

// Encode preserves record order without buffering the full file.
func Encode(w io.Writer, d *Document) error {
	if err := d.Validate(); err != nil {
		return err
	}
	return encodeValidated(w, d)
}

// Caller owns d and has validated it since its last mutation.
func encodeValidated(w io.Writer, d *Document) error {
	// Framing is written directly to w between encoded values. Hide the
	// bytes.Buffer fast path, which assumes exclusive access to its storage.
	e := jsontext.NewEncoder(struct{ io.Writer }{w})
	fields := []struct {
		name  string
		value any
	}{
		{"version", d.Version}, {"chainId", d.ChainID}, {"genesis", d.Genesis}, {"progress", d.Progress},
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	for i, f := range fields {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%q:", f.name); err != nil {
			return err
		}
		if err := json.MarshalEncode(e, f.value); err != nil {
			return err
		}
	}
	if err := encodeArray(w, e, "allowances", d.Allowances); err != nil {
		return err
	}
	if err := encodeArray(w, e, "members", d.Members); err != nil {
		return err
	}
	_, err := io.WriteString(w, "}\n")
	return err
}
func encodeArray[T any](w io.Writer, e *jsontext.Encoder, name string, values []T) error {
	if _, err := fmt.Fprintf(w, ",%q:[", name); err != nil {
		return err
	}
	for i, v := range values {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := json.MarshalEncode(e, v); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}
func hashRecord(h hash.Hash, key, value []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(key)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(key)
	binary.BigEndian.PutUint64(n[:], uint64(len(value)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(value)
}

// indexDigest binds identity and both record sequences in checkpoint order.
// JSON formatting is excluded; reordering either sequence changes the digest.
func (d *Document) indexDigest(ctx context.Context) (common.Hash, error) {
	if err := ctx.Err(); err != nil {
		return common.Hash{}, err
	}
	meta, _ := rlp.EncodeToBytes(struct {
		Version                      uint64
		Genesis                      common.Hash
		Contract                     common.Address
		AllowanceSlot                uint64
		TransferTopic, ApprovalTopic common.Hash
		FirstBlock                   uint64
	}{1, d.Genesis, Contract, 1, TransferTopic, ApprovalTopic, 0})
	h := sha256.New()
	hashRecord(h, []byte("ovm-transfer-v1/meta"), meta)
	position := func(n uint64, h common.Hash) []byte {
		v := make([]byte, 40)
		binary.BigEndian.PutUint64(v, n)
		copy(v[8:], h[:])
		return v
	}
	p := position(d.Progress.BlockNumber, d.Progress.BlockHash)
	hashRecord(h, []byte("ovm-transfer-v1/head"), p)
	hashRecord(h, []byte("ovm-transfer-v1/ready"), p)
	for _, a := range d.Allowances {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, err
		}
		key := AllowanceKey(a.Owner, a.Spender)
		hashRecord(h, append([]byte("ovm-allowance-v1-"), key[:]...), append(append([]byte{}, a.Owner[:]...), a.Spender[:]...))
	}
	for _, m := range d.Members {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, err
		}
		hashRecord(h, append([]byte("ovm-transfer-v1/member/"), m[:]...), nil)
	}
	return common.BytesToHash(h.Sum(nil)), ctx.Err()
}
