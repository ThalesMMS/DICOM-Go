package mesh

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

// This is an experiment, deliberately confined to tests. Analytically supplied
// normals are additional information that an unordered point cloud lacks.
// Nearest tangent-plane signs cannot establish orientation, closure or topology.
func cloudSphere(n int) *PointCloud {
	c := &PointCloud{Units: "mm"}
	golden := math.Pi * (3 - math.Sqrt(5))
	for i := 0; i < n; i++ {
		z := 1 - 2*(float64(i)+0.5)/float64(n)
		r := math.Sqrt(1 - z*z)
		a := golden * float64(i)
		normal := [3]float32{float32(r * math.Cos(a)), float32(r * math.Sin(a)), float32(z)}
		c.Normals = append(c.Normals, normal)
		c.Points = append(c.Points, [3]float32{10 * normal[0], 10 * normal[1], 10 * normal[2]})
	}
	return c
}

type cloudExperiment struct {
	mesh                     *Mesh
	work, gridBytes          int64
	maxSampledSurfaceErrorMM float64
	maxSourceCoverageMM      float64
	top                      topology
	elapsed                  time.Duration
}

func cloudReconstruction(c, original *PointCloud, spacing float64) (*cloudExperiment, error) {
	start := time.Now()
	o, _ := pointCloudOptions(PointCloudOptions{MaxPoints: 10_000, MaxBytes: 64 << 20, MaxWork: 50_000_000})
	run := pointCloudRun{context.Background(), &PointCloudReport{Options: o}}
	// Predeclared experiment bounds: <=150,000 grid samples, 64 MiB logical
	// payload, 50,000,000 charged visits/comparisons; extractor <=8 MiB output.
	side := int(math.Round(24/spacing)) + 1
	cells := int64(side) * int64(side) * int64(side)
	gridBytes := 4 * cells
	if len(c.Points) > 10_000 || cells > 150_000 || gridBytes+int64(len(c.Points))*20+16 > 64<<20 {
		return nil, fmt.Errorf("experiment admission exceeded")
	}
	tree, err := buildPointCloudTree(c.Points, &run)
	if err != nil {
		return nil, err
	}
	field := &DenseField{
		Size: [3]int{side, side, side},
		Affine: GridAffine(render.Vec3{X: -12, Y: -12, Z: -12}, IdentityAxes,
			[3]float64{spacing, spacing, spacing}),
		Data: make([]float32, int(cells)),
	}
	heap := make([]pointCloudNeighbor, 0, 1)
	for z := 0; z < side; z++ {
		for y := 0; y < side; y++ {
			for x := 0; x < side; x++ {
				q := [3]float64{-12 + float64(x)*spacing, -12 + float64(y)*spacing, -12 + float64(z)*spacing}
				heap, err = tree.nearest(q, -1, 1, heap[:0], &run)
				if err != nil {
					return nil, err
				}
				id := heap[0].id
				p, normal := c.Points[id], c.Normals[id]
				// Positive is inside, matching Extract's convention.
				v := 0.0
				for a := 0; a < 3; a++ {
					v += (float64(p[a]) - q[a]) * float64(normal[a])
				}
				field.Data[(z*side+y)*side+x] = float32(v)
			}
		}
	}
	m, err := Extract(context.Background(), field, Options{OpenBoundary: true, SkipNormals: true,
		Workers: 1, MaxBytes: 8 << 20, MaxTriangles: 200_000})
	if err != nil {
		return nil, err
	}
	r := &cloudExperiment{mesh: m, work: run.r.Work, gridBytes: gridBytes, top: meshTopology(m)}
	// These are sampled errors (vertices and each triangle centroid), not an
	// exact Hausdorff bound. An analytic sphere supplies an independent target.
	measure := func(p [3]float64) {
		e := math.Abs(math.Sqrt(p[0]*p[0]+p[1]*p[1]+p[2]*p[2]) - 10)
		r.maxSampledSurfaceErrorMM = math.Max(r.maxSampledSurfaceErrorMM, e)
	}
	for _, p := range m.Vertices {
		measure(pointCloud64(p))
	}
	for _, f := range m.Triangles {
		p := [3]float64{}
		for _, id := range f {
			for a := 0; a < 3; a++ {
				p[a] += float64(m.Vertices[id][a]) / 3
			}
		}
		measure(p)
	}
	for _, p := range original.Points {
		heap, err = tree.nearest(pointCloud64(p), -1, 1, heap[:0], &run)
		if err != nil {
			return nil, err
		}
		r.maxSourceCoverageMM = math.Max(r.maxSourceCoverageMM, math.Sqrt(heap[0].distance2))
	}
	r.work = run.r.Work
	r.elapsed = time.Since(start)
	return r, nil
}

