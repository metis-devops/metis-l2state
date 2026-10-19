package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

var eoa = common.HexToAddress("0x101")
var automatic = common.HexToAddress("0x102")
var ordinary = common.HexToAddress("0x103")
var manual = common.HexToAddress("0x104")

type fixture struct {
	source, runtime string
	header          *types.Header
	genesis         *types.Header
	ovmCode         common.Hash
	storage         map[common.Hash]common.Hash
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newFixture(t testing.TB, slots int, discrepancy uint64) fixture {
	t.Helper()
	return newFixtureWithStorage(t, slots, discrepancy, nil)
}
func newFixtureWithStorage(t testing.TB, slots int, discrepancy uint64, change func(map[common.Hash]common.Hash)) fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "source")
	db, err := leveldb.OpenFile(path, nil)
	must(t, err)
	put := func(k, v []byte) { must(t, db.Put(k, v, nil)) }
	sink := func(_ []byte, h common.Hash, b []byte) { put(h[:], b) }
	balances := map[common.Address]uint64{eoa: 11, automatic: 22, ordinary: 33, manual: 44, ovmAddress: 55}
	ovmStorage := make(map[common.Hash]common.Hash)
	for a, n := range balances {
		ovmStorage[balanceKey(a)] = common.BigToHash(new(big.Int).SetUint64(n))
	}
	supplySlot := common.HexToHash("0x02")
	ovmStorage[crypto.Keccak256Hash(supplySlot[:])] = common.BigToHash(new(big.Int).SetUint64(165 - discrepancy))
	ovmStorage[allowanceKey(eoa, automatic)] = common.HexToHash("0x07")
	if change != nil {
		change(ovmStorage)
	}
	accounts := make(map[common.Hash]*types.StateAccount)
	allStorage := make(map[common.Hash]common.Hash)
	for a := range balances {
		h := crypto.Keccak256Hash(a[:])
		put(append([]byte(preimageKeyPrefix), h[:]...), a[:])
		if a == eoa {
			continue
		}
		storage := map[common.Hash]common.Hash{}
		switch a {
		case ovmAddress:
			storage = ovmStorage
		case ordinary:
			for n := range slots {
				k := crypto.Keccak256Hash(new(big.Int).SetInt64(int64(n)).Bytes())
				v := common.BigToHash(new(big.Int).SetInt64(int64(n + 1)))
				storage[k] = v
				allStorage[k] = v
			}
		}
		stack := trie.NewStackTrie(sink)
		for _, k := range sortedHashes(storage) {
			must(t, stack.Update(k[:], slotRLP(storage[k])))
		}
		code := []byte{0x60, a[19], 0x00}
		codeHash := crypto.Keccak256Hash(code)
		put(codeHash[:], code)
		accounts[h] = &types.StateAccount{Balance: new(uint256.Int), Nonce: 1, Root: stack.Hash(), CodeHash: codeHash.Bytes()}
	}
	stack := trie.NewStackTrie(sink)
	for _, h := range sortedHashes(accounts) {
		b, err := rlp.EncodeToBytes(accounts[h])
		must(t, err)
		must(t, stack.Update(h[:], b))
	}
	root := stack.Hash()
	g := &types.Header{Number: new(big.Int), Difficulty: big.NewInt(1), Root: root, GasLimit: 10000000, UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	head := types.CopyHeader(g)
	head.Number = big.NewInt(1)
	head.ParentHash = g.Hash()
	head.Time = 1
	for _, header := range []*types.Header{g, head} {
		raw, err := rlp.EncodeToBytes(header)
		must(t, err)
		hash := header.Hash()
		put(numberKey(header.Number.Uint64(), hash[:]), raw)
		put(numberKey(header.Number.Uint64(), []byte{'n'}), hash[:])
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], header.Number.Uint64())
		put(append([]byte{'H'}, hash[:]...), n[:])
	}
	hash := head.Hash()
	put([]byte("LastBlock"), hash[:])
	put([]byte("LastHeader"), hash[:])
	must(t, db.Close())
	runtime := filepath.Join(dir, "runtime.hex")
	must(t, os.WriteFile(runtime, []byte("0x600100"), 0600))
	f := fixture{path, runtime, head, g, common.BytesToHash(accounts[ovmHash].CodeHash), allStorage}
	d := ovmevents.New("1088", g.Hash())
	d.Progress = ovmevents.Position{BlockNumber: 1, BlockHash: head.Hash()}
	d.Allowances = []ovmevents.Allowance{{Owner: eoa, Spender: automatic}}
	d.Members = []common.Address{automatic}
	must(t, ovmevents.Save(f.eventPath(), d))
	return f
}
func (f fixture) eventPath() string { return filepath.Join(filepath.Dir(f.source), "events.json") }
func (f fixture) events(t testing.TB) *ovmevents.Document {
	t.Helper()
	d, _, err := ovmevents.ReadFile(context.Background(), f.eventPath())
	must(t, err)
	return d
}
func (f fixture) options(out string) Options {
	return Options{OVMEvents: f.eventPath(), Source: f.source, Output: out, WrappedEtherCode: f.runtime, Workers: 2, CacheMB: 16, Handles: 16}
}
func readArtifact(t testing.TB, path string) (map[common.Address]statefile.Account, map[common.Address]map[common.Hash]common.Hash, statefile.Summary) {
	t.Helper()
	file, err := os.Open(path)
	must(t, err)
	defer func() { must(t, file.Close()) }()
	r, err := statefile.NewReader(context.Background(), file)
	must(t, err)
	defer r.Close()
	accounts := make(map[common.Address]statefile.Account)
	storage := make(map[common.Address]map[common.Hash]common.Hash)
	var current common.Address
	for {
		v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		switch v.Kind {
		case statefile.AccountStart:
			current = v.Account.Address
			accounts[current] = v.Account
			storage[current] = make(map[common.Hash]common.Hash)
		case statefile.StorageEntry:
			storage[current][common.Hash(v.Storage.Key)] = v.Storage.Value
		}
	}
	summary, err := r.Result()
	must(t, err)
	return accounts, storage, summary
}
func filesDigest(t testing.TB, path string) [32]byte {
	t.Helper()
	h := sha256.New()
	must(t, filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(filepath.Base(p)))
		_, _ = h.Write(b)
		return nil
	}))
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func TestMigrationRoundTrip(t *testing.T) {
	f := newFixture(t, 1025, 0)
	before := filesDigest(t, f.source)
	for _, compression := range []string{"none", "zstd"} {
		var first []byte
		for _, workers := range []int{2, 4, 16} {
			out := filepath.Join(t.TempDir(), "state.bin")
			o := f.options(out)
			o.Workers = workers
			o.Compression = compression
			result, err := Migrate(t.Context(), o)
			must(t, err)
			a, s, summary := readArtifact(t, out)
			if summary != result.Summary || summary.SourceRoot != f.header.Root || summary.FinalStateRoot == summary.SourceRoot {
				t.Fatal("root or result mismatch")
			}
			for address, want := range map[common.Address]int64{eoa: 11, automatic: 0, ordinary: 33, manual: 44, ovmAddress: 77} {
				if a[address].Balance.Cmp(big.NewInt(want)) != 0 {
					t.Fatalf("%s balance %s want %d", address, a[address].Balance, want)
				}
			}
			if s[ovmAddress][balanceKey(eoa)] != (common.Hash{}) || s[ovmAddress][balanceKey(automatic)] != common.HexToHash("0x16") || s[ovmAddress][allowanceKey(eoa, automatic)] != common.HexToHash("0x07") || len(s[ordinary]) != 1025 {
				t.Fatal("storage conversion mismatch")
			}
			if a[ovmAddress].CodeHash != crypto.Keccak256Hash([]byte{0x60, 1, 0}) {
				t.Fatal("runtime not replaced")
			}
			data, err := os.ReadFile(out)
			must(t, err)
			if first == nil {
				first = data
			} else if !bytes.Equal(first, data) {
				t.Fatal("workers changed bytes")
			}
		}
	}
	if filesDigest(t, f.source) != before {
		t.Fatal("source mutated")
	}
}

