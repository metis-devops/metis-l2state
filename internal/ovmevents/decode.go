package ovmevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

// object enforces exact required keys, including duplicate and case checks.
func object(d *json.Decoder, fields map[string]func() error) error {
	if err := delim(d, '{'); err != nil {
		return err
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("expected object key")
		}
		read, ok := fields[key]
		if !ok || seen[key] {
			return fmt.Errorf("unknown or duplicate field %q", key)
		}
		seen[key] = true
		if err := read(); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if err := delim(d, '}'); err != nil {
		return err
	}
	if len(seen) != len(fields) {
		return errors.New("missing required fields")
	}
	return nil
}
func delim(d *json.Decoder, want json.Delim) error {
	got, err := d.Token()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("expected %q", want)
	}
	return nil
}
func scalar[T any](d *json.Decoder, v *T) func() error {
	return func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == nil {
			return errors.New("null is not allowed")
		}
		if _, ok := t.(json.Delim); ok {
			return errors.New("expected scalar")
		}
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
}
func position(d *json.Decoder, p *Position) error {
	return object(d, map[string]func() error{"blockNumber": scalar(d, &p.BlockNumber), "blockHash": scalar(d, &p.BlockHash)})
}
func allowance(d *json.Decoder, a *Allowance) error {
	return object(d, map[string]func() error{"owner": scalar(d, &a.Owner), "spender": scalar(d, &a.Spender)})
}
func member(d *json.Decoder, address *common.Address) error {
	return scalar(d, address)()
}
func array[T any](d *json.Decoder, values *[]T, read func(*json.Decoder, *T) error) error {
	if err := delim(d, '['); err != nil {
		return err
	}
	*values = []T{}
	for d.More() {
		var v T
		if err := read(d, &v); err != nil {
			return err
		}
		*values = append(*values, v)
	}
	return delim(d, ']')
}

// Decode streams records and rejects ambiguous JSON before typed decoding.
func Decode(ctx context.Context, r io.Reader) (*Document, error) {
	d := json.NewDecoder(strictio.TokenReader(ctx, r, 4096, 4096, "events"))
	d.UseNumber()
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
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.Join(errors.New("trailing events data"), err)
	}
	return v, v.ValidateContext(ctx)
}
