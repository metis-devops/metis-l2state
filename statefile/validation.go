package statefile

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/storageword"
)

type validator struct {
	accounts                            *trie.StackTrie
	storage                             *trie.StackTrie
	current                             *Account
	lastAccount, lastSlot               common.Hash
	haveAccount, haveSlot, codesStarted bool
	slots                               uint64
	counts                              Counts
	refs                                map[common.Hash]bool
}

func newValidator() *validator {
	return &validator{accounts: trie.NewStackTrie(nil), refs: make(map[common.Hash]bool)}
}

func (v *validator) accept(r Record) error {
	switch r.Kind {
	case AccountStart:
		return v.start(r)
	case StorageEntry:
		return v.slot(r.Storage)
	case AccountEnd:
		return v.end(r)
	case CodeEntry:
		return v.code(r)
	default:
		return errors.New("invalid state record")
	}
}
func (v *validator) start(r Record) error {
	if v.current != nil || v.codesStarted {
		return errors.New("account out of sequence")
	}
	a := r.Account
	if a.Balance == nil || a.Balance.Sign() < 0 || a.Balance.BitLen() > 256 || a.CodeHash == (common.Hash{}) {
		return errors.New("invalid account fields")
	}
	h := crypto.Keccak256Hash(a.Address[:])
	if v.haveAccount && bytes.Compare(h[:], v.lastAccount[:]) <= 0 {
		return errors.New("duplicate or unordered account")
	}
	v.lastAccount, v.haveAccount = h, true
	a.Balance = new(big.Int).Set(a.Balance)
	v.current, v.storage, v.haveSlot, v.slots = &a, trie.NewStackTrie(nil), false, 0
	if a.CodeHash != types.EmptyCodeHash {
		v.refs[a.CodeHash] = false
	}
	v.counts.Accounts++
	return nil
}

func (v *validator) slot(s Storage) error {
	if v.current == nil {
		return errors.New("storage outside account")
	}
	h := common.Hash(s.Key)
	if s.Value == (common.Hash{}) || (v.haveSlot && bytes.Compare(h[:], v.lastSlot[:]) <= 0) {
		return errors.New("zero, duplicate or unordered storage")
	}
	v.lastSlot, v.haveSlot = h, true
	if err := storageword.Update(v.storage, h, s.Value); err != nil {
		return err
	}
	v.slots++
	v.counts.StorageSlots++
	return nil
}

func (v *validator) end(r Record) error {
	if v.current == nil {
		return errors.New("account end without start")
	}
	root := v.storage.Hash()
	if root != r.StorageRoot || v.slots != r.StorageCount {
		return errors.New("storage root or count mismatch")
	}
	balance, overflow := uint256.FromBig(v.current.Balance)
	if overflow {
		return errors.New("balance overflow")
	}
	blob, err := rlp.EncodeToBytes(&types.StateAccount{Nonce: v.current.Nonce, Balance: balance, Root: root, CodeHash: v.current.CodeHash.Bytes()})
	if err != nil {
		return err
	}
	if err := v.accounts.Update(v.lastAccount[:], blob); err != nil {
		return err
	}
	v.current = nil
	v.storage = nil
	return nil
}

func (v *validator) code(r Record) error {
	if v.current != nil {
		return errors.New("code inside account")
	}
	v.codesStarted = true
	if len(r.Code) == 0 || len(r.Code) > MaxCodeSize || crypto.Keccak256Hash(r.Code) != r.CodeHash {
		return errors.New("invalid code hash or length")
	}
	present, referenced := v.refs[r.CodeHash]
	if !referenced {
		return errors.New("unreferenced code")
	}
	if present {
		return errors.New("duplicate code")
	}
	v.refs[r.CodeHash] = true
	v.counts.Codes++
	v.counts.CodeBytes += uint64(len(r.Code))
	return nil
}

func (v *validator) finish(s Summary, source common.Hash) error {
	if v.current != nil {
		return errors.New("unfinished account")
	}
	for h, present := range v.refs {
		if !present {
			return fmt.Errorf("missing code %s", h)
		}
	}
	if s.SourceRoot != source || s.FinalStateRoot != v.accounts.Hash() || s.Counts != v.counts || s.ConvertedStateRoot == (common.Hash{}) {
		return errors.New("summary root or inventory mismatch")
	}
	return validateConversion(s.Conversion)
}

func validateConversion(c Conversion) error {
	values := []string{c.OriginalSupply, c.BalanceSum, c.MigratedNative, c.RemainingSupply, c.SelfBalance}
	n := make([]*big.Int, len(values))
	for i, s := range values {
		// A uint256 decimal has at most 78 digits. Bound work before parsing
		// untrusted footer strings, not after allocating a huge big.Int.
		if len(s) == 0 || len(s) > 78 {
			return errors.New("invalid conversion amount")
		}
		v, ok := new(big.Int).SetString(s, 10)
		if !ok || v.Sign() < 0 || v.BitLen() > 256 || v.String() != s {
			return errors.New("invalid conversion amount")
		}
		n[i] = v
	}
	if new(big.Int).Add(n[2], n[3]).Cmp(n[1]) != 0 || n[4].Cmp(n[3]) > 0 {
		return errors.New("conversion conservation mismatch")
	}
	if c.AuditHash == (common.Hash{}) {
		if c.AuditHeight != 0 || n[0].Cmp(n[1]) != 0 {
			return errors.New("unaudited supply mismatch")
		}
	}
	return nil
}

// IndexVersion zero is an unmodified dump, not conversion provenance.
func validateDumpSummary(s Summary) error {
	zero := Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"}
	if s.ConvertedStateRoot != s.SourceRoot || s.FinalStateRoot != s.SourceRoot || s.ConvertedCounts != s.Counts || s.Conversion != zero {
		return errors.New("invalid dump summary")
	}
	return nil
}
