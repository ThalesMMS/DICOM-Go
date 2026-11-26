package pixeldata_test

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func overlayUS(g, e, v uint16) core.Element {
	return core.NewRawElement(core.NewTag(g, e), core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
}

func overlayText(g, e uint16, vr core.VR, v string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, e), VR: vr}, Value: core.StringValue{v}}
}

func embeddedOverlayImage() *object.Object {
	var native []byte
	for _, v := range []uint16{0x8001, 0x0002, 0x0003, 0x8004} {
		native = binary.LittleEndian.AppendUint16(native, v)
	}
	obj := object.FromElements([]core.Element{
		overlayText(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.4"),
		overlayText(0x0008, 0x0018, core.VRUI, "1.2.3.4"),
		overlayUS(0x0028, 0x0002, 1), overlayText(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		overlayUS(0x0028, 0x0010, 2), overlayUS(0x0028, 0x0011, 2), overlayUS(0x0028, 0x0100, 16),
		overlayUS(0x0028, 0x0101, 12), overlayUS(0x0028, 0x0102, 11), overlayUS(0x0028, 0x0103, 0),
		overlayUS(0x6000, 0x0010, 2), overlayUS(0x6000, 0x0011, 2), overlayText(0x6000, 0x0040, core.VRCS, "G"),
		overlayUS(0x6000, 0x0100, 16), overlayUS(0x6000, 0x0102, 15),
		core.NewRawElement(core.TagPixelData, core.VROW, native),
	}, nil)
	obj.SetValueByteOrder(binary.LittleEndian)
	return obj
}

func TestTranscodeEmbeddedOverlayPolicy(t *testing.T) {
	ctx := context.Background()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	options := pixeldata.TranscodeOptions{EncoderRegistry: encoders}
	if _, _, err := pixeldata.TranscodeDataSet(ctx, embeddedOverlayImage(), transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); !errors.Is(err, pixeldata.ErrTranscodeEmbeddedOverlay) {
		t.Fatalf("encoded target error = %v", err)
	}
	// A native target keeps the bits.
	if _, _, err := pixeldata.TranscodeDataSet(ctx, embeddedOverlayImage(), transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, pixeldata.TranscodeOptions{}); err != nil {
		t.Fatalf("native target: %v", err)
	}
	options.ExtractEmbeddedOverlays = true
	decoders := pixeldata.NewMemoryRegistry()
	if err := rle.Register(decoders); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"explicit dimensions", "missing dimensions"} {
		t.Run(name, func(t *testing.T) {
			source := embeddedOverlayImage()
			if name == "missing dimensions" {
				source.Remove(core.NewTag(0x6000, 0x0010))
				source.Remove(core.NewTag(0x6000, 0x0011))
			}
			encoded, report, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
			if err != nil || report.EmbeddedOverlaysExtracted != 1 {
				t.Fatalf("extract: %v %+v", err, report)
			}
			overlays, err := display.OverlaysFromObject(encoded)
			if err != nil || len(overlays) != 1 || overlays[0].Embedded || overlays[0].Rows != 2 || overlays[0].Columns != 2 || string(overlays[0].Unpacked()) != "\xff\x00\x00\xff" {
				t.Fatalf("moved overlay %+v %v", overlays, err)
			}
			decoded, _, err := pixeldata.DecompressDataSet(encoded, transfer.RLELossless, pixeldata.DecompressOptions{Registry: decoders})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := decoded.GetRaw(core.TagPixelData)
			for i := 0; i+1 < len(raw); i += 2 {
				if binary.LittleEndian.Uint16(raw[i:])&0x8000 != 0 {
					t.Fatalf("overlay bit left in sample %d", i/2)
				}
			}
		})
	}
}

// Overlay oracle: GDCM Testing/Source/MediaStorageAndFileFormat/Cxx/
// TestOverlay3.cxx (MD5 of the unpacked first overlay frame).
func TestOverlaysMatchGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a separately obtained GDCMData checkout")
	}
	for name, want := range map[string]string{
		"MR-SIEMENS-DICOM-WithOverlays-extracted-overlays.dcm":    "d42bff3545ed8c5fccb39d9a61828992",
		"MR-SIEMENS-DICOM-WithOverlays.dcm":                       "d42bff3545ed8c5fccb39d9a61828992",
		"PHILIPS_Brilliance_ExtraBytesInOverlay.dcm":              "2cf60257b75a034fbdc98e560881184e",
		"05115014-mr-siemens-avanto-syngo-with-palette-icone.dcm": "b2dd1007e018b3b9691761cf93f77552",
	} {
		f, err := object.OpenFileWithOptions(filepath.Join(root, name), object.ReadFileOptions{RecoverDelimiterAnomalies: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		overlays, err := display.OverlaysFromObject(f.Dataset)
		if err != nil || len(overlays) == 0 {
			t.Fatalf("%s: %v %d", name, err, len(overlays))
		}
		sum := md5.Sum(overlays[0].Unpacked())
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: md5 %s, want %s", name, got, want)
		}
	}
}
