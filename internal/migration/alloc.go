package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

const ovmAllocMaxCode = 1 << 20

type allocLoader struct {
	ctx     context.Context
	decoder *jsontext.Decoder
	patches map[common.Hash]*accountPatch
}

func readAlloc(ctx context.Context, path string) (patches map[common.Hash]*accountPatch, digest common.Hash, retErr error) {
	f, err := openInput(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	h := sha256.New()
	d := jsontext.NewDecoder(strictio.TokenReader(ctx, io.TeeReader(f, h), 6*(2*ovmAllocMaxCode+2)+2, 32<<10, "GenesisAlloc"))
	l := allocLoader{ctx: ctx, decoder: d, patches: make(map[common.Hash]*accountPatch)}
	if err := l.object(l.account); err != nil {
		return nil, digest, fmt.Errorf("alloc: %w", err)
	}
	if _, err := d.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, digest, errors.New("trailing alloc data")
	}
	return l.patches, common.BytesToHash(h.Sum(nil)), ctx.Err()
}
func (l *allocLoader) object(visit func(string) error) error {
	token, err := l.decoder.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindBeginObject {
		return errors.New("expected alloc object")
	}
	for l.decoder.PeekKind() != jsontext.KindEndObject {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		token, err = l.decoder.ReadToken()
		if err != nil {
			return err
		}
		if token.Kind() != jsontext.KindString {
			return errors.New("invalid alloc key")
		}
		if err := visit(token.String()); err != nil {
			return err
		}
	}
	token, err = l.decoder.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindEndObject {
		return errors.New("expected alloc closing brace")
	}
	return nil
}

// Reject null and containers before geth's JSON methods can buffer a whole
// object or array. Typed decoding handles scalars directly from the stream.
func (l *allocLoader) scalar(value any) error {
	switch l.decoder.PeekKind() {
	case jsontext.KindNull, jsontext.KindBeginObject, jsontext.KindBeginArray:
		return errors.New("expected non-null scalar")
	}
	return json.UnmarshalDecode(l.decoder, value)
}
func (l *allocLoader) account(key string) error {
	var addr common.UnprefixedAddress
	if err := addr.UnmarshalText([]byte(key)); err != nil {
		return err
	}
	a := common.Address(addr)
	if a == ovmAddress {
		return errors.New("alloc cannot modify OVM_ETH")
	}
	hash := crypto.Keccak256Hash(a[:])
	if _, ok := l.patches[hash]; ok {
		return errors.New("duplicate alloc address")
	}
	p := &accountPatch{Address: a, Storage: make(map[common.Hash]common.Hash)}
	l.patches[hash] = p
	seen := make(map[string]bool)
	return l.object(func(field string) error {
		if seen[field] {
			return errors.New("duplicate alloc field")
		}
		seen[field] = true
		return l.field(p, field)
	})
}
func (l *allocLoader) field(p *accountPatch, field string) error {
	if field == "storage" {
		return l.object(func(key string) error { return l.storage(p, key) })
	}
	if field != "balance" && field != "nonce" && field != "code" {
		return errors.New("unknown alloc field")
	}
	switch field {
	case "balance":
		var v gethmath.HexOrDecimal256
		if err := l.scalar(&v); err != nil {
			return err
		}
		b := (*big.Int)(&v)
		if b.Sign() < 0 || b.BitLen() > 256 {
			return errors.New("alloc balance outside uint256")
		}
		p.Balance, _ = uint256.FromBig(b)
	case "nonce":
		var v gethmath.HexOrDecimal64
		if err := l.scalar(&v); err != nil {
			return err
		}
		n := uint64(v)
		p.Nonce = &n
	case "code":
		var v hexutil.Bytes
		if err := l.scalar(&v); err != nil {
			return err
		}
		if len(v) > ovmAllocMaxCode {
			return errors.New("alloc runtime exceeds 1 MiB")
		}
		code := []byte(v)
		p.Code = &code
	}
	return nil
}
func allocWord(s string) (word common.Hash, err error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s) > 64 || len(s)%2 != 0 {
		return word, errors.New("invalid alloc storage word")
	}
	_, err = hex.Decode(word[32-len(s)/2:], []byte(s))
	return word, err
}
func (l *allocLoader) storage(p *accountPatch, key string) error {
	slot, err := allocWord(key)
	if err != nil {
		return err
	}
	hash := crypto.Keccak256Hash(slot[:])
	if _, ok := p.Storage[hash]; ok {
		return errors.New("duplicate alloc storage key")
	}
	var text string
	if err := l.scalar(&text); err != nil {
		return err
	}
	value, err := allocWord(text)
	if err != nil {
		return err
	}
	p.Storage[hash] = value
	return nil
}
