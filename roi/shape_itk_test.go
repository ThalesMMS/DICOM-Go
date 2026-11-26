package roi

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// Test cases ported from ITK's itkShapeLabelMapFilterGTest.cxx and
// itkStatisticsLabelMapFilterGTest.cxx (Modules/Filtering/LabelMap/test,
// ITK commit 35b196e1, Apache-2.0, Copyright NumFOCUS). The synthetic
// images, expected values and tolerances are ITK's; the code is new.
// Modifications: the OBB origin, which depends on the principal-axis signs
// ITK leaves to its eigen solver, is checked as membership of the OBB
// vertex set (as ITK itself does for the spacing case); flatness of the
// rotated flat object, which ITK leaves untested as round-off, is checked
// as 0; and the random-image case uses Go's math/rand instead of C rand().

// itkDirection is the rotation used by ITK's *_Direction cases, given row
// major; its columns are the axis directions.
var itkDirection = [9]float64{
	0.7950707161543119, -0.44533237368675166, 0.41175433605536305,
	-0.6065167008084678, -0.5840224148057925, 0.5394954222649374,
	0.00021898465942798317, -0.6786728931900383, -0.7344406416415056,
}

func itkDirectionColumns() [3]render.Vec3 {
	d := itkDirection
	return [3]render.Vec3{{X: d[0], Y: d[3], Z: d[6]}, {X: d[1], Y: d[4], Z: d[7]}, {X: d[2], Y: d[5], Z: d[8]}}
}

func itkImage(size int, spacing [3]float64) *VolumeMask {
	return NewVolumeMask(size, size, size, spacing)
}

func expectNear(t *testing.T, name string, want, got, tol float64) {
	t.Helper()
	if math.Abs(want-got) > tol || math.IsNaN(got) {
		t.Errorf("%s = %.12g, want %.12g ± %g", name, got, want, tol)
	}
}

func expectVecNear(t *testing.T, name string, want []float64, got []float64, tol float64) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range want {
		if math.Abs(want[i]-got[i]) > tol || math.IsNaN(got[i]) {
			t.Errorf("%s = %v, want %v ± %g", name, got, want, tol)
			return
		}
	}
}

func vec(v render.Vec3) []float64 { return []float64{v.X, v.Y, v.Z} }

func hasVertex(box OrientedBoundingBox, p render.Vec3, tol float64) bool {
	for _, v := range box.Vertices() {
		if v.Sub(p).Length() < tol {
			return true
		}
	}
	return false
}

