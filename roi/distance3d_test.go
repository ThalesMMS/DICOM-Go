package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestFillHolesCanceledFullMask(t *testing.T) {
	mask := NewVolumeMask(2, 2, 2, [3]float64{1, 1, 1})
	for i := range mask.Voxels {
		mask.Voxels[i] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := mask.FillHoles(ctx)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("FillHoles = (%v, %v), want nil and context.Canceled", got, err)
	}
	if mask.Count() != 8 {
		t.Fatalf("source mask changed on cancel: %d voxels", mask.Count())
	}
}

// sphereMask sets the voxels whose centre lies within radius (mm) of centre.
func sphereMask(m *VolumeMask, centre [3]float64, radius float64) {
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				dx, dy, dz := m.X[x]-centre[0], m.Y[y]-centre[1], m.Z[z]-centre[2]
				m.Voxels[m.index(x, y, z)] = dx*dx+dy*dy+dz*dz <= radius*radius
			}
		}
	}
}

// TestDistanceTransformIsExact compares the transform with brute force on
// random masks over anisotropic and irregular grids.
func TestDistanceTransformIsExact(t *testing.T) {
	random := rand.New(rand.NewSource(1017))
	for trial := 0; trial < 20; trial++ {
		m := NewVolumeMask(7+trial%3, 6, 5+trial%4, [3]float64{0.7, 1.3, 2.5})
		if trial%2 == 1 {
			for z := 1; z < m.Slices; z++ {
				m.Z[z] = m.Z[z-1] + 0.5 + random.Float64()*3
			}
		}
		for i := range m.Voxels {
			m.Voxels[i] = random.Float64() < 0.04
		}
		d, err := m.DistanceTransform(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		for z := 0; z < m.Slices; z++ {
			for y := 0; y < m.Rows; y++ {
				for x := 0; x < m.Columns; x++ {
					want := math.Inf(1)
					for zz := 0; zz < m.Slices; zz++ {
						for yy := 0; yy < m.Rows; yy++ {
							for xx := 0; xx < m.Columns; xx++ {
								if m.Voxels[m.index(xx, yy, zz)] {
									dx, dy, dz := m.X[x]-m.X[xx], m.Y[y]-m.Y[yy], m.Z[z]-m.Z[zz]
									want = math.Min(want, math.Sqrt(dx*dx+dy*dy+dz*dz))
								}
							}
						}
					}
					if got := d[m.index(x, y, z)]; !(got == want || math.Abs(got-want) < 1e-9) {
						t.Fatalf("trial %d voxel (%d,%d,%d): %v, brute force %v", trial, x, y, z, got, want)
					}
				}
			}
		}
	}
}

// TestSphereMarginsAreSubVoxel expands and shrinks an analytic sphere on an
// anisotropic grid: the result is the sphere of the grown (shrunk) radius,
// sampled at the voxel centres, within one voxel diagonal of discretisation.
func TestSphereMarginsAreSubVoxel(t *testing.T) {
	spacing := [3]float64{0.8, 0.6, 1.5}
	m := NewVolumeMask(48, 60, 28, spacing)
	centre := [3]float64{19.1, 17.9, 20.3}
	sphereMask(m, centre, 8)
	for _, c := range []struct {
		margin float64
		expand bool
	}{{5, true}, {2.5, true}, {3, false}} {
		var got *VolumeMask
		var err error
		radius := 8 + c.margin
		if c.expand {
			got, err = m.Expand(context.Background(), c.margin)
		} else {
			got, err = m.Shrink(context.Background(), c.margin)
			radius = 8 - c.margin
		}
		if err != nil {
			t.Fatal(err)
		}
		// A voxel differs from the analytic sphere of the new radius only
		// within the discretisation of the original surface (half a voxel
		// diagonal each way).
		band := 0.5 * math.Sqrt(spacing[0]*spacing[0]+spacing[1]*spacing[1]+spacing[2]*spacing[2])
		mismatches := 0
		for z := 0; z < m.Slices; z++ {
			for y := 0; y < m.Rows; y++ {
				for x := 0; x < m.Columns; x++ {
					dx, dy, dz := m.X[x]-centre[0], m.Y[y]-centre[1], m.Z[z]-centre[2]
					r := math.Sqrt(dx*dx + dy*dy + dz*dz)
					want := r <= radius
					if got.Voxels[m.index(x, y, z)] != want {
						if math.Abs(r-radius) > band {
							t.Fatalf("margin %v expand %v: voxel at %.3f mm from centre is %v", c.margin, c.expand, r, !want)
						}
						mismatches++
					}
				}
			}
		}
		t.Logf("margin %v (expand %v): %d boundary voxels differ within %.2f mm", c.margin, c.expand, mismatches, band)
	}
}

func TestOpenCloseAndFillHoles(t *testing.T) {
	m := NewVolumeMask(30, 30, 20, [3]float64{1, 1, 1})
	// Two spheres joined by a one-voxel bridge, and a hollow cube.
	sphereMask(m, [3]float64{7, 7, 10}, 4)
	other := m.Clone()
	sphereMask(other, [3]float64{19, 7, 10}, 4)
	for i, v := range other.Voxels {
		m.Voxels[i] = m.Voxels[i] || v
	}
	for x := 7; x <= 19; x++ {
		m.Voxels[m.index(x, 7, 10)] = true
	}
	opened, err := m.Open(context.Background(), 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Voxels[opened.index(13, 7, 10)] || !opened.Voxels[opened.index(7, 7, 10)] {
		t.Error("opening kept the bridge or lost a sphere")
	}
	gap := NewVolumeMask(30, 10, 10, [3]float64{1, 1, 1})
	for z := 2; z < 8; z++ {
		for y := 2; y < 8; y++ {
			for x := 2; x < 28; x++ {
				gap.Voxels[gap.index(x, y, z)] = x != 15
			}
		}
	}
	closed, err := gap.Close(context.Background(), 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Voxels[closed.index(15, 5, 5)] || closed.Count() > gap.Count()+60 {
		t.Errorf("closing: gap filled %v, count %d from %d", closed.Voxels[closed.index(15, 5, 5)], closed.Count(), gap.Count())
	}
	hollow := NewVolumeMask(12, 12, 12, [3]float64{1, 1, 1})
	for z := 2; z < 10; z++ {
		for y := 2; y < 10; y++ {
			for x := 2; x < 10; x++ {
				shell := x == 2 || x == 9 || y == 2 || y == 9 || z == 2 || z == 9
				hollow.Voxels[hollow.index(x, y, z)] = shell
			}
		}
	}
	filled, err := hollow.FillHoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filled.Count() != 8*8*8 {
		t.Errorf("filled %d voxels, want 512", filled.Count())
	}
	hollow.Voxels[hollow.index(9, 5, 5)] = false // a hole in the shell: not enclosed
	open, _ := hollow.FillHoles(context.Background())
	if open.Count() != hollow.Count() {
		t.Errorf("an open cavity was filled: %d from %d", open.Count(), hollow.Count())
	}
	if _, err := (&VolumeMask{Columns: 2, Rows: 2, Slices: 1}).DistanceTransform(context.Background(), true); err == nil {
		t.Error("inconsistent mask accepted")
	}
	if _, err := m.Expand(context.Background(), -1); err == nil {
		t.Error("negative margin accepted")
	}
}

func TestSegmentationVolumeMaskRoundTrip(t *testing.T) {
	geometry := render.VolumeGeometry{
		Slices:    []render.SliceGeometry{{ColSpacing: 0.5, RowSpacing: 0.75}, {}, {}},
		Positions: []float64{0, 1, 3},
	}
	seg := NewSegmentation3D(geometry, 6, 5)
	seg.SetVoxel(2, 3, 1, true)
	m, err := seg.VolumeMask()
	if err != nil {
		t.Fatal(err)
	}
	if m.X[1] != 0.5 || m.Y[1] != 0.75 || m.Z[2] != 3 || m.Count() != 1 {
		t.Fatalf("mask axes %v %v %v count %d", m.X[:2], m.Y[:2], m.Z, m.Count())
	}
	grown, err := m.Expand(context.Background(), 2.1)
	if err != nil {
		t.Fatal(err)
	}
	if err := seg.SetVolumeMask(grown); err != nil {
		t.Fatal(err)
	}
	// Slice 2 lies 2 mm away (z 1 -> 3): one column over (0.5 mm) is 2.06 mm
	// away, inside the 2.1 mm margin; two columns (1 mm) are 2.24 mm away.
	if !seg.Voxel(2, 3, 2) || !seg.Voxel(3, 3, 2) || seg.Voxel(4, 3, 2) || !seg.Voxel(2, 3, 0) || seg.VoxelCount() != grown.Count() {
		t.Errorf("round trip: %v %v %v %v count %d/%d", seg.Voxel(2, 3, 2), seg.Voxel(3, 3, 2), seg.Voxel(4, 3, 2), seg.Voxel(2, 3, 0), seg.VoxelCount(), grown.Count())
	}
}

// BenchmarkDistanceTransformCT times the transform on a 256×256×128 CT-sized
// grid (0.7×0.7×2 mm) with a sphere.
func BenchmarkDistanceTransformCT(b *testing.B) {
	m := NewVolumeMask(256, 256, 128, [3]float64{0.7, 0.7, 2})
	sphereMask(m, [3]float64{90, 90, 128}, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Expand(context.Background(), 5); err != nil {
			b.Fatal(err)
		}
	}
}
