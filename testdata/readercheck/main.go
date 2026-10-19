// readercheck is an integration-test consumer of the public package, not a CLI command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/metis-devops/metis-l2state/statefile"
)

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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (retErr error) {
	if len(os.Args) != 2 {
		return errors.New("expected file")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	r, err := statefile.NewReader(context.Background(), f)
	if err != nil {
		return err
	}
	defer r.Close()
	d := document{Header: r.Header().HeaderRLP, Accounts: make(map[common.Address]*account), Codes: make(map[common.Hash]hexutil.Bytes)}
	var current *account
	for {
		v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch v.Kind {
		case statefile.AccountStart:
			current = &account{Balance: v.Account.Balance.String(), Nonce: v.Account.Nonce, CodeHash: v.Account.CodeHash, Storage: make(map[common.Hash]common.Hash)}
			d.Accounts[v.Account.Address] = current
		case statefile.StorageEntry:
			current.Storage[common.Hash(v.Storage.Key)] = v.Storage.Value
		case statefile.CodeEntry:
			d.Codes[v.CodeHash] = v.Code
		}
	}
	s, err := r.Result()
	if err != nil {
		return err
	}
	d.Root = s.FinalStateRoot
	return json.NewEncoder(os.Stdout).Encode(d)
}
