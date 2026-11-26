package render

// The cases of testdata/upstream/itk/series_direction.json are ported from
// ITK's Modules/IO/GDCM/test/itkGDCMSeriesDirectionGTest.cxx (ITK commit
// 35b196e1, Copyright NumFOCUS, Apache License 2.0; see THIRD_PARTY_NOTICES.md
// and testdata/upstream/itk/README.md). They were modified for this package:
// the synthetic series are built in memory as render.Frame values instead of
// DICOM files, ITK's image direction and origin are compared with
// render.VolumeGeometry, and the cases whose contract differs are adapted as
// documented below (#1120). ITK's ForceOrthogonalDirection on/off cases do
// not apply: the normal is always Row × Column here, and the Image Position
// differences only classify gantry tilt. RealDicomSeries needs ITK's data.
//
// dicom-go does not keep the input order of a series: slices are sorted along
// the normal Row × Column, so the stacking direction is always +Normal, voxel
// 0 is the slice lowest along it, and VolumeGeometry.ReversedInput records a
// descending input. ITK instead keeps the input order and lets the third
// direction column follow it. The invariant both honour is that every voxel
// sits at the same physical point whatever the input order.

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// directionSeries describes one synthetic series (ITK's SeriesSpec).
type directionSeries struct {
	row, column  Vec3
	origin, step Vec3 // first slice Image Position and the step between slices
	slices       int
}

// frames builds the series as 4×3 frames whose every voxel value is distinct
// (slice*12 + y*4 + x), in the written order.
func (s directionSeries) frames() []*Frame {
	const columns, rows = 4, 3
	frames := make([]*Frame, s.slices)
	for k := range frames {
		data := make([]byte, rows*columns)
		for i := range data {
			data[i] = byte(k*rows*columns + i)
		}
		frame := volumeTestFrame(rows, columns, data, 0)
		frame.ImageOrientation = []float64{s.row.X, s.row.Y, s.row.Z, s.column.X, s.column.Y, s.column.Z}
		position := s.origin.Add(s.step.Scale(float64(k)))
		frame.ImagePosition = []float64{position.X, position.Y, position.Z}
		frames[k] = frame
	}
	return frames
}

