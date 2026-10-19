package migration

import (
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
)

// A consumer can build an ephemeral execution state directly from hashed slots.
// The migration implementation itself never creates this database.
func TestWrappedRuntimeAndContinuation(t *testing.T) {
	f := newFixture(t, 0, 0)
	out := filepath.Join(t.TempDir(), "state")
	o := f.options(out)
	o.WrappedEtherCode = filepath.Join("testdata", "wrapped.hex")
	result, err := Migrate(t.Context(), o)
	must(t, err)
	db := rawdb.NewMemoryDatabase()
	defer func() { must(t, db.Close()) }()
	sink := func(_ []byte, h common.Hash, b []byte) { must(t, db.Put(h[:], b)) }
	accountTrie := trie.NewStackTrie(sink)
	var storageTrie *trie.StackTrie
	var current statefile.Account
	file, err := os.Open(out)
	must(t, err)
	defer func() { must(t, file.Close()) }()
	reader, err := statefile.NewReader(t.Context(), file)
	must(t, err)
	defer reader.Close()
	for {
		r, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		switch r.Kind {
		case statefile.AccountStart:
			current = r.Account
			storageTrie = trie.NewStackTrie(sink)
		case statefile.StorageEntry:
			must(t, storageTrie.Update(r.Storage.Key[:], slotRLP(r.Storage.Value)))
		case statefile.AccountEnd:
			balance, overflow := uint256.FromBig(current.Balance)
			if overflow {
				t.Fatal("balance overflow")
			}
			blob, err := rlp.EncodeToBytes(&types.StateAccount{Nonce: current.Nonce, Balance: balance, Root: storageTrie.Hash(), CodeHash: current.CodeHash.Bytes()})
			must(t, err)
			hash := crypto.Keccak256Hash(current.Address[:])
			must(t, accountTrie.Update(hash[:], blob))
		case statefile.CodeEntry:
			rawdb.WriteCode(db, r.CodeHash, r.Code)
		}
	}
	if accountTrie.Hash() != result.Summary.FinalStateRoot {
		t.Fatal("consumer root mismatch")
	}
	nodes := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() { must(t, nodes.Close()) }()
	st, err := state.New(result.Summary.FinalStateRoot, state.NewDatabase(nodes, state.NewCodeDB(db)))
	must(t, err)
	call := func(sender common.Address, value int64, method string, args ...[]byte) []byte {
		input := append([]byte(nil), crypto.Keccak256([]byte(method))[:4]...)
		for _, a := range args {
			input = append(input, common.LeftPadBytes(a, 32)...)
		}
		output, _, err := runtime.Call(ovmAddress, input, &runtime.Config{State: st, Origin: sender, Value: big.NewInt(value), GasLimit: 1000000})
		must(t, err)
		return output
	}
	read := func(method string, args ...[]byte) uint64 {
		return new(big.Int).SetBytes(call(eoa, 0, method, args...)).Uint64()
	}
	if read("totalSupply()") != 77 || read("balanceOf(address)", automatic[:]) != 22 || read("allowance(address,address)", eoa[:], automatic[:]) != 7 {
		t.Fatal("initial runtime state")
	}
	call(eoa, 5, "deposit()")
	if read("totalSupply()") != 82 || st.GetBalance(ovmAddress).Uint64() != 82 {
		t.Fatal("deposit backing")
	}
	call(eoa, 0, "withdraw(uint256)", []byte{3})
	if st.GetBalance(eoa).Uint64() != 9 || read("balanceOf(address)", eoa[:]) != 2 {
		t.Fatal("withdraw backing")
	}
	call(automatic, 0, "transfer(address,uint256)", eoa[:], []byte{2})
	if read("balanceOf(address)", automatic[:]) != 20 || read("balanceOf(address)", eoa[:]) != 4 {
		t.Fatal("transfer")
	}
	call(automatic, 0, "transferFrom(address,address,uint256)", eoa[:], ordinary[:], []byte{1})
	if read("allowance(address,address)", eoa[:], automatic[:]) != 6 {
		t.Fatal("allowance runtime")
	}
	root, err := st.Commit(params.Rules{IsCancun: true}, 2)
	must(t, err)
	must(t, nodes.Commit(root, false))
	reopened, err := state.New(root, state.NewDatabase(nodes, state.NewCodeDB(db)))
	must(t, err)
	if reopened.GetBalance(eoa).Uint64() != 9 || reopened.GetState(ovmAddress, common.HexToHash("0x02")).Big().Uint64() != 79 {
		t.Fatal("continuation")
	}
}
