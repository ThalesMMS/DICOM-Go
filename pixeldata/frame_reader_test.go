package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// fullDecode decodes every frame of the file at path in memory.
func fullDecode(t testing.TB, path string, registry pixeldata.Registry) ([][]byte, pixeldata.Metadata) {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoded, _, err := pixeldata.DecompressDataSetContext(context.Background(), file.Dataset, file.TransferSyntax, pixeldata.DecompressOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFrames(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data, frames.Metadata
}

// crop cuts region from a decoded frame, the reference for Region.
func crop(frame []byte, m pixeldata.Metadata, r image.Rectangle) []byte {
	sampleBytes, samples := m.BytesPerSample(), int(m.SamplesPerPixel)
	planes, perPixel := 1, samples
	if samples > 1 && m.PlanarConfiguration == 1 {
		planes, perPixel = samples, 1
	}
	var out []byte
	for p := 0; p < planes; p++ {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			start := ((p*int(m.Rows)+y)*int(m.Columns) + r.Min.X) * perPixel * sampleBytes
			out = append(out, frame[start:start+r.Dx()*perPixel*sampleBytes]...)
		}
	}
	return out
}

func TestFrameReaderMatchesFullDecode(t *testing.T) {
	options := streamTestRegistries(t)
	dir := t.TempDir()
	planar := streamTestSource(13, 17, 3, 3, 8)
	planar.Put(core.NewRawElement(core.NewTag(0x0028, 0x0006), core.VRUS, []byte{1, 0}))
	sources := map[string]*object.File{
		"mono16":     {Dataset: streamTestSource(21, 33, 5, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian},
		"rgb8":       {Dataset: streamTestSource(16, 24, 4, 3, 8), TransferSyntax: transfer.ImplicitVRLittleEndian},
		"planar":     {Dataset: planar, TransferSyntax: transfer.ExplicitVRLittleEndian},
		"singlefile": {Dataset: streamTestSource(9, 11, 1, 1, 8), TransferSyntax: transfer.ExplicitVRLittleEndian},
	}
	paths := map[string]string{}
	for name, file := range sources {
		paths[name] = writeStreamTestFile(t, dir, name+".dcm", file)
	}
	for name, target := range map[string]transfer.Syntax{"rle16": transfer.RLELossless, "jls16": transfer.JPEGLSLossless, "bigendian": transfer.ExplicitVRBigEndian} {
		path := filepath.Join(dir, name+".dcm")
		if err := os.WriteFile(path, inMemoryTranscode(t, paths["mono16"], target, options), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	rgbRLE := filepath.Join(dir, "rlergb.dcm")
	if err := os.WriteFile(rgbRLE, inMemoryTranscode(t, paths["rgb8"], transfer.RLELossless, options), 0o600); err != nil {
		t.Fatal(err)
	}
	paths["rlergb"] = rgbRLE

	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			frames, metadata := fullDecode(t, path, options.DecoderRegistry)
			regions := []image.Rectangle{
				image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows)),
				image.Rect(1, 2, 5, 7),
				image.Rect(int(metadata.Columns)-3, int(metadata.Rows)-1, int(metadata.Columns), int(metadata.Rows)),
				image.Rect(0, 3, int(metadata.Columns), 4),
			}
			deferred, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			defer deferred.Close()
			whole, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer whole.Close()
			memory, err := pixeldata.NewFrameReader(context.Background(), whole, nil, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			for readerName, reader := range map[string]*pixeldata.FrameReader{"deferred": deferred, "memory": memory} {
				for i, want := range frames {
					got, m, err := reader.Frame(context.Background(), i)
					if err != nil {
						t.Fatalf("%s frame %d: %v", readerName, i, err)
					}
					if !bytes.Equal(got, want) || m.PhotometricInterpretation != metadata.PhotometricInterpretation || m.NumberOfFrames != 1 {
						t.Fatalf("%s frame %d differs from the full decode", readerName, i)
					}
					raw, err := reader.RawFrame(context.Background(), i)
					if length, lengthErr := reader.RawFrameLength(i); err != nil || lengthErr != nil || length != int64(len(raw)) {
						t.Fatalf("%s frame %d RawFrameLength %d for %d raw bytes (%v, %v)", readerName, i, length, len(raw), err, lengthErr)
					}
					for _, r := range regions {
						region, rm, err := reader.Region(context.Background(), i, r)
						if err != nil {
							t.Fatalf("%s frame %d region %v: %v", readerName, i, r, err)
						}
						if !bytes.Equal(region, crop(want, metadata, r)) || int(rm.Rows) != r.Dy() || int(rm.Columns) != r.Dx() {
							t.Fatalf("%s frame %d region %v differs from the cropped full decode", readerName, i, r)
						}
					}
				}
			}
		})
	}
}