func directionVolume(t *testing.T, frames []*Frame, spacing float64) *Volume {
	t.Helper()
	vol, err := BuildVolume(&Stack{PixelSpacing: []float64{spacing, spacing}, Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

func reversedFrames(frames []*Frame) []*Frame {
	out := make([]*Frame, len(frames))
	for i, frame := range frames {
		out[len(frames)-1-i] = frame
	}
	return out
}

// expectOrthonormal checks that RowDir, ColDir, Normal form a right-handed
// orthonormal basis with Normal = RowDir × ColDir (ITK's expectOrthonormal and
// ThirdColumnEqualsCrossProduct).
func expectOrthonormal(t *testing.T, g VolumeGeometry) {
	t.Helper()
	const tol = 1e-9
	if math.Abs(g.RowDir.Length()-1) > tol || math.Abs(g.ColDir.Length()-1) > tol || math.Abs(g.RowDir.Dot(g.ColDir)) > tol ||
		g.RowDir.Cross(g.ColDir).Sub(g.Normal).Length() > tol {
		t.Fatalf("basis %v %v %v is not orthonormal with Normal = Row × Column", g.RowDir, g.ColDir, g.Normal)
	}
}

// expectPhysicalPointInvariant checks that every voxel of a is at a physical
// point where b has a voxel with the same value (ITK's
// expectPhysicalPointInvariant).
func expectPhysicalPointInvariant(t *testing.T, a, b *Volume) {
	t.Helper()
	if a.Cols != b.Cols || a.Rows != b.Rows || a.Depth != b.Depth {
		t.Fatalf("sizes %dx%dx%d and %dx%dx%d", a.Cols, a.Rows, a.Depth, b.Cols, b.Rows, b.Depth)
	}
	readerA, err := a.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer readerA.Close()
	readerB, err := b.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer readerB.Close()
	for z := 0; z < a.Depth; z++ {
		for y := 0; y < a.Rows; y++ {
			for x := 0; x < a.Cols; x++ {
				p := a.VoxelToPatient(Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				index := b.PatientToVoxel(p)
				if math.Abs(index.X-math.Round(index.X)) > 1e-6 || math.Abs(index.Y-math.Round(index.Y)) > 1e-6 || math.Abs(index.Z-math.Round(index.Z)) > 1e-6 {
					t.Fatalf("voxel (%d,%d,%d) at %v falls between voxels of the other reading (%v)", x, y, z, p, index)
				}
				va, okA := readerA.SamplePatient(p)
				vb, okB := readerB.SamplePatient(p)
				if !okA || !okB || va != vb {
					t.Fatalf("voxel (%d,%d,%d) at %v: %v %v and %v %v", x, y, z, p, va, okA, vb, okB)
				}
			}
		}
	}
}

func obliqueBasis(a, b float64) (row, column Vec3) {
	// Rz(a)·Rx(b) applied to e_x and e_y: no component is 0 or ±1.
	ca, sa, cb, sb := math.Cos(a), math.Sin(a), math.Cos(b), math.Sin(b)
	return Vec3{X: ca, Y: sa * cb, Z: sa * sb}.Normalize(), Vec3{X: -sa, Y: ca * cb, Z: ca * sb}.Normalize()
}

// itkDirectionCase is one case of testdata/upstream/itk/series_direction.json.
type itkDirectionCase struct {
	Name         string     `json:"name"`
	ITKTest      string     `json:"itk_test"`
	Row          [3]float64 `json:"row"`
	Column       [3]float64 `json:"column"`
	Origin       [3]float64 `json:"origin"`
	Step         [3]float64 `json:"step"`
	Slices       int        `json:"slices"`
	SliceSpacing float64    `json:"slice_spacing"`
}

func vec3(v [3]float64) Vec3 { return Vec3{X: v[0], Y: v[1], Z: v[2]} }

func TestITKSeriesDirectionOracles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "itk", "series_direction.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []itkDirectionCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 13 {
		t.Fatalf("%d cases", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		series := directionSeries{row: vec3(tc.Row), column: vec3(tc.Column), origin: vec3(tc.Origin), step: vec3(tc.Step), slices: tc.Slices}
		t.Run(tc.Name, func(t *testing.T) {
			written := series.frames()
			forward := directionVolume(t, written, 0.5)
			reverse := directionVolume(t, reversedFrames(written), 0.5)
			for _, vol := range []*Volume{forward, reverse} {
				g := vol.Geometry()
				expectOrthonormal(t, g)
				// Spacing is positive (SpacingAlwaysPositive, TwoSlices,
				// ManySlices_FineSpacing).
				if math.Abs(vol.SliceSpacing-tc.SliceSpacing) > 1e-9 {
					t.Fatalf("slice spacing %v, want %v", vol.SliceSpacing, tc.SliceSpacing)
				}
				// The in-plane directions come from the unchanged Image
				// Orientation (Patient) in both readings (Oblique case).
				if g.RowDir.Sub(series.row).Length() > 1e-9 || g.ColDir.Sub(series.column).Length() > 1e-9 {
					t.Fatalf("in-plane directions %v %v", g.RowDir, g.ColDir)
				}
			}
			// Adapted contract (ITK: origin is the first listed slice and the
			// third column follows the input order; OriginEqualsFirstListed-
			// SliceIPP, ITKElastix291): here both readings start at the slice
			// lowest along Normal = Row × Column and ReversedInput records a
			// descending input.
			ascending := series.step.Dot(forward.Geometry().Normal) > 0
			lowest := series.origin
			if !ascending {
				lowest = series.origin.Add(series.step.Scale(float64(series.slices - 1)))
			}
			for _, vol := range []*Volume{forward, reverse} {
				if vol.VoxelToPatient(Vec3{}).Sub(lowest).Length() > 1e-9 {
					t.Fatalf("origin %v, want the lowest slice %v", vol.VoxelToPatient(Vec3{}), lowest)
				}
			}
			if forward.Geometry().ReversedInput == ascending || reverse.Geometry().ReversedInput != ascending {
				t.Fatalf("ReversedInput forward %v reverse %v, series ascending %v", forward.Geometry().ReversedInput, reverse.Geometry().ReversedInput, ascending)
			}
			// Walking spacing × (N-1) along Normal from the origin reaches
			// the highest slice (SimpleITK#2292).
			last := forward.VoxelToPatient(Vec3{Z: float64(forward.Depth - 1)})
			if want := lowest.Add(forward.Geometry().Normal.Scale(tc.SliceSpacing * float64(series.slices-1))); last.Sub(want).Length() > 1e-9 {
				t.Fatalf("last slice at %v, want %v", last, want)
			}
			expectPhysicalPointInvariant(t, forward, reverse)
		})
	}
}

// TestITKSeriesDirectionRandomizedStress ports ITK's RandomizedStress case:
// random oblique orientations, steps along either sense of the normal and
// shuffled input orders all place every voxel at the same physical point.
func TestITKSeriesDirectionRandomizedStress(t *testing.T) {
	random := rand.New(rand.NewSource(1120))
	for trial := 0; trial < 40; trial++ {
		row, column := obliqueBasis(random.Float64()*math.Pi, random.Float64()*math.Pi/2)
		step := row.Cross(column).Normalize().Scale((0.5 + random.Float64()*3) * []float64{-1, 1}[random.Intn(2)])
		series := directionSeries{row: row, column: column, origin: Vec3{X: random.NormFloat64() * 50, Y: random.NormFloat64() * 50, Z: random.NormFloat64() * 50}, step: step, slices: 2 + random.Intn(7)}
		written := series.frames()
		shuffled := append([]*Frame(nil), written...)
		random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		a, b := directionVolume(t, written, 0.8), directionVolume(t, shuffled, 0.8)
		expectOrthonormal(t, a.Geometry())
		expectPhysicalPointInvariant(t, a, b)
	}
}
