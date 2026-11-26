package rle

import (
	"bytes"
	"context"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// DCMTK dcmdata/tests/tfrmsiz.cc:99-134 checks decoded size against
// Bits Allocated. Exercise the actual caller-buffer path with hand-authored
// PackBits planes, including RLE's 15-segment limit.
func TestRLEDCMTKDecodedSizeAndBytePlaneOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		samples, bits uint16
		photometric   string
		planeOrder    []byte
	}{
		{"mono64", 1, 64, "MONOCHROME2", []byte{8, 7, 6, 5, 4, 3, 2, 1}},
		{"mono120-at-segment-limit", 1, 120, "MONOCHROME2", []byte{15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{"rgb32-planar-declaration", 3, 32, "RGB", []byte{4, 3, 2, 1, 8, 7, 6, 5, 12, 11, 10, 9}},
		{"rgb40-at-segment-limit", 3, 40, "RGB", []byte{5, 4, 3, 2, 1, 10, 9, 8, 7, 6, 15, 14, 13, 12, 11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := encoderMetadata(2, 3, tc.samples, tc.bits, tc.photometric)
			metadata.NumberOfFrames = 4
			if tc.samples == 3 {
				metadata.PlanarConfiguration = 1
			}
			segments := make([][]byte, len(tc.planeOrder))
			for i, lane := range tc.planeOrder {
				// Two explicitly encoded rows of three literals each.
				segments[i] = []byte{2, lane, lane + 16, lane + 32, 2, lane + 48, lane + 64, lane + 80}
			}
			stream := rleFragmentEncoded(segments...)
			before := bytes.Clone(stream)
			for _, request := range []struct {
				name string
				pixeldata.FrameBufferRequest
				pixelOffsets []byte
			}{
				{"whole", pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, 3, 2), WholeFrame: true}, []byte{0, 16, 32, 48, 64, 80}},
				{"crop", pixeldata.FrameBufferRequest{Region: image.Rect(1, 0, 3, 2)}, []byte{16, 32, 64, 80}},
			} {
				t.Run(request.name, func(t *testing.T) {
					want := make([]byte, 0, len(request.pixelOffsets)*len(tc.planeOrder))
					for _, pixel := range request.pixelOffsets {
						for lane := range tc.planeOrder {
							want = append(want, pixel+byte(lane+1))
						}
					}
					codec := New()
					ctx := context.Background()
					out, err := codec.BufferMetadata(ctx, metadata, request.FrameBufferRequest)
					if err != nil {
						t.Fatal(err)
					}
					if out.FrameSize() != int64(len(want)) || out.Rows != uint16(request.Region.Dy()) ||
						out.Columns != uint16(request.Region.Dx()) || out.NumberOfFrames != 1 ||
						out.BitsAllocated != tc.bits || out.BitsStored != tc.bits || out.SamplesPerPixel != tc.samples ||
						out.PlanarConfiguration != 0 {
						t.Fatalf("buffer metadata = %+v, want %d decoded bytes in %v", out, len(want), request.Region)
					}

					short := bytes.Repeat([]byte{0xcd}, len(want)-1)
					n, diagnostics, err := codec.DecodeFrameInto(ctx, stream, metadata, request.FrameBufferRequest, short)
					var sizeErr *pixeldata.BufferSizeError
					if !errors.As(err, &sizeErr) || sizeErr.Required != int64(len(want)) || sizeErr.Provided != int64(len(short)) ||
						n != 0 || len(diagnostics) != 0 || bytes.Count(short, []byte{0xcd}) != len(short) {
						t.Fatalf("short decode wrote %d bytes, diagnostics %v, error %v, dst %x", n, diagnostics, err, short)
					}
					tail := []byte{0xcd, 0xce}
					dst := append(make([]byte, len(want)), tail...)
					n, diagnostics, err = codec.DecodeFrameInto(ctx, stream, metadata, request.FrameBufferRequest, dst)
					if err != nil || n != len(want) || len(diagnostics) != 0 ||
						!bytes.Equal(dst[:len(want)], want) || !bytes.Equal(dst[len(want):], tail) || !bytes.Equal(stream, before) {
						t.Fatalf("decode wrote %d bytes with diagnostics %v, err %v, dst %x, want %x followed by %x", n, diagnostics, err, dst, want, tail)
					}
				})
			}
		})
	}
}

func TestRLEDCMTKInvalidDecodedSizeLeavesCallerBufferIntact(t *testing.T) {
	const rows, columns = 2, 3
	request := pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, columns, rows), WholeFrame: true}
	stream := rleFragmentEncoded([]byte{2, 1, 2, 3, 2, 4, 5, 6})
	for _, tc := range []struct {
		name          string
		samples, bits uint16
		stored        uint16
		photometric   string
		want          error
	}{
		{"stored-exceeds-allocated", 1, 8, 9, "MONOCHROME2", pixeldata.ErrIncompatiblePixelData},
		{"5-allocated-bits", 1, 5, 1, "MONOCHROME2", ErrUnsupportedBitsAllocated},
		{"12-allocated-bits", 1, 12, 12, "MONOCHROME2", ErrUnsupportedBitsAllocated},
		{"13-allocated-bits", 1, 13, 8, "MONOCHROME2", ErrUnsupportedBitsAllocated},
		{"16-mono-segments", 1, 128, 64, "MONOCHROME2", ErrUnsupportedBitsAllocated},
		{"18-RGB-segments", 3, 48, 40, "RGB", ErrUnsupportedBitsAllocated},
		{"subsampled-RLE", 3, 8, 8, "YBR_FULL_422", pixeldata.ErrUnsupportedPhotometricInterpretation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := encoderMetadata(rows, columns, tc.samples, tc.bits, tc.photometric)
			metadata.BitsStored, metadata.HighBit = tc.stored, tc.stored-1
			codec := New()
			ctx := context.Background()
			out, err := codec.BufferMetadata(ctx, metadata, request)
			if !errors.Is(err, tc.want) || out != (pixeldata.Metadata{}) {
				t.Fatalf("buffer metadata = %+v, %v, want no metadata and %v", out, err, tc.want)
			}
			dst := bytes.Repeat([]byte{0xcd}, 128)
			n, diagnostics, err := codec.DecodeFrameInto(ctx, stream, metadata, request, dst)
			if !errors.Is(err, tc.want) || n != 0 || len(diagnostics) != 0 || bytes.Count(dst, []byte{0xcd}) != len(dst) {
				t.Fatalf("rejected decode wrote %d bytes, diagnostics %v, error %v, want %v", n, diagnostics, err, tc.want)
			}
		})
	}
}
