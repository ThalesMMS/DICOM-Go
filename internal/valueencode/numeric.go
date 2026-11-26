// Package valueencode shares primitive encoding between wire and metadata
// serializers without depending on either parser or object ownership.
package valueencode

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

func ValidateNumeric(vr core.VR, value core.Value) error {
	var allowed []core.VR
	switch value.(type) {
	case core.Uint16Value:
		allowed = []core.VR{core.VRUS, core.VROW}
	case core.Int16Value:
		allowed = []core.VR{core.VRSS}
	case core.Uint32Value:
		allowed = []core.VR{core.VRUL, core.VROL}
	case core.Int32Value:
		allowed = []core.VR{core.VRSL}
	case core.Uint64Value:
		allowed = []core.VR{core.VRUV, core.VROV}
	case core.Int64Value:
		allowed = []core.VR{core.VRSV}
	case core.Float32Value:
		allowed = []core.VR{core.VRFL, core.VROF}
	case core.Float64Value:
		allowed = []core.VR{core.VRFD, core.VROD}
	case core.TagValue:
		allowed = []core.VR{core.VRAT}
	default:
		return fmt.Errorf("%w: unsupported numeric value type %T", core.ErrValueNotRepresentable, value)
	}
	for _, candidate := range allowed {
		if vr == candidate {
			return nil
		}
	}
	return fmt.Errorf("%w: numeric value type %T is incompatible with VR %s", core.ErrValueNotRepresentable, value, vr)
}

func Numeric(value core.Value, order binary.ByteOrder) ([]byte, core.Length, error) {
	if order == nil {
		order = binary.LittleEndian
	}
	switch values := value.(type) {
	case core.Uint16Value:
		return fixed(values, 2, func(dst []byte, v uint16) { order.PutUint16(dst, v) })
	case core.Int16Value:
		return fixed(values, 2, func(dst []byte, v int16) { order.PutUint16(dst, uint16(v)) })
	case core.Uint32Value:
		return fixed(values, 4, func(dst []byte, v uint32) { order.PutUint32(dst, v) })
	case core.Int32Value:
		return fixed(values, 4, func(dst []byte, v int32) { order.PutUint32(dst, uint32(v)) })
	case core.Uint64Value:
		return fixed(values, 8, func(dst []byte, v uint64) { order.PutUint64(dst, v) })
	case core.Int64Value:
		return fixed(values, 8, func(dst []byte, v int64) { order.PutUint64(dst, uint64(v)) })
	case core.Float32Value:
		return fixed(values, 4, func(dst []byte, v float32) { order.PutUint32(dst, math.Float32bits(v)) })
	case core.Float64Value:
		return fixed(values, 8, func(dst []byte, v float64) { order.PutUint64(dst, math.Float64bits(v)) })
	case core.TagValue:
		return fixed(values, 4, func(dst []byte, v core.Tag) { order.PutUint16(dst[:2], v.Group); order.PutUint16(dst[2:], v.Element) })
	default:
		return nil, 0, fmt.Errorf("dicom: unsupported numeric value type %T", value)
	}
}

func fixed[T any](values []T, width int, encode func([]byte, T)) ([]byte, core.Length, error) {
	length, err := fixedLength(len(values), width)
	if err != nil {
		return nil, 0, err
	}
	encoded := make([]byte, int(length))
	for i, v := range values {
		encode(encoded[i*width:(i+1)*width], v)
	}
	return encoded, length, nil
}

// NumericLength shares the allocation guard with Numeric without encoding or
// cloning the caller's typed values.
func NumericLength(value core.Value) (core.Length, error) {
	switch v := value.(type) {
	case core.Uint16Value:
		return fixedLength(len(v), 2)
	case core.Int16Value:
		return fixedLength(len(v), 2)
	case core.Uint32Value:
		return fixedLength(len(v), 4)
	case core.Int32Value:
		return fixedLength(len(v), 4)
	case core.Uint64Value:
		return fixedLength(len(v), 8)
	case core.Int64Value:
		return fixedLength(len(v), 8)
	case core.Float32Value:
		return fixedLength(len(v), 4)
	case core.Float64Value:
		return fixedLength(len(v), 8)
	case core.TagValue:
		return fixedLength(len(v), 4)
	default:
		return 0, fmt.Errorf("dicom: unsupported numeric value type %T", value)
	}
}

func fixedLength(count, width int) (core.Length, error) {
	if count < 0 || width <= 0 || uint64(count) > uint64(^uint64(0))/uint64(width) {
		return 0, dicomenc.ErrLengthOverflow
	}
	total := uint64(count) * uint64(width)
	if total >= uint64(core.UndefinedLength) || total > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("dicom: numeric value length %d exceeds defined/platform length: %w", total, dicomenc.ErrLengthOverflow)
	}
	return core.Length(total), nil
}
