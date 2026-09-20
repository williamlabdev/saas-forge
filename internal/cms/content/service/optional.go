package service

import "encoding/json"

// Optional is a JSON field that distinguishes THREE states, which a plain
// pointer cannot:
//
//	key absent          → Set == false            — leave whatever is stored alone
//	key present, null   → Set == true, Value nil  → reset to "not recorded"
//	key present, value  → Set == true, Value set  → store it (including "")
//
// encoding/json collapses the first two: both leave a *string at nil, so a PATCH
// decoded into pointers cannot tell "I did not mention alt_text" from "clear the
// alt_text". For a tri-state column that difference is the whole feature —
// clearing alt_text means "nobody has described this image", which is not what a
// caller correcting a filename asked for. The distinction is recoverable only at
// decode time, hence a custom UnmarshalJSON rather than a post-hoc check.
//
// encoding/json calls UnmarshalJSON even for a literal null, which is exactly
// what makes the second state observable.
type Optional[T any] struct {
	// Set reports that the key was present in the JSON at all.
	Set bool
	// Value is nil when the key was present and null.
	Value *T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// MarshalJSON exists so a round-trip through this type is lossless; the API only
// ever decodes into Optional, but a struct containing one must still be printable
// in a test failure or a debug log without lying about what was sent.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.Value)
}
