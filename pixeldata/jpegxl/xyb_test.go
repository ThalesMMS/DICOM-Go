package jpegxl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var xybFixtureNames = []string{"tiny-1", "tiny-5", "colors-3", "plain-1", "chaos-1", "tile-3", "groups-1"}

func xybFixture(t *testing.T, name string) ([]byte, *Image) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "xyb", name+".jxl"))
	if err != nil {
		t.Fatal(err)
	}
	ppm, err := os.ReadFile(filepath.Join("testdata", "xyb", name+".ppm"))
	if err != nil {
		t.Fatal(err)
	}
	parts := bytes.SplitN(ppm, []byte{'\n'}, 4)
	var width, height, maxValue int
	if len(parts) != 4 || string(parts[0]) != "P6" {
		t.Fatal("bad fixture PPM")
	}
	if _, err := fmt.Sscanf(string(parts[1])+" "+string(parts[2]), "%d %d %d", &width, &height, &maxValue); err != nil || maxValue != 255 {
		t.Fatal("bad fixture PPM geometry")
	}
	if len(parts[3]) != width*height*3 {
		t.Fatal("bad fixture PPM samples")
	}
	expected := &Image{Width: width, Height: height, Channels: 3, BitsPerSample: 8}
	for _, sample := range parts[3] {
		expected.Samples = append(expected.Samples, int32(sample))
	}
	return payload, expected
}

func requireXYBOracle(t *testing.T, got, want *Image) {
	t.Helper()
	if got == nil || got.Width != want.Width || got.Height != want.Height ||
		got.Channels != want.Channels || got.BitsPerSample != want.BitsPerSample || len(got.Samples) != len(want.Samples) {
		t.Fatalf("decoded geometry differs from independent reference")
	}
	for i, sample := range got.Samples {
		delta := sample - want.Samples[i]
		if delta < -1 || delta > 1 {
			t.Fatalf("sample %d = %d, libjxl = %d, tolerance 1", i, sample, want.Samples[i])
		}
	}
}

func TestXYBVarDCTIndependentPixels(t *testing.T) {
	for _, name := range xybFixtureNames {
		t.Run(name, func(t *testing.T) {
			payload, want := xybFixture(t, name)
			input := bytes.Clone(payload)
			got, err := Decode(payload)
			if err != nil {
				t.Fatal(err)
			}
			requireXYBOracle(t, got, want)
			if !bytes.Equal(input, payload) {
				t.Fatal("decoder changed borrowed input")
			}
		})
	}
}

func TestXYBVarDCTTruncatedInput(t *testing.T) {
	payload, _ := xybFixture(t, "colors-3")
	for length := 0; length < len(payload); length++ {
		if got, err := Decode(payload[:length]); !errors.Is(err, ErrInvalidCodestream) || got != nil {
			t.Fatalf("prefix %d: image=%v err=%v", length, got != nil, err)
		}
	}
	corrupt := append(bytes.Clone(payload), 0, 0)
	if _, err := Decode(corrupt); !errors.Is(err, ErrInvalidCodestream) {
		t.Fatalf("trailing bytes: %v", err)
	}
	if len(payload)&1 != 0 {
		padded, err := Decode(append(bytes.Clone(payload), 0))
		if err != nil || padded == nil {
			t.Fatalf("legal DICOM padding: %v", err)
		}
	}
}

func TestXYBVarDCTRejectsOutsideProfile(t *testing.T) {
	for _, name := range []string{"unsupported-transform", "unsupported-filter"} {
		data, err := os.ReadFile(filepath.Join("testdata", "xyb", name+".jxl"))
		if err != nil {
			t.Fatal(err)
		}
		if img, err := Decode(data); !errors.Is(err, ErrUnsupported) || img != nil {
			t.Errorf("%s: image=%v error=%v", name, img != nil, err)
		}
	}
}

