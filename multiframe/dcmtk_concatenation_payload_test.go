package multiframe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// DCMTK dcmfg/tests/t_concatenation_loader.cc exercises short native Pixel
// Data independently of the concatenation metadata. Here Locate resolves
// that metadata and the public FrameReader enforces the payload boundary.
func TestDCMTKReferenceConcatenationShortNativePayload(t *testing.T) {
	const columns, framesPerPart = 13, 3
	var words []byte
	for range columns {
		words = binary.LittleEndian.AppendUint16(words, 0x3486)
	}
	for _, profile := range []struct {
		name        string
		bits        uint16
		vr          core.VR
		valid       []byte
		wantFrame   []byte
		shortLength int
	}{
		// Three 13-bit frames occupy 39 bits. Packing is continuous across
		// frames, with one even-length value padding byte.
		{"one bit", 1, core.VROB, []byte{0xff, 0xff, 0xff, 0xff, 0x7f, 0}, []byte{0xff, 0x1f}, 2},
		{"eight bit", 8, core.VROB, append(bytes.Repeat([]byte{0x65}, 39), 0), bytes.Repeat([]byte{0x65}, 13), 14},
		{"sixteen bit", 16, core.VROW, bytes.Repeat(words, 3), words, 26},
	} {
		for _, tc := range []struct {
			name    string
			payload []byte
			want    error
		}{
			{"complete", profile.valid, nil},
			{"only first frame of last part", profile.valid[:profile.shortLength], pixeldata.ErrPixelDataSizeMismatch},
			{"last word missing", profile.valid[:len(profile.valid)-2], pixeldata.ErrPixelDataSizeMismatch},
		} {
			t.Run(profile.name+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				parts := splitForTest(t, enhancedFixture(t, 2*framesPerPart), framesPerPart)
				for i, part := range parts {
					part.Dataset.Put(derivedio.US(tagRows, 1))
					part.Dataset.Put(derivedio.US(tagColumns, columns))
					part.Dataset.Put(derivedio.US(tagBitsAllocated, profile.bits))
					part.Dataset.Put(derivedio.US(tagBitsStored, profile.bits))
					part.Dataset.Put(derivedio.US(tagHighBit, profile.bits-1))
					payload := profile.valid
					if i == len(parts)-1 {
						payload = tc.payload
					}
					part.Dataset.Put(core.NewRawElement(core.TagPixelData, profile.vr, payload))
					var wire bytes.Buffer
					if err := object.WriteFile(&wire, part); err != nil {
						t.Fatalf("WriteFile for part %d: %v", i+1, err)
					}
					read, err := object.ReadFile(&wire)
					if err != nil {
						t.Fatalf("ReadFile for part %d: %v", i+1, err)
					}
					parts[i] = read
					t.Cleanup(func() { _ = read.Close() })
				}
				concat, err := NewConcatenation([]*object.File{parts[1], parts[0]})
				if err != nil || concat.NumberOfFrames() != 2*framesPerPart {
					t.Fatalf("NewConcatenation = %+v, %v", concat, err)
				}
				for i, logicalFrame := range []int{framesPerPart - 1, 2*framesPerPart - 1} {
					part, local, err := concat.Locate(logicalFrame)
					if err != nil || part.File != parts[i] || local != framesPerPart-1 {
						t.Fatalf("Locate(%d) = %+v, %d, %v", logicalFrame, part, local, err)
					}
					reader, err := pixeldata.NewFrameReader(ctx, part.File, nil, pixeldata.FrameReaderOptions{})
					if i == 1 && tc.want != nil {
						if reader != nil || !errors.Is(err, tc.want) {
							t.Fatalf("short part published a reader or returned the wrong error: %+v, %v", reader, err)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					pixels, _, err := reader.Frame(ctx, local)
					if err != nil || !bytes.Equal(pixels, profile.wantFrame) {
						t.Fatalf("frame %d = %x, %v, want %x", logicalFrame, pixels, err, profile.wantFrame)
					}
				}
			})
		}
	}
}
