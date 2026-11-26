package jpegxl_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegxl"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// testImage is one synthetic, PHI-free DICOM image.
type testImage struct {
	name           string
	rows, columns  uint16
	samples        uint16
	bitsAllocated  uint16
	bitsStored     uint16
	representation uint16
	photometric    string
	frames         [][]byte
}

func (img testImage) elements() []core.Element {
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.1153001"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, img.rows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, img.columns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, img.samples),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, img.photometric),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(len(img.frames))),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, img.bitsAllocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, img.bitsStored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, img.bitsStored-1),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, img.representation),
	}
	if img.samples == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	return elements
}

func (img testImage) native() *object.Object {
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

func (img testImage) encapsulated(fragments ...[]byte) *object.Object {
	return object.FromElements(append(img.elements(), dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragments...)), nil)
}

// smoothFrame returns a deterministic frame of valid stored values: smooth
// structure plus noise, sign-extended for signed images.
func smoothFrame(img testImage, seed int) []byte {
	n := int(img.rows) * int(img.columns) * int(img.samples)
	bytesPer := int(img.bitsAllocated / 8)
	out := make([]byte, n*bytesPer)
	maxv := float64(int(1)<<img.bitsStored - 1)
	state := uint32(seed)*2654435761 + 1
	for i := 0; i < n; i++ {
		p := i / int(img.samples)
		x, y := p%int(img.columns), p/int(img.columns)
		state = state*1664525 + 1013904223
		v := maxv/2 + maxv/2.5*math.Sin(float64(x+3*(i%int(img.samples))+seed)/5)*math.Cos(float64(y)/7) + float64(int(state>>28)-8)
		u := int64(math.Max(0, math.Min(maxv, v)))
		if img.representation == 1 {
			u -= int64(1) << (img.bitsStored - 1) // signed value
		}
		if bytesPer == 1 {
			out[i] = byte(u)
		} else {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(u))
		}
	}
	return out
}

func newImage(name string, rows, columns, samples, allocated, stored, representation uint16, photometric string, frames int) testImage {
	img := testImage{name: name, rows: rows, columns: columns, samples: samples, bitsAllocated: allocated,
		bitsStored: stored, representation: representation, photometric: photometric}
	for f := 0; f < frames; f++ {
		img.frames = append(img.frames, smoothFrame(img, f+int(rows)))
	}
	return img
}

