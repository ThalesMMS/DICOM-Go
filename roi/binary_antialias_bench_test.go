package roi_test

import (
	"context"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"testing"
)

func BenchmarkBinaryAntiAliasCost(b *testing.B) {
	size := [3]int{24, 20, 12}
	m := aa1542ExternalMask(size, [3]float64{.7, 1.1, 1.4}, "sphere")
	data := make([]float32, len(m.Voxels))
	for i, v := range m.Voxels {
		data[i] = -.35
		if v {
			data[i] = .35
		}
	}
	original := &mesh.DenseField{Size: size, Affine: mesh.GridAffine(render.Vec3{}, mesh.IdentityAxes, [3]float64{.7, 1.1, 1.4}), Data: data}
	for _, name := range []string{"OriginalMarchingCubes", "OriginalVoxelFaces", "AntiAliasOnly", "AntiAliasAndExtract"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				options := mesh.Options{Iso: 0, Workers: 1, SkipNormals: true, MaxTriangles: 100000, MaxBytes: 64 << 20}
				if name == "AntiAliasOnly" || name == "AntiAliasAndExtract" {
					r, e := roi.BinaryAntiAlias(nil, m, roi.BinaryAntiAliasOptions{MaxIterations: 10, Workers: 1, MaxBytes: 64 << 20})
					if e != nil {
						b.Fatal(e)
					}
					b.ReportMetric(float64(r.Report.Iterations), "iterations/op")
					if name == "AntiAliasAndExtract" {
						surface, e := mesh.Extract(context.Background(), r, options)
						if e != nil || len(surface.Triangles) == 0 {
							b.Fatal(surface, e)
						}
					}
				} else {
					if name == "OriginalVoxelFaces" {
						options.Method = mesh.VoxelFaces
					}
					surface, e := mesh.Extract(context.Background(), original, options)
					if e != nil || len(surface.Triangles) == 0 {
						b.Fatal(surface, e)
					}
				}
			}
		})
	}
}
