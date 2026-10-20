package ovmevents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestFrozenEvidenceOwnsRecords(t *testing.T) {
	d := sample()
	want := d.IndexDigest()
	genesis, progress := d.Genesis, d.Progress
	allowances := append([]Allowance(nil), d.Allowances...)
	members := append([]common.Address(nil), d.Members...)
	v := frozenFixture(t, d)
	d.Genesis = common.Hash{}
	d.Progress = Position{}
	for i := range d.Allowances {
		d.Allowances[i] = Allowance{}
	}
	for i := range d.Members {
		d.Members[i] = common.Address{}
	}
	g, p, h, err := v.Identity()
	check(t, err)
	if g != genesis || p != progress || h != want {
		t.Fatal("caller mutated validated identity")
	}
	a, m := 0, 0
	check(t, v.Visit(t.Context(), func(record Allowance) error {
		if record != allowances[a] {
			t.Fatal("caller mutated allowance")
		}
		a++
		return nil
	}, func(record common.Address) error {
		if record != members[m] {
			t.Fatal("caller mutated member")
		}
		m++
		return nil
	}))
	if a != len(allowances) || m != len(members) {
		t.Fatal("records lost")
	}
	if err := d.ValidateContext(t.Context()); err == nil {
		t.Fatal("invalid document received proof")
	}
	if _, _, _, err := new(Validated).Identity(); err == nil {
		t.Fatal("zero value authorized evidence")
	}
}

type cancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}
	return nil
}
func TestEvidenceScansHonorCancellation(t *testing.T) {
	d := sample()
	ctx := &cancelAfterChecks{Context: t.Context(), remaining: 1}
	if err := d.ValidateContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation ignored cancellation: %v", err)
	}
	ctx = &cancelAfterChecks{Context: t.Context(), remaining: 1}
	if _, err := d.indexDigest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("digest ignored cancellation: %v", err)
	}
	v := frozenFixture(t, d)
	ctx = &cancelAfterChecks{Context: t.Context(), remaining: 1}
	err := v.Visit(ctx, func(Allowance) error { return nil }, func(common.Address) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("visit ignored cancellation: %v", err)
	}
	sentinel := errors.New("consumer failed")
	err = v.Visit(t.Context(), func(Allowance) error { return sentinel }, func(common.Address) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

func frozenFixture(t *testing.T, d *Document) *Validated {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.json")
	check(t, Save(path, d))
	v, _, err := ReadValidatedFile(t.Context(), path)
	check(t, err)
	// Later file edits cannot change the input owned by the migration.
	check(t, os.WriteFile(path, []byte("corrupt"), 0600))
	return v
}

// IndexDigest hashes v1 key/value records in checkpoint order, without DB indexes.
func (d *Document) IndexDigest() common.Hash {
	h, _ := d.indexDigest(context.Background())
	return h
}
