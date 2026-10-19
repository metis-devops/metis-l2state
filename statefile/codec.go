package statefile

import (
	"encoding/binary"
	"errors"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

var magic = [8]byte{'L', '2', 'O', 'V', 'M', 'S', 'T', Version}

type headerWire struct {
	HeaderRLP []byte
	Metadata  Metadata
}

func validateHeader(raw []byte, m Metadata) (*types.Header, error) {
	var h types.Header
	if err := rlp.DecodeBytes(raw, &h); err != nil {
		return nil, err
	}
	if h.Number == nil || !h.Number.IsUint64() || h.Root == (common.Hash{}) || h.Difficulty == nil || h.Difficulty.Sign() < 0 {
		return nil, errors.New("invalid source header")
	}
	if m.ToolVersion == "" || m.Genesis == (common.Hash{}) {
		return nil, errors.New("invalid source provenance")
	}
	switch m.IndexVersion {
	case 0:
		if m.IndexHeadNumber != 0 || m.IndexHeadHash != (common.Hash{}) || m.IndexDigest != (common.Hash{}) || m.AllowanceCount != 0 || m.MemberCount != 0 || len(m.Inputs) != 0 {
			return nil, errors.New("invalid dump provenance")
		}
	case 1:
		if m.IndexDigest == (common.Hash{}) || m.IndexHeadNumber != h.Number.Uint64() || m.IndexHeadHash != crypto.Keccak256Hash(raw) {
			return nil, errors.New("invalid index provenance")
		}
	default:
		return nil, errors.New("invalid index provenance")
	}
	last := ""
	for _, d := range m.Inputs {
		if d.Name <= last || d.SHA256 == (common.Hash{}) {
			return nil, errors.New("invalid input provenance")
		}
		last = d.Name
	}
	return &h, nil
}

// Fixed records fit in scratch; code payloads remain separately owned.
const maxFixedPayload = 95

func appendCompact(p, value []byte) []byte {
	for len(value) > 0 && value[0] == 0 {
		value = value[1:]
	}
	p = append(p, byte(len(value)))
	return append(p, value...)
}

func appendCompactUint64(p []byte, value uint64) []byte {
	var word [8]byte
	binary.BigEndian.PutUint64(word[:], value)
	return appendCompact(p, word[:])
}

// takeCompact consumes exactly one canonical length-prefixed integer.
func takeCompact(p *[]byte, limit int) ([]byte, error) {
	if len(*p) == 0 {
		return nil, errors.New("missing integer length")
	}
	n := int((*p)[0])
	if n > limit || len(*p) < 1+n {
		return nil, errors.New("invalid integer length")
	}
	value := (*p)[1 : 1+n]
	if n > 0 && value[0] == 0 {
		return nil, errors.New("noncanonical integer")
	}
	*p = (*p)[1+n:]
	return value, nil
}

func takeCompactUint64(p *[]byte) (uint64, error) {
	value, err := takeCompact(p, 8)
	var n uint64
	for _, b := range value {
		n = n<<8 | uint64(b)
	}
	return n, err
}

func encodeRecord(r Record, scratch *[maxFixedPayload]byte) ([]byte, error) {
	p := scratch[:0]
	switch r.Kind {
	case AccountStart:
		a := r.Account
		if a.Balance == nil || a.Balance.Sign() < 0 || a.Balance.BitLen() > 256 || a.CodeHash == (common.Hash{}) {
			return nil, errors.New("invalid account fields")
		}
		p = append(p, a.Address[:]...)
		flags := byte(0)
		if a.CodeHash != types.EmptyCodeHash {
			flags = 1
		}
		p = append(p, flags)
		var balance [32]byte
		a.Balance.FillBytes(balance[:])
		p = appendCompact(p, balance[:])
		p = appendCompactUint64(p, a.Nonce)
		if flags != 0 {
			p = append(p, a.CodeHash[:]...)
		}
		return p, nil
	case StorageEntry:
		if r.Storage.Value == (common.Hash{}) {
			return nil, errors.New("zero storage value")
		}
		p = append(p, r.Storage.Key[:]...)
		return appendCompact(p, r.Storage.Value[:]), nil
	case AccountEnd:
		p = appendCompactUint64(p, r.StorageCount)
		if r.StorageCount == 0 {
			if r.StorageRoot != types.EmptyRootHash {
				return nil, errors.New("invalid empty storage root")
			}
		} else {
			p = append(p, r.StorageRoot[:]...)
		}
		return p, nil
	case CodeEntry:
		if len(r.Code) == 0 || len(r.Code) > MaxCodeSize {
			return nil, errors.New("invalid code length")
		}
		return append(append([]byte(nil), r.CodeHash[:]...), r.Code...), nil
	default:
		return nil, errors.New("unsupported record")
	}
}

func payloadLimit(k Kind) uint32 {
	switch k {
	case AccountStart:
		return maxFixedPayload
	case StorageEntry:
		return 65
	case AccountEnd:
		return 41
	case CodeEntry:
		return MaxCodeSize + 32
	case Footer:
		return MaxMetadataSize + 32
	}
	return 0
}

func decodeAccount(p []byte) (Account, error) {
	var a Account
	if len(p) < 23 || p[20] & ^byte(1) != 0 {
		return a, errors.New("invalid account length or flags")
	}
	copy(a.Address[:], p[:20])
	flags := p[20]
	p = p[21:]
	balance, err := takeCompact(&p, 32)
	if err != nil {
		return a, err
	}
	a.Balance = new(big.Int).SetBytes(balance)
	a.Nonce, err = takeCompactUint64(&p)
	if err != nil {
		return a, err
	}
	a.CodeHash = types.EmptyCodeHash
	if flags != 0 {
		if len(p) != 32 {
			return a, errors.New("invalid code hash length")
		}
		copy(a.CodeHash[:], p)
		if a.CodeHash == types.EmptyCodeHash || a.CodeHash == (common.Hash{}) {
			return a, errors.New("invalid explicit code hash")
		}
		p = p[32:]
	}
	if len(p) != 0 {
		return a, errors.New("trailing account bytes")
	}
	return a, nil
}

func decodeRecord(k Kind, p []byte) (Record, error) {
	r := Record{Kind: k}
	if len(p) == 0 || uint64(len(p)) > uint64(payloadLimit(k)) {
		return r, errors.New("invalid record length or type")
	}
	switch k {
	case AccountStart:
		a, err := decodeAccount(p)
		r.Account = a
		return r, err
	case StorageEntry:
		if len(p) < 34 {
			return r, errors.New("invalid storage length")
		}
		copy(r.Storage.Key[:], p[:32])
		p = p[32:]
		value, err := takeCompact(&p, 32)
		if err != nil {
			return r, err
		}
		if len(value) == 0 || len(p) != 0 {
			return r, errors.New("zero or trailing storage value")
		}
		copy(r.Storage.Value[32-len(value):], value)
	case AccountEnd:
		count, err := takeCompactUint64(&p)
		if err != nil {
			return r, err
		}
		r.StorageCount = count
		r.StorageRoot = types.EmptyRootHash
		if count != 0 {
			if len(p) != 32 {
				return r, errors.New("invalid storage root length")
			}
			copy(r.StorageRoot[:], p)
			p = p[32:]
		}
		if len(p) != 0 {
			return r, errors.New("trailing account end bytes")
		}
	case CodeEntry:
		if len(p) <= 32 {
			return r, errors.New("invalid code length")
		}
		copy(r.CodeHash[:], p[:32])
		r.Code = p[32:]
	default:
		return r, errors.New("unsupported record")
	}
	return r, nil
}

// readRecordHeader retains the exact wire bytes for the stream digest. Read at
// most five length bytes, rejecting non-minimal and overflowing uint32 LEB128.
func readRecordHeader(in io.Reader, scratch *[6]byte) (Kind, uint32, []byte, error) {
	if _, err := io.ReadFull(in, scratch[:1]); err != nil {
		return 0, 0, nil, err
	}
	var length uint32
	for i := range 5 {
		if _, err := io.ReadFull(in, scratch[1+i:2+i]); err != nil {
			return 0, 0, nil, err
		}
		b := scratch[1+i]
		if i == 4 && b > 15 {
			return 0, 0, nil, errors.New("record length overflow")
		}
		length |= uint32(b&0x7f) << (7 * i)
		if b < 0x80 {
			if i > 0 && b == 0 {
				return 0, 0, nil, errors.New("noncanonical record length")
			}
			return Kind(scratch[0]), length, scratch[:2+i], nil
		}
	}
	return 0, 0, nil, errors.New("record length overflow")
}
