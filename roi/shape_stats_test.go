package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func BenchmarkShapeStatisticsSphere(b *testing.B) {
	// A 48 mm sphere on 0.8 × 0.8 × 1.2 mm voxels (about 380k voxels).
	m := NewVolumeMask(128, 128, 88, [3]float64{0.8, 0.8, 1.2})
	c := render.Vec3{X: 51.2, Y: 51.2, Z: 52.8}
	fillVolume(m, func(p render.Vec3) bool { return p.Sub(c).Length() <= 48 })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.ShapeStatistics(ShapeOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// fillVolume sets every voxel whose centre (mm) satisfies inside.
func fillVolume(m *VolumeMask, inside func(p render.Vec3) bool) {
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				m.Voxels[m.index(x, y, z)] = inside(render.Vec3{X: m.X[x], Y: m.Y[y], Z: m.Z[z]})
			}
		}
	}
}

func relNear(t *testing.T, name string, want, got, rel float64) {
	t.Helper()
	if math.Abs(got-want) > rel*math.Abs(want) || math.IsNaN(got) {
		t.Errorf("%s = %.6g, want %.6g ± %.1f%%", name, got, want, rel*100)
	}
}

func alignedWith(a, b render.Vec3, tol float64) bool {
	return math.Abs(math.Abs(a.Normalize().Dot(b.Normalize()))-1) < tol
}

// rotation returns an orthonormal right-handed basis from Euler angles.
func rotation(a, b, c float64) [3]render.Vec3 {
	x := render.Vec3{X: 1}.Rotate(render.Vec3{Z: 1}, a).Rotate(render.Vec3{X: 1}, b).Rotate(render.Vec3{Z: 1}, c)
	y := render.Vec3{Y: 1}.Rotate(render.Vec3{Z: 1}, a).Rotate(render.Vec3{X: 1}, b).Rotate(render.Vec3{Z: 1}, c)
	return [3]render.Vec3{x, y, x.Cross(y)}
}

func TestShapeStatisticsSphere(t *testing.T) {
	const r = 12.0
	m := NewVolumeMask(31, 31, 31, [3]float64{1, 1, 1})
	c := render.Vec3{X: 15, Y: 15, Z: 15}
	fillVolume(m, func(p render.Vec3) bool { return p.Sub(c).Length() <= r })
	s := shapeOf(t, m, ShapeOptions{})

	relNear(t, "volume", 4.0/3*math.Pi*r*r*r, s.PhysicalSize, 0.02) // lattice-point count
	expectVecNear(t, "centroid", vec(c), vec(s.Centroid), 1e-9)
	for _, pm := range s.PrincipalMoments {
		relNear(t, "moment", r*r/5, pm, 0.02)
	}
	expectNear(t, "elongation", 1, s.Elongation, 1e-9)
	expectNear(t, "flatness", 1, s.Flatness, 1e-9)
	expectNear(t, "Feret", 2*r, s.FeretDiameter, 1e-12)
	expectNear(t, "Feret endpoints", 2*r, s.FeretEndpoints[0].Sub(s.FeretEndpoints[1]).Length(), 1e-12)
	relNear(t, "surface (Crofton)", 4*math.Pi*r*r, s.Perimeter, 0.03)
	expectNear(t, "sphericity", 1, s.Sphericity, 0.03)
	for _, d := range s.EquivalentEllipsoidDiameter {
		relNear(t, "ellipsoid diameter", 2*r, d, 0.01)
	}
	if s.TouchesBorder() {
		t.Error("sphere touches the border")
	}
}

