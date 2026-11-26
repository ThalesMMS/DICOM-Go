package jpeg2000

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var transcodeIconSequenceTag = core.NewTag(0x0088, 0x0200)

func iconTranscodeOptions(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterCore(decoders, CoreOptions{}); err != nil {
		t.Fatal(err)
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: pureGoEncoders(t, false)}
}

func readTranscodedIcon(t *testing.T, dataset *object.Object) *object.Object {
	t.Helper()
	items, ok := dataset.GetSequence(transcodeIconSequenceTag)
	if !ok || len(items) != 1 {
		t.Fatalf("icon items=%d present=%v", len(items), ok)
	}
	return items[0]
}

func canonicalIcon(t *testing.T, icon *object.Object, syntax transfer.Syntax, options pixeldata.TranscodeOptions) []byte {
	t.Helper()
	p, err := pixeldata.ExtractView(icon)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Encapsulated && syntax.Encapsulated {
		syntax = transfer.ExplicitVRLittleEndian
	}
	native, _, err := pixeldata.TranscodeDataSet(context.Background(), icon, syntax, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	p, err = pixeldata.ExtractView(native)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), p.Raw...)
}

func TestCoreTranscodeIconsSynthetic(t *testing.T) {
	for _, compress := range []bool{false, true} {
		options := iconTranscodeOptions(t)
		options.CompressIcons = compress
		main, icon := codecfixture.NativeSmall().Object(), codecfixture.NativeSmall().Object()
		main.Put(core.Element{Header: core.ElementHeader{Tag: transcodeIconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}})
		want := canonicalIcon(t, icon, transfer.ExplicitVRLittleEndian, options)
		converted, report, err := pixeldata.TranscodeDataSet(context.Background(), main, transfer.ExplicitVRLittleEndian, transfer.JPEG2000LosslessOnly, options)
		if err != nil {
			t.Fatal(err)
		}
		p, err := pixeldata.ExtractView(readTranscodedIcon(t, converted))
		if err != nil || p.Encapsulated != compress || len(report.Icons) != 1 {
			t.Fatalf("icon/report=%v/%+v err=%v", p.Encapsulated, report, err)
		}
		if got := canonicalIcon(t, readTranscodedIcon(t, converted), transfer.JPEG2000LosslessOnly, options); !bytes.Equal(got, want) {
			t.Fatal("J2K icon changed")
		}
		back, _, err := pixeldata.TranscodeDataSet(context.Background(), converted, transfer.JPEG2000LosslessOnly, transfer.ExplicitVRLittleEndian, options)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalIcon(t, readTranscodedIcon(t, back), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
			t.Fatal("J2K round-trip icon changed")
		}
	}
}

// Corpus qualification is opt-in; no upstream files are copied into this tree.
func TestCoreTranscodeIconsGDCMFixtures(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to an existing GDCM data directory")
	}
	for _, name := range []string{"KODAK_CompressedIcon.dcm", "simpleImageWithIcon.dcm", "CT-SIEMENS-Icone-With-PaletteColor.dcm"} {
		t.Run(name, func(t *testing.T) {
			options := iconTranscodeOptions(t)
			file, err := object.OpenFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			want := canonicalIcon(t, readTranscodedIcon(t, file.Dataset), file.TransferSyntax, options)
			native, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.ExplicitVRLittleEndian, options)
			if err != nil {
				t.Fatal(err)
			}
			for _, compress := range []bool{false, true} {
				options.CompressIcons = compress
				converted, report, err := pixeldata.TranscodeFile(context.Background(), native, transfer.JPEG2000LosslessOnly, options)
				if name == "simpleImageWithIcon.dcm" && compress {
					if converted != nil || !errors.Is(err, pixeldata.ErrMissingMetadata) {
						t.Fatalf("incomplete icon encode=%v", err)
					}
					recoveryOptions := options
					recoveryOptions.Recovery.DefaultImagePixelModule = true
					converted, report, err = pixeldata.TranscodeFile(context.Background(), native, transfer.JPEG2000LosslessOnly, recoveryOptions)
					if err != nil || len(report.Icons) != 1 || !slices.Contains(report.Icons[0].DecodeDiagnostics, pixeldata.DiagnosticDefaultSamplesPerPixel) {
						t.Fatalf("opt-in J2K recovery report=%+v err=%v", report, err)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Icons) != 1 {
					t.Fatalf("report=%+v", report)
				}
				if got := canonicalIcon(t, readTranscodedIcon(t, converted.Dataset), transfer.JPEG2000LosslessOnly, options); !bytes.Equal(got, want) {
					t.Fatal("corpus J2K icon changed")
				}
				back, _, err := pixeldata.TranscodeFile(context.Background(), converted, transfer.ExplicitVRLittleEndian, options)
				if err != nil {
					t.Fatal(err)
				}
				if got := canonicalIcon(t, readTranscodedIcon(t, back.Dataset), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
					t.Fatal("corpus J2K round-trip icon changed")
				}
			}
		})
	}
}

