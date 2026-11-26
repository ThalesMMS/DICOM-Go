package jpeg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// 12-bit color Process 4 fixtures, 16x8, made by libjpeg-turbo 3.2.0 from
// color12Source: "cjpeg -precision 12 -quality 100 -sample 2x1,1x1,1x1"
// (YCbCr, JFIF, chroma halved horizontally) and "cjpeg -precision 12
// -quality 100 -rgb" (RGB, Adobe transform 0, component IDs R, G, B).
const (
	color12YCbCr422Base64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQH/2wBDAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQH/wQARDAAIABADASEAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAADA3/xAAlEAABAQUIAwAAAAAAAAAAAAARAQALDSE4BhYaKjFBSHRRVmb/xAAVAQEBAAAAAAAAAAAAAAAAAAAAAv/EACYRAAEABQ0AAAAAAAAAAAAAAAEACw0oRgIRFSkxODlEUVhmdvH/2gAMAwEAAhEDEQA/ABj7itgwXR2uFI9E4X9CRWxQ8gI1m3cVsGC6O8qRrcThf0PiiuxAk0qU1eVl0t8AwRHWtHTZ+0ehDOt2lCYF9fft0k3lOtclT//Z"
	color12RGBBase64      = "/9j/7gAOQWRvYmUAZAAAAAAA/9sAQwABAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB/8EAEQwACAAQA1IRAEcRAEIRAP/EABgAAAIDAAAAAAAAAAAAAAAAAAAOAw0P/8QALRAAAAIECwkAAAAAAAAAAAAAATEACQwRDRceISdKa4ipxucZJTU5ZXF1sfD/2gAMA1IARwBCAAA/AF3bgyQq+12PLCRyWW7OkwstK0qLJiv2K/fsnBFU5NQq4IlT4Ie5C99j0RAgyQq+12PLCElluzpMLLStG6KYr9iv+U4IqnJ6FXBEr4Toh/GX/9k="
)

func color12Source(x, y int) [3]int {
	return [3]int{x * 4095 / 15, y * 4095 / 7, (x + y) * 4095 / 22}
}

func color12Fixture(t testing.TB, encoded string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func colorJPEGObject(t testing.TB, stream []byte, rows, columns, bitsAllocated, bitsStored uint16, photometric string) (*object.Object, pixeldata.PixelData) {
	t.Helper()
	obj := object.FromElements([]core.Element{
		dicomtest.Uint16Element(tagRows, core.VRUS, nil, rows),
		dicomtest.Uint16Element(tagColumns, core.VRUS, nil, columns),
		dicomtest.Uint16Element(tagSamplesPerPixel, core.VRUS, nil, 3),
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, photometric),
		dicomtest.Uint16Element(tagPlanarConfiguration, core.VRUS, nil, 0),
		dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, "1"),
		dicomtest.Uint16Element(tagBitsAllocated, core.VRUS, nil, bitsAllocated),
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, bitsStored),
		dicomtest.Uint16Element(tagHighBit, core.VRUS, nil, bitsStored-1),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, 0),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream),
	}, nil)
	pixel, err := pixeldata.Extract(obj)
	if err != nil {
		t.Fatal(err)
	}
	return obj, pixel
}

