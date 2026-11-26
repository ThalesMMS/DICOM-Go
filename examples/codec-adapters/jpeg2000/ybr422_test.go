package jpeg2000

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The YBR_FULL_422 fixture (#2028) is shared with the Rust port: a 17x6
// codestream whose chroma components are sampled at XRsiz=2, YRsiz=1,
// OpenJPEG's per-component reconstruction, and the RGB an independent
// script derived from it. See generate.py and generation.json there.
var ybr422FixtureDir = filepath.Join(libraryRoot, "pixeldata", "codecfixture", "testdata", "codecs", "jpeg2000-ybr422")

const ybr422Rows, ybr422Columns = 6, 17

func readYBR422Fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ybr422FixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ybr422Metadata() jpeg2000MetadataOptions {
	return jpeg2000MetadataOptions{rows: ybr422Rows, columns: ybr422Columns, samplesPerPixel: 3, photometric: "YBR_FULL_422", planarConfiguration: uint16Ptr(0)}
}

// Keep the codestream, the independent reconstruction and the expected RGB
// tied to the reviewed provenance.
func TestYBRFull422FixtureProvenance(t *testing.T) {
	var manifest struct {
		Provenance struct{ Synthetic, NoPHI, VendorImplementationCopied bool }
		Fixtures   []struct {
			ID       string
			Expected struct{ Photometric, LossyImageCompression string }
		}
		Assets []struct {
			Path   string
			Bytes  int
			SHA256 string
		}
	}
	if err := json.Unmarshal(readYBR422Fixture(t, "generation.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Provenance.Synthetic || !manifest.Provenance.NoPHI || manifest.Provenance.VendorImplementationCopied ||
		len(manifest.Fixtures) != 1 || manifest.Fixtures[0].Expected.Photometric != "RGB" ||
		manifest.Fixtures[0].Expected.LossyImageCompression != "01" || len(manifest.Assets) != 4 {
		t.Fatal("incomplete YBR_FULL_422 fixture provenance")
	}
	for _, asset := range manifest.Assets {
		if asset.Path != filepath.Base(asset.Path) {
			t.Fatalf("unsafe asset path %q", asset.Path)
		}
		data := readYBR422Fixture(t, asset.Path)
		digest := sha256.Sum256(data)
		if len(data) != asset.Bytes || hex.EncodeToString(digest[:]) != asset.SHA256 {
			t.Fatalf("fixture asset %q changed without provenance", asset.Path)
		}
	}
}

// The core backend reconstructs the same component planes as OpenJPEG, and
// repeats each chroma sample over the two columns it covers.
func TestCoreYBRFull422PlanesMatchOpenJPEG(t *testing.T) {
	planes := readYBR422Fixture(t, "ybr422-17x6.openjpeg.raw")
	chromaColumns := (ybr422Columns + 1) / 2
	if len(planes) != ybr422Rows*(ybr422Columns+2*chromaColumns) {
		t.Fatalf("reference holds %d samples", len(planes))
	}
	luma, chroma := planes[:ybr422Rows*ybr422Columns], planes[ybr422Rows*ybr422Columns:]
	decoded, err := DecodeCoreReduced(context.Background(), readYBR422Fixture(t, "ybr422-17x6.j2k"), 0, CoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Columns != ybr422Columns || decoded.Rows != ybr422Rows || len(decoded.Components) != 3 {
		t.Fatalf("decoded %dx%d with %d components", decoded.Columns, decoded.Rows, len(decoded.Components))
	}
	for y := 0; y < ybr422Rows; y++ {
		for x := 0; x < ybr422Columns; x++ {
			want := [3]byte{luma[y*ybr422Columns+x], chroma[y*chromaColumns+x/2], chroma[(ybr422Rows+y)*chromaColumns+x/2]}
			for c := range want {
				if got := decoded.Components[c][y*ybr422Columns+x]; got != int32(want[c]) {
					t.Fatalf("component %d at (%d,%d) = %d, want %d", c, x, y, got, want[c])
				}
			}
		}
	}
}

// YBR_FULL_422 with a 4:2:2 SIZ decodes to full-resolution interleaved RGB,
// reported as RGB and lossy, under every registration of the core backend.
func TestDecodeYBRFull422ToRGB(t *testing.T) {
	codestream := readYBR422Fixture(t, "ybr422-17x6.j2k")
	want := readYBR422Fixture(t, "ybr422-17x6.rgb.raw")
	if len(want) != ybr422Rows*ybr422Columns*3 {
		t.Fatalf("expected RGB holds %d bytes", len(want))
	}
	registered := func(register func(pixeldata.Registry) error) *Codec {
		registry := pixeldata.NewMemoryRegistry()
		if err := register(registry); err != nil {
			t.Fatal(err)
		}
		codec, ok := registry.GetCodec(transfer.JPEG2000LosslessOnly.UID)
		if !ok {
			t.Fatal("no .90 codec registered")
		}
		return codec.(*Codec)
	}
	for name, codec := range map[string]*Codec{
		"Register":     registered(Register),
		"RegisterCore": registered(func(r pixeldata.Registry) error { return RegisterCore(r, CoreOptions{}) }),
		"New":          New(),
		"NewCore":      NewCore(CoreOptions{}),
	} {
		t.Run(name, func(t *testing.T) {
			obj, pixel := jpeg2000Object(t, ybr422Metadata(), codestream)
			frames, err := codec.DecodeContext(context.Background(), pixel, obj)
			if err != nil {
				t.Fatal(err)
			}
			if frames.PhotometricInterpretation != "RGB" || !frames.Lossy {
				t.Fatalf("frames report %q lossy=%t, want RGB lossy", frames.PhotometricInterpretation, frames.Lossy)
			}
			if frames.Rows != ybr422Rows || frames.Columns != ybr422Columns || len(frames.Data) != 1 || !bytes.Equal(frames.Data[0], want) {
				t.Fatal("decoded RGB differs from the independent reference")
			}
		})
	}
}

// Decompression writes the RGB frame and records the conversion as lossy,
// although the codestream is reversible; a frame reader returns the same.
func TestDecompressYBRFull422WritesLossyRGB(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	want := readYBR422Fixture(t, "ybr422-17x6.rgb.raw")
	obj, _ := jpeg2000Object(t, ybr422Metadata(), readYBR422Fixture(t, "ybr422-17x6.j2k"))
	file := &object.File{Dataset: obj, TransferSyntax: transfer.JPEG2000LosslessOnly}
	got, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	for tag, value := range map[core.Tag]string{tagPhotometricInterpretation: "RGB", core.NewTag(0x0028, 0x2110): "01"} {
		if got, _ := got.Dataset.GetString(tag); got != value {
			t.Fatalf("%s = %q, want %q", tag, got, value)
		}
	}
	if raw, _ := got.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, want) {
		t.Fatal("decompressed Pixel Data differs from the independent reference")
	}

	reader, err := pixeldata.NewFrameReader(context.Background(), file, nil, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	frame, metadata, err := reader.Frame(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PhotometricInterpretation != "RGB" || !bytes.Equal(frame, want) {
		t.Fatalf("frame reader returned %q, want the RGB reference", metadata.PhotometricInterpretation)
	}
	// Regions fall back to the whole RGB frame, cropped.
	region, _, err := reader.Region(context.Background(), 0, image.Rect(3, 1, 8, 4))
	if err != nil {
		t.Fatal(err)
	}
	var crop []byte
	for y := 1; y < 4; y++ {
		crop = append(crop, want[(y*ybr422Columns+3)*3:(y*ybr422Columns+8)*3]...)
	}
	if !bytes.Equal(region, crop) {
		t.Fatal("region differs from the cropped RGB reference")
	}
}

// patchSIZ rewrites one byte of the main header SIZ marker segment: the
// component parameters (Ssiz, XRsiz, YRsiz) of component c start at offset
// 42+3c of a codestream whose SIZ follows SOC.
func patchSIZ(codestream []byte, offset int, value byte) []byte {
	out := append([]byte(nil), codestream...)
	out[offset] = value
	return out
}

// Other layouts keep failing: only 8-bit 4:2:2 with full-resolution luma,
// matching metadata and no component transform is decoded.
func TestDecodeYBRFull422RejectsOtherLayouts(t *testing.T) {
	codestream := readYBR422Fixture(t, "ybr422-17x6.j2k")
	if codestream[2] != 0xff || codestream[3] != 0x51 || codestream[42] != 7 || codestream[46] != 2 || codestream[47] != 1 {
		t.Fatal("fixture SIZ is not where the patches expect it")
	}
	cod := bytes.Index(codestream, []byte{0xff, 0x52})
	if cod < 0 {
		t.Fatal("fixture has no COD")
	}
	component := func(c, field int) int { return 42 + 3*c + field }
	metadata := ybr422Metadata()
	for _, tc := range []struct {
		name       string
		codestream []byte
		metadata   jpeg2000MetadataOptions
		want       error
	}{
		{"4:2:0 chroma", patchSIZ(patchSIZ(codestream, component(1, 2), 2), component(2, 2), 2), metadata, ErrUnsupportedCodestreamFeature},
		{"chroma components sampled differently", patchSIZ(codestream, component(2, 1), 1), metadata, ErrUnsupportedCodestreamFeature},
		{"subsampled luma", patchSIZ(codestream, component(0, 1), 2), metadata, ErrUnsupportedCodestreamFeature},
		{"4:4:4 codestream", patchSIZ(patchSIZ(codestream, component(1, 1), 1), component(2, 1), 1), metadata, ErrUnsupportedCodestreamFeature},
		{"16-bit codestream", patchSIZ(patchSIZ(patchSIZ(codestream, component(0, 0), 15), component(1, 0), 15), component(2, 0), 15), metadata, ErrUnsupportedCodestreamFeature},
		{"16-bit samples", patchSIZ(patchSIZ(patchSIZ(codestream, component(0, 0), 15), component(1, 0), 15), component(2, 0), 15),
			jpeg2000MetadataOptions{rows: ybr422Rows, columns: ybr422Columns, samplesPerPixel: 3, bitsAllocated: 16, photometric: "YBR_FULL_422", planarConfiguration: uint16Ptr(0)},
			pixeldata.ErrUnsupportedPhotometricInterpretation},
		{"signed components", patchSIZ(codestream, component(1, 0), 0x87), metadata, ErrUnsupportedCodestreamFeature},
		{"multiple component transform", patchSIZ(codestream, cod+8, 1), metadata, ErrUnsupportedCodestreamFeature},
		{"Columns smaller than the codestream", codestream,
			jpeg2000MetadataOptions{rows: ybr422Rows, columns: ybr422Columns - 1, samplesPerPixel: 3, photometric: "YBR_FULL_422", planarConfiguration: uint16Ptr(0)},
			ErrImageSizeMismatch},
		{"one sample per pixel", codestream,
			jpeg2000MetadataOptions{rows: ybr422Rows, columns: ybr422Columns, photometric: "YBR_FULL_422"},
			pixeldata.ErrUnsupportedPhotometricInterpretation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, pixel := jpeg2000Object(t, tc.metadata, tc.codestream)
			_, err := NewCore(CoreOptions{}).DecodeContext(context.Background(), pixel, obj)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeContext() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// Backends that do not convert 4:2:2 chroma keep refusing YBR_FULL_422.
func TestDecodeYBRFull422RefusedWithoutConversion(t *testing.T) {
	codestream := readYBR422Fixture(t, "ybr422-17x6.j2k")
	for name, codec := range map[string]*Codec{
		"go-jpeg2000":    NewWithDecoder(goJPEG2000Decoder{}),
		"custom decoder": NewWithDecoder(&borrowedPayloadDecoder{}),
	} {
		t.Run(name, func(t *testing.T) {
			obj, pixel := jpeg2000Object(t, ybr422Metadata(), codestream)
			if _, err := codec.DecodeContext(context.Background(), pixel, obj); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
				t.Fatalf("DecodeContext() error = %v, want ErrUnsupportedPhotometricInterpretation", err)
			}
		})
	}
	// A CAP marker declaring ISO/IEC 15444-2 routes the frame to go-jpeg2000.
	part2 := append([]byte{0xff, 0x4f, 0xff, 0x50, 0x00, 0x08, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00}, codestream[2:]...)
	if !isPart2Codestream(part2) {
		t.Fatal("patched codestream does not declare Part 2")
	}
	if _, err := (pureGoDecoder{}).DecodeFrame(part2,
		pixeldata.Metadata{PhotometricInterpretation: "YBR_FULL_422"}); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
		t.Fatalf("Part 2 YBR_FULL_422 error = %v, want ErrUnsupportedPhotometricInterpretation", err)
	}
}
