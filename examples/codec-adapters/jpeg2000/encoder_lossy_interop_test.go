//go:build jpeg2000_openjpeg || codecfull

package jpeg2000

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// lossySource is a synthetic native image: a smooth field with some texture,
// two frames, so that PSNR varies with the rate the way it does on images.
func lossySource(columns, rows, components, bits int) (*object.Object, [][]int32) {
	frames := make([][]int32, 2)
	var pixels []byte
	for f := range frames {
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				for c := 0; c < components; c++ {
					top := float64(int(1)<<bits - 1)
					v := 0.5 + 0.35*math.Sin(float64(x+7*f)/17+float64(c))*math.Cos(float64(y)/23) + 0.1*math.Sin(float64(x*y)/211)
					sample := int32(math.Round(math.Max(0, math.Min(1, v)) * top))
					frames[f] = append(frames[f], sample)
					if bits > 8 {
						pixels = binary.LittleEndian.AppendUint16(pixels, uint16(sample))
					} else {
						pixels = append(pixels, byte(sample))
					}
				}
			}
		}
	}
	allocated := 8
	if bits > 8 {
		allocated = 16
	}
	us := func(group, element uint16, v int) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value{uint16(v)}}
	}
	photometric := "MONOCHROME2"
	if components == 3 {
		photometric = "RGB"
	}
	vr := core.VROB
	if allocated == 16 {
		vr = core.VROW
	}
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1.1032.1"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}},
		us(0x0028, 0x0002, components),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{photometric}},
		us(0x0028, 0x0010, rows), us(0x0028, 0x0011, columns),
		us(0x0028, 0x0100, allocated), us(0x0028, 0x0101, bits), us(0x0028, 0x0102, bits-1), us(0x0028, 0x0103, 0),
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: vr}, Value: core.RawValue(pixels)},
	}
	if components == 3 {
		elements = append(elements, us(0x0028, 0x0006, 0))
	}
	return object.FromElements(elements, nil), frames
}