func decodeColor12(t *testing.T, encoded, photometric string, policy pixeldata.JPEGColorConversion) pixeldata.Frames {
	t.Helper()
	obj, pixel := colorJPEGObject(t, color12Fixture(t, encoded), 8, 16, 16, 12, photometric)
	ctx := pixeldata.WithJPEGColorConversion(context.Background(), policy)
	frames, err := New().DecodeContext(ctx, pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestJPEGColorGuessPrefersJFIFOverAdobe(t *testing.T) {
	base := color12Fixture(t, color12YCbCr422Base64)
	adobe := jpegMarkerBytes(t, color12Fixture(t, color12RGBBase64), 0xee)
	stream := insertJPEGBytes(base, 2, adobe)
	got := decodeColor12(t, base64.StdEncoding.EncodeToString(stream), "YBR_FULL_422", pixeldata.JPEGColorGuess)
	want := decodeColor12(t, color12YCbCr422Base64, "YBR_FULL_422", pixeldata.JPEGColorGuess)
	if !equalSamples(got.Data[0], want.Data[0]) || got.PhotometricInterpretation != want.PhotometricInterpretation {
		t.Fatal("Adobe transform 0 overrode the JFIF color interpretation")
	}
}

func TestJPEGExtendedTwelveBitColor(t *testing.T) {
	// YCbCr with 2x1 chroma: guessed YCbCr, converted to RGB close to the
	// source (lossy at quality 100, chroma subsampled).
	frames := decodeColor12(t, color12YCbCr422Base64, "YBR_FULL_422", pixeldata.JPEGColorGuess)
	if frames.PhotometricInterpretation != "RGB" {
		t.Fatalf("photometric %q", frames.PhotometricInterpretation)
	}
	assertColor12NearSource(t, frames.Data[0], 200)

	// RGB stored as such: no conversion, near-exact.
	frames = decodeColor12(t, color12RGBBase64, "RGB", pixeldata.JPEGColorGuess)
	if frames.PhotometricInterpretation != "RGB" {
		t.Fatalf("photometric %q", frames.PhotometricInterpretation)
	}
	assertColor12NearSource(t, frames.Data[0], 24)

	// Never converting keeps YCbCr: a gray-free pixel is not its RGB.
	never := decodeColor12(t, color12YCbCr422Base64, "YBR_FULL_422", pixeldata.JPEGColorNever)
	if never.PhotometricInterpretation != "YBR_FULL" {
		t.Fatalf("never photometric %q", never.PhotometricInterpretation)
	}
	converted := decodeColor12(t, color12YCbCr422Base64, "YBR_FULL_422", pixeldata.JPEGColorPhotometric)
	if equalSamples(never.Data[0], converted.Data[0]) {
		t.Fatal("never and photometric policies decoded the same samples")
	}
	// Trusting a (wrong) RGB photometric leaves the YCbCr samples.
	trusted := decodeColor12(t, color12YCbCr422Base64, "RGB", pixeldata.JPEGColorPhotometric)
	if !equalSamples(trusted.Data[0], never.Data[0]) || trusted.PhotometricInterpretation != "RGB" {
		t.Fatalf("photometric policy with RGB converted (%q)", trusted.PhotometricInterpretation)
	}
	// Always converting an RGB codestream changes it.
	always := decodeColor12(t, color12RGBBase64, "RGB", pixeldata.JPEGColorAlways)
	rgb := decodeColor12(t, color12RGBBase64, "RGB", pixeldata.JPEGColorGuess)
	if equalSamples(always.Data[0], rgb.Data[0]) {
		t.Fatal("always policy did not convert an RGB codestream")
	}
}

func TestJPEGStoredBitsBelowPrecisionAndSigned(t *testing.T) {
	frame := []byte{0, 0xff, 0x80, 0x3f}
	limitStoredBits8(frame, pixeldata.Metadata{BitsStored: 6})
	if frame[1] != 0x3f || frame[2] != 0x3f || frame[3] != 0x3f {
		t.Fatalf("unsigned clamp %v", frame)
	}
	frame = []byte{0xff, 0x80}
	limitStoredBits8(frame, pixeldata.Metadata{BitsStored: 6, PixelRepresentation: 1})
	if frame[0] != 0x3f || frame[1] != 0 {
		t.Fatalf("signed mask %v", frame)
	}
	wide := binary.LittleEndian.AppendUint16(nil, 4095)
	limitStoredBits16(wide, pixeldata.Metadata{BitsStored: 10})
	if binary.LittleEndian.Uint16(wide) != 1023 {
		t.Fatalf("10-bit clamp %v", wide)
	}
	// A signed 12-bit Process 4 image decodes with its bits unchanged.
	obj, pixel := process4Object(t, process4Fixture(t), 8, 8, 16, 12, 11, 1)
	signed, err := New().Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	obj, pixel = process4Object(t, process4Fixture(t), 8, 8, 16, 12, 11, 0)
	unsigned, err := New().Decode(pixel, obj)
	if err != nil || !equalSamples(signed.Data[0], unsigned.Data[0]) {
		t.Fatalf("signed samples changed: %v", err)
	}
	// Ten bits stored in a 12-bit codestream stay within 10 bits.
	obj, pixel = process4Object(t, process4Fixture(t), 8, 8, 16, 10, 9, 0)
	ten, err := New().Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(ten.Data[0]); i += 2 {
		if binary.LittleEndian.Uint16(ten.Data[0][i:]) > 1023 {
			t.Fatal("sample above 10 bits")
		}
	}
}

func assertColor12NearSource(t *testing.T, frame []byte, tolerance int) {
	t.Helper()
	worst := 0
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			want := color12Source(x, y)
			for c := 0; c < 3; c++ {
				got := int(binary.LittleEndian.Uint16(frame[((y*16+x)*3+c)*2:]))
				d := got - want[c]
				if d < 0 {
					d = -d
				}
				worst = max(worst, d)
			}
		}
	}
	if worst > tolerance {
		t.Fatalf("decoded color differs from the source by %d (tolerance %d)", worst, tolerance)
	}
}