func shapeOf(t *testing.T, m *VolumeMask, opts ShapeOptions) ShapeStatistics {
	t.Helper()
	s, err := m.ShapeStatistics(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestITKShape3DT1x1x1(t *testing.T) {
	m := itkImage(25, [3]float64{1, 1, 1})
	m.Voxels[m.index(5, 7, 9)] = true
	s := shapeOf(t, m, ShapeOptions{})

	if s.BoundingBoxMin != [3]int{5, 7, 9} || s.BoundingBoxMax != [3]int{5, 7, 9} {
		t.Errorf("bounding box %v..%v", s.BoundingBoxMin, s.BoundingBoxMax)
	}
	expectVecNear(t, "Centroid", []float64{5, 7, 9}, vec(s.Centroid), 1e-10)
	expectNear(t, "Elongation", 0, s.Elongation, 0)
	expectVecNear(t, "EquivalentEllipsoidDiameter", []float64{0, 0, 0}, s.EquivalentEllipsoidDiameter, 1e-10)
	expectNear(t, "EquivalentSphericalPerimeter", 4.8359758620494109, s.EquivalentSphericalPerimeter, 1e-5)
	expectNear(t, "EquivalentSphericalRadius", 0.62035049089940009, s.EquivalentSphericalRadius, 1e-5)
	expectNear(t, "FeretDiameter", 0, s.FeretDiameter, 0)
	expectNear(t, "Flatness", 0, s.Flatness, 0)
	if s.VoxelCount != 1 || s.VoxelsOnBorder != 0 {
		t.Errorf("VoxelCount %d, VoxelsOnBorder %d", s.VoxelCount, s.VoxelsOnBorder)
	}
	expectVecNear(t, "OBB size", []float64{1, 1, 1}, s.OrientedBoundingBox.Size, 1e-10)
	expectVecNear(t, "OBB origin", []float64{4.5, 6.5, 8.5}, vec(s.OrientedBoundingBox.Origin), 1e-10)
	expectNear(t, "Perimeter", 3.0040803078963907, s.Perimeter, 1e-5)
	expectNear(t, "PerimeterOnBorder", 0, s.PerimeterOnBorder, 0)
	expectNear(t, "PerimeterOnBorderRatio", 0, s.PerimeterOnBorderRatio, 0)
	expectNear(t, "PhysicalSize", 1, s.PhysicalSize, 0)
	expectVecNear(t, "PrincipalMoments", []float64{0, 0, 0}, s.PrincipalMoments, 0)
	expectNear(t, "Roundness", 1.6098024574568734, s.Sphericity, 1e-5)
}

func itk3x2x1(m *VolumeMask) {
	for i := 5; i < 8; i++ {
		m.Voxels[m.index(i, 9, 11)] = true
		m.Voxels[m.index(i, 10, 11)] = true
	}
}

func TestITKShape3DT3x2x1(t *testing.T) {
	m := itkImage(25, [3]float64{1, 1, 1})
	itk3x2x1(m)
	s := shapeOf(t, m, ShapeOptions{})

	if s.BoundingBoxMin != [3]int{5, 9, 11} || s.BoundingBoxMax != [3]int{7, 10, 11} {
		t.Errorf("bounding box %v..%v", s.BoundingBoxMin, s.BoundingBoxMax)
	}
	expectVecNear(t, "Centroid", []float64{6, 9.5, 11}, vec(s.Centroid), 0)
	expectNear(t, "Elongation", 1.63299, s.Elongation, 1e-4)
	expectVecNear(t, "EquivalentEllipsoidDiameter", []float64{0, 0, 0}, s.EquivalentEllipsoidDiameter, 1e-10)
	expectNear(t, "EquivalentSphericalPerimeter", 15.96804047389762, s.EquivalentSphericalPerimeter, 1e-5)
	expectNear(t, "EquivalentSphericalRadius", 1.1272516517868265, s.EquivalentSphericalRadius, 1e-5)
	expectNear(t, "FeretDiameter", 2.23606, s.FeretDiameter, 1e-4)
	expectNear(t, "Flatness", 0, s.Flatness, 0)
	if s.VoxelCount != 6 || s.VoxelsOnBorder != 0 {
		t.Errorf("VoxelCount %d, VoxelsOnBorder %d", s.VoxelCount, s.VoxelsOnBorder)
	}
	expectVecNear(t, "OBB size", []float64{1, 2, 3}, s.OrientedBoundingBox.Size, 1e-10)
	expectVecNear(t, "OBB origin", []float64{7.5, 8.5, 10.5}, vec(s.OrientedBoundingBox.Origin), 1e-10)
	expectNear(t, "Perimeter", 14.624143852112988, s.Perimeter, 1e-5)
	expectNear(t, "PerimeterOnBorder", 0, s.PerimeterOnBorder, 0)
	expectNear(t, "PerimeterOnBorderRatio", 0, s.PerimeterOnBorderRatio, 0)
	expectNear(t, "PhysicalSize", 6, s.PhysicalSize, 0)
	expectVecNear(t, "PrincipalMoments", []float64{0, 0.25, 0.666667}, s.PrincipalMoments, 1e-4)
	expectNear(t, "Roundness", 1.0918957468809676, s.Sphericity, 1e-5)
}

func TestITKShape3DT3x2x1Direction(t *testing.T) {
	m := itkImage(25, [3]float64{1, 1, 1})
	itk3x2x1(m)
	s := shapeOf(t, m, ShapeOptions{Direction: itkDirectionColumns()})

	if s.BoundingBoxMin != [3]int{5, 9, 11} || s.BoundingBoxMax != [3]int{7, 10, 11} {
		t.Errorf("bounding box %v..%v", s.BoundingBoxMin, s.BoundingBoxMax)
	}
	expectVecNear(t, "Centroid", []float64{5.0690644435107242, -3.2528635005915247, -14.524925635405358}, vec(s.Centroid), 1e-4)
	expectNear(t, "Elongation", 1.63299, s.Elongation, 1e-4)
	expectNear(t, "EquivalentSphericalPerimeter", 15.96804047389762, s.EquivalentSphericalPerimeter, 1e-5)
	expectNear(t, "EquivalentSphericalRadius", 1.1272516517868265, s.EquivalentSphericalRadius, 1e-5)
	expectNear(t, "FeretDiameter", 2.23606, s.FeretDiameter, 1e-4)
	expectNear(t, "Flatness", 0, s.Flatness, 0) // ITK: unstable, untested
	if s.VoxelCount != 6 || s.VoxelsOnBorder != 0 {
		t.Errorf("VoxelCount %d, VoxelsOnBorder %d", s.VoxelCount, s.VoxelsOnBorder)
	}
	expectVecNear(t, "OBB size", []float64{1, 2, 3}, s.OrientedBoundingBox.Size, 1e-10)
	if !hasVertex(s.OrientedBoundingBox, render.Vec3{X: 6.0222153120831488, Y: -4.4769132554776352, Z: -15.570490372427054}, 1e-4) {
		t.Errorf("OBB vertices %v miss ITK's origin", s.OrientedBoundingBox.Vertices())
	}
	expectNear(t, "Perimeter", 14.624143852112988, s.Perimeter, 1e-5)
	expectNear(t, "PerimeterOnBorder", 0, s.PerimeterOnBorder, 0)
	expectNear(t, "PerimeterOnBorderRatio", 0, s.PerimeterOnBorderRatio, 0)
	expectNear(t, "PhysicalSize", 6, s.PhysicalSize, 0)
	expectVecNear(t, "PrincipalMoments", []float64{0, 0.25, 0.666667}, s.PrincipalMoments, 1e-4)
	expectNear(t, "Roundness", 1.0918957468809676, s.Sphericity, 1e-5)
}

func itk2x2x2(m *VolumeMask) {
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			m.Voxels[m.index(5+j, 9+i, 11)] = true
			m.Voxels[m.index(5+j, 9+i, 12)] = true
		}
	}
}