func TestMigrationReceiveOnlyEOAFromPreimage(t *testing.T) {
	// Keep the recipient's preimage and token balance, but remove its only
	// Approval relationship. It has no source account or Transfer-from record.
	f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
		delete(storage, allowanceKey(eoa, automatic))
	})
	events := f.events(t)
	events.Allowances = []ovmevents.Allowance{}
	must(t, ovmevents.Save(f.eventPath(), events))
	events = f.events(t)
	if len(events.Allowances) != 0 || slices.Contains(events.Members, eoa) {
		t.Fatal("receive-only EOA must not appear in event relationships")
	}
	before := filesDigest(t, f.source)
	func() {
		s, err := openSource(f.source, 16, 16)
		must(t, err)
		defer func() { must(t, s.close()) }()
		h := crypto.Keccak256Hash(eoa[:])
		preimage, err := s.db.Get(append([]byte(preimageKeyPrefix), h[:]...))
		must(t, err)
		if !bytes.Equal(preimage, eoa[:]) {
			t.Fatal("missing recipient address preimage")
		}
		accounts, err := trie.New(trie.StateTrieID(s.head.StateRoot), s.nodes)
		must(t, err)
		raw, err := accounts.Get(h[:])
		must(t, err)
		if len(raw) != 0 {
			t.Fatal("recipient must not exist in source account trie")
		}
	}()

	out := filepath.Join(t.TempDir(), "state.bin")
	_, err := Migrate(t.Context(), f.options(out))
	must(t, err)
	accounts, storage, _ := readArtifact(t, out)
	a, ok := accounts[eoa]
	if !ok {
		t.Fatal("receive-only EOA missing from statefile")
	}
	if a.Balance == nil || a.Balance.Cmp(big.NewInt(11)) != 0 {
		t.Fatalf("recipient balance %v, want 11", a.Balance)
	}
	if a.Nonce != 0 || a.CodeHash != types.EmptyCodeHash || len(storage[eoa]) != 0 {
		t.Fatalf("recipient must have zero nonce, no code and no storage: %+v, slots %d", a, len(storage[eoa]))
	}
	if _, ok := accounts[ovmAddress]; !ok {
		t.Fatal("OVM account missing from statefile")
	}
	if _, ok := storage[ovmAddress][balanceKey(eoa)]; ok {
		t.Fatal("recipient token balance slot was not removed")
	}
	if filesDigest(t, f.source) != before {
		t.Fatal("source mutated")
	}
}

