package roi

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

// rampImage is a cols×rows image whose value is linear in the physical
// position: 2·X − 4·Y + 100 with X, Y in mm.
func rampImage(cols, rows int, colSpacing, rowSpacing float64) ImageProfileSampler {
	return ImageProfileSampler{
		Columns: cols, Rows: rows, ColumnSpacingMM: colSpacing, RowSpacingMM: rowSpacing,
		ValueAt: func(x, y int) (float64, bool) {
			if x < 0 || y < 0 || x >= cols || y >= rows {
				return 0, false
			}
			return 2*float64(x)*colSpacing - 4*float64(y)*rowSpacing + 100, true
		},
	}
}

func rampImageValue(p render.Vec3) float64 { return 2*p.X - 4*p.Y + 100 }

// rampVolume is a volume with anisotropic spacing (0.8 mm columns, 0.5 mm
// rows, 2.5 mm slices) and HU = 10·col + 20·row + 30·slice − 1024, stored as
// 16-bit values with a −1024 intercept.
func rampVolume(t *testing.T, cols, rows, depth int) *render.Volume {
	t.Helper()
	const rowSpacing, colSpacing, sliceSpacing = 0.5, 0.8, 2.5
	stack := &render.Stack{Modality: "CT", PixelSpacing: []float64{rowSpacing, colSpacing}, SliceThickness: sliceSpacing}
	for z := 0; z < depth; z++ {
		data := make([]byte, cols*rows*2)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				binary.LittleEndian.PutUint16(data[2*(y*cols+x):], uint16(10*x+20*y+30*z))
			}
		}
		stack.Frames = append(stack.Frames, &render.Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(rows), Columns: uint16(cols), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          render.Rescale{Slope: 1, Intercept: -1024},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, float64(z) * sliceSpacing},
			PixelSpacing:     []float64{rowSpacing, colSpacing},
			SliceThickness:   sliceSpacing,
		})
	}
	vol, err := render.BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

// rampVolumeValue is the HU of rampVolume at a patient point.
func rampVolumeValue(p render.Vec3) float64 {
	return 10*p.X/0.8 + 20*p.Y/0.5 + 30*p.Z/2.5 - 1024
}

// checkUniform asserts the end-point contract: first sample exactly at a,
// last exactly at b and the line length, interior samples i·step apart and
// the last interval in (0, step].
func checkUniform(t *testing.T, samples []ProfileSample, a, b render.Vec3, step float64) {
	t.Helper()
	length := b.Sub(a).Length()
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	if samples[0].Point != a || samples[0].DistanceMM != 0 {
		t.Fatalf("first sample %+v, want exactly %+v at 0 mm", samples[0], a)
	}
	last := samples[len(samples)-1]
	if last.Point != b || last.DistanceMM != length {
		t.Fatalf("last sample %+v, want exactly %+v at %v mm", last, b, length)
	}
	if length == 0 {
		if len(samples) != 1 {
			t.Fatalf("zero-length line has %d samples, want 1", len(samples))
		}
		return
	}
	for i := 1; i < len(samples)-1; i++ {
		if samples[i].DistanceMM != float64(i)*step {
			t.Fatalf("sample %d at %v mm, want %v", i, samples[i].DistanceMM, float64(i)*step)
		}
	}
	lastStep := last.DistanceMM - samples[len(samples)-2].DistanceMM
	if lastStep <= 0 || lastStep > step*(1+1e-6) {
		t.Fatalf("last interval %v mm, want (0, %v]", lastStep, step)
	}
	if want := int(math.Ceil(length/step-profileEndpointTolerance)) + 1; len(samples) != max(want, 2) {
		t.Fatalf("%d samples, want %d", len(samples), max(want, 2))
	}
}

