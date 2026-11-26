package object

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

func isBinaryIntegerVR(vr core.VR) bool {
	switch vr {
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
		return true
	default:
		return false
	}
}

// binaryInts decodes a binary integer element, typed or raw in order.
func binaryInts(elem core.Element, order binary.ByteOrder) ([]int64, error) {
	tag, vr := elem.Tag(), elem.VR()
	switch values := elem.Value.(type) {
	case core.Uint16Value:
		out := make([]int64, len(values))
		for i, value := range values {
			out[i] = int64(value)
		}
		return out, nil
	case core.Int32Value:
		out := make([]int64, len(values))
		for i, value := range values {
			out[i] = int64(value)
		}
		return out, nil
	case core.Uint32Value:
		out := make([]int64, len(values))
		for i, value := range values {
			out[i] = int64(value)
		}
		return out, nil
	}
	raw, ok := elem.RawBytes()
	if !ok {
		return nil, fmt.Errorf("dicom: element %s with VR %s is not raw integer data", tag, vr)
	}
	width := 2
	switch vr {
	case core.VRUL, core.VRSL:
		width = 4
	case core.VRUV, core.VRSV:
		width = 8
	}
	if len(raw)%width != 0 {
		return nil, fmt.Errorf("dicom: element %s has %d bytes, want a multiple of %d for VR %s", tag, len(raw), width, vr)
	}
	out := make([]int64, len(raw)/width)
	for i := range out {
		offset := i * width
		switch vr {
		case core.VRUS:
			out[i] = int64(order.Uint16(raw[offset:]))
		case core.VRSS:
			out[i] = int64(int16(order.Uint16(raw[offset:])))
		case core.VRUL:
			out[i] = int64(order.Uint32(raw[offset:]))
		case core.VRSL:
			out[i] = int64(int32(order.Uint32(raw[offset:])))
		case core.VRUV:
			value := order.Uint64(raw[offset:])
			if value > math.MaxInt64 {
				return nil, fmt.Errorf("dicom: element %s unsigned value %d overflows int64", tag, value)
			}
			out[i] = int64(value)
		case core.VRSV:
			out[i] = int64(order.Uint64(raw[offset:]))
		}
	}
	return out, nil
}

// binaryFloats decodes an FL or FD element, typed or raw in order.
func binaryFloats(elem core.Element, order binary.ByteOrder) ([]float64, error) {
	tag, vr := elem.Tag(), elem.VR()
	switch values := elem.Value.(type) {
	case core.Float32Value:
		out := make([]float64, len(values))
		for i, value := range values {
			out[i] = float64(value)
		}
		return out, nil
	case core.Float64Value:
		return append([]float64(nil), values...), nil
	}
	raw, ok := elem.RawBytes()
	if !ok {
		return nil, fmt.Errorf("dicom: element %s with VR %s is not raw floating-point data", tag, vr)
	}
	width := 4
	if vr == core.VRFD {
		width = 8
	}
	if len(raw)%width != 0 {
		return nil, fmt.Errorf("dicom: element %s has %d bytes, want a multiple of %d for VR %s", tag, len(raw), width, vr)
	}
	out := make([]float64, len(raw)/width)
	for i := range out {
		if width == 4 {
			out[i] = float64(math.Float32frombits(order.Uint32(raw[4*i:])))
		} else {
			out[i] = math.Float64frombits(order.Uint64(raw[8*i:]))
		}
	}
	return out, nil
}
