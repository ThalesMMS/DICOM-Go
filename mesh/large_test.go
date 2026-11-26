package mesh

import (
	"context"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

// ctBodyField is an analytic 512×512×600 CT-sized field (0.7 × 0.7 × 0.8 mm):
// a body ellipsoid with a hollow shell, so the surface has an outer and an
// inner sheet like a bone cortex. It is computed on the fly, so the test
// measures the extractor's memory, not the volume's.
func ctBodyField() *FuncField {
	dims := [3]int{512, 512, 600}
	spacing := [3]float64{0.7, 0.7, 0.8}
	affine := GridAffine(render.Vec3{X: -179, Y: -179, Z: -240}, IdentityAxes, spacing)
	return &FuncField{Size: dims, Affine: affine, Value: func(x, y, z int) float64 {
		p := affine.TransformPoint(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
		outer := (p.X*p.X)/(160*160) + (p.Y*p.Y)/(120*120) + (p.Z*p.Z)/(220*220)
		inner := (p.X*p.X)/(150*150) + (p.Y*p.Y)/(110*110) + (p.Z*p.Z)/(210*210)
		if outer <= 1 && inner >= 1 {
			return 700 // cortex
		}
		return -100
	}}
}

// TestLargeCTWithinDocumentedMemory extracts a 512×512×600 field and checks
// the documented bound: everything allocated (TotalAlloc, both passes) is at
// most the mesh itself plus, per worker, 72 bytes per padded slice sample
// (the counting and the generating scratch), plus the per-slice counters
// and a few MiB of slack. The field is computed on the fly, as a snapshot's
// slices are read from the store: nothing volume-sized is allocated.
func TestLargeCTWithinDocumentedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("512×512×600 extraction")
	}
	field := ctBodyField()
	const workers = 4
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	m, err := Extract(context.Background(), field, Options{Iso: 300, Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	allocated := int64(after.TotalAlloc - before.TotalAlloc)
	plane := int64(field.Size[0]+2) * int64(field.Size[1]+2)
	bound := m.Bytes() + workers*72*plane + 64*int64(field.Size[2]+2) + 8<<20
	t.Logf("%d triangles, %d vertices, mesh %d MiB, allocated %d MiB (bound %d MiB), %v",
		len(m.Triangles), len(m.Vertices), m.Bytes()>>20, allocated>>20, bound>>20, elapsed)
	if allocated > bound {
		t.Fatalf("allocated %d bytes, documented bound %d", allocated, bound)
	}
	if len(m.Triangles) == 0 {
		t.Fatal("empty surface")
	}
	// Closed oriented manifold: every directed edge is unique and its
	// reverse is present (sorted, as maps are slow at this size).
	edges := make([]uint64, 0, 3*len(m.Triangles))
	for _, tri := range m.Triangles {
		for i := 0; i < 3; i++ {
			edges = append(edges, uint64(tri[i])<<32|uint64(tri[(i+1)%3]))
		}
	}
	slices.Sort(edges)
	for i, e := range edges {
		if i > 0 && edges[i-1] == e {
			t.Fatalf("directed edge %x repeated", e)
		}
		if _, found := slices.BinarySearch(edges, e<<32|e>>32); !found {
			t.Fatalf("directed edge %x has no reverse", e)
		}
	}
}

// BenchmarkExtract measures both methods on a 128³ sphere held in memory.
func BenchmarkExtract(b *testing.B) {
	source := sphereField([3]int{128, 128, 128}, identityAffine, render.Vec3{X: 63.5, Y: 63.5, Z: 63.5}, 50)
	data := make([]float32, 128*128*128)
	for z := 0; z < 128; z++ {
		if err := source.ReadSlice(context.Background(), z, data[z*128*128:]); err != nil {
			b.Fatal(err)
		}
	}
	field := &DenseField{Size: source.Size, Affine: source.Affine, Data: data}
	for _, method := range []Method{MarchingCubes, VoxelFaces} {
		b.Run(method.String(), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Extract(context.Background(), field, Options{Method: method}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
