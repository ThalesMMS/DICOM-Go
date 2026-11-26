package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func medialMask(n int, spacing [3]float64, inside func(x, y, z float64) bool) *VolumeMask {
	m := NewVolumeMask(n, n, n, spacing)
	for z := 1; z < n-1; z++ {
		for y := 1; y < n-1; y++ {
			for x := 1; x < n-1; x++ {
				m.Voxels[m.index(x, y, z)] = inside(float64(x-n/2)*spacing[0], float64(y-n/2)*spacing[1], float64(z-n/2)*spacing[2])
			}
		}
	}
	return m
}

func TestMedialThicknessGeometryBiasAndOwnership(t *testing.T) {
	for _, spacing := range [][3]float64{{1, 1, 1}, {.5, .5, .5}, {.5, .8, 1.2}} {
		for _, shape := range []string{"tube", "sphere", "slab", "branch", "cavity"} {
			t.Run(shape+fmtSpacing(spacing), func(t *testing.T) {
				m := medialMask(31, spacing, func(x, y, z float64) bool {
					switch shape {
					case "tube":
						return x*x+y*y <= 9 && math.Abs(z) <= 4
					case "sphere":
						return x*x+y*y+z*z <= 9
					case "slab":
						return math.Abs(z) <= 1.2 && math.Abs(x) <= 4 && math.Abs(y) <= 4
					case "branch":
						return (x*x+y*y <= 2.25 && math.Abs(z) <= 4) || (math.Pow(x-z/2, 2)+y*y <= 2.25 && z >= 0 && z <= 4)
					default:
						q := x*x + y*y + z*z
						return q <= 16 && q >= 4
					}
				})
				before := append([]bool(nil), m.Voxels...)
				r, err := m.MedialThickness(context.Background(), MedialThicknessOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, m.Voxels) || len(r.Components) != 1 || r.Distribution.Count == 0 {
					t.Fatal("input changed, component lost, or no samples", r.Distribution)
				}
				var peak float64
				var geometryCount int
				var biasSum, maxGeometryError float64
				diagonalBound := 2 * math.Sqrt(spacing[0]*spacing[0]+spacing[1]*spacing[1]+spacing[2]*spacing[2])
				for _, s := range r.Samples {
					if !before[s.Index] || !r.Skeleton.Voxels[s.Index] || s.ThicknessMM < 0 {
						t.Fatal("sample outside skeleton")
					}
					peak = max(peak, s.ThicknessMM)
					x := float64(s.Index%m.Columns-m.Columns/2) * spacing[0]
					y := float64(s.Index/m.Columns%m.Rows-m.Rows/2) * spacing[1]
					z := float64(s.Index/(m.Columns*m.Rows)-m.Slices/2) * spacing[2]
					var surfaceDistance float64
					known := true
					switch shape {
					case "tube":
						surfaceDistance = min(3-math.Hypot(x, y), 4-math.Abs(z))
					case "sphere":
						surfaceDistance = 3 - math.Sqrt(x*x+y*y+z*z)
					case "slab":
						surfaceDistance = min(1.2-math.Abs(z), 4-math.Abs(x), 4-math.Abs(y))
					case "branch":
						// The negative-z trunk has a known circular cross-section
						// of radius 1.5 mm, with its bottom end cap also included.
						// The positive-z union at the junction has no single diameter.
						known = z < 0
						surfaceDistance = min(1.5-math.Hypot(x, y), 4-math.Abs(z))
					default:
						known = false
					}
					if known {
						bias := s.ThicknessMM - 2*surfaceDistance
						geometryCount++
						biasSum += bias
						maxGeometryError = max(maxGeometryError, math.Abs(bias))
					}
				}
				if shape != "cavity" {
					if geometryCount == 0 || maxGeometryError > diagonalBound+1e-9 {
						t.Fatal("known geometry is absent or exceeds two voxel diagonals", geometryCount, maxGeometryError, diagonalBound)
					}
					t.Logf("continuous-surface comparison: n=%d; mean signed bias %.9g mm; max absolute error %.9g mm; two-diagonal bound %.9g mm", geometryCount, biasSum/float64(geometryCount), maxGeometryError, diagonalBound)
				}
				if shape == "tube" || shape == "sphere" {
					bound := diagonalBound
					if peak > 6+1e-9 || peak < 6-bound-1e-9 {
						t.Fatal("discrete geometry bias exceeds one voxel diagonal", peak, bound)
					}
					t.Logf("known diameter 6 mm; peak %.9g mm; signed bias %.9g mm", peak, peak-6)
				}
				original := r.Skeleton.X[0]
				m.X[0] -= 2
				if r.Skeleton.X[0] != original {
					t.Fatal("borrowed output axis")
				}
			})
		}
	}
}

func fmtSpacing(s [3]float64) string { return fmt.Sprintf("-%g-%g-%g", s[0], s[1], s[2]) }

