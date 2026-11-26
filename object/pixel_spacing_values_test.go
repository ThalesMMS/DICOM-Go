package object

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

// TestSpacingPairValueCounts covers the value counts of Pixel Spacing
// (#1112). The first five cases are those of ITK's itkGDCMImageIOGTest.cxx
// (SingleValuePixelSpacingIsIsotropic, TrailingBackslashPixelSpacingIsStill-
// Isotropic, TwoValuePixelSpacingIsRespected, MalformedSingleValuePixel-
// SpacingThrows, TrailingGarbageSingleValuePixelSpacingThrows), in DICOM
// (row, column) order where ITK reports (x, y).
func TestSpacingPairValueCounts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       string
		row, column float64
		values      SpacingValues
		malformed   bool
	}{
		{"single value is isotropic", "0.5 ", 0.5, 0.5, SpacingValuesSingle, false},
		{"trailing backslash is still isotropic", `0.5\`, 0.5, 0.5, SpacingValuesSingle, false},
		{"two values are respected", `0.5\0.25 `, 0.5, 0.25, SpacingValuesPair, false},
		{"malformed single value", "N/A ", 0, 0, SpacingValuesAbsent, true},
		{"trailing garbage", "0.5abc ", 0, 0, SpacingValuesAbsent, true},
		{"three values use the first two", `0.5\0.25\0.1`, 0.5, 0.25, SpacingValuesExtra, false},
		{"padded values", ` 0.7 \ 0.8 `, 0.7, 0.8, SpacingValuesPair, false},
		{"exponent", `5E-1\2.5e-1`, 0.5, 0.25, SpacingValuesPair, false},
		{"empty", "", 0, 0, SpacingValuesAbsent, false},
		{"only separators", `\`, 0, 0, SpacingValuesAbsent, false},
		{"empty first value", `\0.5`, 0, 0, SpacingValuesAbsent, true},
		{"zero", `0\0.5`, 0, 0, SpacingValuesAbsent, true},
		{"negative", `0.5\-0.5`, 0, 0, SpacingValuesAbsent, true},
		{"not a number", `NaN\0.5`, 0, 0, SpacingValuesAbsent, true},
		{"infinite", `0.5\Inf`, 0, 0, SpacingValuesAbsent, true},
		{"malformed third value is ignored", `0.5\0.25\x`, 0.5, 0.25, SpacingValuesExtra, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := FromElements([]core.Element{core.NewRawElement(tagPixelSpacing, core.VRDS, []byte(tc.value))}, nil)
			row, column, values, err := obj.SpacingPair(tagPixelSpacing)
			if tc.malformed != errors.Is(err, ErrMalformedSpacing) || !tc.malformed && err != nil {
				t.Fatalf("error %v, malformed %v", err, tc.malformed)
			}
			if row != tc.row || column != tc.column || values != tc.values {
				t.Fatalf("SpacingPair = %v, %v, %q; want %v, %v, %q", row, column, values, tc.row, tc.column, tc.values)
			}
			// Frame geometry and the resolved spacing agree with SpacingPair
			// and never invent a value.
			geometry := obj.FrameGeometry()
			if tc.values == SpacingValuesAbsent {
				if geometry.PixelSpacing != nil || (geometry.PixelSpacingErr != nil) != tc.malformed {
					t.Fatalf("geometry %v, error %v", geometry.PixelSpacing, geometry.PixelSpacingErr)
				}
				if obj.PixelSpacingAt(0).Valid() {
					t.Fatal("a malformed or absent spacing resolved")
				}
				return
			}
			if len(geometry.PixelSpacing) != 2 || geometry.PixelSpacing[0] != tc.row || geometry.PixelSpacing[1] != tc.column || geometry.PixelSpacingValues != tc.values {
				t.Fatalf("geometry %v (%q)", geometry.PixelSpacing, geometry.PixelSpacingValues)
			}
			if resolved := obj.PixelSpacingAt(0); resolved.RowSpacing != tc.row || resolved.ColumnSpacing != tc.column || resolved.Values != tc.values {
				t.Fatalf("PixelSpacingAt = %+v", resolved)
			}
		})
	}
	if _, _, _, err := FromElements([]core.Element{core.NewRawElement(tagPixelSpacing, core.VRUS, []byte{1, 0})}, nil).SpacingPair(tagPixelSpacing); !errors.Is(err, ErrMalformedSpacing) {
		t.Fatalf("Pixel Spacing with VR US: %v", err)
	}
	if _, _, values, err := New(nil).SpacingPair(tagPixelSpacing); values != SpacingValuesAbsent || err != nil {
		t.Fatalf("missing Pixel Spacing: %q %v", values, err)
	}
}

// TestFrameGeometryMalformedPerFrameSpacing: a malformed per-frame Pixel
// Measures spacing keeps the top-level value, as PixelSpacingAt does, and
// reports the error.
func TestFrameGeometryMalformedPerFrameSpacing(t *testing.T) {
	measures := FromElements([]core.Element{core.NewRawElement(tagPixelSpacing, core.VRDS, []byte("N/A "))}, nil)
	var top FrameGeometry
	top.PixelSpacing, top.PixelSpacingValues = []float64{1, 1}, SpacingValuesPair
	overlayFrameGeometry(&top, measures.FrameGeometry(), GeometrySourcePerFrameFunctionalGroups)
	if len(top.PixelSpacing) != 2 || !errors.Is(top.PixelSpacingErr, ErrMalformedSpacing) {
		t.Fatalf("overlay kept %v, error %v", top.PixelSpacing, top.PixelSpacingErr)
	}
}
