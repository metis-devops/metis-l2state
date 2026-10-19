package legacyintegration

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/common/hexutil"
	"github.com/MetisProtocol/mvm/l2geth/consensus/ethash"
	"github.com/MetisProtocol/mvm/l2geth/core"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/state"
	"github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/core/vm"
	"github.com/MetisProtocol/mvm/l2geth/params"
	"github.com/MetisProtocol/mvm/l2geth/rlp"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
	"github.com/MetisProtocol/mvm/l2geth/trie"
	"github.com/ethereum/go-ethereum/crypto"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var ovm = common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")
var holder = common.HexToAddress("0x101")
var retained = common.HexToAddress("0x102")
var converted = common.HexToAddress("0x103")

func balanceSlot(a common.Address) common.Hash {
	return hashOf(common.LeftPadBytes(a[:], 32), make([]byte, 32))
}
func allowanceSlot(a, b common.Address) common.Hash {
	slot := make([]byte, 32)
	slot[31] = 1
	return hashOf(common.LeftPadBytes(b[:], 32), crypto.Keccak256(common.LeftPadBytes(a[:], 32), slot))
}
func event(topic string, from, to common.Address, n byte) *types.Log {
	word := make([]byte, 32)
	word[31] = n
	return &types.Log{Address: ovm, Topics: []common.Hash{hashOf([]byte(topic)), common.BytesToHash(from[:]), common.BytesToHash(to[:])}, Data: word}
}