func TestCoreTranscodePaletteIconsLosslessOnly(t *testing.T) {
	for _, bits := range []uint16{8, 16} {
		icon := codecfixture.NativeSmall().Object()
		icon.Put(core.Element{Header: core.ElementHeader{Tag: tagPhotometricInterpretation, VR: core.VRCS}, Value: core.StringValue{"PALETTE COLOR"}})
		icon.Put(core.Element{Header: core.ElementHeader{Tag: tagBitsAllocated, VR: core.VRUS}, Value: core.Uint16Value{bits}})
		icon.Put(core.Element{Header: core.ElementHeader{Tag: tagBitsStored, VR: core.VRUS}, Value: core.Uint16Value{bits}})
		icon.Put(core.Element{Header: core.ElementHeader{Tag: tagHighBit, VR: core.VRUS}, Value: core.Uint16Value{bits - 1}})
		raw := []byte{0, 1, 2, 255}
		if bits == 16 {
			raw = []byte{0, 0, 1, 0, 0x34, 0x12, 0xFF, 0xFF}
		}
		icon.Put(core.NewRawElement(core.TagPixelData, core.VROW, raw))
		for _, element := range []uint16{0x1101, 0x1102, 0x1103} {
			icon.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{4, 0, 16}})
		}
		for _, element := range []uint16{0x1201, 0x1202, 0x1203} {
			icon.Put(core.NewRawElement(core.NewTag(0x0028, element), core.VROW, []byte{0, 0, 0x55, 0x55, 0xAA, 0xAA, 0xFF, 0xFF}))
		}
		main := codecfixture.NativeSmall().Object()
		main.Put(core.Element{Header: core.ElementHeader{Tag: transcodeIconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}})
		options := iconTranscodeOptions(t)
		options.CompressIcons = true
		converted, _, err := pixeldata.TranscodeDataSet(context.Background(), main, transfer.ExplicitVRLittleEndian, transfer.JPEG2000LosslessOnly, options)
		if err != nil {
			t.Fatal(err)
		}
		convertedIcon := readTranscodedIcon(t, converted)
		if got := canonicalIcon(t, convertedIcon, transfer.JPEG2000LosslessOnly, options); !bytes.Equal(got, raw) {
			t.Fatalf("%d-bit palette indices changed", bits)
		}
		back, _, err := pixeldata.TranscodeDataSet(context.Background(), converted, transfer.JPEG2000LosslessOnly, transfer.ExplicitVRLittleEndian, options)
		if err != nil {
			t.Fatal(err)
		}
		for _, element := range []uint16{0x1101, 0x1102, 0x1103, 0x1201, 0x1202, 0x1203} {
			tag := core.NewTag(0x0028, element)
			want, _ := icon.Get(tag)
			got, ok := readTranscodedIcon(t, back).Get(tag)
			if !ok || !reflect.DeepEqual(got.Value, want.Value) || got.VR() != want.VR() {
				t.Fatalf("palette LUT %s changed", tag)
			}
		}
		pixel, err := pixeldata.ExtractView(convertedIcon)
		if err != nil {
			t.Fatal(err)
		}
		for _, register := range []func(pixeldata.Registry) error{func(r pixeldata.Registry) error { return RegisterCore(r, CoreOptions{}) }, Register} {
			registry := pixeldata.NewMemoryRegistry()
			if err := register(registry); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.DecodeFrames(transfer.JPEG2000.UID, pixel, convertedIcon); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
				t.Fatalf(".91 palette accepted: %v", err)
			}
			if _, err := registry.DecodeFrames(transfer.JPEG2000LosslessOnly.UID, pixel, convertedIcon); err != nil {
				t.Fatalf(".90 palette rejected: %v", err)
			}
		}
		convertedIcon.Put(core.Element{Header: core.ElementHeader{Tag: tagPixelRepresentation, VR: core.VRUS}, Value: core.Uint16Value{1}})
		if _, err := options.DecoderRegistry.DecodeFrames(transfer.JPEG2000LosslessOnly.UID, pixel, convertedIcon); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
			t.Fatalf("signed palette accepted: %v", err)
		}
	}
}
