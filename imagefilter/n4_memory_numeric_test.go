package imagefilter

import (
	"context"
	"math"
	"reflect"
	"testing"
)

// Independent polynomial tensor-product oracle; it does not call cubicSpan,
// weightsFor, or the separable production evaluation to construct expectations.
func n41505Oracle(l *bsplineLattice, axes [3]axisSpline, x, y, z int) float64 {
	var start [3]int
	var weight [3][4]float64
	for a, i := range [3]int{x, y, z} {
		spans := float64(l.n[a] - 3)
		q := axes[a].positions[i] * spans
		epsilon := spans * axes[a].step * 0.001
		if math.Abs(q-spans) <= epsilon {
			q = spans - epsilon
		}
		q = math.Max(0, math.Min(math.Nextafter(spans, 0), q))
		start[a] = int(math.Floor(q))
		t := q - float64(start[a])
		weight[a] = [4]float64{(1 - 3*t + 3*t*t - t*t*t) / 6, (4 - 6*t*t + 3*t*t*t) / 6, (1 + 3*t + 3*t*t - 3*t*t*t) / 6, t * t * t / 6}
	}
	out := 0.0
	for c := 0; c < 4; c++ {
		for b := 0; b < 4; b++ {
			for a := 0; a < 4; a++ {
				at := ((start[2]+c)*l.n[1]+start[1]+b)*l.n[0] + start[0] + a
				out += weight[0][a] * weight[1][b] * weight[2][c] * l.phi[at]
			}
		}
	}
	return out
}

func TestN41505IndependentSplineOracle(t *testing.T) {
	for _, dims := range [][3]int{{11, 9, 7}, {11, 9, 1}, {1, 9, 3}} {
		g := &n4Grid{dims: dims}
		axes := g.axes()
		// Irregular parametric positions exercise a nonuniform output region.
		for a := range axes {
			for i := range axes[a].positions {
				axes[a].positions[i] = math.Pow(axes[a].positions[i], 1.3)
			}
		}
		l := &bsplineLattice{n: [3]int{7, 5, 4}, phi: make([]float64, 7*5*4)}
		for i := range l.phi {
			l.phi[i] = math.Sin(float64(i)*0.3) * 0.7
		}
		grid := l.evaluateGrid(axes, 2)
		samples := []int{0, len(grid) / 2, len(grid) - 1}
		at := l.evaluateAt(axes, samples, 2)
		for z := 0; z < dims[2]; z++ {
			for y := 0; y < dims[1]; y++ {
				for x := 0; x < dims[0]; x++ {
					i := (z*dims[1]+y)*dims[0] + x
					want := n41505Oracle(l, axes, x, y, z)
					if math.Abs(grid[i]-want) > 1e-12*math.Max(1, math.Abs(want)) {
						t.Fatalf("dims%v index%d got%.17g want%.17g", dims, i, grid[i], want)
					}
				}
			}
		}
		for k, i := range samples {
			if at[k] != grid[i] {
				t.Fatalf("sample%d: %.17g != %.17g", i, at[k], grid[i])
			}
		}
	}
}

func TestN41505BothOutputsConstantAndGeometry(t *testing.T) {
	for _, dims := range [][3]int{{17, 13, 9}, {17, 13, 1}, {17, 13, 3}} {
		in := NewVolume(dims, [3]float64{0.7, 1.8, 4.2})
		for i := range in.Data {
			in.Data[i] = 123.25
		}
		original := append([]float32(nil), in.Data...)
		mask := make([]bool, len(in.Data))
		for i := range mask {
			mask[i] = i%11 == 0
		}
		originalMask := append([]bool(nil), mask...)
		result, err := N4BiasFieldCorrection(context.Background(), in, N4Options{Mask: mask, ShrinkFactor: 1, Iterations: []int{2, 2}, Workers: 2})
		if err != nil {
			t.Fatal(err)
		}
		if result.Corrected == nil || result.LogBiasField == nil || result.Corrected == in || result.Corrected == result.LogBiasField {
			t.Fatal("outputs missing/aliased")
		}
		if result.Corrected.Dims != dims || result.LogBiasField.Dims != dims || result.Corrected.SpacingMM != in.SpacingMM || result.LogBiasField.SpacingMM != in.SpacingMM {
			t.Fatal("geometry changed")
		}
		if !reflect.DeepEqual(in.Data, original) || !reflect.DeepEqual(mask, originalMask) {
			t.Fatal("input or mask changed")
		}
		for i := range original {
			if result.Corrected.Data[i] != original[i] || math.Abs(float64(result.LogBiasField.Data[i])) > 1e-12 {
				t.Fatalf("constant voxel%d: %.9g, %.9g", i, result.Corrected.Data[i], result.LogBiasField.Data[i])
			}
		}
	}
}