func TestShapeStatisticsEllipsoidAnisotropic(t *testing.T) {
	// Semi-axes 6, 10 and 15 mm along x, y, z on a 0.5 × 0.8 × 1.5 mm grid.
	a, b, c := 6.0, 10.0, 15.0
	m := NewVolumeMask(29, 29, 25, [3]float64{0.5, 0.8, 1.5})
	centre := render.Vec3{X: 7, Y: 11.2, Z: 18} // voxel (14, 14, 12)
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				// Offsets from the centre index keep the mask exactly symmetric.
				dx, dy, dz := float64(x-14)*0.5, float64(y-14)*0.8, float64(z-12)*1.5
				m.Voxels[m.index(x, y, z)] = dx*dx/(a*a)+dy*dy/(b*b)+dz*dz/(c*c) <= 1
			}
		}
	}
	s := shapeOf(t, m, ShapeOptions{})
	relNear(t, "volume", 4.0/3*math.Pi*a*b*c, s.PhysicalSize, 0.01)
	expectVecNear(t, "centroid", vec(centre), vec(s.Centroid), 1e-9)
	want := []float64{a * a / 5, b * b / 5, c * c / 5}
	for i := range want {
		relNear(t, "moment", want[i], s.PrincipalMoments[i], 0.03)
	}
	// Symmetric about the axes: the axes are exact and follow the sign
	// convention (positive, right-handed).
	expectVecNear(t, "axis 0", []float64{1, 0, 0}, vec(s.PrincipalAxes[0]), 1e-9)
	expectVecNear(t, "axis 1", []float64{0, 1, 0}, vec(s.PrincipalAxes[1]), 1e-9)
	expectVecNear(t, "axis 2", []float64{0, 0, 1}, vec(s.PrincipalAxes[2]), 1e-9)
	relNear(t, "elongation", c/b, s.Elongation, 0.02)
	relNear(t, "flatness", b/a, s.Flatness, 0.02)
	expectNear(t, "Feret", 2*c, s.FeretDiameter, 1e-9) // z centres at ±15 mm
	for i, axis := range []float64{a, b, c} {
		relNear(t, "ellipsoid diameter", 2*axis, s.EquivalentEllipsoidDiameter[i], 0.02)
	}
	// Knud Thomsen's approximation of the ellipsoid surface area (±1.1%).
	const p = 1.6075
	area := 4 * math.Pi * math.Pow((math.Pow(a*b, p)+math.Pow(a*c, p)+math.Pow(b*c, p))/3, 1/p)
	// Crofton on this anisotropic grid reads about 17% low (see
	// shape_crofton.go); on a 0.5 mm isotropic grid it is within 2%.
	relNear(t, "surface (Crofton, anisotropic)", area, s.Perimeter, 0.2)
	iso := NewVolumeMask(29, 45, 65, [3]float64{0.5, 0.5, 0.5})
	for z := 0; z < iso.Slices; z++ {
		for y := 0; y < iso.Rows; y++ {
			for x := 0; x < iso.Columns; x++ {
				dx, dy, dz := float64(x-14)*0.5, float64(y-22)*0.5, float64(z-32)*0.5
				iso.Voxels[iso.index(x, y, z)] = dx*dx/(a*a)+dy*dy/(b*b)+dz*dz/(c*c) <= 1
			}
		}
	}
	isoStats := shapeOf(t, iso, ShapeOptions{})
	relNear(t, "surface (Crofton, isotropic)", area, isoStats.Perimeter, 0.02)
	// Wadell sphericity (36πV²)^⅓/A of the ellipsoid.
	relNear(t, "sphericity (isotropic)", math.Cbrt(36*math.Pi*math.Pow(4.0/3*math.Pi*a*b*c, 2))/area, isoStats.Sphericity, 0.03)
	// The OBB of an axis-aligned ellipsoid is its voxelized extent.
	expectNear(t, "OBB z", 2*c+1.5, s.OrientedBoundingBox.Size[2], 1e-9)
}

