package jpegls

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestJPEGLSCallerBufferRecovery(t *testing.T) {
	mono := encoderMetadata(24, 40, 1, 16, "MONOCHROME2")
	mono.PixelRepresentation = 1
	monoPixels := jaiFrame(int(mono.Rows), int(mono.Columns), int(mono.BitsStored))
	monoStream := append(jaiStream(t, monoPixels, mono), 0x20)
	rgbStream, rgbPixels, rgb := hpFixture(t, 16, 3, 2)
	for _, tc := range []struct {
		name           string
		metadata       pixeldata.Metadata
		stream, pixels []byte
		recover        pixeldata.DecodeRecovery
		diagnostics    []string
	}{
		{"signed-mono-JAI", mono, monoStream, monoPixels, pixeldata.DecodeRecovery{JPEGLSJAIThresholds: true}, []string{pixeldata.DiagnosticNonZeroFramePad, pixeldata.DiagnosticJPEGLSJAIThresholds}},
		{"rgb-HP", rgb, rgbStream, rgbPixels, pixeldata.DecodeRecovery{JPEGLSHPColorTransforms: true}, []string{pixeldata.DiagnosticJPEGLSHP3, pixeldata.DiagnosticJPEGLSHPBackendPureGo}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, _ := jpeglsObjectWithFragment(t, tc.metadata, tc.stream)
			t.Cleanup(func() { _ = obj.Close() })
			registry := pixeldata.NewMemoryRegistry()
			if err := Register(registry); err != nil {
				t.Fatal(err)
			}
			reader, err := pixeldata.NewFrameReader(nil, &object.File{Dataset: obj, TransferSyntax: transfer.JPEGLSLossless}, nil, pixeldata.FrameReaderOptions{Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			dst := bytes.Repeat([]byte{0xdc}, len(tc.pixels)+3)
			ctx := pixeldata.DecodeContextWithRecovery(context.Background(), tc.recover)
			got, err := reader.FrameInto(ctx, 0, dst)
			if err != nil || got.Written != len(tc.pixels) || got.Layout.Mode != pixeldata.BufferCodecDirect || got.Layout.Metadata != tc.metadata {
				t.Fatalf("FrameInto = %+v, %v", got, err)
			}
			if !slices.Equal(got.Diagnostics, tc.diagnostics) || !bytes.Equal(dst[:got.Written], tc.pixels) || !bytes.Equal(dst[got.Written:], []byte{0xdc, 0xdc, 0xdc}) {
				t.Fatalf("decoded bytes, diagnostics or tail mismatch. Diagnostics = %v", got.Diagnostics)
			}
			if tc.metadata.SamplesPerPixel == 1 {
				region := image.Rect(15, 7, 23, 13)
				want := cropMonoFrame(tc.pixels, tc.metadata, region)
				dst := make([]byte, len(want))
				got, err := reader.RegionInto(ctx, 0, region, dst)
				if err != nil || got.Written != len(want) || !bytes.Equal(dst, want) || !slices.Equal(got.Diagnostics, tc.diagnostics) {
					t.Fatalf("region recovery = %+v, %v, matching pixels=%t", got, err, bytes.Equal(dst, want))
				}
			}
		})
	}
}

func TestJPEGLSCallerBufferFailureExtent(t *testing.T) {
	metadata := encoderMetadata(24, 40, 1, 16, "MONOCHROME2")
	frame := jaiFrame(int(metadata.Rows), int(metadata.Columns), int(metadata.BitsStored))
	stream := encodedFrame(t, frame, metadata)
	middle := len(stream) / 2
	for stream[middle-1] == 0xff {
		middle--
	}
	shortEntropy := append(bytes.Clone(stream[:middle]), 0xff, 0xd9)
	request := pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows)), WholeFrame: true}
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		stream      []byte
		wantErr     error
		wantWritten int
	}{
		{"truncated-markers", nil, stream[:len(stream)-3], ErrInvalidCodestream, 0},
		{"truncated-entropy", nil, shortEntropy, io.ErrUnexpectedEOF, len(frame)},
		{"canceled-in-scan", &decoderCancelContext{Context: context.Background(), remaining: 15}, stream, context.Canceled, len(frame)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := bytes.Repeat([]byte{0xec}, len(frame)+5)
			before := bytes.Clone(dst)
			n, diagnostics, err := New().DecodeFrameInto(tc.ctx, tc.stream, metadata, request, dst)
			if !errors.Is(err, tc.wantErr) || n != tc.wantWritten || len(diagnostics) != 0 || !bytes.Equal(dst[n:], before[n:]) {
				t.Fatalf("written=%d diagnostics=%v err=%v, want written=%d err=%v with untouched tail", n, diagnostics, err, tc.wantWritten, tc.wantErr)
			}
			if n > 0 && bytes.Equal(dst[:n], before[:n]) {
				t.Fatal("fixture did not reach partial output")
			}
		})
	}
}
