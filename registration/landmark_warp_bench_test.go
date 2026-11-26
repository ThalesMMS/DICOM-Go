package registration

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"testing"
)

func BenchmarkLandmarkWarpCost(b *testing.B) {
	for _, n := range []int{16, 32, 64} {
		fixed := make([]render.Vec3, n)
		for i := range fixed {
			fixed[i] = render.Vec3{X: float64(i % 4), Y: float64((i / 4) % 4), Z: float64(i/16) + .1*math.Sin(float64(i))}
		}
		moving := warpMoving(fixed, .4)
		ctx := context.Background()
		b.Run(fmt.Sprintf("fit-%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{Regularization: .1}); e != nil {
					b.Fatal(e)
				}
			}
		})
		fit, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{Regularization: .1})
		if e != nil {
			b.Fatal(e)
		}
		p := render.Vec3{X: .123, Y: .456, Z: .789}
		b.Run(fmt.Sprintf("query-%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, e := fit.Warp.PointContext(ctx, p); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