func TestLineProfileImageRampIsLinearInDistance(t *testing.T) {
	img := rampImage(40, 30, 0.7, 1.3) // anisotropic pixels
	for _, tc := range []struct {
		name   string
		a, b   render.Vec3
		step   float64
		mode   render.Interpolation
		interp bool // interior of the grid, where cubic reproduces a ramp
	}{
		{"horizontal", img.PointMM(1, 5), img.PointMM(38, 5), 0.5, render.InterpolationLinear, true},
		{"vertical", img.PointMM(4, 1), img.PointMM(4, 28), 1, render.InterpolationLinear, true},
		{"oblique", img.PointMM(0.3, 0.2), img.PointMM(39, 29), 0.37, render.InterpolationLinear, false},
		{"oblique reversed", img.PointMM(37.5, 26.25), img.PointMM(2.2, 1.6), 1.1, render.InterpolationUnknown, false},
		{"cubic interior", img.PointMM(1.4, 1.7), img.PointMM(37.9, 27.3), 0.61, render.InterpolationCubic, true},
		{"step longer than line", img.PointMM(3, 3), img.PointMM(5, 4), 50, render.InterpolationLinear, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples, err := LineProfile(img, tc.a, tc.b, tc.step, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			checkUniform(t, samples, tc.a, tc.b, tc.step)
			fa, fb := rampImageValue(tc.a), rampImageValue(tc.b)
			length := tc.b.Sub(tc.a).Length()
			for i, s := range samples {
				if !s.Valid {
					t.Fatalf("sample %d at %v mm invalid", i, s.DistanceMM)
				}
				want := fa + (fb-fa)*s.DistanceMM/length
				if math.Abs(s.Value-want) > 1e-9 {
					t.Fatalf("sample %d at %v mm = %v, want %v (linear in distance)", i, s.DistanceMM, s.Value, want)
				}
			}
		})
	}
}

func TestLineProfileImageNearestReadsPixels(t *testing.T) {
	img := rampImage(10, 10, 0.5, 0.5)
	samples, err := LineProfile(img, img.PointMM(0, 0), img.PointMM(9, 3), 0.25, render.InterpolationNearest)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		x := math.Floor(s.Point.X/0.5 + 0.5)
		y := math.Floor(s.Point.Y/0.5 + 0.5)
		want, _ := img.ValueAt(int(x), int(y))
		if !s.Valid || s.Value != want {
			t.Fatalf("nearest at %+v = %v (valid %v), want pixel (%v,%v) = %v", s.Point, s.Value, s.Valid, x, y, want)
		}
	}
}

func TestLineProfileVolumeRampAnisotropic(t *testing.T) {
	vol := rampVolume(t, 12, 14, 9)
	at := func(x, y, z float64) render.Vec3 { return vol.VoxelToPatient(render.Vec3{X: x, Y: y, Z: z}) }
	for _, tc := range []struct {
		name string
		a, b render.Vec3
		step float64
		mode render.Interpolation
	}{
		{"along x", at(0, 3, 4), at(11, 3, 4), 0.3, render.InterpolationLinear},
		{"along y", at(5, 0, 2), at(5, 13, 2), 0.45, render.InterpolationLinear},
		{"along z", at(6, 7, 0), at(6, 7, 8), 1, render.InterpolationLinear},
		{"oblique", at(0.2, 0.7, 0.1), at(10.9, 12.3, 7.6), 0.33, render.InterpolationLinear},
		{"oblique cubic interior", at(1.2, 1.7, 1.1), at(9.9, 11.3, 6.6), 0.41, render.InterpolationCubic},
		{"oblique trilinear default", at(10.5, 1, 7.9), at(0.5, 12.5, 0.4), 0.9, render.InterpolationUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples, err := VolumeLineProfile(context.Background(), vol, tc.a, tc.b, tc.step, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			checkUniform(t, samples, tc.a, tc.b, tc.step)
			fa, fb := rampVolumeValue(tc.a), rampVolumeValue(tc.b)
			length := tc.b.Sub(tc.a).Length()
			for i, s := range samples {
				want := fa + (fb-fa)*s.DistanceMM/length
				if !s.Valid || math.Abs(s.Value-want) > 1e-6 {
					t.Fatalf("sample %d at %v mm = %v (valid %v), want %v", i, s.DistanceMM, s.Value, s.Valid, want)
				}
			}
		})
	}
}

func TestLineProfileEndpointsOutsideImage(t *testing.T) {
	img := rampImage(20, 20, 1, 1)
	a, b := render.Vec3{X: -6, Y: 5}, render.Vec3{X: 25, Y: 5}
	samples, err := LineProfile(img, a, b, 1, render.InterpolationLinear)
	if err != nil {
		t.Fatal(err)
	}
	checkUniform(t, samples, a, b, 1)
	for _, s := range samples {
		// Half-pixel border: [-0.5, 19.5] mm is inside.
		inside := s.Point.X >= -0.5 && s.Point.X <= 19.5
		if s.Valid != inside {
			t.Fatalf("sample at x=%v valid %v, want %v", s.Point.X, s.Valid, inside)
		}
		if !s.Valid && s.Value != 0 {
			t.Fatalf("outside sample carries value %v", s.Value)
		}
	}

	vol := rampVolume(t, 6, 6, 4)
	va := vol.VoxelToPatient(render.Vec3{X: -3, Y: 2, Z: 1})
	vb := vol.VoxelToPatient(render.Vec3{X: 9, Y: 2, Z: 1})
	vs, err := VolumeLineProfile(context.Background(), vol, va, vb, 0.4, render.InterpolationCubic)
	if err != nil {
		t.Fatal(err)
	}
	checkUniform(t, vs, va, vb, 0.4)
	if vs[0].Valid || vs[len(vs)-1].Valid {
		t.Fatal("end points outside the volume must be invalid")
	}
	valid := 0
	for _, s := range vs {
		if s.Valid {
			valid++
		}
	}
	if valid == 0 {
		t.Fatal("no sample inside the volume")
	}

	// Off the image plane.
	off, err := LineProfile(img, render.Vec3{X: 1, Y: 1, Z: 1}, render.Vec3{X: 5, Y: 1, Z: 1}, 1, render.InterpolationLinear)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range off {
		if s.Valid {
			t.Fatal("a point off the image plane must be invalid")
		}
	}
}

