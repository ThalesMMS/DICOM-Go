package deflateframe_test

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagRows                      = core.NewTag(0x0028, 0x0010)
	tagColumns                   = core.NewTag(0x0028, 0x0011)
	tagSamplesPerPixel           = core.NewTag(0x0028, 0x0002)
	tagPhotometricInterpretation = core.NewTag(0x0028, 0x0004)
	tagPlanarConfiguration       = core.NewTag(0x0028, 0x0006)
	tagNumberOfFrames            = core.NewTag(0x0028, 0x0008)
	tagBitsAllocated             = core.NewTag(0x0028, 0x0100)
	tagBitsStored                = core.NewTag(0x0028, 0x0101)
	tagHighBit                   = core.NewTag(0x0028, 0x0102)
	tagPixelRepresentation       = core.NewTag(0x0028, 0x0103)
)

// image describes one synthetic, PHI-free test image.
type image struct {
	name           string
	rows, columns  uint16
	samples        uint16
	bitsAllocated  uint16
	bitsStored     uint16
	representation uint16
	photometric    string
	planar         *uint16
	frames         [][]byte
}

func (img image) metadata() pixeldata.Metadata {
	m := pixeldata.Metadata{
		Rows: img.rows, Columns: img.columns, SamplesPerPixel: img.samples,
		BitsAllocated: img.bitsAllocated, BitsStored: img.bitsStored, HighBit: img.bitsStored - 1,
		PixelRepresentation: img.representation, NumberOfFrames: len(img.frames),
		PhotometricInterpretation: img.photometric,
	}
	if img.planar != nil {
		m.PlanarConfiguration, m.PlanarConfigurationPresent = *img.planar, true
	}
	return m
}

func (img image) elements() []core.Element {
	elements := []core.Element{
		// Secondary Capture with a synthetic 2.25 UID: no patient data.
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.1248001"),
		dicomtest.Uint16Element(tagRows, core.VRUS, nil, img.rows),
		dicomtest.Uint16Element(tagColumns, core.VRUS, nil, img.columns),
		dicomtest.Uint16Element(tagSamplesPerPixel, core.VRUS, nil, img.samples),
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, img.photometric),
		dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, strconv.Itoa(len(img.frames))),
		dicomtest.Uint16Element(tagBitsAllocated, core.VRUS, nil, img.bitsAllocated),
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, img.bitsStored),
		dicomtest.Uint16Element(tagHighBit, core.VRUS, nil, img.bitsStored-1),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, img.representation),
	}
	if img.planar != nil {
		elements = append(elements, dicomtest.Uint16Element(tagPlanarConfiguration, core.VRUS, nil, *img.planar))
	}
	return elements
}

// encapsulated returns the image with the given fragments as Pixel Data.
func (img image) encapsulated(offsetTable []byte, fragments ...[]byte) *object.Object {
	return object.FromElements(append(img.elements(),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, offsetTable, fragments...)), nil)
}

// native returns the image with native Pixel Data.
func (img image) native() *object.Object {
	var raw []byte
	for _, frame := range img.frames {
		raw = append(raw, frame...)
	}
	vr := core.VROB
	if img.bitsAllocated > 8 {
		vr = core.VROW
	}
	return object.FromElements(append(img.elements(), core.NewRawElement(core.TagPixelData, vr, raw)), nil)
}

// noise is a deterministic pseudo-random sequence that deflate cannot
// shrink much, so expansion-ratio policies stay out of the way.
func noise(n int, seed uint32) []byte {
	out := make([]byte, n)
	state := seed*2654435761 + 1
	for i := range out {
		state = state*1664525 + 1013904223
		out[i] = byte(state >> 24)
	}
	return out
}

func uint16Ptr(v uint16) *uint16 { return &v }

