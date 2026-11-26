package parser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// A malformed stream can return to Explicit VR after each implicit element.
// These synthetic bytes independently locate the native pixel header; recovery
// must not reinterpret subsequent Explicit VR lengths as Implicit VR lengths.
func mixedVRPixelStream() (stream, pixels []byte, headerOffset int64) {
	stream = append(stream, explicitShort(0x0008, 0x0060, "CS", []byte("CT"))...)
	stream = append(stream, implicitEl(0x0008, 0x0080, []byte("LAB"))...)
	stream = append(stream, explicitShort(0x0008, 0x1030, "LO", []byte("TEST"))...)
	stream = append(stream, implicitEl(0x0028, 0x0010, []byte{1, 0})...)
	stream = append(stream, explicitShort(0x0028, 0x0011, "US", []byte{2, 0})...)
	headerOffset = int64(len(stream))
	pixels = []byte{0, 0, 255, 127}
	stream = append(stream, explicitLong(0x7fe0, 0x0010, "OW", pixels)...)
	return
}

func TestMixedVRRecoveryPreservesPhysicalPixelOffset(t *testing.T) {
	stream, pixels, headerOffset := mixedVRPixelStream()
	options := ReaderOptions{Dictionary: std.Dictionary, RecoverImplicitVRElements: true,
		OddLengthPolicy: AcceptOddLength, DeferPixelData: true}
	reader := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, options)
	elements, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 6 || elements[2].Header.VR != core.VRLO || elements[4].Header.VR != core.VRUS || elements[5].Header.VR != core.VROW {
		t.Fatalf("recovery did not return to explicit headers: %+v", elements)
	}
	location, ok := reader.ValueLocation(core.NewTag(0x7fe0, 0x0010))
	if !ok || location.ValueOffset != headerOffset+12 || location.Length != int64(len(pixels)) || location.ItemOffsetSet {
		t.Fatalf("physical pixel location: %+v, found=%v, header=%d", location, ok, headerOffset)
	}
	if reader.Position() != int64(len(stream)) {
		t.Fatalf("end position = %d, want %d", reader.Position(), len(stream))
	}
	var replay bytes.Buffer
	n, err := reader.CopyElementValueAt(location, &replay)
	if err != nil || n != int64(len(pixels)) || !bytes.Equal(replay.Bytes(), pixels) || reader.Position() != int64(len(stream)) {
		t.Fatalf("deferred replay = %x, bytes=%d, position=%d, err=%v", replay.Bytes(), n, reader.Position(), err)
	}
	recoveries, truncated := reader.BoundaryRecoveries()
	if truncated || len(recoveries) != 2 || recoveries[0].Reason != BoundaryImplicitVRElement || recoveries[0].Path != "(0008,0080)" || recoveries[1].Path != "(0028,0010)" {
		t.Fatalf("per-element diagnostics: %+v, truncated=%v", recoveries, truncated)
	}
	options.RecoverImplicitVRElements = false
	if _, err := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, options).ReadAll(); err == nil {
		t.Fatal("strict parsing accepted mixed VR")
	}
	options.RecoverImplicitVRElements = true
	options.MaxImplicitVRElements = 1
	if _, err := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, options).ReadAll(); !errors.Is(err, ErrMaxImplicitVRElements) {
		t.Fatalf("recovery budget = %v, want ErrMaxImplicitVRElements", err)
	}
}

func TestMixedVRSelectiveStopUsesPhysicalPixelHeader(t *testing.T) {
	stream, _, headerOffset := mixedVRPixelStream()
	for _, seekable := range []bool{false, true} {
		var source io.Reader = bytes.NewReader(stream)
		if !seekable {
			source = struct{ io.Reader }{source}
		}
		reader, err := NewSelectiveReader(context.Background(), source, transfer.ExplicitVRLittleEndian,
			ReaderOptions{Dictionary: std.Dictionary, RecoverImplicitVRElements: true, OddLengthPolicy: AcceptOddLength},
			SelectiveReaderOptions{Select: func(_ context.Context, _ validation.Path, header core.ElementHeader, offset int64) (SelectiveDisposition, error) {
				if header.Tag == core.NewTag(0x7fe0, 0x0010) {
					if offset != headerOffset {
						t.Fatalf("seekable=%v header offset=%d, want %d", seekable, offset, headerOffset)
					}
					return SelectiveStop, nil
				}
				return SelectiveMaterialize, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		for {
			token, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			if token.Kind == TokenStop {
				if token.Offset != headerOffset || reader.Position() != headerOffset+12 {
					t.Fatalf("seekable=%v stop offset=%d position=%d, header=%d", seekable, token.Offset, reader.Position(), headerOffset)
				}
				break
			}
		}
	}
}
