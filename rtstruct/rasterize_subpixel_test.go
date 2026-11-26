package rtstruct

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	dicomroi "github.com/ThalesMMS/dicom-go/roi"
)

func subPixelTestGeometry(origin render.Vec3, columnSpacing, rowSpacing, sliceSpacing float64, columns, rows, slices int) render.VolumeGeometry {
	geometry := make([]render.SliceGeometry, slices)
	for i := range geometry {
		geometry[i] = render.SliceGeometry{
			Origin:     origin.Add(render.Vec3{Z: float64(i) * sliceSpacing}),
			RowDir:     render.Vec3{X: 1},
			ColDir:     render.Vec3{Y: 1},
			Normal:     render.Vec3{Z: 1},
			RowSpacing: rowSpacing,
			ColSpacing: columnSpacing,
			Rows:       rows,
			Columns:    columns,
		}
	}
	return render.BuildVolumeGeometry(geometry, render.DefaultGeometryTolerances())
}

// TestRasterizeSamplesSubPixelContoursAtVoxelCentres replaces the former
// behaviour of rounding every contour vertex to the nearest pixel. The square
// [2.6, 7.4) mm on a 1 mm grid covers the voxel centres 3..7 (25 voxels for an
// area of 23.04 mm^2); rounding the vertices to [3, 7) kept only 16.
func TestRasterizeSamplesSubPixelContoursAtVoxelCentres(t *testing.T) {
	set := &StructureSet{ROIs: []ROI{{Number: 1, Contours: []Contour{{
		GeometricType: ContourClosedPlanar,
		Points:        []Point3D{{X: 2.6, Y: 2.6}, {X: 7.4, Y: 2.6}, {X: 7.4, Y: 7.4}, {X: 2.6, Y: 7.4}},
	}}}}}
	mask, err := Rasterize(set, RasterizeOptions{ROINumber: 1, Geometry: testRTGeometry(), Columns: 10, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := mask.VoxelCount(); got != 25 {
		t.Fatalf("voxel count = %d, want 25", got)
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			want := x >= 3 && x <= 7 && y >= 3 && y <= 7
			if mask.Voxel(x, y, 0) != want {
				t.Fatalf("voxel (%d, %d) = %v, want %v", x, y, !want, want)
			}
		}
	}
}

// TestRasterizeSphereVolumeMatchesAnalytic rasterizes the planar sections of
// a sphere with a sub-voxel centre on an anisotropic grid; the voxel volume
// matches 4/3*pi*R^3 and every section matches the analytic circle area.
func TestRasterizeSphereVolumeMatchesAnalytic(t *testing.T) {
	const (
		columnSpacing, rowSpacing, sliceSpacing = 0.8, 0.6, 1.5
		columns, rows, slices                   = 64, 80, 24
		radius                                  = 14.0
		vertices                                = 720
	)
	origin := render.Vec3{X: -20.3, Y: -18.45, Z: -12.9}
	centre := render.Vec3{X: 5.37, Y: 4.91, Z: 4.43}
	geometry := subPixelTestGeometry(origin, columnSpacing, rowSpacing, sliceSpacing, columns, rows, slices)
	roi := ROI{Number: 7}
	expected := 0.0
	for slice := 0; slice < slices; slice++ {
		z := origin.Z + float64(slice)*sliceSpacing
		dz := z - centre.Z
		if math.Abs(dz) >= radius {
			continue
		}
		r := math.Sqrt(radius*radius - dz*dz)
		expected += math.Pi * r * r * sliceSpacing
		points := make([]Point3D, vertices)
		for k := range points {
			angle := 2 * math.Pi * float64(k) / vertices
			points[k] = Point3D{X: centre.X + r*math.Cos(angle), Y: centre.Y + r*math.Sin(angle), Z: z}
		}
		roi.Contours = append(roi.Contours, Contour{GeometricType: ContourClosedPlanar, Points: points})
	}
	mask, err := Rasterize(&StructureSet{ROIs: []ROI{roi}}, RasterizeOptions{ROINumber: 7, Geometry: geometry, Columns: columns, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	voxelVolume := columnSpacing * rowSpacing * sliceSpacing
	volume := float64(mask.VoxelCount()) * voxelVolume
	sphere := 4.0 / 3.0 * math.Pi * radius * radius * radius
	// The slab sum of the sections is the midpoint rule for the sphere; the
	// pixel sampling itself must stay far below one voxel layer.
	if relative := math.Abs(volume-expected) / expected; relative > 0.005 {
		t.Fatalf("voxel volume %.1f mm^3, sections %.1f mm^3 (%.2f%%)", volume, expected, 100*relative)
	}
	if relative := math.Abs(volume-sphere) / sphere; relative > 0.01 {
		t.Fatalf("voxel volume %.1f mm^3, sphere %.1f mm^3 (%.2f%%)", volume, sphere, 100*relative)
	}
	t.Logf("voxel volume %.1f mm^3, sections %.1f mm^3, sphere %.1f mm^3", volume, expected, sphere)
}

// TestRasterizeRoundTripContourOnPixelCentresMatchesIntegerContour checks
// that vertices authored on pixel centres, written in patient coordinates with
// non-dyadic spacings and origins, rasterize exactly like the integer contour:
// roi.RasterizePolygon absorbs the round-off, which would otherwise move
// whole rows or columns of edge ties, while a genuine sub-pixel vertex keeps
// its position.
func TestRasterizeRoundTripContourOnPixelCentresMatchesIntegerContour(t *testing.T) {
	corners := [][2]float64{{3, 4}, {11, 4}, {17, 9}, {11, 13}, {3, 13}, {3.25, 8.5}}
	integer := make([]dicomroi.Point2D, len(corners))
	for i, corner := range corners {
		integer[i] = dicomroi.Point2D{X: corner[0], Y: corner[1]}
	}
	want := dicomroi.RasterizePolygon(integer, 32, 32)
	if !want.Get(3, 4) || want.Get(11, 13) || want.Get(3, 8) || !want.Get(4, 8) {
		t.Fatal("edge ties of the integer contour changed")
	}
	inexact := 0
	for trial := 0; trial < 64; trial++ {
		spacing := 0.3 + 0.0137*float64(trial) + 1e-7
		origin := render.Vec3{X: -174.658203 + 3.1*float64(trial), Y: -191.358203 + 1.7*float64(trial), Z: 31.7}
		geometry := subPixelTestGeometry(origin, spacing, spacing, 2.5, 32, 32, 1)
		points := make([]Point3D, len(corners))
		for i, corner := range corners {
			points[i] = Point3D{X: origin.X + corner[0]*spacing, Y: origin.Y + corner[1]*spacing, Z: origin.Z}
		}
		_, pixels, err := contourToImage(Contour{Points: points}, geometry)
		if err != nil {
			t.Fatal(err)
		}
		for i, corner := range corners {
			if math.Abs(pixels[i].X-corner[0]) > 1e-9 || math.Abs(pixels[i].Y-corner[1]) > 1e-9 {
				t.Fatalf("trial %d vertex %d = %+v, want %v", trial, i, pixels[i], corner)
			}
			if pixels[i].X != corner[0] || pixels[i].Y != corner[1] {
				inexact++
			}
		}
		mask, err := Rasterize(&StructureSet{ROIs: []ROI{{Number: 1, Contours: []Contour{{GeometricType: ContourClosedPlanar, Points: points}}}}},
			RasterizeOptions{ROINumber: 1, Geometry: geometry, Columns: 32, Rows: 32})
		if err != nil {
			t.Fatal(err)
		}
		got := mask.Mask(0)
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				if got.Get(x, y) != want.Get(x, y) {
					t.Fatalf("trial %d voxel (%d, %d) = %v, want %v", trial, x, y, got.Get(x, y), want.Get(x, y))
				}
			}
		}
	}
	if inexact == 0 {
		t.Fatal("every round trip was exact; the fixture no longer exercises round-off")
	}
}