func TestITKShape3DT2x2x2Spacing(t *testing.T) {
	for _, direction := range []bool{false, true} {
		m := itkImage(25, [3]float64{1.0, 1.1, 2.2})
		itk2x2x2(m)
		opts := ShapeOptions{}
		if direction {
			opts.Direction = itkDirectionColumns()
		}
		s := shapeOf(t, m, opts)

		if s.BoundingBoxMin != [3]int{5, 9, 11} || s.BoundingBoxMax != [3]int{6, 10, 12} {
			t.Errorf("bounding box %v..%v", s.BoundingBoxMin, s.BoundingBoxMax)
		}
		centroid, obbOrigin := []float64{5.5, 10.45, 25.3}, render.Vec3{X: 4.5, Y: 9.35, Z: 23.1}
		if direction {
			centroid = []float64{10.136550336022847, 4.2103580941358141, -25.672275551739141}
			obbOrigin = render.Vec3{X: 9.7574735481346888, Y: 5.3613400676410672, Z: -28.034804130519035}
		}
		expectVecNear(t, "Centroid", centroid, vec(s.Centroid), 1e-4)
		expectNear(t, "Elongation", 2.0, s.Elongation, 1e-4)
		expectVecNear(t, "EquivalentEllipsoidDiameter", []float64{2.4814019635976252, 2.7295421599572678, 5.4590843199148482}, s.EquivalentEllipsoidDiameter, 1e-5)
		expectNear(t, "EquivalentSphericalPerimeter", 34.867517413417708, s.EquivalentSphericalPerimeter, 1e-5)
		expectNear(t, "EquivalentSphericalRadius", 1.6657337346779293, s.EquivalentSphericalRadius, 1e-5)
		expectNear(t, "FeretDiameter", 2.65518, s.FeretDiameter, 1e-4)
		expectNear(t, "Flatness", 1.1, s.Flatness, 1e-4)
		if s.VoxelCount != 8 || s.VoxelsOnBorder != 0 {
			t.Errorf("VoxelCount %d, VoxelsOnBorder %d", s.VoxelCount, s.VoxelsOnBorder)
		}
		expectVecNear(t, "OBB size", []float64{2.0, 2.2, 4.4}, s.OrientedBoundingBox.Size, 1e-10)
		expectNear(t, "Perimeter", 28.391854811919647, s.Perimeter, 1e-5)
		expectNear(t, "PerimeterOnBorder", 0, s.PerimeterOnBorder, 0)
		expectNear(t, "PerimeterOnBorderRatio", 0, s.PerimeterOnBorderRatio, 0)
		expectNear(t, "PhysicalSize", 19.36, s.PhysicalSize, 1e-10)
		if !hasVertex(s.OrientedBoundingBox, obbOrigin, 1e-4) {
			t.Errorf("direction=%v: OBB vertices %v miss %v", direction, s.OrientedBoundingBox.Vertices(), obbOrigin)
		}
		expectVecNear(t, "PrincipalMoments", []float64{0.25, 0.3025, 1.21}, s.PrincipalMoments, 1e-4)
		expectNear(t, "Roundness", 1.2280817031643669, s.Sphericity, 1e-5)
	}
}

