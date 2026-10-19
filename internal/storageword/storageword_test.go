package storageword

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

func TestEncodingAndTrieOwnership(t *testing.T) {
	got, want := trie.NewStackTrie(nil), trie.NewStackTrie(nil)
	var buf [33]byte
	index := uint64(0)
	// Every significant length and leading byte, including the single-byte RLP
	// boundary and zero. Reuse and overwrite the encoding buffer between inserts.
	for length := range 33 {
		for leading := range 256 {
			var value, key common.Hash
			if length != 0 {
				value[32-length] = byte(leading)
				for i := 33 - length; i < 32; i++ {
					value[i] = byte(i + leading)
				}
			}
			binary.BigEndian.PutUint64(key[24:], index)
			index++
			expected, err := rlp.EncodeToBytes(bytes.TrimLeft(value[:], "\x00"))
			if err != nil {
				t.Fatal(err)
			}
			encoded := encode(&buf, value)
			if !bytes.Equal(encoded, expected) {
				t.Fatalf("RLP mismatch: %x", value)
			}
			if err := got.Update(key[:], encoded); err != nil {
				t.Fatal(err)
			}
			if err := want.Update(key[:], expected); err != nil {
				t.Fatal(err)
			}
			for i := range buf {
				buf[i] = 0xff
			}
		}
	}
	if got.Hash() != want.Hash() {
		t.Fatal("StackTrie retained encoding scratch")
	}
}

func FuzzEncoding(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{0x7f})
	f.Add([]byte{0x80})
	f.Add(bytes.Repeat([]byte{0xff}, 32))
	f.Fuzz(func(t *testing.T, data []byte) {
		value := common.BytesToHash(data)
		var buf [33]byte
		expected, err := rlp.EncodeToBytes(bytes.TrimLeft(value[:], "\x00"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encode(&buf, value), expected) {
			t.Fatal("RLP mismatch")
		}
	})
}