func equalSamples(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestJPEGExtendedTwelveBitColorLibjpegTurboInterop encodes the color source
// with cjpeg at several sampling factors and restart intervals and compares
// the decode with djpeg (float IDCT, default fancy upsampling). Enable with
// DICOMGO_JPEG12_INTEROP=1.
func TestJPEGExtendedTwelveBitColorLibjpegTurboInterop(t *testing.T) {
	if os.Getenv("DICOMGO_JPEG12_INTEROP") != "1" {
		t.Skip("set DICOMGO_JPEG12_INTEROP=1 to verify against libjpeg-turbo cjpeg/djpeg")
	}
	cjpeg, err := exec.LookPath("cjpeg")
	if err != nil {
		t.Skipf("cjpeg unavailable: %v", err)
	}
	djpeg, err := exec.LookPath("djpeg")
	if err != nil {
		t.Skipf("djpeg unavailable: %v", err)
	}
	const width, height = 37, 21 // not a multiple of the MCU
	var ppm bytes.Buffer
	fmt.Fprintf(&ppm, "P6\n%d %d\n4095\n", width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			for _, v := range []int{(x*97 + y*13) % 4096, (x*31 + y*211) % 4096, (x*x + y*y*3) % 4096} {
				ppm.Write(binary.BigEndian.AppendUint16(nil, uint16(v)))
			}
		}
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "in.ppm")
	if err := os.WriteFile(input, ppm.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct {
		name string
		args []string
	}{
		{"h1v1", []string{"-sample", "1x1,1x1,1x1"}},
		{"h2v1", []string{"-sample", "2x1,1x1,1x1"}},
		{"h2v2", []string{"-sample", "2x2,1x1,1x1"}},
		{"h2v2-restart", []string{"-sample", "2x2,1x1,1x1", "-restart", "1B"}},
		{"rgb", []string{"-rgb"}},
	} {
		stream := filepath.Join(dir, variant.name+".jpg")
		args := append([]string{"-precision", "12", "-quality", "90", "-dct", "float", "-outfile", stream}, variant.args...)
		if out, err := runJPEGInteropCommand(cjpeg, append(args, input)...); err != nil {
			t.Fatalf("cjpeg %s: %v: %s", variant.name, err, out)
		}
		reference := filepath.Join(dir, variant.name+".ppm")
		if out, err := runJPEGInteropCommand(djpeg, "-dct", "float", "-pnm", "-outfile", reference, stream); err != nil {
			t.Fatalf("djpeg %s: %v: %s", variant.name, err, out)
		}
		data, _ := os.ReadFile(stream)
		want, _ := os.ReadFile(reference)
		want = want[len(want)-width*height*6:]
		obj, pixel := colorJPEGObject(t, data, height, width, 16, 12, "YBR_FULL_422")
		frames, err := New().Decode(pixel, obj)
		if err != nil {
			t.Fatalf("%s: %v", variant.name, err)
		}
		worst := 0
		for i := 0; i < width*height*3; i++ {
			got := int(binary.LittleEndian.Uint16(frames.Data[0][2*i:]))
			d := got - int(binary.BigEndian.Uint16(want[2*i:]))
			if d < 0 {
				d = -d
			}
			worst = max(worst, d)
		}
		t.Logf("%s: max |diff| from djpeg %d/4095", variant.name, worst)
		if worst > 2 {
			t.Errorf("%s differs from djpeg by %d", variant.name, worst)
		}
	}
}

func TestJPEGSequentialScanQuirksAreOptInAndReported(t *testing.T) {
	stream := append([]byte(nil), process4Fixture(t)...)
	sos := jpegSegment(t, stream, 0xda)
	// Se=0 in a sequential scan, as some encoders write it.
	stream[sos+4] = 0
	obj, pixel := process4Object(t, stream, 8, 8, 16, 12, 11, 0)
	if _, err := New().Decode(pixel, obj); err == nil {
		t.Fatal("strict decode accepted Se=0")
	}
	ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGSequentialScanQuirks: true})
	frames, err := New().DecodeContext(ctx, pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, code := range frames.Diagnostics {
		reported = reported || code == pixeldata.DiagnosticJPEGScanParametersIgnored
	}
	obj, pixel = process4Object(t, process4Fixture(t), 8, 8, 16, 12, 11, 0)
	clean, err := New().Decode(pixel, obj)
	if err != nil || !reported || !equalSamples(frames.Data[0], clean.Data[0]) {
		t.Fatalf("recovered decode: reported=%v err=%v", reported, err)
	}
}
