package jpegls

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func cropMonoFrame(frame []byte, metadata pixeldata.Metadata, region image.Rectangle) []byte {
	sampleBytes := metadata.BytesPerSample()
	stride := int(metadata.Columns) * sampleBytes
	roi := make([]byte, 0, region.Dx()*region.Dy()*sampleBytes)
	for y := region.Min.Y; y < region.Max.Y; y++ {
		roi = append(roi, frame[y*stride+region.Min.X*sampleBytes:y*stride+region.Max.X*sampleBytes]...)
	}
	return roi
}

func checkRegionalFrame(t *testing.T, file *object.File, expected []byte) {
	t.Helper()
	ctx := context.Background()
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNearLossless(registry); err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	caps, err := reader.PartialDecodeCapabilities(ctx, 0)
	if err != nil || caps != (pixeldata.PartialDecodeCapabilities{Region: true}) {
		t.Fatalf("capabilities=%+v error=%v", caps, err)
	}
	full, meta, err := reader.Frame(ctx, 0)
	if err != nil || !bytes.Equal(full, expected) {
		t.Fatalf("full decode differs from independent reconstruction: %v", err)
	}
	w, h := int(meta.Columns), int(meta.Rows)
	for _, region := range []image.Rectangle{
		image.Rect(0, 0, 1, 1),
		image.Rect(w-1, h-1, w, h),
		image.Rect(w/3, h/2, w, h),
		image.Rect(0, 0, w, h),
	} {
		want := cropMonoFrame(expected, meta, region)
		cropped, err := reader.DecodePartial(ctx, pixeldata.PartialDecodeRequest{Region: region, NoFallback: true})
		if err != nil || cropped.Path != pixeldata.PartialDecodeCodec || cropped.Fallback || cropped.Region != region || !bytes.Equal(cropped.Data, want) {
			t.Fatalf("partial decode %v path=%s fallback=%t result=%v error=%v", region, cropped.Path, cropped.Fallback, cropped.Region, err)
		}
		buf := bytes.Repeat([]byte{0xdc}, len(want)+1)
		into, err := reader.RegionInto(ctx, 0, region, buf)
		if err != nil || into.Layout.Mode != pixeldata.BufferCodecDirect || into.Written != len(want) || !bytes.Equal(buf[:len(want)], want) || buf[len(want)] != 0xdc {
			t.Fatalf("region into %v result=%+v error=%v", region, into, err)
		}
	}
}

func TestJPEGLSRegionalDecodeMatchesIndependentReconstructions(t *testing.T) {
	t.Run("CharLS8bit", func(t *testing.T) {
		meta := encoderMetadata(2, 2, 1, 8, "MONOCHROME2")
		dataset, _ := jpeglsObjectWithFragment(t, meta, charLSLossless2x2)
		checkRegionalFrame(t, &object.File{Dataset: dataset, TransferSyntax: transfer.JPEGLSLossless}, []byte{0, 64, 128, 255})
	})
	root := filepath.Join("..", "codecfixture", "testdata", "codecfull", "pydicom")
	for _, fixture := range []struct {
		name, compressed, reference string
	}{
		{"CharLS16bit", "MR_small_jpeg_ls_lossless.dcm", "MR_small.dcm"},
		{"NearLossless8bit", "JPEGLSNearLossless_08.dcm", "JPEGLSNearLossless_08.raw"},
		{"NearLossless16bit", "JPEGLSNearLossless_16.dcm", "JPEGLSNearLossless_16.raw"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			file, err := object.OpenFile(filepath.Join(root, fixture.compressed))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var want []byte
			if filepath.Ext(fixture.reference) == ".dcm" {
				native, err := object.OpenFile(filepath.Join(root, fixture.reference))
				if err != nil {
					t.Fatal(err)
				}
				defer native.Close()
				pixels, err := pixeldata.ExtractNativeFramesView(native.Dataset)
				if err != nil {
					t.Fatal(err)
				}
				want = pixels.Data[0]
			} else {
				want, err = os.ReadFile(filepath.Join(root, fixture.reference))
				if err != nil {
					t.Fatal(err)
				}
			}
			checkRegionalFrame(t, file, want)
		})
	}
}