func syntheticImages() []image {
	return []image{
		{name: "mono8 odd geometry", rows: 7, columns: 5, samples: 1, bitsAllocated: 8, bitsStored: 8,
			photometric: "MONOCHROME2", frames: [][]byte{noise(35, 1)}},
		{name: "mono16 signed 12 stored", rows: 9, columns: 11, samples: 1, bitsAllocated: 16, bitsStored: 12,
			representation: 1, photometric: "MONOCHROME1", frames: [][]byte{noise(9*11*2, 2)}},
		{name: "rgb8 interleaved", rows: 6, columns: 4, samples: 3, bitsAllocated: 8, bitsStored: 8,
			photometric: "RGB", planar: uint16Ptr(0), frames: [][]byte{noise(6*4*3, 3)}},
		{name: "rgb8 planar", rows: 5, columns: 3, samples: 3, bitsAllocated: 8, bitsStored: 8,
			photometric: "RGB", planar: uint16Ptr(1), frames: [][]byte{noise(5*3*3, 4)}},
		{name: "multi-frame mono16", rows: 8, columns: 8, samples: 1, bitsAllocated: 16, bitsStored: 16,
			photometric: "MONOCHROME2", frames: [][]byte{noise(128, 5), noise(128, 6), make([]byte, 128)}},
		{name: "multi-frame rgb8", rows: 3, columns: 3, samples: 3, bitsAllocated: 8, bitsStored: 8,
			photometric: "RGB", planar: uint16Ptr(0), frames: [][]byte{noise(27, 7), noise(27, 8)}},
		{name: "single bit 3x5", rows: 3, columns: 5, samples: 1, bitsAllocated: 1, bitsStored: 1,
			photometric: "MONOCHROME2", frames: [][]byte{{0xA5, 0x5A}, {0xFF, 0x7F}}},
		{name: "mono32", rows: 2, columns: 3, samples: 1, bitsAllocated: 32, bitsStored: 32,
			photometric: "MONOCHROME2", frames: [][]byte{noise(24, 9)}},
	}
}