// Exercise the real BlockChain -> syncOVMIndex -> atomic index batch path.
// Receipt logs are synthetic; this checks index interoperability, not consensus execution.
func legacySource(t *testing.T, path string) (*types.Header, common.Hash) {
	t.Helper()
	db, err := rawdb.NewLevelDBDatabase(path, 16, 16, "integration")
	check(t, err)
	previous := rcfg.UsingOVM
	rcfg.UsingOVM = false
	defer func() { rcfg.UsingOVM = previous }()
	storage := map[common.Hash]common.Hash{balanceSlot(holder): common.HexToHash("0x0b"), balanceSlot(retained): common.HexToHash("0x16"), balanceSlot(converted): common.HexToHash("0x21"), common.HexToHash("0x02"): common.HexToHash("0x42"), allowanceSlot(holder, retained): common.HexToHash("0x07")}
	g := &core.Genesis{Config: params.AllEthashProtocolChanges, GasLimit: 10000000, Difficulty: big.NewInt(1), Alloc: core.GenesisAlloc{ovm: {Balance: new(big.Int), Code: []byte{0}, Storage: storage}, retained: {Balance: new(big.Int), Nonce: 1, Code: []byte{0}}, converted: {Balance: new(big.Int), Nonce: 1, Code: []byte{0}}}}
	extra, _ := strconv.Atoi(os.Getenv("L2STATE_BENCH_ACCOUNTS"))
	distinctCodes := 0
	if value := os.Getenv("L2STATE_BENCH_CODES"); value != "" {
		distinctCodes, err = strconv.Atoi(value)
		check(t, err)
		if distinctCodes < 0 || distinctCodes > extra {
			t.Fatal("benchmark codes must be between zero and extra accounts")
		}
	}
	codeBytes := 10
	if value := os.Getenv("L2STATE_BENCH_CODE_BYTES"); value != "" {
		codeBytes, err = strconv.Atoi(value)
		check(t, err)
		if codeBytes < 10 || codeBytes > 1<<20 {
			t.Fatal("benchmark code size must be 10 bytes through 1 MiB")
		}
	}
	storageContracts, storageSlots := benchmarkStorage(t, extra)
	for n := range extra {
		a := common.BigToAddress(big.NewInt(int64(0x1000 + n)))
		account := core.GenesisAccount{Balance: new(big.Int), Nonce: 1}
		if n < distinctCodes {
			// PUSH8 <unique counter>; STOP. Optional deterministic entropy after
			// STOP exercises bytecode retention without changing execution.
			account.Code = make([]byte, codeBytes)
			account.Code[0] = 0x67
			binary.BigEndian.PutUint64(account.Code[1:9], uint64(n))
			for offset := 10; offset < codeBytes; offset += 32 {
				var seed [16]byte
				binary.BigEndian.PutUint64(seed[:8], uint64(n))
				binary.BigEndian.PutUint64(seed[8:], uint64(offset))
				hash := sha256.Sum256(seed[:])
				copy(account.Code[offset:], hash[:])
			}
		}

		if n < storageContracts {
			if len(account.Code) == 0 {
				account.Code = []byte{0}
			}
			account.Storage = make(map[common.Hash]common.Hash)
			for slot := 0; slot < storageSlots; slot++ {
				account.Storage[common.BigToHash(big.NewInt(int64(slot)))] = common.BigToHash(big.NewInt(int64(slot + 1)))
			}
		}
		g.Alloc[a] = account
		storage[balanceSlot(a)] = common.HexToHash("0x01")
	}
	storage[common.HexToHash("0x02")] = common.BigToHash(big.NewInt(int64(66 + extra)))
	genesis := g.MustCommit(db)
	rcfg.UsingOVM = true
	bc, err := core.NewBlockChain(db, &core.CacheConfig{TrieCleanLimit: 16, TrieDirtyDisabled: true}, g.Config, ethash.NewFaker(), vm.Config{}, nil)
	check(t, err)
	st, err := bc.State()
	check(t, err)
	st.SetBalance(holder, big.NewInt(11))
	st.SetBalance(retained, big.NewInt(22))
	st.SetBalance(converted, big.NewInt(33))
	st.SetBalance(ovm, new(big.Int))
	for n := range extra {
		st.SetBalance(common.BigToAddress(big.NewInt(int64(0x1000+n))), big.NewInt(1))
	}
	logs := []*types.Log{event("Transfer(address,address,uint256)", retained, holder, 1), event("Transfer(address,address,uint256)", converted, holder, 0), event("Approval(address,address,uint256)", holder, retained, 7)}
	for _, log := range logs {
		st.AddLog(log)
	}
	receipt := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, GasUsed: 21000, Logs: logs}
	receipts := types.Receipts{receipt}
	receipt.Bloom = types.CreateBloom(receipts)
	h := &types.Header{ParentHash: genesis.Hash(), Number: big.NewInt(1), Difficulty: big.NewInt(2), GasLimit: 10000000, GasUsed: 21000, Time: 1, Root: st.IntermediateRoot(true)}
	tx := types.NewTransaction(0, ovm, new(big.Int), 21000, new(big.Int), nil)
	index := uint64(0)
	tx.SetTransactionMeta(types.NewTransactionMeta(big.NewInt(1), 1, &holder, types.QueueOriginSequencer, &index, nil, nil))
	block := types.NewBlock(h, []*types.Transaction{tx}, nil, receipts)
	_, err = bc.WriteBlockWithState(block, receipts, logs, st, false)
	check(t, err)
	ready, err := rawdb.ReadOVMIndexPosition(db, rawdb.OVMIndexReadyKey)
	check(t, err)
	if ready == nil || ready.Hash != block.Hash() {
		t.Fatal("real node did not publish ready index")
	}
	if member, err := rawdb.ReadOVMTransferMember(db, converted); err != nil || member != nil {
		t.Fatal("zero transfer granted membership")
	}
	if pair, err := rawdb.ReadOVMAllowance(db, rawdb.OVMAllowanceTrieKey(holder, retained)); err != nil || len(pair) != 40 {
		t.Fatal("node did not record allowance")
	}
	bc.Stop()
	check(t, db.Close())
	syncEvents(t, path, genesis.Header(), block.Header(), logs, tx.Hash(), g.Config.ChainID.String())
	return block.Header(), genesis.Hash()
}

type account struct {
	Balance  string
	Nonce    uint64
	CodeHash common.Hash
	Storage  map[common.Hash]common.Hash
}
type document struct {
	Header   hexutil.Bytes
	Root     common.Hash
	Accounts map[common.Address]*account
	Codes    map[common.Hash]hexutil.Bytes
}

