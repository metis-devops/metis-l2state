package migration

import (
	"bytes"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/syndtr/goleveldb/leveldb"
)

// Trap every old namespace, including identity markers and iterator scans.
type noEventIndexDB struct{ ethdb.Database }

func oldIndex(key []byte) bool {
	return bytes.HasPrefix(key, []byte("ovm-transfer-v1/")) || bytes.HasPrefix(key, []byte("ovm-allowance-v1-"))
}
func (d noEventIndexDB) Get(key []byte) ([]byte, error) {
	if oldIndex(key) {
		return nil, errors.New("forbidden node event index read")
	}
	return d.Database.Get(key)
}
func (d noEventIndexDB) Has(key []byte) (bool, error) {
	if oldIndex(key) {
		return false, errors.New("forbidden node event index lookup")
	}
	return d.Database.Has(key)
}
func (d noEventIndexDB) NewIterator(prefix, start []byte) ethdb.Iterator {
	if oldIndex(prefix) || len(prefix) == 0 {
		panic("forbidden node index iteration")
	}
	return d.Database.NewIterator(prefix, start)
}
func TestEventsReplaceNodeIndex(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	s.db = noEventIndexDB{s.db}
	x, err := s.prepare(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	if x.metadata.AllowanceCount != 1 || x.metadata.MemberCount != 1 {
		t.Fatal("events not loaded")
	}
}
func TestEventBindingFailsClosed(t *testing.T) {
	for _, name := range []string{"missing", "genesis", "behind", "ahead", "hash", "allowance", "member"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 0, 0)
			d := f.events(t)
			switch name {
			case "missing":
				must(t, os.Remove(f.eventPath()))
			case "genesis":
				d.Genesis = common.HexToHash("0x99")
			case "behind":
				d.Progress = ovmevents.Position{BlockNumber: 0, BlockHash: d.Genesis}
			case "ahead":
				d.Progress.BlockNumber = 2
			case "hash":
				d.Progress.BlockHash = common.HexToHash("0x99")
			case "allowance":
				d.Allowances = append(d.Allowances, d.Allowances[0])
			case "member":
				d.Members = append(d.Members, d.Members[0])
			}
			if name == "genesis" || name == "behind" || name == "ahead" || name == "hash" {
				// These documents are well-formed; rejection must come from source binding.
				must(t, d.Validate())
			}
			if name != "missing" {
				// Deliberately bypass the validated writer to test hostile operator input.
				raw, err := json.Marshal(d)
				must(t, err)
				must(t, os.WriteFile(f.eventPath(), raw, 0600))
			}
			out := filepath.Join(t.TempDir(), "state")
			if _, err := Migrate(t.Context(), f.options(out)); err == nil {
				t.Fatal("accepted bad events")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published bad input")
			}
		})
	}
}

func TestMemberSetRetainsEligibilityAtLaterCheckpoint(t *testing.T) {
	f := newFixture(t, 0, 0)
	d := f.events(t)
	// Membership remains valid at a later checkpoint without per-member positions.
	// The checkpoint must still bind exactly to the new canonical LastBlock.
	h := types.CopyHeader(f.header)
	h.Number = big.NewInt(2)
	h.ParentHash = f.header.Hash()
	h.Time++
	hash := h.Hash()
	raw, err := rlp.EncodeToBytes(h)
	must(t, err)
	mutate(t, f, func(db *leveldb.DB) {
		must(t, db.Put(numberKey(2, hash[:]), raw, nil))
		must(t, db.Put(numberKey(2, []byte{'n'}), hash[:], nil))
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], 2)
		must(t, db.Put(append([]byte{'H'}, hash[:]...), n[:], nil))
		must(t, db.Put([]byte("LastBlock"), hash[:], nil))
	})
	d.Progress = ovmevents.Position{BlockNumber: 2, BlockHash: hash}
	d.Members = append(d.Members, ordinary)
	d.Sort()
	o := f.options(filepath.Join(t.TempDir(), "state"))
	must(t, ovmevents.Save(f.eventPath(), d))
	_, err = Migrate(t.Context(), o)
	must(t, err)
	accounts, _, _ := readArtifact(t, o.Output)
	if accounts[ordinary].Balance.Sign() != 0 {
		t.Fatal("Transfer-from eligibility was lost at a later checkpoint")
	}
}

func TestMigrationRejectsMemberObjects(t *testing.T) {
	f := newFixture(t, 0, 0)
	d := f.events(t)
	raw, err := json.Marshal(map[string]any{
		"version": d.Version, "chainId": d.ChainID, "genesis": d.Genesis,
		"progress": d.Progress, "allowances": d.Allowances,
		"members": []any{map[string]any{"address": automatic, "blockNumber": 1, "blockHash": f.header.Hash()}},
	})
	must(t, err)
	must(t, os.WriteFile(f.eventPath(), raw, 0600))
	dir := t.TempDir()
	if _, err := Migrate(t.Context(), f.options(filepath.Join(dir, "state"))); err == nil {
		t.Fatal("accepted legacy member object")
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("rejected member object left an artifact or partial")
	}
}
