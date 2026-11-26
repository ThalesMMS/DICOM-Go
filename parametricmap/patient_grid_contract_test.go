package parametricmap

import (
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

// The real-world mapping is finite, and every stored value is in its domain.
// Only the product for one voxel exceeds the float64 range.
func TestReferenceParametricMapOverflowInvalidatesOnlyAffectedVoxel(t *testing.T) {
	const overflowingStoredValue = 1 << 20
	scale := math.Ldexp(1, 1010)
	file := testMapFile(t, PayloadFloat64, []float64{1, overflowingStoredValue, 2, 3, 4, 5, 6, 7}, 2, false, core.Tag{})
	file.Dataset.Put(derivedio.Seq(tagRealWorldValueMappingSeq,
		testBoundedMapping("scaled", "ms", "milliseconds", "", 0, overflowingStoredValue, scale, 0),
	))
	wire := rereadPart10(t, file)
	defer wire.Close()
	m, err := Read(wire)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("finite frame", func(t *testing.T) {
		values, err := m.FrameValues(1)
		want := []float64{4 * scale, 5 * scale, 6 * scale, 7 * scale}
		if err != nil || !slices.Equal(values, want) {
			t.Fatalf("FrameValues(1) = %v, %v; want %v", values, err, want)
		}
	})
	t.Run("overflow in one voxel", func(t *testing.T) {
		values := make([]float64, 4)
		counts, err := m.DecodeFrameInto(0, values, nil)
		if err != nil {
			t.Fatalf("DecodeFrameInto rejected a frame with three valid voxels: %v", err)
		}
		if values[0] != scale || !math.IsNaN(values[1]) || values[2] != 2*scale || values[3] != 3*scale || counts.Valid != 3 || counts.Unmeasured() != 1 {
			t.Fatalf("values = %v, counts = %+v; want [%g NaN %g %g] and one unmeasured voxel", values, counts, scale, 2*scale, 3*scale)
		}
	})
	t.Run("patient sampling and reslice", func(t *testing.T) {
		if got, ok, err := m.SamplePatient(render.Vec3{}); err != nil || !ok || got != scale {
			t.Errorf("finite patient sample = %g, %t, %v; want %g", got, ok, err, scale)
		}
		if got, ok, err := m.SamplePatient(render.Vec3{X: 1}); err != nil || ok {
			t.Errorf("overflowing patient sample = %g, %t, %v; want unmeasured", got, ok, err)
		}
		values, valid, err := m.Resample(m.Frames[0].Geometry)
		if err != nil || len(values) != 4 || !slices.Equal(valid, []bool{true, false, true, true}) {
			t.Fatalf("reslice values = %v, valid = %v, err = %v; want exactly one unmeasured voxel", values, valid, err)
		}
		if values[0] != scale || values[2] != 2*scale || values[3] != 3*scale {
			t.Fatalf("reslice changed finite voxels: %v", values)
		}
	})
}

// The two stored frames are identical. Each frame resolves its own disjoint
// real-world ranges, so one has an unmeasured corner and one has a measured
// corner. A sagittal fixture makes row, column and plane errors independent.
func TestReferenceParametricMapPatientGrid(t *testing.T) {
	for _, payload := range []PayloadKind{PayloadInteger, PayloadFloat32, PayloadFloat64} {
		t.Run(string(payload), func(t *testing.T) {
			file := testMapFile(t, payload, []float64{1, 3, 5, 7, 1, 3, 5, 7}, 2, false, core.Tag{})
			file.Dataset.Put(derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(
				derivedio.Seq(testPlaneOrientationSeq, derivedio.DataSet(derivedio.DS(testImageOrientation, 0, 1, 0, 0, 0, -1))),
				derivedio.Seq(testPixelMeasuresSeq, derivedio.DataSet(derivedio.DS(testPixelSpacing, 2, 4))),
			)))
			mapping := func(label string, first, last uint16, slope, intercept float64) core.DataSet {
				item := testBoundedMapping(label, "ms", "milliseconds", "", float64(first), float64(last), slope, intercept)
				if payload != PayloadInteger {
					return item
				}
				integer := object.FromDataSet(item, nil)
				integer.Remove(tagDoubleRWVFirstMapped)
				integer.Remove(tagDoubleRWVLastMapped)
				integer.Put(derivedio.US(tagRWVFirstMapped, first))
				integer.Put(derivedio.US(tagRWVLastMapped, last))
				return integer.ToDataSet()
			}
			frame := func(x float64, mappings ...core.DataSet) core.DataSet {
				return derivedio.DataSet(
					derivedio.Seq(testPlanePositionSeq, derivedio.DataSet(derivedio.DS(testImagePosition, x, 10, -2))),
					derivedio.Seq(tagRealWorldValueMappingSeq, mappings...),
				)
			}
			file.Dataset.Put(derivedio.Seq(tagPerFrameFunctionalGroups,
				frame(24, mapping("low", 1, 5, 5, 5), mapping("high", 8, 9, 4, 40)),
				frame(20, mapping("high", 7, 9, 4, 36), mapping("low", 1, 5, 2, 38)),
			))
			wire := rereadPart10(t, file)
			defer wire.Close()
			m, err := Read(wire)
			if err != nil {
				t.Fatal(err)
			}

			// The target spans -0.75..1.75 voxels on each axis in quarter-
			// voxel increments. Test the two half-open edges independently in
			// the sampling and reslice APIs, including reversed target axes.
			const n = 11
			for index, planeX := range []float64{24, 20} {
				for _, direction := range []struct {
					name           string
					startX, startY float64
					xSign, ySign   float64
				}{
					{"forward", -0.75, -0.75, 1, 1},
					{"reverse columns", 1.75, -0.75, -1, 1},
					{"reverse rows", -0.75, 1.75, 1, -1},
					{"reverse both", 1.75, 1.75, -1, -1},
				} {
					t.Run(direction.name, func(t *testing.T) {
						point := func(x, y float64) render.Vec3 {
							return render.Vec3{X: planeX, Y: 10 + 4*x, Z: -2 - 2*y}
						}
						target := render.SliceGeometry{
							Origin: point(direction.startX, direction.startY),
							RowDir: render.Vec3{Y: direction.xSign}, ColDir: render.Vec3{Z: -direction.ySign},
							Rows: n, Columns: n, RowSpacing: 0.5, ColSpacing: 1,
						}
						values, valid, err := m.Resample(target)
						if err != nil || len(values) != n*n || len(valid) != n*n {
							t.Fatalf("Resample frame %d returned %d values and %d mask entries, error %v", index, len(values), len(valid), err)
						}
						for row := 0; row < n; row++ {
							for column := 0; column < n; column++ {
								x := direction.startX + 0.25*float64(column)*direction.xSign
								y := direction.startY + 0.25*float64(row)*direction.ySign
								want, measured := parametricGridOracle(index, x, y)
								i := row*n + column
								if valid[i] != measured || measured && values[i] != want {
									t.Fatalf("frame %d Resample(%g,%g) = %g, %t; want %g, %t", index, x, y, values[i], valid[i], want, measured)
								}
								got, ok, err := m.SamplePatient(point(x, y))
								if err != nil || ok != measured || measured && got != want {
									t.Fatalf("frame %d SamplePatient(%g,%g) = %g, %t, %v; want %g, %t", index, x, y, got, ok, err, want, measured)
								}
							}
						}
					})
				}
			}
		})
	}
}

func parametricGridOracle(frame int, x, y float64) (float64, bool) {
	if x < -0.5 || x >= 1.5 || y < -0.5 || y >= 1.5 {
		return 0, false
	}
	x, y = math.Max(0, math.Min(x, 1)), math.Max(0, math.Min(y, 1))
	if frame == 0 {
		if x > 0 && y > 0 {
			return 0, false
		}
		return 10 + 10*x + 20*y, true
	}
	// The four measured corners are 40, 44, 48 and 64.
	return 40 + 4*x + 8*y + 12*x*y, true
}
