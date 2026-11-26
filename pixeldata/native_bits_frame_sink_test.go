package pixeldata

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The parser's frame sink and ExtractNativeFrames must split one-bit native
// Pixel Data identically, including frames that start mid-byte.
func TestParserFrameSinkMatchesNativeOneBitSplit(t *testing.T) {
	for _, tc := range []struct {
		rows, columns uint16
		frames        int
	}{
		{5, 5, 3}, {3, 3, 2}, {4, 4, 3}, {1, 9, 1}, {7, 3, 5},
	} {
		bits := int(tc.rows) * int(tc.columns) * tc.frames
		value := make([]byte, (bits+7)/8)
		for i := range value {
			value[i] = byte(i*37 + 11)
		}
		if len(value)%2 == 1 {
			value = append(value, 0)
		}
		elements := []core.Element{
			dicomtest.NewUShortElement(tagSamplesPerPixel, 1),
			dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
			dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, strconv.Itoa(tc.frames)),
			dicomtest.NewUShortElement(tagRows, tc.rows),
			dicomtest.NewUShortElement(tagColumns, tc.columns),
			dicomtest.NewUShortElement(tagBitsAllocated, 1),
			dicomtest.NewUShortElement(tagBitsStored, 1),
			dicomtest.NewUShortElement(tagHighBit, 0),
			dicomtest.NewUShortElement(tagPixelRepresentation, 0),
			core.NewRawElement(core.TagPixelData, core.VROB, value),
		}
		native, err := ExtractNativeFrames(object.FromElements(elements, nil))
		if err != nil {
			t.Fatalf("%dx%dx%d ExtractNativeFrames: %v", tc.rows, tc.columns, tc.frames, err)
		}
		var streamed [][]byte
		sink := object.FrameSinkFunc(func(frame object.Frame) error {
			streamed = append(streamed, frame.Data)
			return nil
		})
		stream := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, elements...)
		if _, err := object.ReadDataSetWithOptions(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{FrameSink: sink}); err != nil {
			t.Fatalf("%dx%dx%d frame sink: %v", tc.rows, tc.columns, tc.frames, err)
		}
		if len(streamed) != len(native.Data) {
			t.Fatalf("%dx%dx%d frame count: sink %d, extract %d", tc.rows, tc.columns, tc.frames, len(streamed), len(native.Data))
		}
		for i := range streamed {
			if !bytes.Equal(streamed[i], native.Data[i]) {
				t.Fatalf("%dx%dx%d frame %d: sink %08b, extract %08b", tc.rows, tc.columns, tc.frames, i, streamed[i], native.Data[i])
			}
		}
	}
}
