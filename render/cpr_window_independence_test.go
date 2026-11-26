package render

import (
	"context"
	"math"
	"slices"
	"testing"
)

func TestCPRQuantitativeOutputSurvivesPresentationChanges(t *testing.T) {
	ctx := context.Background()
	geometry := mprCorpusGeometry{
		origin: Vec3{X: 45, Y: -12, Z: -30},
		right:  Vec3{X: .6, Y: .8},
		down:   Vec3{X: -.64, Y: .48, Z: .6},
		normal: Vec3{X: .48, Y: -.36, Z: .8},
	}
	start, end := Vec3{X: 1, Y: 1.5, Z: 2.5}, Vec3{X: 3, Y: 1.5, Z: 2.5}
	path, err := NewCPRPathChecked([]Vec3{geometry.patient(start), geometry.patient(end)})
	if err != nil {
		t.Fatal(err)
	}
	for _, storage := range []struct {
		name           string
		signed, varied bool
	}{
		{"signed-stored", true, false},
		{"unsigned-stored", false, false},
		{"per-slice-calibration", false, true},
	} {
		t.Run(storage.name, func(t *testing.T) {
			volume, err := BuildVolume(mprCorpusStack(geometry, storage.signed, storage.varied))
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			for _, mode := range []struct {
				name string
				view CPRMode
			}{
				{"straightened", CPRStraightened},
				{"stretched", CPRStretched},
				{"transverse", CPRTransverse},
			} {
				t.Run(mode.name, func(t *testing.T) {
					request := CPRRequest{
						Mode: mode.view, Volume: volume, Path: path, Width: 3, ArcSpacing: mprCorpusColSpacing,
						CrossSpacing: .25, ArcLength: mprCorpusColSpacing, RotationDegrees: 29,
						ThicknessMM: .5, SlabMode: SlabAverage,
						Window: WindowLevel{Center: -100, Width: 1},
					}
					before, err := RenderCPRScalar(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					// The midpoint of each reformation samples an affine field
					// in the interior. Its symmetric average equals the value
					// at the midpoint, regardless of the sampling basis.
					want := mprCorpusValue(Vec3{X: 2, Y: 1.5, Z: 2.5})
					if before.Width != 3 || before.Height != 3 || len(before.Valid) != 9 || len(before.Values) != 9 || !before.Valid[4] ||
						math.IsNaN(before.Values[4]) || math.Abs(before.Values[4]-want) > 1e-9 {
						t.Fatalf("quantitative midpoint of %dx%d = %v (valid %v), want %g", before.Width, before.Height, before.Values, before.Valid, want)
					}
					bright, err := RenderCPR(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					request.Window.Center = 100
					dark, err := RenderCPR(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					if grayAt(bright, 1, 1) != 255 || grayAt(dark, 1, 1) != 0 {
						t.Fatalf("presentation midpoint after windows = %d, %d", grayAt(bright, 1, 1), grayAt(dark, 1, 1))
					}
					after, err := RenderCPRScalar(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					if before.Width != after.Width || before.Height != after.Height ||
						before.ColumnSpacing != after.ColumnSpacing || before.RowSpacing != after.RowSpacing ||
						!slices.Equal(before.Values, after.Values) || !slices.Equal(before.Valid, after.Valid) {
						t.Fatalf("window change affected quantitative output: before %+v, after %+v", before, after)
					}
					if mode.view == CPRTransverse {
						if before.Plane == nil || after.Plane == nil || *before.Plane != *after.Plane {
							t.Fatalf("transverse plane changed with window or was absent: before %v, after %v", before.Plane, after.Plane)
						}
					} else if before.Plane != nil || after.Plane != nil {
						t.Fatal("longitudinal reformation unexpectedly reports a single plane")
					}
				})
			}
		})
	}
}