func TestXYBVarDCTHeaderLimits(t *testing.T) {
	payload, _ := xybFixture(t, "tiny-1")
	for _, tc := range []struct {
		name string
		edit func(*imageMetadata, *frameHeader)
		want error
	}{
		{"width", func(m *imageMetadata, _ *frameHeader) { m.width = maxXYBDimension + 1 }, ErrLimit},
		{"height", func(m *imageMetadata, _ *frameHeader) { m.height = maxXYBDimension + 1 }, ErrLimit},
		{"bit depth", func(m *imageMetadata, _ *frameHeader) { m.depth.bitsPerSample = 16 }, ErrUnsupported},
		{"ICC", func(m *imageMetadata, _ *frameHeader) { m.colour.isSRGB = false }, ErrUnsupported},
		{"custom opsin", func(m *imageMetadata, _ *frameHeader) { m.customOpsin = true }, ErrUnsupported},
		{"tone map", func(m *imageMetadata, _ *frameHeader) { m.customTone = true }, ErrUnsupported},
		{"animation", func(m *imageMetadata, _ *frameHeader) { m.haveAnimation = true }, ErrUnsupported},
		{"alpha", func(m *imageMetadata, _ *frameHeader) { m.extraChannels = []extraChannelInfo{{}} }, ErrUnsupported},
		{"orientation", func(m *imageMetadata, _ *frameHeader) { m.orientation = 6 }, ErrUnsupported},
		{"multipass", func(_ *imageMetadata, f *frameHeader) { f.numPasses = 2 }, ErrUnsupported},
		{"LF frame", func(_ *imageMetadata, f *frameHeader) { f.flags |= flagUseLF }, ErrUnsupported},
		{"blend", func(_ *imageMetadata, f *frameHeader) { f.blendMode = 1 }, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br := newBitReader(payload)
			hdr, err := readCodestreamHeader(br)
			if err != nil {
				t.Fatal(err)
			}
			f, offsets, sizes, err := readFirstFrame(br, hdr)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&hdr.meta, f)
			_, err = decodeXYB(context.Background(), hdr, br, payload, f, offsets, sizes)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

// A cancellation receipt with deterministic checkpoint counts avoids timing
// races while proving early and in-frame cancellation reaches the caller.
type xybCancelContext struct {
	context.Context
	mu              sync.Mutex
	calls, cancelAt int
}

func (c *xybCancelContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestXYBVarDCTCancellation(t *testing.T) {
	payload, expected := xybFixture(t, "groups-1")
	for _, checkpoint := range []int{1, 3, 20, 70} {
		ctx := &xybCancelContext{Context: context.Background(), cancelAt: checkpoint}
		got, err := decode(ctx, payload, true)
		if !errors.Is(err, context.Canceled) || got != nil {
			t.Fatalf("checkpoint %d: image=%v err=%v", checkpoint, got != nil, err)
		}
	}
	ctx := &xybCancelContext{Context: context.Background(), cancelAt: 20}
	pixel := pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{payload}}}
	frames, err := New().DecodeContext(ctx, pixel, xybObject(expected, payload))
	if !errors.Is(err, context.Canceled) || len(frames.Data) != 0 {
		t.Fatalf("cancelled DICOM decode returned frames=%d error=%v", len(frames.Data), err)
	}
}

func xybObject(expected *Image, payload []byte) *object.Object {
	return object.FromElements([]core.Element{
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, uint16(expected.Height)),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, uint16(expected.Width)),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, 3),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, 8),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, 8),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, 7),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, 0),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, payload),
	}, nil)
}

func TestXYBVarDCTFrameReaderBuffer(t *testing.T) {
	payload, want := xybFixture(t, "colors-3")
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(context.Background(), &object.File{
		Dataset: xybObject(want, payload), TransferSyntax: transfer.JPEGXL,
	}, nil, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	size := len(want.Samples)
	short := bytes.Repeat([]byte{0xcd}, size-1)
	if _, err := reader.FrameInto(context.Background(), 0, short); err == nil || !bytes.Equal(short, bytes.Repeat([]byte{0xcd}, len(short))) {
		t.Fatal("short destination was accepted or modified")
	}
	out := bytes.Repeat([]byte{0xcd}, size+8)
	result, err := reader.FrameInto(context.Background(), 0, out)
	if err != nil || result.Written != size || !bytes.Equal(out[size:], bytes.Repeat([]byte{0xcd}, 8)) {
		t.Fatalf("destination result=%+v error=%v", result, err)
	}
	for i, v := range out[:size] {
		if d := int32(v) - want.Samples[i]; d < -1 || d > 1 {
			t.Fatalf("DICOM sample %d differs by %d", i, d)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	untouched := bytes.Repeat([]byte{0xfe}, size)
	if _, err := reader.FrameInto(ctx, 0, untouched); !errors.Is(err, context.Canceled) ||
		!bytes.Equal(untouched, bytes.Repeat([]byte{0xfe}, size)) {
		t.Fatal("cancelled destination was modified")
	}
}

func TestXYBVarDCTRegistryScope(t *testing.T) {
	payload, expected := xybFixture(t, "tiny-1")
	obj := xybObject(expected, payload)
	pixel := pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{payload}}}
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.JPEGXLLossless, transfer.JPEGXLJPEGRecompression} {
		codec, ok := registry.GetCodec(syntax.UID)
		if !ok {
			t.Fatal("registered codec absent")
		}
		if _, err := codec.Decode(pixel, obj); !errors.Is(err, ErrUnsupported) {
			t.Errorf("syntax %s accepted lossy XYB: %v", syntax.UID, err)
		}
	}
	defaults, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := defaults.GetCodec(transfer.JPEGXL.UID); ok {
		t.Fatal("default registry gained a JPEG XL decoder")
	}
}

func TestXYBVarDCTConcurrentDecode(t *testing.T) {
	payload, want := xybFixture(t, "groups-1")
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for i := 0; i < cap(errorsCh); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Decode(payload)
			if err == nil {
				for i, sample := range got.Samples {
					if d := sample - want.Samples[i]; d < -1 || d > 1 {
						err = fmt.Errorf("sample %d differs by %d", i, d)
						break
					}
				}
			}
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestXYBVarDCTFixtureHashes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "xyb", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []struct {
			Path  string `json:"path"`
			Hash  string `json:"sha256"`
			Bytes int    `json:"bytes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 16 {
		t.Fatalf("manifest files=%d, want 16", len(manifest.Files))
	}
	for _, item := range manifest.Files {
		raw, err := os.ReadFile(filepath.Join("testdata", "xyb", item.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if len(raw) != item.Bytes || fmt.Sprintf("%x", sum) != item.Hash {
			t.Errorf("fixture identity changed: %s", item.Path)
		}
	}
}