func decoders(t *testing.T) *pixeldata.MemoryRegistry {
	t.Helper()
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func decode(t *testing.T, obj *object.Object) (pixeldata.Frames, error) {
	t.Helper()
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	return decoders(t).DecodeFramesContext(context.Background(), deflateframe.UID, pixel, obj)
}

func encodeFrames(t *testing.T, img image) [][]byte {
	t.Helper()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := deflateframe.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	fragments := make([][]byte, len(img.frames))
	for i, frame := range img.frames {
		encoded, err := encoders.EncodeFrame(context.Background(), deflateframe.UID, frame, img.metadata())
		if err != nil {
			t.Fatalf("encode frame %d: %v", i, err)
		}
		if encoded.PhotometricInterpretation != "" || encoded.PlanarConfiguration != nil {
			t.Fatalf("encoder changed metadata: %+v", encoded)
		}
		fragments[i] = encoded.Data
	}
	return fragments
}

// rawInflate is the standard-library RFC 1951 reader, used as an oracle
// independent of the codec's bounded decoder.
func rawInflate(t *testing.T, stream []byte) []byte {
	t.Helper()
	out, err := io.ReadAll(flate.NewReader(bytes.NewReader(stream)))
	if err != nil {
		t.Fatalf("raw inflate: %v", err)
	}
	return out
}

func TestEncodeDecodeRoundTripIsBitExact(t *testing.T) {
	for _, img := range syntheticImages() {
		t.Run(img.name, func(t *testing.T) {
			fragments := encodeFrames(t, img)
			for i, fragment := range fragments {
				if len(fragment)%2 != 0 {
					t.Fatalf("fragment %d has odd length %d", i, len(fragment))
				}
				if got := rawInflate(t, fragment); !bytes.Equal(got, img.frames[i]) {
					t.Fatalf("fragment %d inflates to %x, want %x", i, got, img.frames[i])
				}
			}
			frames, err := decode(t, img.encapsulated(nil, fragments...))
			if err != nil {
				t.Fatal(err)
			}
			if frames.Rows != int(img.rows) || frames.Columns != int(img.columns) || len(frames.Data) != len(img.frames) {
				t.Fatalf("decoded geometry %dx%d frames %d", frames.Rows, frames.Columns, len(frames.Data))
			}
			for i := range img.frames {
				if !bytes.Equal(frames.Data[i], img.frames[i]) {
					t.Fatalf("frame %d = %x, want %x", i, frames.Data[i], img.frames[i])
				}
			}
			if len(frames.Diagnostics) != 0 {
				t.Fatalf("diagnostics = %v", frames.Diagnostics)
			}
		})
	}
}

// Streams produced by an independent implementation (C zlib through Python
// 3, raw deflate, wbits=-15, level 9) decode to the source samples.
func TestDecodeIndependentZlibVectors(t *testing.T) {
	tests := []struct {
		name   string
		img    image
		stream string
	}{
		{
			name: "mono8 4x3",
			img: image{rows: 3, columns: 4, samples: 1, bitsAllocated: 8, bitsStored: 8, photometric: "MONOCHROME2",
				frames: [][]byte{mustHex("0014283c5064788ca0b4c8dc")}},
			stream: "6310d1b00948a9e859b0e5c41d00",
		},
		{
			name: "mono16 3x2",
			img: image{rows: 2, columns: 3, samples: 1, bitsAllocated: 16, bitsStored: 16, photometric: "MONOCHROME2",
				frames: [][]byte{mustHex("000001000001ff7f0080ffff")}},
			stream: "636060646060fc5fcfd0f0ff3f00",
		},
		{
			// 11-byte stream: the fragment carries one trailing NULL pad.
			name: "rgb8 2x2 odd stream",
			img: image{rows: 2, columns: 2, samples: 3, bitsAllocated: 8, bitsStored: 8, photometric: "RGB", planar: uint16Ptr(0),
				frames: [][]byte{mustHex("ff000000ff000000ff808080")}},
			stream: "fbcfc0c0f01f841b1a1a0000",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frames, err := decode(t, test.img.encapsulated(nil, mustHex(test.stream)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frames.Data[0], test.img.frames[0]) {
				t.Fatalf("frame = %x, want %x", frames.Data[0], test.img.frames[0])
			}
		})
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func deflateAll(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mono8(rows, columns uint16, frames int) image {
	img := image{rows: rows, columns: columns, samples: 1, bitsAllocated: 8, bitsStored: 8, photometric: "MONOCHROME2"}
	img.frames = make([][]byte, frames)
	return img
}

func TestDecodeRefusesStreamLongerThanFrame(t *testing.T) {
	img := mono8(4, 4, 1)
	stream := deflateAll(t, noise(17, 11), flate.BestCompression)
	_, err := decode(t, img.encapsulated(nil, stream))
	assertExpansionLimit(t, err, 16)
}

// A decompression bomb (64 MiB of zeros in about 64 KiB) declared as a
// 16x16 frame is refused after at most 257 inflated bytes; the decoder never
// materializes the expansion.
func TestDecodeRefusesDecompressionBombWithBoundedMemory(t *testing.T) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for range 64 {
		if _, err := w.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := buf.Bytes()
	if len(bomb)%2 != 0 {
		bomb = append(bomb, 0)
	}
	img := mono8(16, 16, 1)
	obj := img.encapsulated(nil, bomb)
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	registry := decoders(t)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = registry.DecodeFramesContext(context.Background(), deflateframe.UID, pixel, obj)
	runtime.ReadMemStats(&after)
	assertExpansionLimit(t, err, 256)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("decoding the bomb allocated %d bytes", allocated)
	}

	if _, err := deflateframe.InflateFrame(bomb, 256); !errors.Is(err, deflateframe.ErrExpansionLimit) {
		t.Fatalf("InflateFrame(bomb) error = %v", err)
	}
}

func assertExpansionLimit(t *testing.T, err error, limit int64) {
	t.Helper()
	if !errors.Is(err, deflateframe.ErrExpansionLimit) {
		t.Fatalf("error = %v, want ErrExpansionLimit", err)
	}
	if !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) || !errors.Is(err, pixeldata.ErrCodecDecodeFailed) {
		t.Fatalf("error = %v, want size-mismatch and codec-decode classification", err)
	}
	var expansion *deflateframe.ExpansionLimitError
	if !errors.As(err, &expansion) {
		t.Fatalf("error = %T, want *ExpansionLimitError in chain", err)
	}
	if expansion.Limit != limit || expansion.Frame != 0 {
		t.Fatalf("ExpansionLimitError = %+v, want frame 0 limit %d", expansion, limit)
	}
}

func TestDecodeTypedStreamErrors(t *testing.T) {
	frame := noise(16, 12)
	valid := deflateAll(t, frame, flate.DefaultCompression)
	zlibStream := func() []byte {
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(frame)
		_ = w.Close()
		return buf.Bytes()
	}()
	tests := []struct {
		name      string
		fragments [][]byte
		want      error
	}{
		{"short stream", [][]byte{deflateAll(t, frame[:15], flate.DefaultCompression)}, deflateframe.ErrShortFrame},
		{"truncated stream", [][]byte{valid[:len(valid)/2]}, deflateframe.ErrInvalidStream},
		{"corrupt stream", [][]byte{{0xFF, 0xFF, 0xFF, 0xFF}}, deflateframe.ErrInvalidStream},
		{"empty fragment", [][]byte{{}}, deflateframe.ErrInvalidStream},
		{"zlib wrapper", [][]byte{zlibStream}, nil},
		{"two trailing bytes", [][]byte{append(append([]byte(nil), valid...), 0, 0)}, deflateframe.ErrTrailingData},
		{"too few fragments", [][]byte{}, deflateframe.ErrInvalidStream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decode(t, mono8(4, 4, 1).encapsulated(nil, test.fragments...))
			if err == nil {
				t.Fatal("expected an error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !errors.Is(err, pixeldata.ErrCodecDecodeFailed) {
				t.Fatalf("error = %v, want ErrCodecDecodeFailed classification", err)
			}
		})
	}
}

func TestDecodeFrameFragmentLayouts(t *testing.T) {
	frames := [][]byte{noise(16, 21), noise(16, 22)}
	first := deflateAll(t, frames[0], flate.BestCompression)
	second := deflateAll(t, frames[1], flate.BestCompression)
	pad := func(b []byte) []byte {
		if len(b)%2 != 0 {
			return append(append([]byte(nil), b...), 0)
		}
		return b
	}
	first, second = pad(first), pad(second)

	t.Run("one frame split across fragments", func(t *testing.T) {
		img := mono8(4, 4, 1)
		split := len(first) / 2 &^ 1
		got, err := decode(t, img.encapsulated(nil, first[:split], first[split:]))
		if err != nil || !bytes.Equal(got.Data[0], frames[0]) {
			t.Fatalf("decode = %x, %v", got.Data, err)
		}
	})
	t.Run("frames grouped by Basic Offset Table", func(t *testing.T) {
		img := mono8(4, 4, 2)
		split := len(first) / 2 &^ 1
		bot := make([]byte, 8)
		binary.LittleEndian.PutUint32(bot[4:], uint32(8+split+8+len(first)-split))
		got, err := decode(t, img.encapsulated(bot, first[:split], first[split:], second))
		if err != nil || !bytes.Equal(got.Data[0], frames[0]) || !bytes.Equal(got.Data[1], frames[1]) {
			t.Fatalf("decode = %x, %v", got.Data, err)
		}
	})
	t.Run("fewer fragments than frames", func(t *testing.T) {
		img := mono8(4, 4, 2)
		if _, err := decode(t, img.encapsulated(nil, first)); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("nonzero pad is tolerated and reported", func(t *testing.T) {
		img := mono8(4, 4, 1)
		stream := deflateAll(t, frames[0], flate.BestCompression)
		if len(stream)%2 == 0 {
			stream = deflateAll(t, frames[0], flate.HuffmanOnly)
		}
		if len(stream)%2 == 0 {
			t.Skip("no odd-length stream for this vector")
		}
		got, err := decode(t, img.encapsulated(nil, append(stream, 0x20)))
		if err != nil || !bytes.Equal(got.Data[0], frames[0]) {
			t.Fatalf("decode = %x, %v", got.Data, err)
		}
		if len(got.Diagnostics) != 1 || got.Diagnostics[0] != pixeldata.DiagnosticNonZeroFramePad {
			t.Fatalf("diagnostics = %v", got.Diagnostics)
		}
	})
}

func TestDecodeRejectsNativePixelDataAndUnsupportedMetadata(t *testing.T) {
	img := mono8(2, 2, 1)
	img.frames[0] = []byte{1, 2, 3, 4}
	obj := img.native()
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deflateframe.New().Decode(pixel, obj); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("native Pixel Data error = %v", err)
	}
	img.bitsAllocated, img.bitsStored = 12, 12
	if _, err := decode(t, img.encapsulated(nil, deflateAll(t, []byte{1, 2, 3, 4, 5, 6}, 9))); !errors.Is(err, deflateframe.ErrUnsupportedMetadata) {
		t.Fatalf("BitsAllocated=12 error = %v", err)
	}
}

func TestDecodeHonorsCancellation(t *testing.T) {
	img := mono8(4, 4, 1)
	obj := img.encapsulated(nil, deflateAll(t, noise(16, 1), 9))
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := deflateframe.New().DecodeContext(ctx, pixel, obj); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestEncoderValidatesInput(t *testing.T) {
	encoder := deflateframe.NewEncoder()
	img := mono8(2, 2, 1)
	if _, err := encoder.EncodeFrame(context.Background(), []byte{1, 2, 3}, img.metadata()); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("short frame error = %v", err)
	}
	img.photometric = "YBR_PARTIAL_420"
	if _, err := encoder.EncodeFrame(context.Background(), []byte{1, 2, 3, 4}, img.metadata()); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Fatalf("photometric error = %v", err)
	}
	if _, err := deflateframe.NewEncoderLevel(10); err == nil {
		t.Fatal("level 10 accepted")
	}
	for _, level := range []int{flate.HuffmanOnly, flate.NoCompression, flate.BestSpeed, flate.BestCompression} {
		levelEncoder, err := deflateframe.NewEncoderLevel(level)
		if err != nil {
			t.Fatal(err)
		}
		frame := noise(4, uint32(level+10))
		encoded, err := levelEncoder.EncodeFrame(context.Background(), frame, mono8(2, 2, 1).metadata())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := deflateframe.InflateFrame(encoded.Data, 4); err != nil || !bytes.Equal(got, frame) {
			t.Fatalf("level %d round trip = %x, %v", level, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := encoder.EncodeFrame(ctx, []byte{1, 2, 3, 4}, mono8(2, 2, 1).metadata()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled encode error = %v", err)
	}
}

func TestCapabilitiesAndRegistration(t *testing.T) {
	capabilities := deflateframe.NewEncoder().Capabilities()
	if capabilities.TransferSyntaxUID != transfer.DeflatedImageFrameCompression.UID || !capabilities.Lossless || !capabilities.SupportsMultiFrame {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if err := deflateframe.Register(nil); !errors.Is(err, pixeldata.ErrCodecRegistryNil) {
		t.Fatalf("Register(nil) = %v", err)
	}
	if err := deflateframe.RegisterEncoder(nil); !errors.Is(err, pixeldata.ErrEncoderRegistryNil) {
		t.Fatalf("RegisterEncoder(nil) = %v", err)
	}
	// Without registration the hint names this package, not a streaming
	// pipeline.
	err := pixeldata.CheckCodecAvailability(pixeldata.NewMemoryRegistry(), deflateframe.UID)
	if !errors.Is(err, pixeldata.ErrCodecNotFound) || !strings.Contains(err.Error(), "pixeldata/deflateframe") {
		t.Fatalf("availability error = %v", err)
	}
}

// Errors carry only frame indices and sizes, never sample values.
func TestErrorsCarryNoPixelValues(t *testing.T) {
	frame := []byte("PATIENT^NAME0123")
	_, err := deflateframe.InflateFrame(deflateAll(t, append(frame, frame...), 9), len(frame))
	if err == nil || strings.Contains(err.Error(), "PATIENT") {
		t.Fatalf("error = %v", err)
	}
}
