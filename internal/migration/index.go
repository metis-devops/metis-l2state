package migration

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
)

// preimageKeyPrefix is the legacy database namespace for Keccak preimages.
const preimageKeyPrefix = "secure-key-"

var ovmAddress = common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")
var ovmHash = crypto.Keccak256Hash(ovmAddress[:])

type indexes struct {
	// addresses resolves hashed account trie keys to known address preimages.
	addresses map[common.Hash]common.Address
	// balances maps hashed OVM_ETH balance storage keys to their owners, not values.
	balances map[common.Hash]common.Address
	// allowances contains already-hashed storage keys from validated event relationships.
	allowances map[common.Hash]struct{}
	// members contains nonzero Transfer senders; contract members retain ERC20 balances.
	members map[common.Address]struct{}
	// metadata records index provenance and export inputs for the statefile header.
	metadata statefile.Metadata
	// validation binds completed source validation to its source, head and export policy.
	validation *sourceValidation
}

func newIndexes() *indexes {
	return &indexes{addresses: make(map[common.Hash]common.Address), balances: make(map[common.Hash]common.Address), allowances: make(map[common.Hash]struct{}), members: make(map[common.Address]struct{})}
}
func balanceKey(a common.Address) common.Hash {
	return crypto.Keccak256Hash(crypto.Keccak256(common.LeftPadBytes(a[:], 32), make([]byte, 32)))
}
func allowanceKey(owner, spender common.Address) common.Hash {
	return ovmevents.AllowanceKey(owner, spender)
}
func (x *indexes) addAddress(a common.Address) error {
	return x.addKnownAddress(crypto.Keccak256Hash(a[:]), a)
}

// h must already be verified as keccak(a). Successful insertion establishes
// both maps; repeated relationships need not derive the balance key again.
func (x *indexes) addKnownAddress(h common.Hash, a common.Address) error {
	if old, ok := x.addresses[h]; ok {
		if old != a {
			return errors.New("ambiguous account preimage")
		}
		return nil
	}
	b := balanceKey(a)
	if old, ok := x.balances[b]; ok && old != a {
		return errors.New("ambiguous balance ownership")
	}
	x.addresses[h] = a
	x.balances[b] = a
	return nil
}
func (s *source) loadIndexes(ctx context.Context, l limiter, events *ovmevents.Validated) (*indexes, error) {
	genesis, progress, digest, err := events.Identity()
	if err != nil {
		return nil, err
	}
	if progress.BlockNumber != s.head.BlockNumber || progress.BlockHash != s.head.BlockHash {
		return nil, errors.New("events progress does not match LastBlock")
	}
	x := newIndexes()
	x.metadata = statefile.Metadata{IndexVersion: 1, Genesis: genesis, IndexHeadNumber: s.head.BlockNumber, IndexHeadHash: s.head.BlockHash, IndexDigest: digest}
	if err := s.confirmGenesis(genesis); err != nil {
		return nil, err
	}
	if err := events.Visit(ctx, x.addAllowance, x.addMember); err != nil {
		return nil, err
	}
	if err := s.indexRecords(ctx, l, []byte(preimageKeyPrefix), x.preimage); err != nil {
		return nil, err
	}
	if err := x.addAddress(ovmAddress); err != nil {
		return nil, err
	}
	return x, nil
}

func (s *source) indexRecords(ctx context.Context, l limiter, prefix []byte, visit func([]byte, []byte) error) error {
	it := s.db.NewIterator(prefix, nil)
	defer it.Release()
	for {
		var next bool
		err := l.run(ctx, func() error {
			next = it.Next()
			if !next {
				return it.Error()
			}
			return visit(it.Key(), it.Value())
		})
		if err != nil {
			return fmt.Errorf("node index %q: %w", prefix, err)
		}
		if !next {
			break
		}
	}

	return it.Error()
}
func preimageHash(k, v []byte) (common.Hash, error) {
	if len(k) != len(preimageKeyPrefix)+common.HashLength {
		return common.Hash{}, errors.New("invalid preimage key")
	}
	h := crypto.Keccak256Hash(v)
	if h != common.BytesToHash(k[len(preimageKeyPrefix):]) {
		return common.Hash{}, errors.New("invalid preimage hash")
	}
	return h, nil
}
func (x *indexes) preimage(k, v []byte) error {
	h, err := preimageHash(k, v)
	if err != nil {
		return err
	}
	if len(v) == 20 {
		return x.addKnownAddress(h, common.Address(v))
	}
	return nil
}

// addAllowance consumes only records delivered by Validated.Visit.
func (x *indexes) addAllowance(a ovmevents.Allowance) error {
	x.allowances[allowanceKey(a.Owner, a.Spender)] = struct{}{}
	if err := x.addAddress(a.Owner); err != nil {
		return err
	}
	if err := x.addAddress(a.Spender); err != nil {
		return err
	}
	x.metadata.AllowanceCount++
	return nil
}

// addMember consumes only records delivered by Validated.Visit.
func (x *indexes) addMember(address common.Address) error {
	x.members[address] = struct{}{}
	if err := x.addAddress(address); err != nil {
		return err
	}
	x.metadata.MemberCount++
	return nil
}