func TestRetainAndAlloc(t *testing.T) {
	f := newFixture(t, 3, 0)
	dir := t.TempDir()
	retain := filepath.Join(dir, "retain")
	must(t, os.WriteFile(retain, []byte(manual.Hex()+"\r\n"), 0600))
	slot := common.HexToHash("0x99")
	alloc := filepath.Join(dir, "alloc.json")
	body := fmt.Sprintf(`{"%s":{"balance":"0x05","nonce":"0x0","code":"0x","storage":{"0x99":"0x23"}},"0x0000000000000000000000000000000000000099":{"balance":"0x0"},"0x0000000000000000000000000000000000000098":{}}`, eoa.Hex())
	must(t, os.WriteFile(alloc, []byte(body), 0600))
	out := filepath.Join(dir, "out")
	o := f.options(out)
	o.RetainList = retain
	o.Alloc = alloc
	_, err := Migrate(t.Context(), o)
	must(t, err)
	a, s, summary := readArtifact(t, out)
	if a[eoa].Balance.Uint64() != 5 || a[eoa].Nonce != 0 || a[eoa].CodeHash != types.EmptyCodeHash || a[manual].Balance.Sign() != 0 || a[ovmAddress].Balance.Uint64() != 121 || s[eoa][crypto.Keccak256Hash(slot[:])] != common.HexToHash("0x23") {
		t.Fatal("overlay semantics")
	}
	if _, ok := a[common.HexToAddress("0x99")]; !ok {
		t.Fatal("explicit zero account missing")
	}
	if _, ok := a[common.HexToAddress("0x98")]; ok {
		t.Fatal("empty alloc created account")
	}
	if summary.ConvertedStateRoot == summary.FinalStateRoot || summary.Conversion.MigratedNative != "44" {
		t.Fatal("conversion evidence")
	}
}

func mutate(t *testing.T, f fixture, change func(*leveldb.DB)) {
	t.Helper()
	db, err := leveldb.OpenFile(f.source, nil)
	must(t, err)
	change(db)
	must(t, db.Close())
}
func TestFailClosed(t *testing.T) {
	for _, name := range []string{"preimage", "supply", "audit-cold"} {
		t.Run(name, func(t *testing.T) {
			delta := uint64(0)
			if name == "supply" {
				delta = 1
			}
			f := newFixture(t, 0, delta)
			mutate(t, f, func(db *leveldb.DB) {
				switch name {
				case "preimage":
					h := crypto.Keccak256Hash(ordinary[:])
					must(t, db.Delete(append([]byte(preimageKeyPrefix), h[:]...), nil))

				}
			})
			out := filepath.Join(t.TempDir(), "out")
			o := f.options(out)
			if name == "audit-cold" {
				o.AuditSummary = writeAudit(t, f, 0)
				mutate(t, f, func(db *leveldb.DB) { hash := f.genesis.Hash(); must(t, db.Delete(numberKey(0, hash[:]), nil)) })
			}
			_, err := Migrate(t.Context(), o)
			if err == nil {
				t.Fatal("accepted invalid source")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published invalid output")
			}
			partials, err := filepath.Glob(filepath.Join(filepath.Dir(out), ".*.partial-*"))
			must(t, err)
			if len(partials) != 0 {
				t.Fatal("partial leaked")
			}
		})
	}
}

