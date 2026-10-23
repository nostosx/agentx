package loop

import (
	"encoding/json"
	"fmt"
)

// DecodeValue decodes the run value of key, as stored by EncodeValue,
// into a T. It reports false if the run has no such value; a value that is
// not JSON of a T is an error.
func (r *Run) DecodeValue[T any](key string) (T, bool, error) {
	var v T
	s, ok := r.Value(key)
	if !ok {
		return v, false, nil
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return v, true, fmt.Errorf("loop: run value %s: %w", key, err)
	}
	return v, true, nil
}

// EncodeValue stores v as the run value of key, encoded as JSON, so
// values other than strings survive checkpoints; read it with
// DecodeValue. A nil v deletes the key. Like SetValue, it must not be
// called from tools that may run concurrently.
func (r *Run) EncodeValue(key string, v any) error {
	if v == nil {
		r.SetValue(key, "")
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("loop: run value %s: %w", key, err)
	}
	r.SetValue(key, string(b))
	return nil
}