func TestShapeStatisticsRotatedEllipsoidAxes(t *testing.T) {
	a, b, c := 5.0, 8.0, 13.0
	rot := rotation(0.4, 0.9, -0.3)
	m := NewVolumeMask(36, 36, 36, [3]float64{1, 1, 1})
	centre := render.Vec3{X: 17.5, Y: 17.5, Z: 17.5}
	fillVolume(m, func(p render.Vec3) bool {
		d := p.Sub(centre)
		u, v, w := d.Dot(rot[0]), d.Dot(rot[1]), d.Dot(rot[2])
		return u*u/(a*a)+v*v/(b*b)+w*w/(c*c) <= 1
	})
	check := func(t *testing.T, s ShapeStatistics, frame [3]render.Vec3) {
		t.Helper()
		for i, axis := range []float64{a, b, c} {
			relNear(t, "moment", axis*axis/5, s.PrincipalMoments[i], 0.03)
			want := render.Vec3{}
			for k := 0; k < 3; k++ { // rot[i] expressed through the frame
				want = want.Add(frame[k].Scale([3]float64{rot[i].X, rot[i].Y, rot[i].Z}[k]))
			}
			if !alignedWith(s.PrincipalAxes[i], want, 1e-3) {
				t.Errorf("axis %d = %v, want ±%v", i, s.PrincipalAxes[i], want)
			}
		}
		axes := s.PrincipalAxes
		if det := axes[0].Cross(axes[1]).Dot(axes[2]); math.Abs(det-1) > 1e-9 {
			t.Errorf("axes are not a right-handed orthonormal basis: det %g", det)
		}
		relNear(t, "OBB volume", 8*a*b*c, s.OrientedBoundingBox.Size[0]*s.OrientedBoundingBox.Size[1]*s.OrientedBoundingBox.Size[2], 0.25)
	}
	t.Run("grid", func(t *testing.T) {
		check(t, shapeOf(t, m, ShapeOptions{}), [3]render.Vec3{{X: 1}, {Y: 1}, {Z: 1}})
	})
	t.Run("patient direction", func(t *testing.T) {
		frame := rotation(-1.1, 0.2, 2.5)
		s := shapeOf(t, m, ShapeOptions{Direction: frame, Origin: render.Vec3{X: -100, Y: 50, Z: 20}})
		check(t, s, frame)
		want := render.Vec3{X: -100, Y: 50, Z: 20}.Add(frame[0].Scale(17.5)).Add(frame[1].Scale(17.5)).Add(frame[2].Scale(17.5))
		expectVecNear(t, "centroid", vec(want), vec(s.Centroid), 1e-6)
	})
}

func TestShapeStatisticsBox(t *testing.T) {
	// 36 × 16 × 6 voxels of 0.7 × 1.3 × 2.5 mm.
	sp := [3]float64{0.7, 1.3, 2.5}
	n := [3]int{36, 16, 6}
	m := NewVolumeMask(40, 22, 10, sp)
	for z := 2; z < 2+n[2]; z++ {
		for y := 3; y < 3+n[1]; y++ {
			for x := 1; x < 1+n[0]; x++ {
				m.Voxels[m.index(x, y, z)] = true
			}
		}
	}
	s := shapeOf(t, m, ShapeOptions{})
	ext := [3]float64{float64(n[0]) * sp[0], float64(n[1]) * sp[1], float64(n[2]) * sp[2]}
	expectNear(t, "volume", ext[0]*ext[1]*ext[2], s.PhysicalSize, 1e-9)
	diag := math.Sqrt(math.Pow(float64(n[0]-1)*sp[0], 2) + math.Pow(float64(n[1]-1)*sp[1], 2) + math.Pow(float64(n[2]-1)*sp[2], 2))
	expectNear(t, "Feret (centre diagonal)", diag, s.FeretDiameter, 1e-9)
	// Discrete uniform variances (n²-1)/12·s², ascending: z 4.86, y 36.47, x 52.88.
	moment := func(k int) float64 { return float64(n[k]*n[k]-1) / 12 * sp[k] * sp[k] }
	expectVecNear(t, "moments", []float64{moment(2), moment(1), moment(0)}, s.PrincipalMoments, 1e-9)
	expectVecNear(t, "OBB size", []float64{ext[2], ext[1], ext[0]}, s.OrientedBoundingBox.Size, 1e-9)
	lo := render.Vec3{X: 0.5 * sp[0], Y: 2.5 * sp[1], Z: 1.5 * sp[2]}
	for _, corner := range []render.Vec3{lo, lo.Add(render.Vec3{X: ext[0], Y: ext[1], Z: ext[2]})} {
		if !hasVertex(s.OrientedBoundingBox, corner, 1e-9) {
			t.Errorf("OBB vertices %v miss box corner %v", s.OrientedBoundingBox.Vertices(), corner)
		}
	}
	area := 2 * (ext[0]*ext[1] + ext[1]*ext[2] + ext[0]*ext[2])
	relNear(t, "surface (Crofton)", area, s.Perimeter, 0.2)
}

