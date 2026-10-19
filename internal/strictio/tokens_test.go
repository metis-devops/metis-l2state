package strictio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestTokenReaderFragmentationAndRawDigest(t *testing.T) {
	raw := " \n\t" + `{"escaped":"a\\\"b\u0041", "literal":" white  space ", "n":1234567890123456789,"a":[true,false,null]}` + strings.Repeat(" \n", 1000)
	for _, chunk := range []int{1, 2, 7, 4096} {
		hash := sha256.New()
		d := json.NewDecoder(TokenReader(t.Context(), io.TeeReader(strings.NewReader(raw), hash), 4096, chunk, "test"))
		d.UseNumber()
		reference := json.NewDecoder(strings.NewReader(raw))
		reference.UseNumber()
		var got, want any
		if err := d.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if err := reference.Decode(&want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("filter changed JSON values")
		}
		if _, err := d.Token(); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		expected := sha256.Sum256([]byte(raw))
		if string(hash.Sum(nil)) != string(expected[:]) {
			t.Fatal("digest did not cover exact raw bytes")
		}
	}
}

type dataAndError struct {
	data string
	err  error
}

func (r *dataAndError) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestTokenReaderLimitAndStickyErrors(t *testing.T) {
	for _, chunk := range []int{1, 2, 32} {
		if _, err := io.ReadAll(TokenReader(t.Context(), strings.NewReader(`"123456"`), 8, chunk, "test")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(TokenReader(t.Context(), strings.NewReader(`"1234567"`), 8, chunk, "test")); err == nil {
			t.Fatal("oversized token accepted")
		}
	}
	sentinel := errors.New("upstream read failure")
	r := TokenReader(t.Context(), &dataAndError{data: `"123456789"`, err: sentinel}, 8, 32, "test")
	_, err := io.ReadAll(r)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("lost combined error: %v", err)
	}
	if _, err = r.Read(make([]byte, 32)); !errors.Is(err, sentinel) {
		t.Fatal("read error was not sticky")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := io.ReadAll(TokenReader(ctx, strings.NewReader(strings.Repeat(" ", 10000)), 8, 1, "test")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