func TestPointCloudReconstructionExperiment(t *testing.T) {
	original := cloudSphere(2048)
	reduced := cloudProcess(t, original, PointCloudOptions{MinDistanceMM: 0.9, Seed: 73}).Cloud
	for _, spacing := range []float64{1, 0.5} {
		for _, item := range []struct {
			name string
			c    *PointCloud
		}{{"original", original}, {"reduced", reduced}} {
			t.Run(fmt.Sprintf("%s_h%g", item.name, spacing), func(t *testing.T) {
				r, err := cloudReconstruction(item.c, original, spacing)
				if err != nil {
					t.Fatal(err)
				}
				// Predeclared sphere-specific physical/topological gate. Passing
				// this ideal case does not promote arbitrary-cloud reconstruction.
				if r.maxSampledSurfaceErrorMM > 0.5 || r.top.euler() != 2 || r.top.nonManifold != 0 || r.top.unpaired != 0 {
					t.Fatalf("ideal sphere gate failed: %+v", r)
				}
				t.Logf("points=%d h_mm=%g grid_bytes=%d mesh_bytes=%d vertices=%d triangles=%d max_sampled_error_mm=%.6f coverage_mm=%.6f euler=%d defective_edges=%d unpaired=%d work=%d elapsed=%s",
					len(item.c.Points), spacing, r.gridBytes, r.mesh.Bytes(), len(r.mesh.Vertices), len(r.mesh.Triangles),
					r.maxSampledSurfaceErrorMM, r.maxSourceCoverageMM, r.top.euler(), r.top.nonManifold, r.top.unpaired, r.work, r.elapsed)
			})
		}
	}
	flipped := cloudProcess(t, original, PointCloudOptions{}).Cloud
	for i, p := range flipped.Points {
		if p[2] > 0 {
			for a := 0; a < 3; a++ {
				flipped.Normals[i][a] = -flipped.Normals[i][a]
			}
		}
	}
	missingCap := &PointCloud{Units: "mm"}
	for i, p := range original.Points {
		if p[2] <= 6 {
			missingCap.Points = append(missingCap.Points, p)
			missingCap.Normals = append(missingCap.Normals, original.Normals[i])
		}
	}
	for _, item := range []struct {
		name string
		c    *PointCloud
	}{{"ambiguous_normals", flipped}, {"missing_cap", missingCap}} {
		t.Run(item.name, func(t *testing.T) {
			r, err := cloudReconstruction(item.c, original, 1)
			if err != nil {
				t.Fatal(err)
			}
			if r.maxSampledSurfaceErrorMM <= 0.5 && r.top.euler() == 2 && r.top.nonManifold == 0 && r.top.unpaired == 0 {
				t.Fatal("known ambiguity passed promotion gate unexpectedly")
			}
			t.Logf("NOT PROMOTED: points=%d max_sampled_error_mm=%.6f coverage_mm=%.6f euler=%d defective_edges=%d unpaired=%d work=%d elapsed=%s",
				len(item.c.Points), r.maxSampledSurfaceErrorMM, r.maxSourceCoverageMM, r.top.euler(), r.top.nonManifold, r.top.unpaired, r.work, r.elapsed)
		})
	}
}

func BenchmarkPointCloudPipeline(b *testing.B) {
	c := cloudSphere(10_000)
	c.Points = append(c.Points, [3]float32{100, 100, 100})
	c.Normals = append(c.Normals, [3]float32{0, 0, 1})
	for _, item := range []struct {
		name string
		o    PointCloudOptions
	}{
		{"original", PointCloudOptions{}},
		{"clean", PointCloudOptions{Neighbors: 8, SigmaFactor: 2, RemoveOutliers: true}},
		{"reduced", PointCloudOptions{Neighbors: 8, SigmaFactor: 2, RemoveOutliers: true, MinDistanceMM: 0.4, Seed: 73}},
	} {
		b.Run(item.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r, err := ProcessPointCloud(context.Background(), c, item.o)
				if err != nil || !r.Report.Complete {
					b.Fatal(err)
				}
				b.ReportMetric(float64(r.Report.ReservedBytes), "reserved-B/op")
				b.ReportMetric(float64(r.Report.OutputPoints), "points/op")
				b.ReportMetric(float64(r.Report.OutliersRemoved), "outliers/op")
				b.ReportMetric(float64(r.Report.Work), "work/op")
			}
		})
	}
}

func BenchmarkPointCloudReconstructionExperiment(b *testing.B) {
	original := cloudSphere(2048)
	reduced, err := ProcessPointCloud(context.Background(), original, PointCloudOptions{MinDistanceMM: 0.9, Seed: 73})
	if err != nil {
		b.Fatal(err)
	}
	for _, item := range []struct {
		name string
		c    *PointCloud
	}{{"original", original}, {"reduced", reduced.Cloud}} {
		b.Run(item.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := cloudReconstruction(item.c, original, 0.5); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
