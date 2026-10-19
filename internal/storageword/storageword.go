// Package storageword encodes uint256 storage values for Ethereum tries.
package storageword

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie"
)

// Update inserts the canonical RLP of the minimally encoded integer. StackTrie
// copies key/value on Update, so the encoding buffer stays local to this call.
func Update(t *trie.StackTrie, key, value common.Hash) error {
	var buf [33]byte
	return t.Update(key[:], encode(&buf, value))
}

func encode(buf *[33]byte, value common.Hash) []byte {
	word := bytes.TrimLeft(value[:], "\x00")
	if len(word) == 1 && word[0] < 0x80 {
		buf[0] = word[0]
		return buf[:1]
	}
	buf[0] = 0x80 + byte(len(word))
	copy(buf[1:], word)
	return buf[:len(word)+1]
}
