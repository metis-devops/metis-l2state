// Package statefile implements the versioned, streaming legacy state file format.
// Header.Root describes the SOURCE state. Summary.FinalStateRoot describes the
// emitted state. Storage keys are already hashed; never hash them a second time.
package statefile

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Format constants define v1 encoding and bounded record sizes.
const (
	Version         = 1
	CompressionZstd = "zstd"
	CompressionNone = "none"
	MaxCodeSize     = 128 << 20
	MaxMetadataSize = 1 << 20
)

// StorageKeyHash is keccak256 of the original EVM slot, not an EVM slot itself.
type StorageKeyHash common.Hash

// Account contains scalar state; storage follows as separate streaming records.
type Account struct {
	Address  common.Address
	CodeHash common.Hash
	Balance  *big.Int
	Nonce    uint64
}

// Storage is one nonzero word keyed by its already-hashed slot.
type Storage struct {
	Key   StorageKeyHash
	Value common.Hash
}

// InputDigest commits to exact operator input bytes without recording paths.
type InputDigest struct {
	Name   string
	SHA256 common.Hash
}

// Metadata is deterministic provenance, not an independent historical proof.
// IndexVersion 0 denotes an original-state dump with no index or input fields;
// IndexVersion 1 denotes OVM migration provenance.
type Metadata struct {
	ToolVersion     string
	IndexVersion    uint64
	Genesis         common.Hash
	IndexHeadNumber uint64
	IndexHeadHash   common.Hash
	IndexDigest     common.Hash
	AllowanceCount  uint64
	MemberCount     uint64
	Inputs          []InputDigest
}

// Counts is the inventory of emitted accounts, slots and unique code.
type Counts struct {
	Accounts     uint64
	StorageSlots uint64
	Codes        uint64
	CodeBytes    uint64
}

// Conversion records amounts before alloc. Decimal strings are canonical.
// For dumps, amounts are "0" and all other fields are zero (no conversion).
type Conversion struct {
	OriginalSupply     string
	BalanceSum         string
	MigratedNative     string
	RemainingSupply    string
	SelfBalance        string
	EOAs               uint64
	ConvertedContracts uint64
	RetainedContracts  uint64
	AuditHeight        uint64
	AuditHash          common.Hash
}

// Summary separates original, converted, and post-alloc roots.
// Dumps require all roots and both inventories to be identical.
type Summary struct {
	SourceRoot         common.Hash
	ConvertedStateRoot common.Hash
	FinalStateRoot     common.Hash
	Counts             Counts
	ConvertedCounts    Counts
	Conversion         Conversion
}

// Kind identifies an ordered file record.
type Kind byte

// Format constants define v1 encoding and bounded record sizes.
const (
	AccountStart Kind = iota + 1
	StorageEntry
	AccountEnd
	CodeEntry
	Footer
)

// Record owns its data. Code is delivered after all accounts; it is not
// necessarily available when an AccountStart is returned.
type Record struct {
	Kind         Kind
	Account      Account
	Storage      Storage
	StorageRoot  common.Hash
	StorageCount uint64
	CodeHash     common.Hash
	Code         []byte
	Summary      Summary
}

// FileHeader preserves exact source RLP alongside its parsed representation.
type FileHeader struct {
	HeaderRLP   []byte
	Header      *types.Header
	Metadata    Metadata
	Compression string
}
