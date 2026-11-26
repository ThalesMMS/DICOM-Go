package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestJPEGExtendedRasterPeakCountsSelectedLayout(t *testing.T) {
	const monoWidth, monoHeight = 31, 19
	mono := extendedEncoderMetadata(monoHeight, monoWidth, 12)
	strict := packedMonoFixture(t, mono)
	quirked := bytes.Clone(strict)
	quirked[jpegSegment(t, quirked, 0xda)+4] = 0
	color := extendedEncoderMetadata(8, 16, 12)
	color.SamplesPerPixel = 3
	for _, tc := range []struct {
		name    string
		m       pixeldata.Metadata
		stream  []byte
		recover bool
		peak    uint64
	}{
		{"compact_mono", mono, strict, false, monoWidth * monoHeight * 2},
		{"padded_recovered_mono", mono, quirked, true, 32*24*2 + 2*monoWidth*monoHeight*2},
		{"color_444", color, color12Fixture(t, color12RGBBase64), false, 3 * 16 * 8 * 3 * 2},
		{"color_422", color, color12Fixture(t, color12YCbCr422Base64), false, (16+8+8)*8*2 + 2*16*8*3*2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			want, _, _, err := decodeExtendedProcess4Options(ctx, tc.stream, tc.m, tc.recover, 0)
			if err != nil {
				t.Fatal(err)
			}
			reserved := extendedProcess4MaxDecodedBytes - tc.peak
			got, _, ignored, err := decodeExtendedProcess4Options(ctx, tc.stream, tc.m, tc.recover, reserved)
			if err != nil || ignored != tc.recover || !bytes.Equal(got, want) {
				t.Fatalf("exact-limit decode: ignored=%t err=%v", ignored, err)
			}
			d := extendedProcess4Decoder{
				data: tc.stream, pos: 2, ctx: ctx, adobeTransform: -1,
				tolerateScanParameters: tc.recover, reservedBytes: reserved + 1,
			}
			out, _, err := d.decode(tc.m)
			if err == nil || !strings.Contains(err.Error(), "exceeds resource limit") || out != nil || len(d.packedMono) != 0 {
				t.Fatalf("over-limit decode exported %d bytes, err=%v", len(out), err)
			}
			for _, component := range d.components {
				if len(component.samples) != 0 {
					t.Fatal("over-limit decode allocated a padded plane")
				}
			}
		})
	}
}

func TestJPEGExtendedColorChargesRasterPeakBeforeEntropy(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, photometric string
		side                       uint16
		frames                     int
	}{
		{"frame_alone_exceeds_limit", color12RGBBase64, "RGB", 6000, 1},
		// A 4800² raster would fit alone. Keeping the other packed frame
		// alive during reconstruction exceeds the same request limit.
		{"retained_frame_exceeds_limit", color12RGBBase64, "RGB", 4800, 2},
		// Subsampled input still expands to three native samples per pixel.
		{"retained_subsampled_output", color12YCbCr422Base64, "YBR_FULL_422", 5000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := color12Fixture(t, tc.fixture)
			sof := jpegSegment(t, stream, 0xc1)
			binary.BigEndian.PutUint16(stream[sof+1:], tc.side)
			binary.BigEndian.PutUint16(stream[sof+3:], tc.side)
			obj, _ := colorJPEGObject(t, stream, tc.side, tc.side, 16, 12, tc.photometric)
			if err := obj.SetIS(tagNumberOfFrames, int64(tc.frames)); err != nil {
				t.Fatal(err)
			}
			fragments := make([][]byte, tc.frames)
			for i := range fragments {
				fragments[i] = stream
			}
			obj.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragments...))
			pixel, err := pixeldata.Extract(obj)
			if err != nil {
				t.Fatal(err)
			}
			frames, err := New().Decode(pixel, obj)
			if !errors.Is(err, ErrInvalidFragment) || !strings.Contains(err.Error(), "decoded frame exceeds resource limit") || len(frames.Data) != 0 {
				t.Fatalf("oversize decode exported %d frames, err=%v", len(frames.Data), err)
			}
		})
	}
}