func itk2D(t *testing.T, pixels [][2]int, direction [3]render.Vec3) ShapeStatistics {
	t.Helper()
	m := NewRasterMask(25, 25)
	for _, p := range pixels {
		m.Set(p[0], p[1], true)
	}
	s, err := m.ShapeStatistics(1, 1, ShapeOptions{Direction: direction})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestITKShape2DOrientedBoundingBox(t *testing.T) {
	flip := [3]render.Vec3{{Y: 1}, {X: 1}}
	tests := []struct {
		name      string
		pixels    [][2]int
		direction [3]render.Vec3
		size      []float64
		origin    render.Vec3
		vertices  []render.Vec3
	}{
		{name: "T1x1", pixels: [][2]int{{5, 7}}, size: []float64{1, 1}, origin: render.Vec3{X: 4.5, Y: 6.5}},
		{name: "T1_1", pixels: [][2]int{{5, 7}, {6, 8}}, size: []float64{math.Sqrt2, 2 * math.Sqrt2}, origin: render.Vec3{X: 4, Y: 7}},
		{name: "T1_1_FlipDirection", pixels: [][2]int{{5, 7}, {6, 8}}, direction: flip, size: []float64{math.Sqrt2, 2 * math.Sqrt2}, origin: render.Vec3{X: 6, Y: 5}},
		{name: "T1_2_Direction", pixels: [][2]int{{5, 7}, {5, 8}}, direction: flip, vertices: []render.Vec3{{X: 8.5, Y: 4.5}, {X: 8.5, Y: 5.5}, {X: 6.5, Y: 4.5}, {X: 6.5, Y: 5.5}}},
		{name: "T2x4", pixels: func() (p [][2]int) {
			for i := 4; i < 6; i++ {
				for j := 3; j < 7; j++ {
					p = append(p, [2]int{i, j})
				}
			}
			return p
		}(), size: []float64{2, 4}, origin: render.Vec3{X: 3.5, Y: 2.5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := itk2D(t, tt.pixels, tt.direction)
			if s.Dimension != 2 {
				t.Fatalf("Dimension = %d", s.Dimension)
			}
			if tt.size != nil {
				expectVecNear(t, "OBB size", tt.size, s.OrientedBoundingBox.Size, 1e-4)
				if !hasVertex(s.OrientedBoundingBox, tt.origin, 1e-4) {
					t.Errorf("OBB vertices %v miss ITK's origin %v", s.OrientedBoundingBox.Vertices(), tt.origin)
				}
			}
			if tt.vertices != nil {
				if n := len(s.OrientedBoundingBox.Vertices()); n != 4 {
					t.Fatalf("%d vertices", n)
				}
				for _, v := range tt.vertices {
					if !hasVertex(s.OrientedBoundingBox, v, 1e-8) {
						t.Errorf("OBB vertices %v miss %v", s.OrientedBoundingBox.Vertices(), v)
					}
				}
			}
		})
	}
}

func TestITKShape3DDegenerateFlatObjectNumericallyStable(t *testing.T) {
	m := NewVolumeMask(64, 64, 64, [3]float64{0.9, 1.1, 1.7})
	for x := 8; x < 56; x++ {
		m.Voxels[m.index(x, 32, 31)] = true
	}
	s := shapeOf(t, m, ShapeOptions{Direction: itkDirectionColumns(), Origin: render.Vec3{X: 1e8, Y: -1e8, Z: 0.5e8}})
	maxMoment := 0.0
	for i := 0; i < 3; i++ {
		pm, d := s.PrincipalMoments[i], s.EquivalentEllipsoidDiameter[i]
		if math.IsNaN(pm) || math.IsInf(pm, 0) || pm < 0 || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			t.Fatalf("moment %d = %g, ellipsoid diameter %g", i, pm, d)
		}
		maxMoment = math.Max(maxMoment, pm)
	}
	if !(maxMoment > 0) {
		t.Fatal("all principal moments are zero")
	}
	det := s.PrincipalMoments[0] * s.PrincipalMoments[1] * s.PrincipalMoments[2]
	if math.IsNaN(det) || det < 0 {
		t.Fatalf("moment product %g", det)
	}
	// Beyond ITK: the line's variance is exact ((n²-1)/12·s²).
	expectNear(t, "largest moment", (48*48-1)/12.0*0.81, s.PrincipalMoments[2], 1e-9)
	expectNear(t, "FeretDiameter", 47*0.9, s.FeretDiameter, 1e-9)
}

// Ports of itkStatisticsLabelMapFilterGTest.cxx on a 25×25 label image.

