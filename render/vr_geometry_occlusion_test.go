package render

import (
	"context"
	"math"
	"testing"
)

// TestRasterizeVRGeometryAppliesOcclusion (#1237): per-vertex ambient
// accessibility scales the shaded colour, interpolated across a triangle.
func TestRasterizeVRGeometryAppliesOcclusion(t *testing.T) {
	cam := NewVRCamera(20)
	cam.Pitch = 0
	square := VRGeometryMesh{
		Vertices:  []Vec3{{X: -5, Y: -2, Z: -5}, {X: 5, Y: -2, Z: -5}, {X: 5, Y: -2, Z: 5}, {X: -5, Y: -2, Z: 5}},
		Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}},
		Color:     RGBA{R: 0.8, G: 0.8, B: 0.8},
	}
	const w, h = 32, 32
	raster := func(m VRGeometryMesh) *VRDepthImage {
		t.Helper()
		image, err := RasterizeVRGeometryContext(context.Background(), cam, w, h, &VRGeometry{Meshes: []VRGeometryMesh{m}})
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	lit := raster(square)
	half := square
	half.Occlusion = []float32{0.5, 0.5, 0.5, 0.5}
	dark := raster(half)
	ramp := square
	ramp.Occlusion = []float32{0, 1, 1, 0} // dark at X = -5, lit at X = +5
	graded := raster(ramp)
	mismatched := square
	mismatched.Occlusion = []float32{0}
	ignored := raster(mismatched)
	covered := 0
	type sample struct {
		x     int
		ratio float64
	}
	var samples []sample
	minX, maxX := w, -1
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			tLit, full, hit := lit.At(px, py)
			if !hit {
				continue
			}
			covered++
			tDark, shaded, _ := dark.At(px, py)
			if tDark != tLit || math.Abs(shaded.R-full.R/2) > 1e-6 {
				t.Fatalf("pixel %d,%d: %v at %v, want half of %v", px, py, shaded.R, tDark, full.R)
			}
			if _, same, _ := ignored.At(px, py); same != full {
				t.Fatalf("pixel %d,%d: a mismatched occlusion changed the colour", px, py)
			}
			_, g, _ := graded.At(px, py)
			if g.R > full.R+1e-6 {
				t.Fatalf("pixel %d,%d: occlusion brightened the colour", px, py)
			}
			samples = append(samples, sample{px, g.R / full.R})
			minX, maxX = min(minX, px), max(maxX, px)
		}
	}
	// The ramp runs along X; screen columns follow X one way or the other,
	// so compare the outer thirds of the square's columns.
	var low, high []float64
	for _, s := range samples {
		switch {
		case 3*(s.x-minX) < maxX-minX:
			low = append(low, s.ratio)
		case 3*(maxX-s.x) < maxX-minX:
			high = append(high, s.ratio)
		}
	}
	average := func(values []float64) float64 {
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	}
	if covered == 0 || len(low) == 0 || len(high) == 0 || math.Abs(average(low)-average(high)) < 0.3 {
		t.Fatalf("%d pixels; graded occlusion %.2f against %.2f", covered, average(low), average(high))
	}
}
