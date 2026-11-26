package pixeldata_test

import (
	"bytes"
	"errors"
	"image/color"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// These generated 1x2 inputs qualify the acceptance harness contract. They are
// not the unavailable SIEMENS/PMS/GEMS files and never qualify their GDCM hashes.
func TestIncompleteNativeAcceptanceSyntheticContract(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			wire := incompleteNativeFile(t, syntax, 9)
			before := bytes.Clone(wire)
			out, report, err := incompleteNativeAcceptance(wire)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			pixel, err := pixeldata.Extract(out.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			// Independent canonical little-endian samples: 1, 0x1234,
			// 0x4567, 0xfedc; the fifth partial sample must be discarded.
			want := []byte{1, 0, 0x34, 0x12, 0x67, 0x45, 0xdc, 0xfe}
			if !bytes.Equal(pixel.Raw, want) || !bytes.Equal(wire, before) {
				t.Fatalf("prefix=%x want=%x or source mutated", pixel.Raw, want)
			}
			images, err := incompleteAcceptanceImages(out, report)
			if err != nil {
				t.Fatal(err)
			}
			// DICOM linear full-range window maps these unsigned values to
			// the following bytes; test actual pixels, not just render success.
			for i, expected := range [][2]uint8{{0, 18}, {69, 254}} {
				for x, value := range expected {
					got := color.GrayModel.Convert(images[i].At(x, 0)).(color.Gray).Y
					if got != value {
						t.Fatalf("frame=%d x=%d gray=%d want=%d", i, x, got, value)
					}
				}
			}
			missingDiagnostic := report
			missingDiagnostic.Diagnostics = nil
			if _, err := incompleteAcceptanceImages(out, missingDiagnostic); err == nil {
				t.Fatal("acceptance admitted missing diagnostic")
			}
			wrongCount := report
			copy := *report.Incomplete
			copy.RetainedFrames++
			wrongCount.Incomplete = &copy
			if _, err := incompleteAcceptanceImages(out, wrongCount); err == nil {
				t.Fatal("acceptance admitted a contradictory frame count")
			}
		})
	}
}

func TestIncompleteNativeAcceptanceSyntheticNoFrame(t *testing.T) {
	// One 16-bit sample plus a trailing byte is not a 1x2 complete frame.
	out, report, err := incompleteNativeAcceptance(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 3))
	if out != nil || report.Incomplete != nil || !errors.Is(err, pixeldata.ErrNoCompleteFrames) {
		t.Fatalf("partial frame became accepted display: out=%v report=%+v err=%v", out, report, err)
	}
}

func TestIncompleteNativeAcceptanceSyntheticRefusals(t *testing.T) {
	// A complete file cannot be silently accepted as an incomplete-data case.
	if out, _, err := incompleteNativeAcceptance(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 12)); out != nil || err == nil {
		t.Fatal("complete input incorrectly qualified the recovery acceptance case")
	}
	out, report, err := incompleteNativeAcceptance(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 9))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	// Byte counts alone cannot qualify an image whose presentation is unsupported.
	out.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"UNSUPPORTED"}})
	if _, err := incompleteAcceptanceImages(out, report); err == nil {
		t.Fatal("acceptance ignored a display failure")
	}
}
