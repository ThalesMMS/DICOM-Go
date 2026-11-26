package render

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/storedsample"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type PixelValue struct {
	X        int
	Y        int
	Stored   int32
	Rescaled float64
}

func PixelValueAt(slice *Frame, x, y int) (PixelValue, bool) {
	loaded, err := loadFrameContext(context.Background(), slice)
	if err != nil {
		return PixelValue{}, false
	}
	slice = loaded
	if slice == nil {
		return PixelValue{}, false
	}
	return SamplePixelValue(&slice.Metadata, slice.ByteOrder, slice.PixelBytes, slice.Rescale, x, y)
}

// SamplePixelValue samples one single-channel native pixel without constructing
// a Frame. It exists for adapters that already own the frame fields and need a
// hot path for MPR/VR sampling.
func SamplePixelValue(metadata *pixeldata.Metadata, order binary.ByteOrder, pixelBytes []byte, rescale Rescale, x, y int) (PixelValue, bool) {
	if metadata == nil || x < 0 || y < 0 {
		return PixelValue{}, false
	}
	if metadata.SamplesPerPixel != 1 {
		return PixelValue{}, false
	}
	cols := int(metadata.Columns)
	rows := int(metadata.Rows)
	if cols <= 0 || rows <= 0 || x >= cols || y >= rows {
		return PixelValue{}, false
	}
	bytesPerSample := int(metadata.BitsAllocated / 8)
	if bytesPerSample != 1 && bytesPerSample != 2 {
		return PixelValue{}, false
	}
	offset := (y*cols + x) * bytesPerSample
	if offset < 0 || offset+bytesPerSample > len(pixelBytes) {
		return PixelValue{}, false
	}
	if order == nil {
		order = binary.LittleEndian
	}
	stored := storedPixelValue(pixelBytes[offset:], metadata.BitsAllocated, metadata.BitsStored, metadata.HighBit, metadata.PixelRepresentation != 0, order)
	slope := rescale.Slope
	if slope == 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
		slope = 1
	}
	intercept := rescale.Intercept
	if math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		intercept = 0
	}
	return PixelValue{
		X:        x,
		Y:        y,
		Stored:   stored,
		Rescaled: float64(stored)*slope + intercept,
	}, true
}

func storedPixelValue(data []byte, bitsAllocated, bitsStored, highBit uint16, signed bool, order binary.ByteOrder) int32 {
	if bitsAllocated != 8 && bitsAllocated != 16 {
		return 0
	}
	reader := storedsample.Legacy(int(bitsAllocated), int(bitsStored), int(highBit), signed, order)
	return int32(reader.Value(data))
}