func TestCroftonCubeQuadratureBias(t *testing.T) {
	// For an axis-aligned cube the 13-direction quadrature gives
	// 4·Σ w_d·(|d_x|+|d_y|+|d_z|)/|d|·L² instead of 6L²: a known 7.3%
	// under-estimate that the intercept counts reproduce.
	const n = 20
	m := NewVolumeMask(n+4, n+4, n+4, [3]float64{1, 1, 1})
	for z := 2; z < n+2; z++ {
		for y := 2; y < n+2; y++ {
			for x := 2; x < n+2; x++ {
				m.Voxels[m.index(x, y, z)] = true
			}
		}
	}
	s := shapeOf(t, m, ShapeOptions{})
	w := croftonWeights3D
	limit := 4 * (3*w[0] + 6*w[3]*math.Sqrt2 + 4*w[6]*math.Sqrt(3)) / 6
	expectNear(t, "quadrature limit", 0.9266, limit, 1e-4)
	// Digitization adds about 1/n more for n = 20 (0.9002 of 6n²).
	if ratio := s.Perimeter / (6 * n * n); ratio > limit || ratio < limit-0.04 {
		t.Errorf("cube surface ratio %.4f, want just below %.4f", ratio, limit)
	}
	sum := 3*w[0] + 2*3*w[3] + 4*w[6]
	expectNear(t, "weight sum", 1, sum, 1e-12)
}

