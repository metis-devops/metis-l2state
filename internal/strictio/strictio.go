// Package strictio provides bounded reads for migration inputs.
package strictio

import (
	jsonv1 "encoding/json"
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
	// Preserve the existing typed-decoding semantics while still rejecting
	// unknown fields and trailing values. Audit schema validation is separate.
	if err := json.Unmarshal(data, &value, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	return value, nil
}

// WriteJSON writes one newline-terminated result with the established JSON
// representation (including escaping, map ordering, nil values and omitempty).
func WriteJSON(w io.Writer, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(w), value, jsonv1.DefaultOptionsV1())
}
