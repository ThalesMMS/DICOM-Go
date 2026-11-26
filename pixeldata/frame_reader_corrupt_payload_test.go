package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFrameReaderCorruptMiddleFrameIsIsolated(t *testing.T) {
	ctx := context.Background()
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	opts := pixeldata.FrameReaderOptions{Registry: registry}
	// A 64-byte header followed by a literal run of three samples.
	literal := func(samples ...byte) []byte {
		header := make([]byte, 64)
		binary.LittleEndian.PutUint32(header, 1)
		binary.LittleEndian.PutUint32(header[4:], 64)
		return append(append(header, 2), samples...)
	}
	first, last := []byte{17, 29, 41}, []byte{53, 67, 79}
	for _, tc := range []struct {
		name   string
		middle []byte
		want   error
	}{
		{"short-header", []byte{1, 0}, rle.ErrInvalidHeader},
		{"truncated-literal", literal(83), rle.ErrSegmentDecodeFailed},
	} {
		for _, storage := range []string{"materialized", "deferred"} {
			t.Run(tc.name+"/"+storage, func(t *testing.T) {
				dataset := streamTestSource(1, 3, 3, 1, 8)
				dataset.Put(core.Element{
					Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
					Value:  core.FragmentSequence{Fragments: [][]byte{literal(first...), tc.middle, literal(last...)}},
				})
				file := &object.File{Dataset: dataset, TransferSyntax: transfer.RLELossless}
				var reader *pixeldata.FrameReader
				var err error
				if storage == "materialized" {
					reader, err = pixeldata.NewFrameReader(ctx, file, nil, opts)
				} else {
					path := writeStreamTestFile(t, t.TempDir(), "rle-with-bad-frame.dcm", file)
					reader, err = pixeldata.OpenFrameReader(ctx, path, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reader.Close() })
				verifyFrame := func(i int, want []byte) {
					t.Helper()
					got, metadata, err := reader.Frame(ctx, i)
					if err != nil || !bytes.Equal(got, want) || metadata.NumberOfFrames != 1 {
						t.Fatalf("frame %d = %v, frames = %d, error = %v", i, got, metadata.NumberOfFrames, err)
					}
				}
				verifyFrame(2, last)
				verifyFrame(0, first)
				raw, err := reader.RawFrame(ctx, 1)
				if err != nil || !bytes.Equal(raw, tc.middle) {
					t.Fatalf("encoded frame 1 = %x, error = %v", raw, err)
				}
				got, metadata, err := reader.Frame(ctx, 1)
				if !errors.Is(err, tc.want) || got != nil || metadata != (pixeldata.Metadata{}) {
					t.Fatalf("decoded frame 1 = %v, metadata = %+v, error = %v, want %v", got, metadata, err, tc.want)
				}
				dst := make([]byte, 3)
				if _, err := reader.FrameInto(ctx, 1, dst); !errors.Is(err, tc.want) {
					t.Fatalf("FrameInto frame 1 error = %v, want %v", err, tc.want)
				}
				verifyFrame(2, last)
				verifyFrame(0, first)
			})
		}
	}
}