func TestCroftonAnisotropicSphereAndDisk(t *testing.T) {
	const r = 10.0
	m := NewVolumeMask(44, 44, 10, [3]float64{0.5, 0.5, 2.5})
	c := render.Vec3{X: 11, Y: 11, Z: 12.5}
	fillVolume(m, func(p render.Vec3) bool { return p.Sub(c).Length() <= r })
	s := shapeOf(t, m, ShapeOptions{})
	relNear(t, "sphere surface (0.5×0.5×2.5 mm)", 4*math.Pi*r*r, s.Perimeter, 0.03)
	relNear(t, "sphere volume", 4.0/3*math.Pi*r*r*r, s.PhysicalSize, 0.03)

	disk := NewRasterMask(100, 20)
	for y := 0; y < 20; y++ {
		for x := 0; x < 100; x++ {
			if math.Hypot(float64(x)*0.4-20, float64(y)*2-19) <= 15 {
				disk.Set(x, y, true)
			}
		}
	}
	d, err := disk.ShapeStatistics(0.4, 2, ShapeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	relNear(t, "disk perimeter (0.4×2 mm)", 2*math.Pi*15, d.Perimeter, 0.03)
}

func TestShapeStatisticsRotatedBoxOBB(t *testing.T) {
	// A 20 × 8 × 4 mm slab rotated in the grid: the OBB recovers its size
	// to within the voxel staircase.
	rot := rotation(0.5, 0.3, 0.2)
	m := NewVolumeMask(40, 40, 40, [3]float64{0.5, 0.5, 0.5})
	centre := render.Vec3{X: 10, Y: 10, Z: 10}
	half := [3]float64{10, 4, 2}
	fillVolume(m, func(p render.Vec3) bool {
		d := p.Sub(centre)
		return math.Abs(d.Dot(rot[0])) <= half[0] && math.Abs(d.Dot(rot[1])) <= half[1] && math.Abs(d.Dot(rot[2])) <= half[2]
	})
	s := shapeOf(t, m, ShapeOptions{})
	for i, want := range []float64{2 * half[2], 2 * half[1], 2 * half[0]} {
		if got := s.OrientedBoundingBox.Size[i]; math.Abs(got-want) > 1.0 {
			t.Errorf("OBB size[%d] = %g, want %g ± 1 mm", i, got, want)
		}
	}
	if !alignedWith(s.PrincipalAxes[2], rot[0], 1e-3) {
		t.Errorf("major axis %v, want ±%v", s.PrincipalAxes[2], rot[0])
	}
	relNear(t, "Feret", 2*math.Sqrt(half[0]*half[0]+half[1]*half[1]+half[2]*half[2]), s.FeretDiameter, 0.05)
}

func TestShapeStatisticsIrregularSlicesAndBorder(t *testing.T) {
	m := &VolumeMask{Columns: 3, Rows: 2, Slices: 4,
		X: []float64{0, 1, 2}, Y: []float64{0, 2}, Z: []float64{0, 1, 3, 7}}
	m.Voxels = make([]bool, 24)
	for i := range m.Voxels {
		m.Voxels[i] = true
	}
	s := shapeOf(t, m, ShapeOptions{})
	// Cells: x [-0.5, 2.5], y [-1, 3], z [-0.5, 9].
	expectNear(t, "volume", 3*4*9.5, s.PhysicalSize, 1e-9)
	if s.VoxelsOnBorder != 24 || !s.TouchesBorder() {
		t.Errorf("VoxelsOnBorder = %d", s.VoxelsOnBorder)
	}
	expectNear(t, "border area", 2*(3*4+4*9.5+3*9.5), s.PerimeterOnBorder, 1e-9)
	// Volume-weighted centroid along z: Σ w·z / Σ w with widths 1, 1.5, 3, 4.
	expectNear(t, "centroid z", (0*1+1*1.5+3*3+7*4)/9.5, s.Centroid.Z, 1e-9)
	expectNear(t, "Feret", math.Sqrt(4+4+49), s.FeretDiameter, 1e-12)
	expectVecNear(t, "OBB size sorted", []float64{3, 4, 9.5}, s.OrientedBoundingBox.Size, 1e-9)
}

func TestShapeStatisticsDisk2D(t *testing.T) {
	const r = 30.0
	m := NewRasterMask(80, 80)
	for y := 0; y < 80; y++ {
		for x := 0; x < 80; x++ {
			if math.Hypot(float64(x)*0.5-20, float64(y)*0.5-20) <= r/2 {
				m.Set(x, y, true)
			}
		}
	}
	// 0.5 mm pixels: a disk of radius 15 mm.
	s, err := m.ShapeStatistics(0.5, 0.5, ShapeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rr := r / 2
	relNear(t, "area", math.Pi*rr*rr, s.PhysicalSize, 0.01)
	relNear(t, "perimeter (Crofton)", 2*math.Pi*rr, s.Perimeter, 0.03)
	expectNear(t, "Feret", 2*rr, s.FeretDiameter, 1e-12)
	expectNear(t, "roundness", 1, s.Sphericity, 0.03)
	expectVecNear(t, "centroid", []float64{20, 20, 0}, vec(s.Centroid), 1e-9)
	if len(s.PrincipalMoments) != 2 || len(s.OrientedBoundingBox.Vertices()) != 4 {
		t.Fatalf("2D moments %v", s.PrincipalMoments)
	}
	for _, pm := range s.PrincipalMoments {
		relNear(t, "moment", rr*rr/4, pm, 0.02)
	}
}

func TestShapeStatisticsEmptyAndInvalid(t *testing.T) {
	m := NewVolumeMask(4, 4, 4, [3]float64{1, 1, 1})
	s := shapeOf(t, m, ShapeOptions{})
	if s.VoxelCount != 0 || s.PhysicalSize != 0 || s.Dimension != 3 || s.PrincipalAxes != nil {
		t.Fatalf("empty stats = %+v", s)
	}
	if _, err := m.ShapeStatistics(ShapeOptions{Planar: true}); !errors.Is(err, ErrInvalidShapeInput) {
		t.Fatalf("planar on 4 slices: %v", err)
	}
	if _, err := m.ShapeStatistics(ShapeOptions{Direction: [3]render.Vec3{{X: 1}, {X: 1}, {Z: 1}}}); !errors.Is(err, ErrInvalidShapeInput) {
		t.Fatalf("non-orthogonal direction: %v", err)
	}
	if _, err := NewRasterMask(3, 3).ShapeStatistics(0, 1, ShapeOptions{}); !errors.Is(err, ErrInvalidShapeInput) {
		t.Fatalf("zero spacing: %v", err)
	}
	stats, err := LabelShapeStatistics(context.Background(), &LabelVolume{Grid: shapeAxisGrid(2, 2, 1, [3]float64{1, 1, 1}),
		Labels: make([]uint16, 4)}, ShapeOptions{})
	if err != nil || len(stats) != 0 {
		t.Fatalf("empty label map = %v, %v", stats, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LabelShapeStatistics(ctx, &LabelVolume{Grid: shapeAxisGrid(1, 1, 1, [3]float64{1, 1, 1}),
		Labels: []uint16{1}}, ShapeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// bruteFeret is the all-pairs maximum centre distance.
func bruteFeret(m *VolumeMask) float64 {
	var pts [][3]float64
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				if m.Voxels[m.index(x, y, z)] {
					pts = append(pts, [3]float64{m.X[x], m.Y[y], m.Z[z]})
				}
			}
		}
	}
	best := 0.0
	for i := range pts {
		for j := i + 1; j < len(pts); j++ {
			best = math.Max(best, dist2(pts[i], pts[j]))
		}
	}
	return math.Sqrt(best)
}

func TestFeretDiameterMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1097))
	for trial := 0; trial < 60; trial++ {
		m := NewVolumeMask(9+rng.Intn(6), 9+rng.Intn(6), 3+rng.Intn(8), [3]float64{0.5 + rng.Float64(), 0.5 + rng.Float64(), 0.5 + 3*rng.Float64()})
		density := 0.05 + 0.6*rng.Float64()
		for i := range m.Voxels {
			m.Voxels[i] = rng.Float64() < density
		}
		m.Voxels[rng.Intn(len(m.Voxels))] = true
		s := shapeOf(t, m, ShapeOptions{})
		if want := bruteFeret(m); math.Abs(s.FeretDiameter-want) > 1e-12 {
			t.Fatalf("trial %d: Feret %g, brute force %g", trial, s.FeretDiameter, want)
		}
		if d := s.FeretEndpoints[0].Sub(s.FeretEndpoints[1]).Length(); math.Abs(d-s.FeretDiameter) > 1e-12 {
			t.Fatalf("endpoints %v are %g apart", s.FeretEndpoints, d)
		}
	}
}