func TestLineProfileZeroLength(t *testing.T) {
	img := rampImage(8, 8, 0.6, 0.9)
	p := img.PointMM(3, 4)
	samples, err := LineProfile(img, p, p, 0.5, render.InterpolationLinear)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Point != p || samples[0].DistanceMM != 0 || !samples[0].Valid || samples[0].Value != rampImageValue(p) {
		t.Fatalf("zero-length profile %+v", samples)
	}
}

// TestLineProfileITKEndpointVectors carries the end-point cases of ITK's
// itkBresenhamLineGTest.cxx (FinishAtEndIndex, FinishAtEndIndexNegative-
// Direction, SteepDiagonal3D, SinglePixelLine, AllEightOctants2D,
// NegativeCoordinates, VeryLongLine; ITK commits 59e3c0690d, b7248b1ca4,
// 92b3aaf86d, Apache-2.0) over to physical profiles on a 1 mm grid: whatever
// the direction, the profile starts exactly at the first point and ends
// exactly at the last one.
func TestLineProfileITKEndpointVectors(t *testing.T) {
	sampler := constantSampler{}
	v := func(x, y, z float64) render.Vec3 { return render.Vec3{X: x, Y: y, Z: z} }
	cases := [][2]render.Vec3{
		{v(0, 0, 0), v(250, 250, 1)},
		{v(250, 250, 1), v(0, 0, 0)},
		{v(5, 10, 15), v(5, 10, 15)},
		{v(0, 0, 0), v(10, 0, 0)}, {v(0, 0, 0), v(-10, 0, 0)},
		{v(0, 0, 0), v(0, 10, 0)}, {v(0, 0, 0), v(0, -10, 0)},
		{v(0, 0, 0), v(10, 3, 0)}, {v(0, 0, 0), v(3, 10, 0)},
		{v(0, 0, 0), v(-3, 10, 0)}, {v(0, 0, 0), v(-10, 3, 0)},
		{v(0, 0, 0), v(-10, -3, 0)}, {v(0, 0, 0), v(-3, -10, 0)},
		{v(0, 0, 0), v(3, -10, 0)}, {v(0, 0, 0), v(10, -3, 0)},
		{v(-10, -10, 0), v(-1, -1, 0)},
		{v(-5, -3, 0), v(5, 3, 0)},
		{v(-20, -10, -5), v(20, 10, 5)},
		{v(0, 0, 0), v(10000, 7777, 0)},
	}
	for _, step := range []float64{1, 0.3, 0.7} {
		for _, tc := range cases {
			samples, err := LineProfile(sampler, tc[0], tc[1], step, render.InterpolationLinear)
			if err != nil {
				t.Fatal(err)
			}
			checkUniform(t, samples, tc[0], tc[1], step)
		}
	}
	// A length that is a multiple of the step up to rounding has no sliver.
	samples, err := LineProfile(sampler, v(0, 0, 0), v(0.1*3, 0, 0), 0.1, render.InterpolationLinear)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 4 {
		t.Fatalf("0.3 mm at 0.1 mm: %d samples, want 4", len(samples))
	}
}

type constantSampler struct{}

func (constantSampler) SampleMM(render.Vec3, render.Interpolation) (float64, bool) { return 1, true }

