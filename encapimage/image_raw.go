package encapimage

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// RawOptions describes exact interleaved stored samples; no geometry is guessed.
// Offset skips an explicit prefix. The remaining input must match exactly.
type RawOptions struct {
	Rows, Columns, Frames, Samples int
	BitsAllocated, BitsStored      int
	Signed                         bool
	ByteOrder                      string // little (default) or big; 8-bit inputs are byte order neutral.
	Offset                         int64
}

func readRaw(ctx context.Context, b []byte, o RawOptions, limits ImportLimits) ([]sourceFrame, error) {
	if o.Frames == 0 {
		o.Frames = 1
	}
	if o.Samples == 0 {
		o.Samples = 1
	}
	if o.BitsStored == 0 {
		o.BitsStored = o.BitsAllocated
	}
	if o.Frames < 1 || o.Frames > limits.MaxFrames || o.Offset < 0 || o.Offset > int64(len(b)) || o.BitsStored < 1 || o.BitsStored > o.BitsAllocated || (o.BitsAllocated != 8 && o.BitsAllocated != 16) || (o.ByteOrder != "" && o.ByteOrder != "little" && o.ByteOrder != "big") {
		return nil, ErrInvalidOption
	}
	if o.Frames > 1 && pageVectorBytes(o.Frames) > 65534 {
		return nil, ErrImportLimit
	}
	l := imageLayout{rows: o.Rows, columns: o.Columns, samples: o.Samples, bits: o.BitsStored, allocated: o.BitsAllocated, lossless: true, process: "native", photometric: "MONOCHROME2"}
	if o.Signed {
		l.signed = 1
	}
	if o.Samples == 3 {
		l.photometric = "RGB"
	}
	n, err := admitImage(l, limits)
	if err != nil {
		return nil, err
	}
	// Raw allocation is defined by BitsAllocated, even when BitsStored <=8.
	stride := int64(o.Rows) * int64(o.Columns) * int64(o.Samples) * int64(o.BitsAllocated/8)
	if stride > limits.MaxDecodedBytes/int64(o.Frames) || stride*int64(o.Frames) != int64(len(b))-o.Offset {
		return nil, fmt.Errorf("%w: raw length", ErrMalformedImage)
	}
	frames := make([]sourceFrame, o.Frames)
	for f := range frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := int(o.Offset) + f*int(stride)
		data := make([]byte, int(n))
		copy(data, b[start:start+int(stride)])
		if o.BitsAllocated == 16 && o.ByteOrder == "big" {
			for p := 0; p < len(data); p += 2 {
				if p&65535 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				v := binary.BigEndian.Uint16(data[p:])
				binary.LittleEndian.PutUint16(data[p:], v)
			}
		}
		if err := checkCodes(ctx, data, l); err != nil {
			return nil, err
		}
		frames[f] = sourceFrame{imageLayout: l, data: data, syntax: transfer.ExplicitVRLittleEndian, format: Raw}
	}
	return frames, nil
}
func checkCodes(ctx context.Context, data []byte, l imageLayout) error {
	width := effectiveAllocated(l) / 8
	mask := uint16((uint32(1) << uint(l.bits)) - 1)
	for p := 0; p < len(data); p += width {
		if p&65535 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		value := uint16(data[p])
		if width == 2 {
			value = binary.LittleEndian.Uint16(data[p:])
		}
		unused := value & ^mask
		if l.signed == 0 && unused != 0 {
			return fmt.Errorf("%w: samples exceed precision", ErrMalformedImage)
		}
		if l.signed != 0 {
			fullmask := uint16(65535)
			if width == 1 {
				fullmask = 255
			}
			want := uint16(0)
			if value&(uint16(1)<<uint(l.bits-1)) != 0 {
				want = fullmask & ^mask
			}
			if unused != want {
				return fmt.Errorf("%w: invalid sign extension", ErrMalformedImage)
			}
		}
	}
	return nil
}
