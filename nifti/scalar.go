package nifti

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/storedsample"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func encodeStoredFrame(ctx context.Context, raw []byte, metadata pixeldata.Metadata, order binary.ByteOrder, transform linearTransform, datatype int16) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if order == nil {
		order = binary.LittleEndian
	}
	reader, err := storedsample.New(int(metadata.BitsAllocated), int(metadata.BitsStored), int(metadata.HighBit), metadata.PixelRepresentation == 1, order)
	if err != nil {
		return nil, ErrUnsupportedPixels
	}
	width := reader.Width()
	count64, ok := checkedMul(uint64(metadata.Rows), uint64(metadata.Columns))
	if !ok || count64 > uint64(math.MaxInt) || width <= 0 {
		return nil, ErrUnsupportedPixels
	}
	count := int(count64)
	want, ok := checkedMul(count64, uint64(width))
	if !ok || want != uint64(len(raw)) {
		return nil, ErrUnsupportedPixels
	}
	outWidth, ok := datatypeBytes(datatype)
	if !ok || count > math.MaxInt/outWidth {
		return nil, ErrUnsupportedPixels
	}
	out := make([]byte, count*outWidth)
	for index := 0; index < count; index++ {
		if index&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// Stored values are sign-extended from BitsStored when signed, so the
		// low bits of either view are the destination's two's complement.
		signed := reader.Value(raw[index*width:])
		unsigned := uint64(signed)
		destination := out[index*outWidth:]
		switch datatype {
		case DatatypeUint8:
			destination[0] = byte(unsigned)
		case DatatypeInt8:
			destination[0] = byte(int8(signed))
		case DatatypeUint16:
			binary.LittleEndian.PutUint16(destination, uint16(unsigned))
		case DatatypeInt16:
			binary.LittleEndian.PutUint16(destination, uint16(int16(signed)))
		case DatatypeUint32:
			binary.LittleEndian.PutUint32(destination, uint32(unsigned))
		case DatatypeInt32:
			binary.LittleEndian.PutUint32(destination, uint32(int32(signed)))
		case DatatypeFloat32:
			value := float64(signed)*transform.slope + transform.intercept
			converted := float32(value)
			if !finite(value) || math.IsNaN(float64(converted)) || math.IsInf(float64(converted), 0) {
				return nil, ErrUnsupportedScaling
			}
			binary.LittleEndian.PutUint32(destination, math.Float32bits(converted))
		case DatatypeFloat64:
			value := float64(signed)*transform.slope + transform.intercept
			if !finite(value) {
				return nil, ErrUnsupportedScaling
			}
			binary.LittleEndian.PutUint64(destination, math.Float64bits(value))
		default:
			return nil, ErrUnsupportedPixels
		}
	}
	return out, nil
}

func encodeFloat64Values(ctx context.Context, values []float64) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(values) > math.MaxInt/8 {
		return nil, ErrLimitExceeded
	}
	out := make([]byte, len(values)*8)
	if err := encodeFloat64ValuesInto(ctx, out, values); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeFloat64ValuesInto writes calibrated values as little-endian float64.
// NaN and ±Inf are legal IEEE 754 payload values in a float NIfTI volume and
// are written unchanged; the caller has already classified and counted them.
func encodeFloat64ValuesInto(ctx context.Context, destination []byte, values []float64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(values) > math.MaxInt/8 || len(destination) < len(values)*8 {
		return ErrLimitExceeded
	}
	for index, value := range values {
		if index&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		binary.LittleEndian.PutUint64(destination[index*8:], math.Float64bits(value))
	}
	return nil
}

func datatypeBytes(datatype int16) (int, bool) {
	switch datatype {
	case DatatypeUint8, DatatypeInt8:
		return 1, true
	case DatatypeInt16, DatatypeUint16:
		return 2, true
	case DatatypeInt32, DatatypeUint32, DatatypeFloat32:
		return 4, true
	case DatatypeFloat64:
		return 8, true
	default:
		return 0, false
	}
}

func writeExact(destination io.Writer, payload []byte) error {
	written, err := destination.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}
