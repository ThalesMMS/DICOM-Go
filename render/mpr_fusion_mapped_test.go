package render

import (
	"context"
	"errors"
	"fmt"
	stdimage "image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func TestResliceMappedMPRScalarMatchesFusedOverlayLayer(t *testing.T) {
	base, err := BuildVolume(gradientXZStack(3, 4, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	overlay, err := BuildVolume(gradientXZStack(3, 4, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()
	// A tilted plane through the volume: every sample needs trilinear
	// interpolation across slices, so the single-layer path must reuse the
	// fused overlay kernel exactly.
	plane := Plane{Origin: Vec3{Y: 0.25, Z: 0.5}, U: Vec3{X: 3, Z: 1}, V: Vec3{Y: 1.5}}
	mapper := func(point Vec3) (Vec3, error) { return point.Add(Vec3{Z: 0.25}), nil }
	fused, err := ResliceFusedMPRScalarContext(context.Background(), FusedMPRRequest{
		Base: base, Overlay: overlay, TargetPlane: plane, Width: 4, Height: 3, TargetToSource: mapper,
	})
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{
		Source: overlay, TargetPlane: plane, Width: 4, Height: 3, TargetToSource: mapper,
	})
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			want, wantOK := fused.Overlay.ValueAt(x, y)
			got, gotOK := mapped.Plane.ValueAt(x, y)
			if got != want || gotOK != wantOK {
				t.Fatalf("pixel (%d,%d) mapped=%v/%v, fused overlay=%v/%v", x, y, got, gotOK, want, wantOK)
			}
		}
	}
	if mapped.Report.TargetSamples != 12 || mapped.Report.BaseInBounds != 0 || mapped.Report.OverlayInBounds != fused.Report.OverlayInBounds {
		t.Fatalf("mapped report = %+v, fused report = %+v", mapped.Report, fused.Report)
	}
}

func TestResliceMappedMPRScalarDomainMissesAndFailures(t *testing.T) {
	source, err := BuildVolume(gradientXZStack(3, 4, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	plane := source.OrthogonalPlane(MPRPlaneAxial, Vec3{Z: 1})

	if _, err := ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{TargetPlane: plane, Width: 4, Height: 3}); !errors.Is(err, ErrMPRInvalidInput) {
		t.Fatalf("nil source error = %v, want ErrMPRInvalidInput", err)
	}

	outside := func(point Vec3) (Vec3, error) {
		if point.X >= 2 {
			return Vec3{}, fmt.Errorf("grid edge: %w", ErrMapperOutsideDomain)
		}
		return point, nil
	}
	result, err := ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{
		Source: source, TargetPlane: plane, Width: 4, Height: 3, TargetToSource: outside,
	})
	if err != nil {
		t.Fatalf("domain-miss reslice error = %v", err)
	}
	if value, ok := result.Plane.ValueAt(1, 1); !ok || value != 51 {
		t.Fatalf("in-domain sample = %v/%v, want 51", value, ok)
	}
	if value, ok := result.Plane.ValueAt(2, 1); ok {
		t.Fatalf("outside-domain sample = %v, want unset", value)
	}
	if result.Report.OverlayInBounds != 6 {
		t.Fatalf("domain-miss report = %+v, want 6 overlay samples", result.Report)
	}

	sentinel := errors.New("mapper sentinel")
	result, err = ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{
		Source: source, TargetPlane: plane, Width: 4, Height: 3,
		TargetToSource: func(point Vec3) (Vec3, error) {
			if point.X >= 3 {
				return Vec3{}, sentinel
			}
			return point, nil
		},
	})
	if result != nil || !errors.Is(err, ErrFusionMapping) || !errors.Is(err, sentinel) {
		t.Fatalf("mapper failure result/error = %v/%v, want atomic mapping failure", result, err)
	}

	result, err = ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{
		Source: source, TargetPlane: plane, Width: 4, Height: 3,
		TargetToSource: func(point Vec3) (Vec3, error) { return point.Add(Vec3{Z: 100}), nil },
	})
	if !errors.Is(err, ErrFusionNoOverlap) || result == nil || result.Plane == nil || result.Report.OverlayInBounds != 0 {
		t.Fatalf("no-overlap result/error = %+v/%v, want explicit no-overlap plane", result, err)
	}
}

func TestScalarPlaneCompositeOverKeepsUnsampledBasePixels(t *testing.T) {
	source, err := BuildVolume(gradientXZStack(2, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	result, err := ResliceMappedMPRScalarContext(context.Background(), MappedMPRRequest{
		Source: source, TargetPlane: source.OrthogonalPlane(MPRPlaneAxial, Vec3{Z: 1}), Width: 2, Height: 2,
		TargetToSource: func(point Vec3) (Vec3, error) {
			if point.Y >= 1 {
				return Vec3{}, ErrMapperOutsideDomain
			}
			return point, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2))
	for index := 0; index < len(base.Pix); index += 4 {
		base.Pix[index+0], base.Pix[index+1], base.Pix[index+2], base.Pix[index+3] = 40, 60, 80, 255
	}
	layer := FusionLayerPresentation{Window: WindowLevel{Center: 50, Width: 100}, Palette: display.HotIronLUT(), Alpha: 0.5}
	out, err := result.Plane.CompositeOver(context.Background(), base, layer)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := result.Plane.ValueAt(1, 0)
	gray := displayGrayMapped(value, prepareWindow(layer.Window), result.Plane.photometric)
	r, g, b := layer.Palette.At(int(gray))
	want := []uint8{
		uint8(math.Round((40 + float64(r)) / 2)),
		uint8(math.Round((60 + float64(g)) / 2)),
		uint8(math.Round((80 + float64(b)) / 2)),
		255,
	}
	got := out.Pix[4:8]
	for channel := range want {
		// Half-way blends of odd sums sit exactly on a rounding boundary.
		if math.Abs(float64(got[channel])-float64(want[channel])) > 1 {
			t.Fatalf("blended pixel = %v, want %v", got, want)
		}
	}
	if got := out.Pix[8:12]; got[0] != 40 || got[1] != 60 || got[2] != 80 || got[3] != 255 {
		t.Fatalf("unsampled pixel = %v, want the untouched base", got)
	}
	gray8 := stdimage.NewGray(stdimage.Rect(0, 0, 3, 2))
	if _, err := result.Plane.CompositeOver(context.Background(), gray8, layer); !errors.Is(err, ErrMPRInvalidInput) {
		t.Fatalf("dimension mismatch error = %v, want ErrMPRInvalidInput", err)
	}
}
