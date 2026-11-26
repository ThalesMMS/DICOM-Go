package render

import (
	"context"
	"errors"
	"math"
	"testing"
)

func mprAntialiasFixture(t testing.TB, n int, value func(int, int, int) int) *Volume {
	t.Helper()
	v, err := BuildVolume(stack16(n, true, func(int) Rescale { return Rescale{Slope: 1} }, value))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// Alternating subpixel struts have an analytically known block mean. The
// noninteger output step makes point sampling produce a low-frequency beat.
func TestMPRAntialiasStrutPhantom(t *testing.T) {
	v := mprAntialiasFixture(t, 64, func(x, y, z int) int { return 1000 + 800*(2*(x%2)-1) })
	plane := v.OrthogonalPlane(MPRPlaneAxial, v.VoxelToPatient(Vec3{Z: 31}))
	point, err := ResliceObliqueScalarContext(context.Background(), v, plane, 22, 22)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 22, 22, MPRResliceOptions{Antialias2x: true})
	if err != nil {
		t.Fatal(err)
	}
	if !filtered.Antialiased() || point.Antialiased() {
		t.Fatal("sampling provenance is wrong")
	}
	var pointEnergy, filteredEnergy float64
	for y := 0; y < 22; y++ {
		for x := 0; x < 22; x++ {
			p, validPoint := point.ValueAt(x, y)
			f, validFiltered := filtered.ValueAt(x, y)
			if !validPoint || !validFiltered {
				t.Fatalf("lost coverage at %d,%d", x, y)
			}
			pointEnergy += (p - 1000) * (p - 1000)
			filteredEnergy += (f - 1000) * (f - 1000)
		}
	}
	if pointEnergy/484 < 100000 || filteredEnergy > 1e-6 {
		t.Fatalf("beat energy point=%g filtered=%g", pointEnergy/484, filteredEnergy/484)
	}
	level := v.interactive
	_, err = ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 22, 22, MPRResliceOptions{Antialias2x: true})
	if err != nil || level != v.interactive {
		t.Fatalf("warm request rebuilt level: %v", err)
	}
	if err := v.Close(); err != nil || !level.closed.Load() {
		t.Fatalf("cached level not retired: %v", err)
	}
}

func TestMPRAntialiasPhysicalObliqueRamp(t *testing.T) {
	stack := stack16(32, true, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int { return 7*x + 11*y + 3*z })
	angle := math.Pi / 5
	stack.PixelSpacing = []float64{.7, .5}
	for i, frame := range stack.Frames {
		frame.ImageOrientation = []float64{math.Cos(angle), math.Sin(angle), 0, -math.Sin(angle), math.Cos(angle), 0}
		frame.ImagePosition[2] = 1.5 * float64(i)
	}
	v, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	plane := Plane{Origin: v.VoxelToPatient(Vec3{X: 3, Y: 3, Z: 3}), U: v.AxisX.Scale(12).Add(v.Normal.Scale(3)), V: v.AxisY.Scale(16.8)}
	p, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 4, 4, MPRResliceOptions{Antialias2x: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Antialiased() {
		t.Fatal("coarse oblique grid did not use level")
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			q := v.PatientToVoxel(plane.At(float64(x)/3, float64(y)/3))
			want := 7*q.X + 11*q.Y + 3*q.Z
			got, ok := p.ValueAt(x, y)
			if !ok || math.Abs(got-want) > 1e-4 {
				t.Fatalf("patient-space ramp (%d,%d)=%g want %g valid=%v", x, y, got, want, ok)
			}
		}
	}
}

func TestMPRAntialiasAdmissionAndUnchangedSamples(t *testing.T) {
	for _, n := range []int{32, 33} {
		v := mprAntialiasFixture(t, n, func(x, y, z int) int { return x*x + y + z })
		plane := v.OrthogonalPlane(MPRPlaneAxial, v.VoxelToPatient(Vec3{Z: float64(n / 2)}))
		for _, width := range []int{n, 12} {
			p, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, width, width, MPRResliceOptions{Antialias2x: true})
			if err != nil {
				t.Fatal(err)
			}
			if width == n || n%2 != 0 {
				if p.Antialiased() {
					t.Fatal("native or odd grid averaged")
				}
				exact, err := ResliceObliqueScalarContext(context.Background(), v, plane, width, width)
				if err != nil {
					t.Fatal(err)
				}
				for i := range p.values {
					if p.values[i] != exact.values[i] || p.valid[i] != exact.valid[i] {
						t.Fatal("point-sampling contract changed")
					}
				}
			}
		}
	}
	v := mprAntialiasFixture(t, 32, func(x, y, z int) int { return x + y + z })
	plane := v.OrthogonalPlane(MPRPlaneAxial, v.VoxelToPatient(Vec3{Z: 16}))
	for _, tc := range []struct {
		options MPRResliceOptions
		want    error
	}{
		{MPRResliceOptions{Antialias2x: true, Limits: MPRLimits{MaxWorkingBytes: 4096}}, ErrMPRResourceLimit},
		{MPRResliceOptions{Antialias2x: true, Interpolation: InterpolationNearest}, ErrMPRInvalidInput},
	} {
		_, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 12, 12, tc.options)
		if !errors.Is(err, tc.want) {
			t.Fatalf("admission=%v want %v", err, tc.want)
		}
	}
	if v.interactive != nil {
		t.Fatal("rejected request allocated a level")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, err := ResliceObliqueScalarWithOptionsContext(ctx, v, plane, 12, 12, MPRResliceOptions{Antialias2x: true}); p != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v/%v", p, err)
	}
	if _, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), v, plane, 12, 12, 3, SlabMIP, MPRResliceOptions{Antialias2x: true}); !errors.Is(err, ErrMPRInvalidInput) {
		t.Fatalf("slab silently ignored option: %v", err)
	}
	_ = v.Close()
	if _, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 12, 12, MPRResliceOptions{Antialias2x: true}); !errors.Is(err, ErrVolumeStoreClosed) {
		t.Fatalf("closed source=%v", err)
	}
}

func BenchmarkMPRAntialiasWarm(b *testing.B) {
	v := mprAntialiasFixture(b, 128, func(x, y, z int) int { return 1000 + 800*(2*(x%2)-1) })
	plane := v.OrthogonalPlane(MPRPlaneCoronal, v.VoxelToPatient(Vec3{Y: 64}))
	if _, err := v.InteractiveVolumeContext(context.Background()); err != nil {
		b.Fatal(err)
	}
	for _, aa := range []bool{false, true} {
		name := "point"
		if aa {
			name = "binshrink2x"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ResliceObliqueScalarWithOptionsContext(context.Background(), v, plane, 48, 48, MPRResliceOptions{Antialias2x: aa}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