func decoders(t *testing.T) *pixeldata.MemoryRegistry {
	t.Helper()
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := jpegxl.Register(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func options(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegxl.RegisterLosslessEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	if err := jpegxl.RegisterLossyEncoder(encoders, 90); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.RegisterEncoder(encoders, 90); err != nil {
		t.Fatal(err)
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders(t), EncoderRegistry: encoders, CodestreamTranscoder: jpegxl.Recompressor{}}
}

func nativeFrames(t *testing.T, obj *object.Object) [][]byte {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFramesView(obj)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data
}

func fragments(t *testing.T, obj *object.Object) [][]byte {
	t.Helper()
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !pixel.Encapsulated {
		t.Fatal("pixel data is not encapsulated")
	}
	return pixel.Sequence.Fragments
}

func TestLosslessTranscodeRoundTripIsExact(t *testing.T) {
	images := []testImage{
		newImage("mono8", 7, 5, 1, 8, 8, 0, "MONOCHROME2", 1),
		newImage("mono16 signed 12 stored", 33, 29, 1, 16, 12, 1, "MONOCHROME1", 1),
		newImage("mono16 signed 16 stored", 20, 18, 1, 16, 16, 1, "MONOCHROME2", 1),
		newImage("mono16 unsigned 16 stored", 40, 30, 1, 16, 16, 0, "MONOCHROME2", 1),
		newImage("mono16 10 stored multi-frame", 16, 24, 1, 16, 10, 0, "MONOCHROME2", 3),
		newImage("signed 8", 9, 9, 1, 8, 8, 1, "MONOCHROME2", 1),
		newImage("rgb8 multi-frame", 21, 19, 3, 8, 8, 0, "RGB", 2),
		newImage("rgb16 12 stored", 12, 10, 3, 16, 12, 0, "RGB", 1),
		newImage("large mono16 multi-group", 300, 260, 1, 16, 14, 0, "MONOCHROME2", 1),
	}
	opts := options(t)
	for _, img := range images {
		t.Run(img.name, func(t *testing.T) {
			encoded, report, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGXLLossless, opts)
			if err != nil {
				t.Fatal(err)
			}
			if report.Lossy || report.Frames != len(img.frames) {
				t.Fatalf("report = %+v", report)
			}
			if got := len(fragments(t, encoded)); got != len(img.frames) {
				t.Fatalf("%d fragments for %d frames", got, len(img.frames))
			}
			back, _, err := pixeldata.TranscodeDataSet(context.Background(), encoded, transfer.JPEGXLLossless, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			got := nativeFrames(t, back)
			for i := range img.frames {
				if !bytes.Equal(got[i], img.frames[i]) {
					t.Fatalf("frame %d differs after .110 round trip", i)
				}
			}
			t.Logf("%d native bytes -> %d encoded", report.NativeBytes, report.EncodedBytes)
		})
	}
}

// Signed samples are stored offset-binary: stored = value + 2^(BitsStored-1).
func TestSignedSamplesUseOffsetBinaryLevelShift(t *testing.T) {
	m := pixeldata.Metadata{Rows: 1, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 12, HighBit: 11,
		PixelRepresentation: 1, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
	values := []int16{-2048, -1, 0, 2047}
	frame := make([]byte, 8)
	for i, v := range values {
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(v))
	}
	encoded, err := jpegxl.LosslessEncoder{}.EncodeFrame(context.Background(), frame, m)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpegxl.Decode(encoded.Data)
	if err != nil {
		t.Fatal(err)
	}
	if img.BitsPerSample != 12 {
		t.Fatalf("codestream bit depth %d, want 12", img.BitsPerSample)
	}
	for i, v := range values {
		if want := int32(v) + 2048; img.Samples[i] != want {
			t.Fatalf("stored sample %d = %d, want %d", i, img.Samples[i], want)
		}
	}
}

func TestLossyEncoderRequiresAllowLossyAndRecordsCompression(t *testing.T) {
	for _, img := range []testImage{
		newImage("mono8", 64, 48, 1, 8, 8, 0, "MONOCHROME2", 2),
		newImage("rgb8", 40, 36, 3, 8, 8, 0, "RGB", 1),
	} {
		t.Run(img.name, func(t *testing.T) {
			opts := options(t)
			_, _, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGXL, opts)
			if err == nil {
				t.Fatal("lossy .112 encode without AllowLossy succeeded")
			}
			opts.AllowLossy = true
			encoded, report, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGXL, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Lossy {
				t.Fatal("report is not lossy")
			}
			method, _ := encoded.GetString(core.NewTag(0x0028, 0x2114))
			if !strings.Contains(method, jpegxl.LossyMethod) {
				t.Fatalf("Lossy Image Compression Method = %q", method)
			}
			back, _, err := pixeldata.TranscodeDataSet(context.Background(), encoded, transfer.JPEGXL, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			got := nativeFrames(t, back)
			for i, want := range img.frames {
				var se float64
				for k := range want {
					d := float64(got[i][k]) - float64(want[k])
					se += d * d
				}
				psnr := 10 * math.Log10(255*255*float64(len(want))/math.Max(se, 1e-9))
				if psnr < 30 {
					t.Fatalf("frame %d PSNR %.1f dB", i, psnr)
				}
			}
		})
	}
}

// JPEG Baseline frames recompress to .111 and reconstruct to the identical
// fragments; both syntaxes decode to the same pixels.
func TestRecompressorRoundTripsJPEGBaselineExactly(t *testing.T) {
	img := newImage("rgb8", 45, 61, 3, 8, 8, 0, "RGB", 2)
	opts := options(t)
	opts.AllowLossy = true
	baseline, _, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, opts)
	if err != nil {
		t.Fatal(err)
	}
	recompressed, report, err := pixeldata.TranscodeDataSet(context.Background(), baseline, transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lossy || report.EncodedBytes <= 0 {
		t.Fatalf("report = %+v", report)
	}
	jpegFragments, jxlFragments := fragments(t, baseline), fragments(t, recompressed)
	for i := range jpegFragments {
		restored, err := jpegxl.ReconstructJPEG(jxlFragments[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restored, bytes.TrimSuffix(jpegFragments[i], []byte{0})) && !bytes.Equal(restored, jpegFragments[i]) {
			t.Fatalf("frame %d: reconstructed JPEG differs", i)
		}
	}
	decodedJPEG, _, err := pixeldata.TranscodeDataSet(context.Background(), baseline, transfer.JPEGBaseline, transfer.ExplicitVRLittleEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	decodedJXL, _, err := pixeldata.TranscodeDataSet(context.Background(), recompressed, transfer.JPEGXLJPEGRecompression, transfer.ExplicitVRLittleEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	want, got := nativeFrames(t, decodedJPEG), nativeFrames(t, decodedJXL)
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d: .111 pixels differ from .50 pixels", i)
		}
	}
	restored, _, err := pixeldata.TranscodeDataSet(context.Background(), recompressed, transfer.JPEGXLJPEGRecompression, transfer.JPEGBaseline, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, fragment := range fragments(t, restored) {
		if !bytes.Equal(fragment, jpegFragments[i]) {
			t.Fatalf("frame %d: .111 -> .50 fragment differs from the source", i)
		}
	}
	// A restored Baseline frame is not a JPEG Extended (SOF1) frame.
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), recompressed, transfer.JPEGXLJPEGRecompression, transfer.JPEGExtended, opts); !errors.Is(err, jpegxl.ErrUnsupported) {
		t.Fatalf(".111 -> .51 of a SOF0 frame: err = %v", err)
	}
	// ForceReencode takes the pixel route, for which no .111 encoder exists.
	opts.ForceReencode = true
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), baseline, transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression, opts); err == nil {
		t.Fatal("ForceReencode to .111 succeeded without an encoder")
	}
}