func TestMedialThicknessReservationBeforeAllocation(t *testing.T) {
	m := NewVolumeMask(9, 9, 9, [3]float64{1, 1, 1})
	m.Voxels[m.index(4, 4, 4)] = true
	reserved, err := MedialThicknessReservation(9, 9, 9, MedialThicknessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.MedialThickness(context.Background(), MedialThicknessOptions{MaxWorkingBytes: reserved})
	if err != nil || r.ReservedBytes != reserved {
		t.Fatal("reservation differs from enforced workspace", r, err)
	}
	if _, err := MedialThicknessReservation(9, 9, 9, MedialThicknessOptions{MaxWorkingBytes: reserved - 1}); !errors.Is(err, ErrMedialThicknessLimit) {
		t.Fatal("admitted insufficient workspace", err)
	}
	if _, err := MedialThicknessReservation(math.MaxInt, math.MaxInt, math.MaxInt, MedialThicknessOptions{}); !errors.Is(err, ErrMedialThicknessLimit) {
		t.Fatal("dimension overflow admitted", err)
	}
}

func TestMedialThicknessUndefinedZeroEmptyAndComponents(t *testing.T) {
	m := NewVolumeMask(9, 9, 9, [3]float64{.5, 1, 2})
	m.Voxels[m.index(2, 2, 2)] = true
	m.Voxels[m.index(3, 3, 3)] = true // 26-connected diagonal.
	m.Voxels[m.index(6, 6, 6)] = true
	r, err := m.MedialThickness(context.Background(), MedialThicknessOptions{})
	if err != nil || len(r.Components) != 2 || r.Components[0].ForegroundVoxels != 2 || r.Components[1].ForegroundVoxels != 1 {
		t.Fatal(r, err)
	}
	if value, valid := r.At(6, 6, 6); !valid || value != 0 {
		t.Fatal("boundary zero was lost", value, valid)
	}
	for _, point := range [][3]int{{0, 0, 0}, {-1, 0, 0}, {9, 9, 9}} {
		if v, ok := r.At(point[0], point[1], point[2]); ok || !math.IsNaN(v) {
			t.Fatal("undefined value was filled with zero", v, ok)
		}
	}
	for _, c := range r.Components {
		count := 0
		for _, v := range c.Distribution.Histogram {
			count += v
		}
		if count != c.Distribution.Count {
			t.Fatal("histogram count")
		}
	}
	for i := range m.Voxels {
		m.Voxels[i] = false
	}
	empty, err := m.MedialThickness(context.Background(), MedialThicknessOptions{})
	if err != nil || len(empty.Samples) != 0 || len(empty.Components) != 0 || !math.IsNaN(empty.Distribution.MeanMM) {
		t.Fatal(empty, err)
	}
}

func TestMedialThicknessRefusalLimitsAndCancellation(t *testing.T) {
	m := NewVolumeMask(9, 9, 9, [3]float64{1, 1, 1})
	m.Voxels[m.index(2, 2, 2)] = true
	m.Voxels[m.index(6, 6, 6)] = true
	for _, o := range []MedialThicknessOptions{{MaxVoxels: 728}, {MaxWorkingBytes: 1}, {MaxComponents: 1}, {MaxVoxels: MedialThicknessMaxVoxels + 1}, {MaxComponents: -1}, {MaxWorkingBytes: MedialThicknessMaxBytes + 1}} {
		if r, err := m.MedialThickness(context.Background(), o); r != nil || !errors.Is(err, ErrMedialThicknessLimit) {
			t.Fatal("unbounded or partial result", r, err)
		}
	}
	for _, mutate := range []func(*VolumeMask){func(m *VolumeMask) { m.Z[3] += .1 }, func(m *VolumeMask) { m.Voxels[0] = true }} {
		bad := m.Clone()
		mutate(bad)
		if r, e := bad.MedialThickness(context.Background(), MedialThicknessOptions{}); r != nil || !errors.Is(e, ErrMedialThicknessDomain) {
			t.Fatal(r, e)
		}
	}
	bad := m.Clone()
	bad.X[0] = math.NaN()
	if r, e := bad.MedialThickness(context.Background(), MedialThicknessOptions{}); r != nil || !errors.Is(e, ErrInvalidVolumeMask) {
		t.Fatal(r, e)
	}
	for _, bad := range []*VolumeMask{nil, {Columns: math.MaxInt, Rows: math.MaxInt, Slices: math.MaxInt}} {
		if r, e := bad.MedialThickness(context.Background(), MedialThicknessOptions{}); r != nil || e == nil {
			t.Fatal("overflow input admitted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := m.MedialThickness(ctx, MedialThicknessOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(r, e)
	}
	large := medialMask(49, [3]float64{1, 1, 1}, func(x, y, z float64) bool { return x*x+y*y+z*z <= 400 })
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(5*time.Millisecond, cancel)
	defer timer.Stop()
	if r, e := large.MedialThickness(ctx, MedialThicknessOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("cancel published a result", r, e)
	}
}
