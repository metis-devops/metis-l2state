// Package strictio provides bounded reads for migration inputs.
package strictio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxMetadataSize bounds audit input documents.
const MaxMetadataSize int64 = 1 << 20

// DecodeJSON decodes exactly one strict JSON value.
func DecodeJSON[T any](data []byte, label string) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode %s: trailing JSON value", label)
		}
		return value, fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return value, nil
}