func TestRecompressorRefusesIrreversibleJPEG(t *testing.T) {
	// The Go encoder's grey JPEG carries an unused chroma quantization table.
	img := newImage("mono8", 16, 16, 1, 8, 8, 0, "MONOCHROME2", 1)
	opts := options(t)
	opts.AllowLossy = true
	baseline, _, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = pixeldata.TranscodeDataSet(context.Background(), baseline, transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression, opts)
	var transcodeErr *pixeldata.TranscodeError
	if !errors.Is(err, jpegxl.ErrNotReversible) || !errors.As(err, &transcodeErr) || transcodeErr.Stage != "codestream" {
		t.Fatalf("err = %v, want a codestream-stage ErrNotReversible", err)
	}
	r := jpegxl.Recompressor{}
	for _, pair := range [][2]string{
		{transfer.JPEGLosslessSV1.UID, transfer.JPEGXLJPEGRecompression.UID},
		{transfer.JPEGBaseline.UID, transfer.JPEGXLLossless.UID},
		{transfer.JPEGXLJPEGRecompression.UID, transfer.JPEG2000.UID},
	} {
		if r.CanTranscodeCodestream(pair[0], pair[1]) {
			t.Errorf("CanTranscodeCodestream(%s, %s) = true", pair[0], pair[1])
		}
	}
}

func TestCodecRejectsMismatchedFrames(t *testing.T) {
	img := newImage("mono8", 8, 8, 1, 8, 8, 0, "MONOCHROME2", 1)
	encoded, err := jpegxl.LosslessEncoder{}.EncodeFrame(context.Background(), img.frames[0], pixeldata.Metadata{
		Rows: 8, Columns: 8, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"})
	if err != nil {
		t.Fatal(err)
	}
	codec := jpegxl.New()
	decode := func(img testImage, fragments ...[]byte) error {
		obj := img.encapsulated(fragments...)
		pixel, err := pixeldata.ExtractView(obj)
		if err != nil {
			t.Fatal(err)
		}
		_, err = codec.Decode(pixel, obj)
		return err
	}
	if err := decode(img, encoded.Data); err != nil {
		t.Fatalf("valid frame: %v", err)
	}
	wrongSize := img
	wrongSize.rows = 9
	if err := decode(wrongSize, encoded.Data); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("size mismatch: err = %v", err)
	}
	twoFrames := img
	twoFrames.frames = append(twoFrames.frames, img.frames[0], img.frames[0])
	if err := decode(twoFrames, encoded.Data, encoded.Data); !errors.Is(err, jpegxl.ErrFragmentLayout) {
		t.Fatalf("fragment layout: err = %v", err)
	}
	if err := decode(img, []byte{0xFF, 0x0A, 0x00}); !errors.Is(err, jpegxl.ErrInvalidCodestream) {
		t.Fatalf("truncated: err = %v", err)
	}
	signedRGB := newImage("rgb", 2, 2, 3, 8, 8, 0, "RGB", 1)
	signedRGB.representation = 1
	if err := decode(signedRGB, encoded.Data); !errors.Is(err, pixeldata.ErrUnsupportedPixelRepresentation) {
		t.Fatalf("signed RGB: err = %v", err)
	}
}

func TestLosslessEncoderRejectsUnsupportedMetadata(t *testing.T) {
	base := pixeldata.Metadata{Rows: 2, Columns: 2, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7,
		NumberOfFrames: 1, PhotometricInterpretation: "RGB", PlanarConfigurationPresent: true}
	for field, mutate := range map[string]func(*pixeldata.Metadata){
		"PlanarConfiguration":       func(m *pixeldata.Metadata) { m.PlanarConfiguration = 1 },
		"PhotometricInterpretation": func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "YBR_FULL" },
		"PixelRepresentation":       func(m *pixeldata.Metadata) { m.PixelRepresentation = 1 },
		"BitsAllocated":             func(m *pixeldata.Metadata) { m.BitsAllocated = 32; m.BitsStored = 32; m.HighBit = 31 },
		"HighBit":                   func(m *pixeldata.Metadata) { m.HighBit = 6 },
	} {
		m := base
		mutate(&m)
		_, err := jpegxl.LosslessEncoder{}.EncodeFrame(context.Background(), make([]byte, 12), m)
		var unsupported *pixeldata.UnsupportedEncoderMetadataError
		if !errors.As(err, &unsupported) || unsupported.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
}
