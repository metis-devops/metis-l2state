// Package strictio provides bounded reads for migration inputs.
package strictio

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
)

// MaxMetadataSize bounds audit input documents.
const MaxMetadataSize int64 = 1 << 20

// DecodeJSON decodes exactly one strict JSON value.
func DecodeJSON[T any](data []byte, label string) (T, error) {
	var value T
	if err := json.Unmarshal(data, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	return value, nil
}

// WriteJSON writes one newline-terminated result using JSON v2 semantics.
func WriteJSON(w io.Writer, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(w), value)
}