func TestFrameReaderRetainsSelectedFramesAfterSourceRemoval(t *testing.T) {
	path := writeStreamTestFile(t, t.TempDir(), "selected.dcm", &object.File{
		Dataset: streamTestSource(9, 11, 2, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	want, _ := fullDecode(t, path, nil)
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".held"); err != nil {
		t.Fatal("selected reader blocked archive removal", err)
	}
	for index, expected := range want {
		got, _, err := reader.Frame(context.Background(), index)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("selected frame %d changed after unlink: %v", index, err)
		}
	}
}

func TestFrameReaderRegionRejectsSubsampled422(t *testing.T) {
	for _, photometric := range []string{"YBR_FULL_422", "YBR_PARTIAL_422"} {
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
			t.Run(photometric+"/"+syntax.UID, func(t *testing.T) {
				dataset := streamTestSource(2, 4, 1, 3, 8)
				dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte(photometric)))
				pixels := make([]byte, 2*4*2)
				pixel := core.NewRawElement(core.TagPixelData, core.VROB, pixels)
				if syntax.Encapsulated {
					pixel = core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
						Value: core.FragmentSequence{Fragments: [][]byte{pixels}}}
				}
				dataset.Put(pixel)
				reader, err := pixeldata.NewFrameReader(context.Background(), &object.File{Dataset: dataset, TransferSyntax: syntax}, nil, pixeldata.FrameReaderOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				if frame, _, err := reader.Frame(context.Background(), 0); err != nil || !bytes.Equal(frame, pixels) {
					t.Fatalf("full frame = %v, error = %v", frame, err)
				}
				if region, _, err := reader.Region(context.Background(), 0, image.Rect(3, 1, 4, 2)); !errors.Is(err, pixeldata.ErrFrameLayout) || region != nil {
					t.Fatalf("region = %v, error = %v; want ErrFrameLayout", region, err)
				}
			})
		}
	}
}

func TestFrameReaderRejectsBadRequests(t *testing.T) {
	path := writeStreamTestFile(t, t.TempDir(), "mono.dcm", &object.File{Dataset: streamTestSource(8, 8, 2, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian})
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, r := range []image.Rectangle{{}, image.Rect(0, 0, 9, 1), image.Rect(-1, 0, 2, 2)} {
		if _, _, err := reader.Region(context.Background(), 0, r); !errors.Is(err, pixeldata.ErrInvalidRegion) {
			t.Errorf("region %v: %v", r, err)
		}
	}
	if _, _, err := reader.Frame(context.Background(), 2); err == nil {
		t.Error("frame 2 of 2 read")
	}
	packed := streamTestSource(8, 8, 1, 1, 8)
	packed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, []byte{2, 0}))
	packed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, []byte{1, 0}))
	packed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, []byte{0, 0}))
	packed.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 8)))
	if _, err := pixeldata.NewFrameReader(context.Background(), &object.File{Dataset: packed, TransferSyntax: transfer.ExplicitVRLittleEndian}, nil, pixeldata.FrameReaderOptions{}); !errors.Is(err, pixeldata.ErrFrameLayout) {
		t.Errorf("unsupported 2-bit Pixel Data: %v", err)
	}
}

// TestFrameReaderRegionReadsOnlyTheRegion checks a native region read does
// not load the frame.
func TestFrameReaderRegionReadsOnlyTheRegion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.dcm")
	writeLargeNativeFile(t, path, 2048, 2048, 2) // 8 MiB frames
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	region, _, err := reader.Region(context.Background(), 1, image.Rect(1000, 1000, 1064, 1064))
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes for a 64x64 region of an 8 MiB frame", allocated)
	}
	want := binary.LittleEndian.AppendUint16(nil, uint16((1000*2048+1000)/64+131)%4096)
	if !bytes.Equal(region[:2], want) {
		t.Fatalf("first sample % x, want % x", region[:2], want)
	}
}

func benchmarkRegion(b *testing.B, path string, registry pixeldata.Registry, region image.Rectangle, whole bool) {
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		b.Fatal(err)
	}
	defer reader.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if whole {
			frame, m, err := reader.Frame(context.Background(), 0)
			if err != nil {
				b.Fatal(err)
			}
			_ = crop(frame, m, region)
		} else if _, _, err := reader.Region(context.Background(), 0, region); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFrameReaderRegion compares a 256x256 region read with decoding
// the whole 4096x4096 16-bit frame and cropping it, for native and RLE data.
func BenchmarkFrameReaderRegion(b *testing.B) {
	dir := b.TempDir()
	native := filepath.Join(dir, "native.dcm")
	writeLargeNativeFile(b, native, 4096, 4096, 1)
	options := streamTestRegistries(b)
	rlePath := filepath.Join(dir, "rle.dcm")
	file, err := object.OpenFile(native)
	if err != nil {
		b.Fatal(err)
	}
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: options.EncoderRegistry, Limits: pixeldata.TranscodeLimits{MaxPixels: 1 << 26}})
	file.Close()
	if err != nil {
		b.Fatal(err)
	}
	var data bytes.Buffer
	if err := object.WriteFile(&data, encoded); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(rlePath, data.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	top := image.Rect(1024, 256, 1280, 512)
	for _, c := range []struct {
		name  string
		path  string
		whole bool
	}{
		{"native/region", native, false}, {"native/frame+crop", native, true},
		{"rle/region", rlePath, false}, {"rle/frame+crop", rlePath, true},
	} {
		b.Run(c.name, func(b *testing.B) { benchmarkRegion(b, c.path, options.DecoderRegistry, top, c.whole) })
	}
}