func build(t *testing.T, target, dir string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.Base(target))
	cmd := exec.Command("go", "build", "-o", path, target)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", b, err)
	}
	return path
}
func TestNodeIndexToBinary(t *testing.T) {
	dir := t.TempDir()
	binary := build(t, "./cmd/l2state", dir)
	reader := build(t, "./testdata/readercheck", dir)
	source := filepath.Join(dir, "source")
	header, _ := legacySource(t, source)
	removeEventIndexes(t, source)
	if _, err := os.Stat(filepath.Join(source, "ancient")); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly has ancient")
	}
	var first []byte
	for _, workers := range []string{"2", "8"} {
		out := filepath.Join(dir, "state-"+workers)
		cmd := exec.Command(binary, "migrate", "--source-chaindata", source, "--ovm-events", filepath.Join(dir, "events.json"), "--out", out, "--wrapped-ether-code", "../../internal/migration/testdata/wrapped.hex", "--workers", workers, "--cache-mb", "16", "--handles", "16", "--quiet")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if b, err := cmd.Output(); err != nil {
			t.Fatalf("migrate %s %s %v", b, stderr.String(), err)
		}
		data, err := os.ReadFile(out)
		check(t, err)
		if first == nil {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatal("workers changed file")
		}
		raw, err := exec.Command(reader, out).Output()
		check(t, err)
		var d document
		check(t, json.Unmarshal(raw, &d))
		original, err := rlp.EncodeToBytes(header)
		check(t, err)
		if !bytes.Equal(d.Header, original) {
			t.Fatal("source header changed")
		}
		validateLegacy(t, d)
		if d.Root != expectedRoot(t, false) {
			t.Fatal("output differs from independently constructed policy result")
		}
	}
}
func rebuildLegacyState(t *testing.T, d document) *state.StateDB {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { check(t, db.Close()) })
	nodes := trie.NewDatabase(db)
	accounts, err := trie.New(common.Hash{}, nodes)
	check(t, err)
	for address, a := range d.Accounts {
		storage, err := trie.New(common.Hash{}, nodes)
		check(t, err)
		for key, value := range a.Storage {
			blob, err := rlp.EncodeToBytes(bytes.TrimLeft(value[:], "\x00"))
			check(t, err)
			check(t, storage.TryUpdate(key[:], blob))
		}
		root, err := storage.Commit(nil)
		check(t, err)
		check(t, nodes.Commit(root, false))
		balance, ok := new(big.Int).SetString(a.Balance, 10)
		if !ok {
			t.Fatal("bad balance")
		}
		blob, err := rlp.EncodeToBytes(state.Account{Nonce: a.Nonce, Balance: balance, Root: root, CodeHash: a.CodeHash.Bytes()})
		check(t, err)
		check(t, accounts.TryUpdate(crypto.Keccak256(address[:]), blob))
	}
	for hash, code := range d.Codes {
		if hashOf(code) != hash {
			t.Fatal("bad code")
		}
		check(t, db.Put(hash[:], code))
	}
	root, err := accounts.Commit(nil)
	check(t, err)
	check(t, nodes.Commit(root, false))
	if root != d.Root {
		t.Fatalf("independent legacy root %s != %s", root, d.Root)
	}
	st, err := state.New(root, state.NewDatabase(db))
	check(t, err)
	return st
}

func validateLegacy(t *testing.T, d document) {
	t.Helper()
	previous := rcfg.UsingOVM
	rcfg.UsingOVM = false
	defer func() { rcfg.UsingOVM = previous }()
	st := rebuildLegacyState(t, d)
	for address, want := range map[common.Address]int64{holder: 11, retained: 0, converted: 33, ovm: 22} {
		if st.GetBalance(address).Cmp(big.NewInt(want)) != 0 {
			t.Fatalf("legacy balance %s: %s", address, st.GetBalance(address))
		}
	}
	if st.GetState(ovm, balanceSlot(holder)) != (common.Hash{}) || st.GetState(ovm, balanceSlot(retained)) != common.HexToHash("0x16") || st.GetState(ovm, allowanceSlot(holder, retained)) != common.HexToHash("0x07") {
		t.Fatal("legacy storage mismatch")
	}
}

func hashOf(data ...[]byte) common.Hash { return common.BytesToHash(crypto.Keccak256(data...)) }

