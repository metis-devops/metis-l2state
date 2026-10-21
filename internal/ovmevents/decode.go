package ovmevents

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

// object enforces exact required keys. jsontext rejects duplicate names.
func object(d *jsontext.Decoder, fields map[string]func() error) error {
	if err := delim(d, jsontext.KindBeginObject); err != nil {
		return err
	}
	count := 0
	for d.PeekKind() != jsontext.KindEndObject {
		token, err := d.ReadToken()
		if err != nil {
			return err
		}
		if token.Kind() != jsontext.KindString {
			return errors.New("expected object key")
		}
		key := token.String()
		read, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		count++
		if err := read(); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if err := delim(d, jsontext.KindEndObject); err != nil {
		return err
	}
	if count != len(fields) {
		return errors.New("missing required fields")
	}
	return nil
}
func delim(d *jsontext.Decoder, want jsontext.Kind) error {
	got, err := d.ReadToken()
	if err != nil {
		return err
	}
	if got.Kind() != want {
		return fmt.Errorf("expected %q", want)
	}
	return nil
}

// scalar rejects null and containers before typed decoding. In particular,
// geth's UnmarshalJSON must never buffer an arbitrary object or array here.
func scalar[T uint64 | string | common.Address | common.Hash](d *jsontext.Decoder, v *T) func() error {
	return func() error {
		switch d.PeekKind() {
		case jsontext.KindNull:
			return errors.New("null is not allowed")
		case jsontext.KindBeginObject, jsontext.KindBeginArray:
			return errors.New("expected scalar")
		}
		return json.UnmarshalDecode(d, v)
	}
}
func position(d *jsontext.Decoder, p *Position) error {
	return object(d, map[string]func() error{"blockNumber": scalar(d, &p.BlockNumber), "blockHash": scalar(d, &p.BlockHash)})
}
func allowance(d *jsontext.Decoder, a *Allowance) error {
	if err := delim(d, jsontext.KindBeginArray); err != nil {
		return err
	}
	if err := scalar(d, &a.Owner)(); err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	if err := scalar(d, &a.Spender)(); err != nil {
		return fmt.Errorf("spender: %w", err)
	}
	return delim(d, jsontext.KindEndArray)
}
func member(d *jsontext.Decoder, address *common.Address) error {
	return scalar(d, address)()
}
func array[T any](d *jsontext.Decoder, values *[]T, read func(*jsontext.Decoder, *T) error) error {
	if err := delim(d, jsontext.KindBeginArray); err != nil {
		return err
	}
	*values = []T{}
	for d.PeekKind() != jsontext.KindEndArray {
		var v T
		if err := read(d, &v); err != nil {
			return err
		}
		*values = append(*values, v)
	}
	return delim(d, jsontext.KindEndArray)
}

// Decode streams records and rejects ambiguous JSON before typed decoding.
func Decode(ctx context.Context, r io.Reader) (*Document, error) {
	d := jsontext.NewDecoder(strictio.TokenReader(ctx, r, 4096, 4096, "events"))
	v := new(Document)
	err := object(d, map[string]func() error{
		"version": scalar(d, &v.Version), "chainId": scalar(d, &v.ChainID), "genesis": scalar(d, &v.Genesis),
		"progress":   func() error { return position(d, &v.Progress) },
		"allowances": func() error { return array(d, &v.Allowances, allowance) },
		"members":    func() error { return array(d, &v.Members, member) },
	})
	if err != nil {
		return nil, err
	}
	if _, err := d.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.Join(errors.New("trailing events data"), err)
	}
	return v, v.ValidateContext(ctx)
}
