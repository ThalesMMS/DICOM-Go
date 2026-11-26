package core

import "errors"

// ErrDiscardedValue means a payload was deliberately consumed without retention.
// It cannot be serialized or replayed; the caller must supply replacement data.
var ErrDiscardedValue = errors.New("dicom: payload was discarded during streaming")

// ErrValueNotRepresentable means an authored value cannot be encoded in its
// element's VR: a type that does not match the VR, a number outside the VR's
// range, or text that violates the VR's syntax. Serializers return it instead
// of truncating, reinterpreting or omitting the value.
var ErrValueNotRepresentable = errors.New("dicom: value is not representable in its VR")

// DiscardedValue preserves the distinction between a discarded payload and an
// empty or deferred value. In particular, copying its element to another dataset
// must not turn it into writable empty Pixel Data.
type DiscardedValue struct{}

func (DiscardedValue) Kind() ValueKind               { return ValueDiscarded }
func (DiscardedValue) EncodedLength() (Length, bool) { return UndefinedLength, false }