// Construct the expected post-policy state from fixture inputs, independently of
// file records and the new migration implementation, using legacy Genesis/StateDB.
func expectedRoot(t *testing.T, overlay bool) common.Hash {
	t.Helper()
	text, err := os.ReadFile("../../internal/migration/testdata/wrapped.hex")
	check(t, err)
	runtime, err := hexutil.Decode(strings.TrimSpace(string(text)))
	check(t, err)
	backing := int64(22)
	storage := map[common.Hash]common.Hash{balanceSlot(retained): common.HexToHash("0x16"), allowanceSlot(holder, retained): common.HexToHash("0x07")}
	alloc := core.GenesisAlloc{
		holder:    {Balance: big.NewInt(11)},
		retained:  {Balance: new(big.Int), Nonce: 1, Code: []byte{0}},
		converted: {Balance: big.NewInt(33), Nonce: 1, Code: []byte{0}},
	}
	if overlay {
		backing = 55
		storage[balanceSlot(converted)] = common.HexToHash("0x21")
		alloc[holder] = core.GenesisAccount{Balance: big.NewInt(5)}
		alloc[converted] = core.GenesisAccount{Balance: new(big.Int), Nonce: 1, Code: []byte{0x60, 0x10}}
		alloc[common.HexToAddress("0x99")] = core.GenesisAccount{Balance: new(big.Int)}
		alloc[common.HexToAddress("0x98")] = core.GenesisAccount{Balance: new(big.Int), Code: []byte{0x60, 2, 0}, Storage: map[common.Hash]common.Hash{common.HexToHash("0x99"): common.HexToHash("0x23")}}
	}
	storage[common.HexToHash("0x02")] = common.BigToHash(big.NewInt(backing))
	alloc[ovm] = core.GenesisAccount{Balance: big.NewInt(backing), Code: runtime, Storage: storage}
	db := rawdb.NewMemoryDatabase()
	defer func() { check(t, db.Close()) }()
	previous := rcfg.UsingOVM
	rcfg.UsingOVM = false
	defer func() { rcfg.UsingOVM = previous }()
	return (&core.Genesis{Alloc: alloc}).ToBlock(db).Root()
}

func TestRetainAllocIndependentRoot(t *testing.T) {
	dir := t.TempDir()
	binary := build(t, "./cmd/l2state", dir)
	reader := build(t, "./testdata/readercheck", dir)
	source := filepath.Join(dir, "source")
	legacySource(t, source)
	retain := filepath.Join(dir, "retain")
	check(t, os.WriteFile(retain, []byte(converted.Hex()+"\n"), 0600))
	alloc := filepath.Join(dir, "alloc.json")
	data, err := json.Marshal(map[string]any{
		holder.Hex():                      map[string]any{"balance": "0x05", "nonce": "0x0", "code": "0x"},
		converted.Hex():                   map[string]any{"code": "0x6010"},
		common.HexToAddress("0x99").Hex(): map[string]any{"balance": "0x0"},
		common.HexToAddress("0x98").Hex(): map[string]any{"code": "0x600200", "storage": map[string]string{"0x99": "0x23"}},
	})
	check(t, err)
	check(t, os.WriteFile(alloc, data, 0600))
	for _, compression := range []string{"none", "zstd"} {
		out := filepath.Join(dir, "state-"+compression)
		cmd := exec.Command(binary, "migrate", "--source-chaindata", source, "--ovm-events", filepath.Join(dir, "events.json"), "--out", out, "--wrapped-ether-code", "../../internal/migration/testdata/wrapped.hex", "--ovm-erc20-retain-list", retain, "--ovm-genesis-alloc", alloc, "--compression", compression, "--workers", "2", "--quiet")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("overlay migrate: %s %v", b, err)
		}
		raw, err := exec.Command(reader, out).Output()
		check(t, err)
		var d document
		check(t, json.Unmarshal(raw, &d))
		if want := expectedRoot(t, true); d.Root != want {
			t.Fatalf("independent overlay root %s != %s", want, d.Root)
		}
	}
}

// This opt-in entrypoint creates new benchmark data only, never updates a corpus.
func TestBenchmarkFixture(t *testing.T) {
	path := os.Getenv("L2STATE_BENCH_FIXTURE")
	if path == "" {
		t.Skip("benchmark fixture destination not supplied")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("benchmark destination must be new")
	}
	check(t, os.Mkdir(path, 0700))
	legacySource(t, filepath.Join(path, "source"))
}