func TestJPEGLSRegionValidatesPresetAndCompleteEntropy(t *testing.T) {
	ctx := context.Background()
	codec := New()
	meta := encoderMetadata(31, 73, 1, 8, "MONOCHROME1")
	native := make([]byte, meta.FrameSize())
	for i := range native {
		native[i] = byte(i*17 + i/7)
	}
	stream := encodedFrame(t, native, meta)
	sos := markerOffset(t, stream, 0xda)
	t1, t2, t3 := defaultThresholds(255)
	preset := insertAt(stream, sos, lseID1(255, uint16(t1), uint16(t2), uint16(t3), basicReset))
	region := image.Rect(72, 30, 73, 31)
	got, err := codec.DecodeFrameRegion(ctx, preset, meta, region)
	want := native[len(native)-1:]
	if err != nil || len(got.Data) != 1 || !bytes.Equal(got.Data[0], want) {
		t.Fatalf("explicit preset bottom-right pixel = %v, error=%v, want %v", got.Data, err, want)
	}

	// Leave syntactically valid EOI and enough entropy to reach an early ROI,
	// but truncate rows after it. The ROI must fail rather than publish.
	mid := len(preset) / 2
	for preset[mid-1] == 0xff {
		mid--
	}
	shortEntropy := append(bytes.Clone(preset[:mid]), 0xff, 0xd9)
	if output, err := codec.DecodeFrameRegion(ctx, shortEntropy, meta, image.Rect(0, 0, 1, 1)); !errors.Is(err, io.ErrUnexpectedEOF) || len(output.Data) != 0 {
		t.Fatalf("late truncated entropy published %d outputs, err=%v", len(output.Data), err)
	}
	if output, err := codec.DecodeFrameRegion(ctx, preset[:len(preset)-2], meta, region); !errors.Is(err, ErrInvalidCodestream) || len(output.Data) != 0 {
		t.Fatalf("missing EOI published %d outputs, err=%v", len(output.Data), err)
	}
	if output, err := codec.DecodeFrameRegion(&decoderCancelContext{Context: ctx, remaining: 40}, preset, meta, region); !errors.Is(err, context.Canceled) || len(output.Data) != 0 {
		t.Fatalf("canceled region published %d outputs, err=%v", len(output.Data), err)
	}
}

func TestJPEGLSColorRegionRequiresExplicitFallback(t *testing.T) {
	ctx := context.Background()
	meta := encoderMetadata(16, 32, 3, 8, "RGB")
	native := interleaveSource(32, 16, 8, 8, 3)
	stream := encodedFrame(t, native, meta)
	dataset, _ := jpeglsObjectWithFragment(t, meta, stream)
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(ctx, &object.File{Dataset: dataset, TransferSyntax: transfer.JPEGLSLossless}, nil, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	caps, err := reader.PartialDecodeCapabilities(ctx, 0)
	if err != nil || caps != (pixeldata.PartialDecodeCapabilities{}) {
		t.Fatalf("color capabilities=%+v error=%v", caps, err)
	}
	request := pixeldata.PartialDecodeRequest{Region: image.Rect(0, 0, 1, 1), NoFallback: true}
	if _, err := reader.DecodePartial(ctx, request); !errors.Is(err, pixeldata.ErrPartialDecodeUnsupported) {
		t.Fatalf("color region without fallback = %v", err)
	}
	request.NoFallback = false
	result, err := reader.DecodePartial(ctx, request)
	if err != nil || !result.Fallback || result.Path != pixeldata.PartialDecodeFullFrame || !bytes.Equal(result.Data, native[:3]) {
		t.Fatalf("color fallback path=%s applied=%t error=%v", result.Path, result.Fallback, err)
	}
}

func BenchmarkJPEGLSRegionalDecode(b *testing.B) {
	const side = 2048
	ctx := context.Background()
	meta := encoderMetadata(side, side, 1, 16, "MONOCHROME2")
	tile := ctBenchFrame()
	frame := bytes.Repeat(tile, int(meta.FrameSize())/len(tile))
	encoded, err := NewEncoder().EncodeFrame(ctx, frame, meta)
	if err != nil {
		b.Fatal(err)
	}
	codec := New()
	region := image.Rect(side-96, side-96, side, side)
	for _, mode := range []string{"full_crop", "region", "region_into"} {
		b.Run(mode, func(b *testing.B) {
			var out []byte
			if mode == "region_into" {
				out = make([]byte, region.Dx()*region.Dy()*meta.BytesPerSample())
			}
			request := pixeldata.FrameBufferRequest{Region: region}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch mode {
				case "full_crop":
					full, err := decodeFrame(encoded.Data, meta)
					if err != nil {
						b.Fatal(err)
					}
					out = cropMonoFrame(full, meta, region)
				case "region":
					got, err := codec.DecodeFrameRegion(ctx, encoded.Data, meta, region)
					if err != nil {
						b.Fatal(err)
					}
					out = got.Data[0]
				case "region_into":
					if _, _, err := codec.DecodeFrameInto(ctx, encoded.Data, meta, request, out); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			if want := cropMonoFrame(frame, meta, region); !bytes.Equal(out, want) {
				b.Fatal("benchmark output differs")
			}
			// Peak active raster buffers, excluding the
			// compressed input, codec state, and idle pool capacity.
			rows := 3 * side * int(nativeIntBytes())
			liveBytes := len(out) + rows
			if mode == "full_crop" {
				liveBytes = len(frame) + max(rows, len(out))
			}
			b.ReportMetric(float64(liveBytes), "peak-raster-B")
		})
	}
}