func TestSegmentationAndLabelVolumeMatchVolumeMask(t *testing.T) {
	// The same two blobs as a VolumeMask, a Segmentation3D with rotated
	// geometry and irregular slices, and a two-label LabelVolume.
	rot := rotation(0.3, -0.7, 1.2)
	positions := []float64{-10, -8, -5, -4, 0, 3, 5}
	origin := render.Vec3{X: 40, Y: -30, Z: 12}
	colSp, rowSp := 0.8, 0.6
	nx, ny, nz := 20, 16, len(positions)
	blob := func(x, y, z int) uint16 {
		switch {
		case math.Hypot(float64(x-6), float64(y-7)) <= 4 && z >= 1 && z <= 5:
			return 1
		case x >= 12 && x <= 17 && y >= 2 && y <= 12 && z >= 2:
			return 2
		}
		return 0
	}
	zs := make([]float64, nz)
	for i, p := range positions {
		zs[i] = p - positions[0]
	}
	opts := ShapeOptions{Origin: origin, Direction: rot}
	lv := &LabelVolume{Grid: Grid{Columns: nx, Rows: ny, Slices: nz, Origin: origin, RowDir: rot[0], ColDir: rot[1], SliceDir: rot[2],
		ColSpacing: colSp, RowSpacing: rowSp, SliceOffsets: zs}, Labels: make([]uint16, nx*ny*nz)}
	values := make([]float64, len(lv.Labels))
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := (z*ny+y)*nx + x
				lv.Labels[i] = blob(x, y, z)
				values[i] = float64(x*7+y*3) - float64(z)
			}
		}
	}
	labelShapes, err := LabelShapeStatistics(context.Background(), lv, opts)
	if err != nil || len(labelShapes) != 2 {
		t.Fatalf("label shapes %v, %v", labelShapes, err)
	}
	labelValues, err := LabelIntensityStatistics(context.Background(), lv, values)
	if err != nil || len(labelValues) != 2 {
		t.Fatalf("label values %v, %v", labelValues, err)
	}

	slices := make([]render.SliceGeometry, nz)
	for i, p := range positions {
		slices[i] = render.SliceGeometry{Origin: origin.Add(rot[2].Scale(p - positions[0])), RowDir: rot[0], ColDir: rot[1], Normal: rot[2],
			ColSpacing: colSp, RowSpacing: rowSp, Columns: nx, Rows: ny}
	}
	geom := render.VolumeGeometry{Slices: slices, RowDir: rot[0], ColDir: rot[1], Normal: rot[2], Positions: positions}

	for li, label := range []uint16{1, 2} {
		m := &VolumeMask{Columns: nx, Rows: ny, Slices: nz, X: uniformAxis(nx, colSp), Y: uniformAxis(ny, rowSp), Z: zs, Voxels: make([]bool, len(lv.Labels))}
		seg := NewSegmentation3D(geom, nx, ny)
		for i, l := range lv.Labels {
			if l == label {
				m.Voxels[i] = true
				x, y, z := i%nx, (i/nx)%ny, i/(nx*ny)
				seg.SetVoxel(x, y, z, true)
			}
		}
		want := shapeOf(t, m, opts)
		segStats, err := seg.ShapeStatistics()
		if err != nil {
			t.Fatal(err)
		}
		got := labelShapes[li]
		if got.Label != label {
			t.Fatalf("label order: %d", got.Label)
		}
		for name, s := range map[string]ShapeStatistics{"label": got, "segmentation": segStats} {
			if s.VoxelCount != want.VoxelCount || s.BoundingBoxMin != want.BoundingBoxMin {
				t.Fatalf("%s %d: count %d/%d", name, label, s.VoxelCount, want.VoxelCount)
			}
			expectNear(t, name+" volume", want.PhysicalSize, s.PhysicalSize, 1e-9)
			expectVecNear(t, name+" centroid", vec(want.Centroid), vec(s.Centroid), 1e-9)
			expectVecNear(t, name+" moments", want.PrincipalMoments, s.PrincipalMoments, 1e-9)
			expectNear(t, name+" Feret", want.FeretDiameter, s.FeretDiameter, 1e-9)
			expectNear(t, name+" perimeter", want.Perimeter, s.Perimeter, 1e-9)
			for k := range want.PrincipalAxes {
				expectVecNear(t, name+" axis", vec(want.PrincipalAxes[k]), vec(s.PrincipalAxes[k]), 1e-9)
			}
			expectVecNear(t, name+" OBB origin", vec(want.OrientedBoundingBox.Origin), vec(s.OrientedBoundingBox.Origin), 1e-9)
		}

		wantValues, err := m.IntensityStatistics(values)
		if err != nil {
			t.Fatal(err)
		}
		segValues := seg.IntensityStatistics(func(x, y, z int) (float64, bool) { return values[(z*ny+y)*nx+x], true })
		for _, s := range []IntensityStatistics{labelValues[li], segValues} {
			if s.Count != wantValues.Count || s.Median != wantValues.Median || math.Abs(s.Mean-wantValues.Mean) > 1e-9 {
				t.Fatalf("label %d intensity %+v, want %+v", label, s, wantValues)
			}
		}
	}
}