// The node creates the reference indexes; the new executable independently
// reconstructs them over standard JSON-RPC and resumes from a genesis checkpoint.
func syncEvents(t *testing.T, source string, genesis, head *types.Header, logs []*types.Log, tx common.Hash, chainID string) {
	t.Helper()
	for i, l := range logs {
		l.BlockNumber = head.Number.Uint64()
		l.BlockHash = head.Hash()
		l.TxHash = tx
		l.TxIndex = 0
		l.Index = uint(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var result any
		switch request.Method {
		case "eth_chainId":
			n, _ := new(big.Int).SetString(chainID, 10)
			result = fmt.Sprintf("0x%x", n)
		case "eth_getBlockByNumber":
			var tag string
			if err := json.Unmarshal(request.Params[0], &tag); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			switch tag {
			case "0x0":
				result = genesis
			case "0x1", "latest":
				result = head
			default:
				http.Error(w, "unexpected block", 400)
				return
			}
		case "eth_getLogs":
			var filter struct {
				From    hexutil.Uint64  `json:"fromBlock"`
				To      hexutil.Uint64  `json:"toBlock"`
				Address common.Address  `json:"address"`
				Topics  [][]common.Hash `json:"topics"`
			}
			if err := json.Unmarshal(request.Params[0], &filter); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if filter.Address != ovm || len(filter.Topics) != 1 || len(filter.Topics[0]) != 2 {
				http.Error(w, "wrong log filter", 400)
				return
			}
			result = []*types.Log{}
			if uint64(filter.From) <= head.Number.Uint64() && uint64(filter.To) >= head.Number.Uint64() {
				result = logs
			}
		default:
			http.Error(w, "unexpected RPC method", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	dir := filepath.Dir(source)
	binary := build(t, "./cmd/ovm-event-sync", dir)
	path := filepath.Join(dir, "events.json")
	for _, target := range []string{"0", "1"} {
		cmd := exec.Command(binary, "--rpc-url", server.URL, "--out", path, "--to-block", target, "--quiet")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sync events: %s %v", out, err)
		}
	}
	raw, err := os.ReadFile(path)
	check(t, err)
	var d struct {
		Version  uint64
		ChainID  string
		Genesis  common.Hash
		Progress struct {
			BlockNumber uint64
			BlockHash   common.Hash
		}
		Allowances []struct {
			Owner, Spender common.Address
		}
		Members []common.Address
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	check(t, decoder.Decode(&d))
	if d.Version != 1 || d.ChainID != chainID || d.Genesis != genesis.Hash() {
		t.Fatal("RPC checkpoint identity differs from node")
	}
	if d.Progress.BlockNumber != 1 || d.Progress.BlockHash != head.Hash() || len(d.Allowances) != 1 || len(d.Members) != 1 {
		t.Fatal("RPC index inventory differs from node")
	}
	a, m := d.Allowances[0], d.Members[0]
	if a.Owner != holder || a.Spender != retained || m != retained {
		t.Fatal("RPC relationships differ from node index")
	}
}
func removeEventIndexes(t *testing.T, path string) {
	t.Helper()
	db, err := rawdb.NewLevelDBDatabase(path, 16, 16, "remove-test-index")
	check(t, err)
	for _, prefix := range [][]byte{[]byte("ovm-transfer-v1/"), rawdb.OVMAllowancePrefix} {
		it := db.NewIteratorWithPrefix(prefix)
		var keys [][]byte
		for it.Next() {
			keys = append(keys, bytes.Clone(it.Key()))
		}
		check(t, it.Error())
		it.Release()
		for _, key := range keys {
			check(t, db.Delete(key))
		}
	}
	check(t, db.Close())
}

func benchmarkStorage(t *testing.T, extra int) (int, int) {
	t.Helper()
	values := make([]int, 2)
	for i, key := range []string{"L2STATE_BENCH_STORAGE_CONTRACTS", "L2STATE_BENCH_STORAGE_SLOTS"} {
		if raw := os.Getenv(key); raw != "" {
			n, err := strconv.Atoi(raw)
			check(t, err)
			if n < 0 {
				t.Fatal("negative storage fixture size")
			}
			values[i] = n
		}
	}
	if values[0] > extra || (values[0] == 0) != (values[1] == 0) {
		t.Fatal("invalid storage fixture dimensions")
	}
	return values[0], values[1]
}
