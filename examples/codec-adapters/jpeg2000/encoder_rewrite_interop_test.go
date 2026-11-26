//go:build jpeg2000_openjpeg || codecfull

package jpeg2000

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func rewriteUS(group, element uint16, v int) core.Element {
	return core.NewRawElement(core.NewTag(group, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
}

// rewriteSource is a native two-frame 24x16 image the OpenJPEG encoder
// refuses as it is stored.
func rewriteSource(photometric string, samples, bits, planar int, pixels []byte, extra ...core.Element) *object.Object {
	vr := core.VROB
	if bits > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1.1036.2"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{photometric}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}},
		rewriteUS(0x0028, 0x0002, samples), rewriteUS(0x0028, 0x0010, 16), rewriteUS(0x0028, 0x0011, 24),
		rewriteUS(0x0028, 0x0100, bits), rewriteUS(0x0028, 0x0101, bits), rewriteUS(0x0028, 0x0102, bits-1), rewriteUS(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if samples == 3 {
		elements = append(elements, rewriteUS(0x0028, 0x0006, planar))
	}
	return object.FromElements(append(elements, extra...), nil)
}

// TestOpenJPEGEncodesRewrittenRepresentations transcodes PALETTE COLOR,
// YBR_FULL and planar RGB, which the OpenJPEG encoder refuses, to JPEG 2000
// lossless through TranscodeOptions.Rewrite, and checks that opj_decompress
// and the pure-Go core decode exactly the RGB samples RewriteNative gives.
// Enable with DICOMGO_J2K_ENCODER_INTEROP=1 and DICOM_GO_OPENJPEG_DECOMPRESS.
func TestOpenJPEGEncodesRewrittenRepresentations(t *testing.T) {
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if os.Getenv("DICOMGO_J2K_ENCODER_INTEROP") != "1" || decompress == "" {
		t.Skip("set DICOMGO_J2K_ENCODER_INTEROP=1 and DICOM_GO_OPENJPEG_DECOMPRESS for the qualified runtime profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const pixels = 2 * 16 * 24
	indices := make([]byte, pixels)
	ybr := make([]byte, 3*pixels)
	rgb16 := make([]byte, 6*pixels)
	for i := 0; i < pixels; i++ {
		indices[i] = byte(i * 7)
		ybr[3*i], ybr[3*i+1], ybr[3*i+2] = byte(40+i%170), byte(128+(i*3)%60-30), byte(128+(i*5)%50-25)
		for c := 0; c < 3; c++ {
			binary.LittleEndian.PutUint16(rgb16[2*(3*i+c):], uint16((i*97+c*4099)%65536))
		}
	}
	var palette []core.Element
	for c := 0; c < 3; c++ {
		descriptor := binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(nil, 256), 0), 16)
		var data []byte
		for i := 0; i < 256; i++ {
			data = binary.LittleEndian.AppendUint16(data, uint16((i*257*(c+1)+c*9000)%65536))
		}
		palette = append(palette, core.NewRawElement(core.NewTag(0x0028, 0x1101+uint16(c)), core.VRUS, descriptor),
			core.NewRawElement(core.NewTag(0x0028, 0x1201+uint16(c)), core.VROW, data))
	}
	planarZero := uint16(0)
	for _, tc := range []struct {
		name    string
		source  *object.Object
		rewrite pixeldata.NativeRewrite
		bits    int
	}{
		{"palette", rewriteSource("PALETTE COLOR", 1, 8, 0, indices, palette...), pixeldata.NativeRewrite{PaletteToRGB: true}, 16},
		{"ybr-full-planar", rewriteSource("YBR_FULL", 3, 8, 1, ybr), pixeldata.NativeRewrite{YBRFullToRGB: true, PlanarConfiguration: &planarZero}, 8},
		{"rgb16-planar", rewriteSource("RGB", 3, 16, 1, rgb16), pixeldata.NativeRewrite{PlanarConfiguration: &planarZero}, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := pixeldata.NewMemoryEncoderRegistry()
			if err := RegisterOpenJPEGLosslessEncoder(ctx, registry, OpenJPEGEncoderOptions{}); err != nil {
				t.Fatal(err)
			}
			options := pixeldata.TranscodeOptions{EncoderRegistry: registry}
			if _, _, err := pixeldata.TranscodeDataSet(ctx, tc.source, transfer.ExplicitVRLittleEndian, transfer.JPEG2000LosslessOnly, options); err == nil {
				t.Fatal("the encoder accepted the source without a rewrite")
			}
			options.Rewrite = &tc.rewrite
			encoded, report, err := pixeldata.TranscodeDataSet(ctx, tc.source, transfer.ExplicitVRLittleEndian, transfer.JPEG2000LosslessOnly, options)
			if err != nil || report.Rewrite == nil || report.Frames != 2 {
				t.Fatalf("transcode: %+v %v", report, err)
			}
			want, _, err := pixeldata.RewriteNative(ctx, tc.source, tc.rewrite)
			if err != nil {
				t.Fatal(err)
			}
			wantFrames, err := pixeldata.ExtractNativeFrames(want)
			if err != nil {
				t.Fatal(err)
			}
			pixel, err := pixeldata.Extract(encoded)
			if err != nil {
				t.Fatal(err)
			}
			payloads, err := framePayloads(pixel, 2)
			if err != nil {
				t.Fatal(err)
			}
			for f, payload := range payloads {
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
				decoded, err := DecodeCoreReduced(ctx, payload, 0, CoreOptions{})
				if err != nil {
					t.Fatal(err)
				}
				expected := wantFrames.Data[f]
				for c := 0; c < 3; c++ {
					var plane []int32
					plane, raw, err = readPlane(raw, 16*24, tc.bits, false)
					if err != nil {
						t.Fatal(err)
					}
					for i, v := range plane {
						var w int32
						if tc.bits > 8 {
							w = int32(binary.LittleEndian.Uint16(expected[2*(3*i+c):]))
						} else {
							w = int32(expected[3*i+c])
						}
						if v != w || decoded.Components[c][i] != w {
							t.Fatalf("frame %d component %d sample %d: OpenJPEG %d, core %d, want %d", f, c, i, v, decoded.Components[c][i], w)
						}
					}
				}
			}
		})
	}
}