func itkLabelStats(t *testing.T, labels []uint16, values []float64) IntensityStatistics {
	t.Helper()
	v := &LabelVolume{Grid: shapeAxisGrid(25, 25, 1, [3]float64{1, 1, 1}), Labels: labels}
	out, err := LabelIntensityStatistics(context.Background(), v, values)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out {
		if s.Label == 1 {
			if s.Count == 0 {
				t.Fatal("label 1 is empty")
			}
			return s
		}
	}
	t.Fatal("label 1 missing")
	return IntensityStatistics{}
}

// shapeAxisGrid is an identity-oriented grid with origin 0.
func shapeAxisGrid(columns, rows, slices int, spacing [3]float64) Grid {
	return Grid{Columns: columns, Rows: rows, Slices: slices,
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: spacing[0], RowSpacing: spacing[1], SliceSpacing: spacing[2]}
}

func exactMedian(labels []uint16, values []float64) float64 {
	var in []float64
	for i, l := range labels {
		if l == 1 {
			in = append(in, values[i])
		}
	}
	sort.Float64s(in)
	n := len(in) / 2
	if len(in)%2 == 0 {
		return 0.5 * (in[n] + in[n-1])
	}
	return in[n]
}

func at(x, y int) int { return y*25 + x }

func TestITKStatistics2DZero(t *testing.T) {
	labels, values := make([]uint16, 625), make([]float64, 625)
	for i := range labels {
		labels[i] = 1
	}
	s := itkLabelStats(t, labels, values)
	expectNear(t, "Minimum", 0, s.Min, 1e-12)
	expectNear(t, "Maximum", 0, s.Max, 1e-12)
	expectNear(t, "Median", exactMedian(labels, values), s.Median, 1e-12)
	expectNear(t, "Sum", 0, s.Sum, 1e-12)
	expectNear(t, "Variance", 0, s.Variance, 1e-12)
	expectNear(t, "StandardDeviation", 0, s.StandardDeviation, 0.5)
}

func TestITKStatistics2DOnesWithOutliers(t *testing.T) {
	labels, values := make([]uint16, 625), make([]float64, 625)
	for i := range labels {
		labels[i], values[i] = 1, 1
	}
	values[at(0, 0)], values[at(0, 1)] = 32000, -32000
	labels[at(0, 0)], labels[at(0, 1)] = 0, 0
	s := itkLabelStats(t, labels, values)
	expectNear(t, "Minimum", 1, s.Min, 1e-12)
	expectNear(t, "Maximum", 1, s.Max, 1e-12)
	expectNear(t, "Median", exactMedian(labels, values), s.Median, 1e-12)
	expectNear(t, "Sum", 25*25-2, s.Sum, 1e-12)
	expectNear(t, "Variance", 0, s.Variance, 1e-12)
	expectNear(t, "StandardDeviation", 0, s.StandardDeviation, 1e-12)
}

func TestITKStatistics2DRandWithOutliers(t *testing.T) {
	rng := rand.New(rand.NewSource(0))
	labels, values := make([]uint16, 625), make([]float64, 625)
	for i := range labels {
		labels[i], values[i] = 1, float64(rng.Intn(500))
	}
	values[at(0, 0)], values[at(0, 1)] = 32000, -2000
	values[at(0, 2)], values[at(0, 3)] = 0, 500
	labels[at(0, 0)], labels[at(0, 1)] = 0, 0
	s := itkLabelStats(t, labels, values)
	expectNear(t, "Minimum", 0, s.Min, 1e-12)
	expectNear(t, "Maximum", 500, s.Max, 1e-12)
	expectNear(t, "Median", exactMedian(labels, values), s.Median, 1e-12)
}

func TestITKStatistics2DEvenAndThree(t *testing.T) {
	tests := []struct {
		name                 string
		values               []float64
		min, max, sum, vari  float64
		stddev, varTol, sdTo float64
	}{
		{"even", []float64{10, 100, 1, 200}, 1, 200, 311, 8640.25, 92.95294, 1e-10, 1e-5},
		{"three", []float64{1, 3, 10}, 1, 10, 14, 22.33333, 4.725815, 1e-5, 1e-5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, values := make([]uint16, 625), make([]float64, 625)
			for i, v := range tt.values {
				labels[at(0, i)], values[at(0, i)] = 1, v
			}
			s := itkLabelStats(t, labels, values)
			expectNear(t, "Minimum", tt.min, s.Min, 1e-12)
			expectNear(t, "Maximum", tt.max, s.Max, 1e-12)
			expectNear(t, "Median", exactMedian(labels, values), s.Median, 1e-12)
			expectNear(t, "Sum", tt.sum, s.Sum, 1e-12)
			expectNear(t, "Variance", tt.vari, s.Variance, tt.varTol)
			expectNear(t, "StandardDeviation", tt.stddev, s.StandardDeviation, tt.sdTo)
		})
	}
}