func TestLineProfileRejectsInvalidInput(t *testing.T) {
	img := rampImage(4, 4, 1, 1)
	a, b := render.Vec3{}, render.Vec3{X: 3}
	for name, run := range map[string]func() error{
		"nil sampler":   func() error { _, err := LineProfile(nil, a, b, 1, render.InterpolationLinear); return err },
		"zero step":     func() error { _, err := LineProfile(img, a, b, 0, render.InterpolationLinear); return err },
		"negative step": func() error { _, err := LineProfile(img, a, b, -1, render.InterpolationLinear); return err },
		"NaN step":      func() error { _, err := LineProfile(img, a, b, math.NaN(), render.InterpolationLinear); return err },
		"NaN point": func() error {
			_, err := LineProfile(img, render.Vec3{X: math.NaN()}, b, 1, render.InterpolationLinear)
			return err
		},
		"too many samples": func() error {
			_, err := LineProfile(img, a, render.Vec3{X: 2000}, 1e-3, render.InterpolationLinear)
			return err
		},
		"interpolation": func() error { _, err := LineProfile(img, a, b, 1, render.Interpolation(99)); return err },
		"bad image": func() error {
			_, err := LineProfile(ImageProfileSampler{Columns: 4, Rows: 4, ColumnSpacingMM: 1}, a, b, 1, render.InterpolationLinear)
			return err
		},
		"nil reader": func() error {
			_, err := LineProfile(VolumeProfileSampler{}, a, b, 1, render.InterpolationLinear)
			return err
		},
		"nil volume": func() error {
			_, err := VolumeLineProfile(context.Background(), nil, a, b, 1, render.InterpolationLinear)
			return err
		},
	} {
		if run() == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestLineProfileCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LineProfileContext(ctx, constantSampler{}, render.Vec3{}, render.Vec3{X: 10}, 1, render.InterpolationLinear)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLineProfileMissingPixelIsInvalid(t *testing.T) {
	img := rampImage(6, 6, 1, 1)
	inner := img.ValueAt
	img.ValueAt = func(x, y int) (float64, bool) {
		if x == 3 {
			return 0, false
		}
		return inner(x, y)
	}
	samples, err := LineProfile(img, render.Vec3{X: 0, Y: 2}, render.Vec3{X: 5, Y: 2}, 0.5, render.InterpolationLinear)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		needsColumn3 := s.Point.X > 2 && s.Point.X < 4
		if s.Valid == needsColumn3 {
			t.Fatalf("sample at x=%v valid %v", s.Point.X, s.Valid)
		}
	}
}

// The Bresenham cases below are ITK's itkBresenhamLineGTest.cxx
// (Apache-2.0, Copyright NumFOCUS), ported to Go slices.

func bresenham(t *testing.T, p0, p1 []int) [][]int {
	t.Helper()
	points, err := BresenhamLine(p0, p1)
	if err != nil {
		t.Fatal(err)
	}
	return points
}

func chebyshev(p0, p1 []int) int {
	d := 0
	for i := range p0 {
		d = max(d, abs(p1[i]-p0[i]))
	}
	return d
}

func samePoint(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkBresenham(t *testing.T, p0, p1 []int) [][]int {
	t.Helper()
	points := bresenham(t, p0, p1)
	if !samePoint(points[0], p0) || !samePoint(points[len(points)-1], p1) {
		t.Fatalf("%v→%v runs %v→%v", p0, p1, points[0], points[len(points)-1])
	}
	if len(points) != chebyshev(p0, p1)+1 {
		t.Fatalf("%v→%v has %d points, want %d", p0, p1, len(points), chebyshev(p0, p1)+1)
	}
	for i := 1; i < len(points); i++ {
		if chebyshev(points[i-1], points[i]) != 1 {
			t.Fatalf("%v→%v: gap or repeat between %v and %v", p0, p1, points[i-1], points[i])
		}
	}
	return points
}

func TestBresenhamLineITKCases(t *testing.T) {
	// BuildLineFromIndices: the diagonal.
	for i, p := range checkBresenham(t, []int{0, 0}, []int{39, 39}) {
		if !samePoint(p, []int{i, i}) {
			t.Fatalf("diagonal point %d = %v", i, p)
		}
	}
	// FinishAtEndIndex, FinishAtEndIndexNegativeDirection, SteepDiagonal3D,
	// ReverseSymmetry.
	checkBresenham(t, []int{0, 0, 0}, []int{250, 250, 1})
	checkBresenham(t, []int{250, 250, 1}, []int{0, 0, 0})
	fwd := checkBresenham(t, []int{10, 20, 3}, []int{200, 50, 80})
	rev := checkBresenham(t, []int{200, 50, 80}, []int{10, 20, 3})
	if len(fwd) != len(rev) {
		t.Fatal("forward and reverse lengths differ")
	}
	// SinglePixelLine.
	if points := checkBresenham(t, []int{5, 10, 15}, []int{5, 10, 15}); len(points) != 1 {
		t.Fatalf("single pixel line %v", points)
	}
	// AxisAligned2D and AxisAligned3D.
	for _, p := range checkBresenham(t, []int{0, 5}, []int{10, 5}) {
		if p[1] != 5 {
			t.Fatalf("horizontal line leaves row 5: %v", p)
		}
	}
	for _, p := range checkBresenham(t, []int{3, 0}, []int{3, 7}) {
		if p[0] != 3 {
			t.Fatalf("vertical line leaves column 3: %v", p)
		}
	}
	for _, tc := range [][2][]int{
		{{0, 5, 10}, {20, 5, 10}}, {{3, 0, 7}, {3, 15, 7}}, {{1, 2, 0}, {1, 2, 30}},
	} {
		for _, p := range checkBresenham(t, tc[0], tc[1]) {
			for axis := range p {
				if tc[0][axis] == tc[1][axis] && p[axis] != tc[0][axis] {
					t.Fatalf("%v→%v leaves axis %d: %v", tc[0], tc[1], axis, p)
				}
			}
		}
	}
	// Connectivity2D/3D, PixelCountMatchesChebyshev, AllEightOctants2D,
	// NegativeCoordinates.
	for _, tc := range [][2][]int{
		{{0, 0}, {100, 37}}, {{0, 0, 0}, {50, 80, 30}},
		{{0, 0, 0}, {10, 0, 0}}, {{0, 0, 0}, {10, 10, 0}}, {{0, 0, 0}, {10, 10, 10}}, {{0, 0, 0}, {100, 50, 25}},
		{{0, 0}, {10, 0}}, {{0, 0}, {-10, 0}}, {{0, 0}, {0, 10}}, {{0, 0}, {0, -10}},
		{{0, 0}, {10, 3}}, {{0, 0}, {3, 10}}, {{0, 0}, {-3, 10}}, {{0, 0}, {-10, 3}},
		{{0, 0}, {-10, -3}}, {{0, 0}, {-3, -10}}, {{0, 0}, {3, -10}}, {{0, 0}, {10, -3}},
		{{-10, -10}, {-1, -1}}, {{-5, -3}, {5, 3}}, {{-20, -10, -5}, {20, 10, 5}},
	} {
		checkBresenham(t, tc[0], tc[1])
	}
	// NoDuplicatePixels.
	for _, tc := range [][2][]int{
		{{0, 0, 0}, {50, 30, 10}}, {{0, 0, 0}, {10, 10, 10}}, {{0, 0, 0}, {100, 1, 0}}, {{5, 5, 5}, {-5, -5, -5}},
	} {
		points := checkBresenham(t, tc[0], tc[1])
		seen := map[[3]int]bool{}
		for _, p := range points {
			key := [3]int{p[0], p[1], p[2]}
			if seen[key] {
				t.Fatalf("%v→%v repeats %v", tc[0], tc[1], p)
			}
			seen[key] = true
		}
	}
	// SingleStepAdjacentPixels.
	for _, nb := range [][]int{{6, 5}, {6, 6}, {5, 6}, {4, 6}, {4, 5}, {4, 4}, {5, 4}, {6, 4}} {
		if points := checkBresenham(t, []int{5, 5}, nb); len(points) != 2 {
			t.Fatalf("adjacent %v: %v", nb, points)
		}
	}
	for _, nb := range [][]int{{6, 5, 5}, {6, 6, 5}, {6, 6, 6}, {4, 5, 5}, {4, 4, 4}} {
		if points := checkBresenham(t, []int{5, 5, 5}, nb); len(points) != 2 {
			t.Fatalf("adjacent %v: %v", nb, points)
		}
	}
	// SteepVsShallowSlopes: the dominant axis advances every step.
	shallow := checkBresenham(t, []int{0, 0}, []int{20, 3})
	steep := checkBresenham(t, []int{0, 0}, []int{3, 20})
	for i := 1; i < len(shallow); i++ {
		if shallow[i][0]-shallow[i-1][0] != 1 || steep[i][1]-steep[i-1][1] != 1 {
			t.Fatalf("dominant axis skipped at step %d", i)
		}
	}
	// VeryLongLine: monotone along x.
	long := checkBresenham(t, []int{0, 0}, []int{10000, 7777})
	for i := 1; i < len(long); i++ {
		if long[i][0] < long[i-1][0] {
			t.Fatalf("x decreases at %d", i)
		}
	}
	if _, err := BresenhamLine([]int{0, 0}, []int{1, 2, 3}); err == nil {
		t.Fatal("mismatched dimensions accepted")
	}
	if _, err := BresenhamLine(nil, nil); err == nil {
		t.Fatal("empty points accepted")
	}
}