func writeAudit(t *testing.T, f fixture, delta uint64) string {
	t.Helper()
	supply := fmt.Sprint(165 - delta)
	a := ovmAuditSummary{Version: 1, Identity: ovmAuditIdentity{Version: 1, Genesis: f.genesis.Hash(), ConfigHash: common.HexToHash("0x01"), WitnessHash: common.HexToHash("0x02"), To: 1, ToHash: f.header.Hash()}, ClientVersion: "test", From: 1, VerifiedBlocks: 1, Genesis: ovmAuditScan{Root: f.genesis.Root, CodeHash: f.ovmCode, BalanceSlots: 5, Balances: supply, Supply: supply, Difference: "0"}, Target: ovmAuditScan{Root: f.header.Root, CodeHash: f.ovmCode, BalanceSlots: 5, Balances: "165", Supply: supply, Difference: fmt.Sprint(delta)}, Totals: ovmAuditDelta{Balance: fmt.Sprint(delta), Supply: "0", Difference: fmt.Sprint(delta), Gas: fmt.Sprint(delta), Suicide: "0", Unexplained: "0"}, ExecutionVerified: true, AccountingReconciled: true, IntervalCausesFullyExplained: true, GenesisDifferenceExplained: true}
	b, err := json.Marshal(a)
	must(t, err)
	p := filepath.Join(t.TempDir(), "audit.json")
	must(t, os.WriteFile(p, b, 0600))
	return p
}
func TestAudit(t *testing.T) {
	f := newFixture(t, 0, 1)
	for _, historical := range []bool{false, true} {
		out := filepath.Join(t.TempDir(), "state")
		o := f.options(out)
		o.AuditSummary = writeAudit(t, f, 1)
		if historical {
			mutate(t, f, func(db *leveldb.DB) {
				h := types.CopyHeader(f.header)
				h.Number = big.NewInt(2)
				h.ParentHash = f.header.Hash()
				h.Time++
				hash := h.Hash()
				raw, err := rlp.EncodeToBytes(h)
				must(t, err)
				must(t, db.Put(numberKey(2, hash[:]), raw, nil))
				must(t, db.Put(numberKey(2, []byte{'n'}), hash[:], nil))
				n := make([]byte, 8)
				binary.BigEndian.PutUint64(n, 2)
				must(t, db.Put(append([]byte{'H'}, hash[:]...), n, nil))
				must(t, db.Put([]byte("LastBlock"), hash[:], nil))
				events := f.events(t)
				events.Progress = ovmevents.Position{BlockNumber: 2, BlockHash: hash}
				must(t, ovmevents.Save(f.eventPath(), events))
			})
		}
		_, err := Migrate(t.Context(), o)
		must(t, err)
		a, _, s := readArtifact(t, out)
		if a[eoa].Balance.Uint64() != 11 || s.Conversion.BalanceSum != "165" || s.Conversion.OriginalSupply != "164" {
			t.Fatal("audit changed holders")
		}
	}
}
func TestCancelAndPublication(t *testing.T) {
	f := newFixture(t, 5000, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := filepath.Join(t.TempDir(), "out")
	_, err := Migrate(ctx, f.options(out))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	must(t, os.WriteFile(out, []byte("keep"), 0600))
	_, err = Migrate(t.Context(), f.options(out))
	if err == nil {
		t.Fatal("overwrote existing output")
	}
	b, err := os.ReadFile(out)
	must(t, err)
	if string(b) != "keep" {
		t.Fatal("existing output changed")
	}
	_, err = Migrate(t.Context(), f.options(filepath.Join(f.source, "output")))
	if err == nil {
		t.Fatal("accepted source alias")
	}
	p, err := newPublication(f.source, filepath.Join(t.TempDir(), "final"))
	must(t, err)
	_, err = p.file.Write([]byte("x"))
	must(t, err)
	must(t, p.close())
	err = p.publish(renameNoReplace, func(string) error { return errors.New("injected sync error") })
	if _, ok := errors.AsType[*PublicationDurabilityError](err); !ok {
		t.Fatalf("durability error %v", err)
	}
	must(t, p.abort())
	if _, err := os.Stat(p.final); err != nil {
		t.Fatal("deleted published file")
	}
}
func TestStorageThresholdAndPipelineCancellation(t *testing.T) {
	for _, slots := range []int{0, 1, 1024, 1025, 10000} {
		f := newFixture(t, slots, 0)
		s, err := openSource(f.source, 16, 16)
		must(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		l := make(limiter, 2)
		r := accountReader{s: s}
		h := crypto.Keccak256Hash(ordinary[:])
		a, err := r.get(h)
		must(t, err)
		var keys []common.Hash
		err = s.storage(ctx, h, a.Root, l, func(k, _ common.Hash) error { keys = append(keys, k); return nil })
		must(t, err)
		if len(keys) != slots || !slices.IsSortedFunc(keys, func(a, b common.Hash) int { return bytes.Compare(a[:], b[:]) }) {
			t.Fatal("storage ordering/threshold")
		}
		calls := 0
		err = s.storage(ctx, h, a.Root, l, func(_, _ common.Hash) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil
		})
		if slots > 1 && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %v", err)
		}
		cancel()
		must(t, s.close())
	}
}
