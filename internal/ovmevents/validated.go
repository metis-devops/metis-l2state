package ovmevents

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

// Validated owns immutable evidence. No method exposes its mutable document or
// slices: consumers receive scalar identities and record values only.
type Validated struct {
	document *Document
	digest   common.Hash
}

// ReadValidatedFile transfers a freshly decoded, validated document into an
// immutable owner, avoiding another validation scan or whole-document copy.
func ReadValidatedFile(ctx context.Context, path string) (*Validated, common.Hash, error) {
	d, rawHash, err := ReadFile(ctx, path)
	if err != nil {
		return nil, rawHash, err
	}
	v, err := ownValidated(ctx, d)
	return v, rawHash, err
}

func ownValidated(ctx context.Context, d *Document) (*Validated, error) {
	digest, err := d.indexDigest(ctx)
	if err != nil {
		return nil, err
	}
	return &Validated{document: d, digest: digest}, nil
}

// Identity returns the source binding and the precomputed normalized digest.
func (v *Validated) Identity() (common.Hash, Position, common.Hash, error) {
	if v == nil || v.document == nil {
		return common.Hash{}, Position{}, common.Hash{}, errors.New("OVM events are required")
	}
	return v.document.Genesis, v.document.Progress, v.digest, nil
}

// Visit streams copies of validated records, with cancellation between records.
func (v *Validated) Visit(ctx context.Context, allowance func(Allowance) error, member func(common.Address) error) error {
	if _, _, _, err := v.Identity(); err != nil {
		return err
	}
	for _, a := range v.document.Allowances {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := allowance(a); err != nil {
			return err
		}
	}
	for _, m := range v.document.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := member(m); err != nil {
			return err
		}
	}
	return ctx.Err()
}
