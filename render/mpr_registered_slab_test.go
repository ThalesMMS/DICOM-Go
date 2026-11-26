package render

import (
	"context"
	"fmt"
	"testing"
)

func TestMPRRegisteredSlabReducesOnlyMappedSamples(t *testing.T) {
	geometry := mprCorpusGeometry{
		origin: Vec3{X: 10, Y: 20, Z: 30}, right: Vec3{X: 1}, down: Vec3{Y: 1}, normal: Vec3{Z: 1},
	}
	vol, err := BuildVolume(mprCorpusStack(geometry, false, true))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	plane := Plane{
		Origin: geometry.patient(Vec3{Z: 2}),
		U:      Vec3{X: 2 * mprCorpusColSpacing},
		V:      Vec3{Y: mprCorpusRowSpacing},
	}
	// Non-rigid displacement depends on which sample of the slab is mapped.
	// At the middle depth the field has a hole in column one. At the top,
	// the field is defined but pushes column two beyond the source volume.
	mapSample := func(patient Vec3) (Vec3, error) {
		switch {
		case patient.Z < plane.Origin.Z:
			patient.X += mprCorpusColSpacing
		case patient.Z > plane.Origin.Z:
			patient.X += 3 * mprCorpusColSpacing
		case patient.X == plane.Origin.X+mprCorpusColSpacing:
			return Vec3{}, fmt.Errorf("undefined cell: %w", ErrMapperOutsideDomain)
		}
		return patient, nil
	}
	// At each output pixel the unwarped samples are [c-9, c, c+9].
	// The overlay samples by column are [c-6,c,c+18], [c-6,c+18], [c-6,c].
	for _, tc := range []struct {
		name          string
		mode          SlabMode
		baseOffset    float64
		overlayOffset [3]float64
		terms         [3]float64
		wantCoverage  FusedMPRSamplingReport
	}{
		{"single", SlabNone, 0, [3]float64{}, [3]float64{1, 0, 1}, FusedMPRSamplingReport{TargetSamples: 6, BaseInBounds: 6, OverlayInBounds: 4}},
		{"maximum", SlabMIP, 9, [3]float64{18, 18, 0}, [3]float64{1, 1, 1}, FusedMPRSamplingReport{TargetSamples: 18, BaseInBounds: 18, OverlayInBounds: 14}},
		{"minimum", SlabMinIP, -9, [3]float64{-6, -6, -6}, [3]float64{1, 1, 1}, FusedMPRSamplingReport{TargetSamples: 18, BaseInBounds: 18, OverlayInBounds: 14}},
		{"average", SlabAverage, 0, [3]float64{4, 6, -3}, [3]float64{1, 1, 1}, FusedMPRSamplingReport{TargetSamples: 18, BaseInBounds: 18, OverlayInBounds: 14}},
		{"sum", SlabSum, 0, [3]float64{12, 12, -6}, [3]float64{3, 2, 2}, FusedMPRSamplingReport{TargetSamples: 18, BaseInBounds: 18, OverlayInBounds: 14}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ResliceFusedMPRScalarContext(context.Background(), FusedMPRRequest{
				Base: vol, Overlay: vol, TargetPlane: plane, Width: 3, Height: 2, TargetToSource: mapSample,
				Slab: FusedMPRSlab{Mode: tc.mode, ThicknessMM: 2 * mprCorpusSliceSpacing, SampleSpacingMM: mprCorpusSliceSpacing},
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Report != tc.wantCoverage {
				t.Errorf("coverage = %+v, want %+v", out.Report, tc.wantCoverage)
			}
			for y := range 2 {
				for x := range 3 {
					center := mprCorpusValue(Vec3{X: float64(x), Y: float64(y), Z: 2})
					wantBase := center + tc.baseOffset
					if tc.mode == SlabSum {
						wantBase = 3 * center
					}
					if got, valid := out.Base.ValueAt(x, y); !valid || got != wantBase {
						t.Errorf("base at (%d,%d) = %g (valid %v), want %g", x, y, got, valid, wantBase)
					}
					wantValid := tc.terms[x] > 0
					wantOverlay := center*tc.terms[x] + tc.overlayOffset[x]
					if got, valid := out.Overlay.ValueAt(x, y); valid != wantValid || got != wantOverlay {
						t.Errorf("overlay at (%d,%d) = %g (valid %v), want %g (valid %v)", x, y, got, valid, wantOverlay, wantValid)
					}
				}
			}
		})
	}
}