// TestOpenJPEGLossyEncoderRatesAndCrossDecode transcodes native images to
// .91 at several rates and PSNR targets, checks the lossy metadata the
// transcoder records, and decodes every frame with opj_decompress and with
// the pure-Go core: the two must agree within one step, and the PSNR against
// the source is reported per rate. Enable with DICOMGO_J2K_ENCODER_INTEROP=1,
// DICOM_GO_OPENJPEG_COMPRESS and DICOM_GO_OPENJPEG_DECOMPRESS.
func TestOpenJPEGLossyEncoderRatesAndCrossDecode(t *testing.T) {
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if os.Getenv("DICOMGO_J2K_ENCODER_INTEROP") != "1" || decompress == "" {
		t.Skip("set DICOMGO_J2K_ENCODER_INTEROP=1 and DICOM_GO_OPENJPEG_DECOMPRESS for the qualified runtime profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	images := []struct {
		name                     string
		columns, rows, comp, bit int
	}{
		{"mono12-256x192", 256, 192, 1, 12},
		{"rgb8-160x120", 160, 120, 3, 8},
	}
	codings := []struct {
		name   string
		coding OpenJPEGLossyOptions
	}{
		{"rate-80", OpenJPEGLossyOptions{Rates: []float64{80}}},
		{"rate-40", OpenJPEGLossyOptions{Rates: []float64{40}}},
		{"rate-20", OpenJPEGLossyOptions{Rates: []float64{20}}},
		{"rate-10", OpenJPEGLossyOptions{Rates: []float64{10}}},
		{"rate-5", OpenJPEGLossyOptions{Rates: []float64{5}}},
		{"layers-40-10-tiles-rpcl", OpenJPEGLossyOptions{Rates: []float64{40, 10}, TileWidth: 64, TileHeight: 64, Resolutions: 4, Progression: "RPCL"}},
		{"psnr-45", OpenJPEGLossyOptions{Quality: []float64{45}}},
		{"reversible-truncated-20", OpenJPEGLossyOptions{Rates: []float64{20}, Reversible: true}},
		{"rgb-kept", OpenJPEGLossyOptions{Rates: []float64{20}, KeepRGB: true}},
	}
	var table []string
	for _, image := range images {
		source, want := lossySource(image.columns, image.rows, image.comp, image.bit)
		for _, c := range codings {
			if c.coding.KeepRGB && image.comp != 3 {
				continue
			}
			t.Run(image.name+"/"+c.name, func(t *testing.T) {
				registry := pixeldata.NewMemoryEncoderRegistry()
				if err := RegisterOpenJPEGLossyEncoder(ctx, registry, OpenJPEGEncoderOptions{}, c.coding); err != nil {
					t.Fatal(err)
				}
				converted, report, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.JPEG2000, pixeldata.TranscodeOptions{EncoderRegistry: registry, AllowLossy: true})
				if err != nil || !report.Lossy || report.Frames != 2 {
					t.Fatalf("transcode: %+v %v", report, err)
				}
				defer converted.Close()
				if v, _ := converted.GetString(core.NewTag(0x0028, 0x2110)); strings.TrimSpace(v) != "01" {
					t.Fatalf("Lossy Image Compression %q", v)
				}
				method, _ := converted.GetStrings(core.NewTag(0x0028, 0x2114))
				ratios, _ := converted.GetStrings(core.NewTag(0x0028, 0x2112))
				if len(method) != 1 || strings.TrimSpace(method[0]) != "ISO_15444_1" || len(ratios) != 1 {
					t.Fatalf("method %q ratio %q", method, ratios)
				}
				meta, err := pixeldata.ExtractMetadata(converted)
				if err != nil {
					t.Fatal(err)
				}
				wantPhotometric := "MONOCHROME2"
				if image.comp == 3 {
					wantPhotometric = "YBR_ICT"
					if c.coding.Reversible {
						wantPhotometric = "YBR_RCT"
					}
					if c.coding.KeepRGB {
						wantPhotometric = "RGB"
					}
				}
				if meta.PhotometricInterpretation != wantPhotometric {
					t.Fatalf("Photometric Interpretation %q, want %q", meta.PhotometricInterpretation, wantPhotometric)
				}
				pixel, err := pixeldata.Extract(converted)
				if err != nil {
					t.Fatal(err)
				}
				payloads, err := framePayloads(pixel, 2)
				if err != nil {
					t.Fatal(err)
				}
				var sumSquares, encoded float64
				count := 0
				worstCross := int32(0)
				for f, payload := range payloads {
					encoded += float64(len(payload))
					path := filepath.Join(t.TempDir(), "frame.j2k")
					if err := os.WriteFile(path, payload, 0o600); err != nil {
						t.Fatal(err)
					}
					out := filepath.Join(t.TempDir(), "frame.rawl")
					runOpenJPEG(t, decompress, "-i", path, "-o", out)
					raw, err := os.ReadFile(out)
					if err != nil {
						t.Fatal(err)
					}
					core, err := DecodeCoreReduced(ctx, payload, 0, CoreOptions{})
					if err != nil {
						t.Fatal(err)
					}
					for comp := 0; comp < image.comp; comp++ {
						var openjpeg []int32
						openjpeg, raw, err = readPlane(raw, image.columns*image.rows, image.bit, false)
						if err != nil {
							t.Fatal(err)
						}
						for i, v := range openjpeg {
							d := core.Components[comp][i] - v
							worstCross = max(worstCross, d, -d)
							e := float64(v - want[f][i*image.comp+comp])
							sumSquares += e * e
							count++
						}
					}
				}
				if worstCross > 1 {
					t.Fatalf("the core and OpenJPEG decode the frames %d steps apart", worstCross)
				}
				top := float64(int(1)<<image.bit - 1)
				psnr := 10 * math.Log10(top*top/(sumSquares/float64(count)))
				native := float64(2 * image.columns * image.rows * image.comp * ((image.bit + 7) / 8))
				line := fmt.Sprintf("| %s | %s | %.1f:1 | %.1f dB | %d |", image.name, c.name, native/encoded, psnr, worstCross)
				table = append(table, line)
				if psnr < 25 {
					t.Fatalf("PSNR %.1f dB", psnr)
				}
			})
		}
	}
	for _, line := range table {
		t.Log(line)
	}
}