func TestIntensityPercentilesAgainstSortedReference(t *testing.T) {
	s := NewIntensityStatistics([]float64{4, math.NaN(), 1, 3, 2})
	if s.Count != 4 || s.Median != 2.5 || s.Min != 1 || s.Max != 4 {
		t.Fatalf("stats %+v", s)
	}
	// NumPy: np.percentile([1,2,3,4], [0,25,50,75,100]).
	expectVecNear(t, "percentiles", []float64{1, 1.75, 2.5, 3.25, 4}, s.Percentiles(0, 25, 50, 75, 100), 1e-12)
	// np.percentile([15, 20, 35, 40, 50], 40) = 29.
	p, ok := NewIntensityStatistics([]float64{50, 15, 40, 20, 35}).Percentile(40)
	if !ok || math.Abs(p-29) > 1e-12 {
		t.Fatalf("P40 = %g", p)
	}
	if _, ok := s.Percentile(101); ok {
		t.Fatal("P101 accepted")
	}
	if _, ok := (IntensityStatistics{}).Percentile(50); ok {
		t.Fatal("percentile of an empty set")
	}

	// Random values: every percentile lies between the ranks it
	// interpolates, and the variance matches a direct computation.
	rng := rand.New(rand.NewSource(7))
	values := make([]float64, 1001)
	for i := range values {
		values[i] = rng.NormFloat64()*40 + 100
	}
	s = NewIntensityStatistics(values)
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	for _, q := range []float64{1, 5, 10, 50, 90, 95, 99} {
		got, _ := s.Percentile(q)
		rank := q / 100 * 1000
		i := int(rank)
		want := sorted[i] + (rank-float64(i))*(sorted[i+1]-sorted[i])
		expectNear(t, "percentile", want, got, 1e-12)
	}
	expectNear(t, "median", sorted[500], s.Median, 0)
	mean, sq := 0.0, 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	for _, v := range values {
		sq += (v - mean) * (v - mean)
	}
	expectNear(t, "variance", sq/1000, s.Variance, 1e-9)
}
