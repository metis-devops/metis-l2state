package main

import (
	"errors"
	"os"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/metis-devops/metis-l2state/internal/ethjson"
)

// blockHeader accepts numeric fixture quantities and RPC hex quantities,
// validating the declared hash before returning the header's RLP.
func blockHeader(path string) (data []byte, retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	h, err := ethjson.DecodeHeader(f, true)
	if err != nil {
		return nil, err
	}
	return rlp.EncodeToBytes(h)
}
