package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Independent definition oracle: threshold at every source intensity, BFS each
// binary level set, and reconstruct from retained components. It builds no
// union-find/component tree and does not use production neighbor/sort helpers.
func attributeLevelOracle(v *imagefilter.Volume, threshold, measure float64, connectivity int, closing bool) []float32 {
	out := make([]float32, len(v.Data))
	base := v.Data[0]
	for _, x := range v.Data {
		if (!closing && x < base) || (closing && x > base) {
			base = x
		}
	}
	for i := range out {
		out[i] = base
	}
	levels := map[float32]bool{}
	for _, level := range v.Data {
		levels[level] = true
	}
	for level := range levels {
		seen := make([]bool, len(v.Data))
		for seed, value := range v.Data {
			inside := value >= level
			if closing {
				inside = value <= level
			}
			if seen[seed] || !inside {
				continue
			}
			queue := []int{seed}
			seen[seed] = true
			for head := 0; head < len(queue); head++ {
				p := queue[head]
				px, py, pz := p%v.Dims[0], (p/v.Dims[0])%v.Dims[1], p/(v.Dims[0]*v.Dims[1])
				for dz := -1; dz <= 1; dz++ {
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							manhattan := absAttributeTest(dx) + absAttributeTest(dy) + absAttributeTest(dz)
							if manhattan == 0 || ((connectivity == 4 || connectivity == 8) && dz != 0) || ((connectivity == 4 || connectivity == 6) && manhattan != 1) || (connectivity == 18 && manhattan > 2) {
								continue
							}
							x, y, z := px+dx, py+dy, pz+dz
							if x < 0 || y < 0 || z < 0 || x >= v.Dims[0] || y >= v.Dims[1] || z >= v.Dims[2] {
								continue
							}
							q := (z*v.Dims[1]+y)*v.Dims[0] + x
							inside := v.Data[q] >= level
							if closing {
								inside = v.Data[q] <= level
							}
							if inside && !seen[q] {
								seen[q] = true
								queue = append(queue, q)
							}
						}
					}
				}
			}
			if float64(len(queue))*measure >= threshold {
				for _, p := range queue {
					if (!closing && level > out[p]) || (closing && level < out[p]) {
						out[p] = level
					}
				}
			}
		}
	}
	return out
}

func absAttributeTest(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestGrayscaleAttributeIndependentLevelsAndAlgebra(t *testing.T) {
	rng := rand.New(rand.NewSource(1511))
	for _, shape := range [][3]int{{5, 4, 1}, {4, 3, 3}, {1, 5, 1}, {2, 2, 1}} {
		conns := []int{4, 8}
		if shape[2] > 1 {
			conns = []int{6, 18, 26}
		}
		for _, conn := range conns {
			for run := 0; run < 12; run++ {
				v := imagefilter.NewVolume(shape, [3]float64{.7, 1.3, 2.5})
				for i := range v.Data {
					v.Data[i] = float32(rng.Intn(7)-3) / 4
				}
				before := append([]float32(nil), v.Data...)
				for _, threshold := range []float64{0, 1, 2.5, 6, float64(len(v.Data) + 1)} {
					o := GrayscaleAttributeOptions{Connectivity: conn}
					opened, err := GrayscaleAttributeOpen(nil, v, threshold, o)
					if err != nil {
						t.Fatal(err)
					}
					closed, err := GrayscaleAttributeClose(nil, v, threshold, o)
					if err != nil {
						t.Fatal(err)
					}
					for mode, got := range []*imagefilter.Volume{opened, closed} {
						want := attributeLevelOracle(v, threshold, 1, conn, mode == 1)
						if !reflect.DeepEqual(got.Data, want) {
							t.Fatalf("dims%v conn%d mode%d threshold%g: %v want%v", shape, conn, mode, threshold, got.Data, want)
						}
						if got.Dims != v.Dims || got.SpacingMM != v.SpacingMM {
							t.Fatal("geometry changed")
						}
					}
					again, _ := GrayscaleAttributeOpen(nil, opened, threshold, o)
					againClose, _ := GrayscaleAttributeClose(nil, closed, threshold, o)
					if !reflect.DeepEqual(again.Data, opened.Data) || !reflect.DeepEqual(againClose.Data, closed.Data) {
						t.Fatal("idempotence")
					}
					neg := v.Clone()
					for i := range neg.Data {
						neg.Data[i] = -neg.Data[i]
					}
					dual, _ := GrayscaleAttributeClose(nil, neg, threshold, o)
					for i, x := range v.Data {
						if opened.Data[i] > x || closed.Data[i] < x || opened.Data[i] != -dual.Data[i] {
							t.Fatal("anti/extensivity or duality")
						}
					}
					if !reflect.DeepEqual(v.Data, before) {
						t.Fatal("input changed")
					}
				}
			}
		}
	}
}

func TestGrayscaleAttributePlateausDiagonalsBordersAndPhysicalSize(t *testing.T) {
	// A corner contact preserves the diagonal pair only with8-connectivity;
	// the connected three-sample bent plateau survives regardless of shape.
	v := &imagefilter.Volume{Dims: [3]int{5, 3, 1}, SpacingMM: [3]float64{2, 3, 0}, Data: []float32{8, 0, 0, 5, 5, 0, 8, 0, 0, 5, 0, 0, 0, 0, 0}}
	four, err := GrayscaleAttributeOpen(nil, v, 2, GrayscaleAttributeOptions{Connectivity: 4})
	if err != nil {
		t.Fatal(err)
	}
	eight, err := GrayscaleAttributeOpen(nil, v, 2, GrayscaleAttributeOptions{Connectivity: 8})
	if err != nil {
		t.Fatal(err)
	}
	if four.Data[0] != 0 || four.Data[6] != 0 || eight.Data[0] != 8 || eight.Data[6] != 8 || four.Data[3] != 5 || four.Data[4] != 5 || four.Data[9] != 5 {
		t.Fatal("diagonal/shape/border")
	}
	physical, err := GrayscaleAttributeOpen(nil, v, 12, GrayscaleAttributeOptions{Connectivity: 8, Unit: AttributeSquareMM})
	if err != nil || !reflect.DeepEqual(physical.Data, eight.Data) {
		t.Fatal("mm2 area equivalent to samples", err)
	}
	// Native floats are not intensity-bin rounded and extreme values need no negation.
	floats := &imagefilter.Volume{Dims: [3]int{3, 1, 1}, SpacingMM: [3]float64{1, 0, 0}, Data: []float32{-math.MaxFloat32, math.SmallestNonzeroFloat32, math.MaxFloat32}}
	for _, closing := range []bool{false, true} {
		var got *imagefilter.Volume
		if closing {
			got, err = GrayscaleAttributeClose(nil, floats, 2, GrayscaleAttributeOptions{})
		} else {
			got, err = GrayscaleAttributeOpen(nil, floats, 2, GrayscaleAttributeOptions{})
		}
		if err != nil || !reflect.DeepEqual(got.Data, attributeLevelOracle(floats, 2, 1, 4, closing)) {
			t.Fatal("native float domain", err)
		}
	}
	thin := v.Clone()
	thin.SpacingMM[2] = 4
	volume, err := GrayscaleAttributeOpen(nil, thin, 48, GrayscaleAttributeOptions{Dimension: Attribute3D, Connectivity: 26, Unit: AttributeCubicMM})
	if err != nil || !reflect.DeepEqual(volume.Data, eight.Data) {
		t.Fatal("explicit single-slice3D mm3", err)
	}
	// 18-connectivity includes an edge contact; only26 includes a body diagonal.
	for _, conn := range []int{6, 18, 26} {
		cube := imagefilter.NewVolume([3]int{2, 2, 2}, [3]float64{1, 2, 3})
		cube.Data[0], cube.Data[7] = 9, 9
		got, err := GrayscaleAttributeOpen(nil, cube, 2, GrayscaleAttributeOptions{Connectivity: conn})
		if err != nil {
			t.Fatal(err)
		}
		want := float32(0)
		if conn == 26 {
			want = 9
		}
		if got.Data[0] != want || got.Data[7] != want {
			t.Fatal("3D corner connectivity")
		}
		cube.Data[7], cube.Data[3] = 0, 9
		got, _ = GrayscaleAttributeOpen(nil, cube, 2, GrayscaleAttributeOptions{Connectivity: conn})
		want = 9
		if conn == 6 {
			want = 0
		}
		if got.Data[0] != want {
			t.Fatal("3D edge connectivity")
		}
	}
}

type attributeCancelContext struct {
	context.Context
	calls, at int
}

func (c *attributeCancelContext) Err() error {
	c.calls++
	if c.calls >= c.at {
		return context.Canceled
	}
	return nil
}

func TestGrayscaleAttributeLimitsValidationAndCancellation(t *testing.T) {
	v := &imagefilter.Volume{Dims: [3]int{3, 2, 1}, SpacingMM: [3]float64{1, 1, 0}, Data: []float32{0, 1, 2, 3, 4, 5}}
	for _, threshold := range []float64{-1, math.NaN(), math.Inf(1)} {
		if got, err := GrayscaleAttributeOpen(nil, v, threshold, GrayscaleAttributeOptions{}); got != nil || !errors.Is(err, ErrInvalidGrayscaleAttribute) {
			t.Fatal("threshold validation")
		}
	}
	for _, o := range []GrayscaleAttributeOptions{{MaxBytes: -1}, {MaxVoxels: -1}, {Connectivity: 6}, {Dimension: 99}, {Unit: AttributeCubicMM}, {Unit: 99}} {
		if got, err := GrayscaleAttributeOpen(nil, v, 2, o); got != nil || !errors.Is(err, ErrInvalidGrayscaleAttribute) {
			t.Fatal("option validation", o, err)
		}
	}
	for _, o := range []GrayscaleAttributeOptions{{MaxBytes: attributeFixedBytes + attributeBytesPerSample*6 - 1}, {MaxVoxels: 5}} {
		if got, err := GrayscaleAttributeOpen(nil, v, 2, o); got != nil || !errors.Is(err, ErrGrayscaleAttributeLimit) {
			t.Fatal("preallocation admission", o, err)
		}
	}
	for _, bad := range []*imagefilter.Volume{nil, {}, {Dims: [3]int{2, 2, 1}, Data: make([]float32, 3)}, {Dims: [3]int{math.MaxInt, 2, 1}}} {
		if got, err := GrayscaleAttributeOpen(nil, bad, 2, GrayscaleAttributeOptions{}); got != nil || !errors.Is(err, ErrInvalidGrayscaleAttribute) {
			t.Fatal("invalid volume", err)
		}
	}
	for _, x := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		bad := v.Clone()
		bad.Data[2] = x
		if got, err := GrayscaleAttributeClose(nil, bad, 2, GrayscaleAttributeOptions{}); got != nil || !errors.Is(err, ErrInvalidGrayscaleAttribute) {
			t.Fatal("nonfinite rejected")
		}
	}
	bad := v.Clone()
	bad.SpacingMM[0] = math.NaN()
	if _, err := GrayscaleAttributeOpen(nil, bad, 2, GrayscaleAttributeOptions{}); !errors.Is(err, ErrInvalidGrayscaleAttribute) {
		t.Fatal("spacing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := GrayscaleAttributeOpen(ctx, v, 2, GrayscaleAttributeOptions{}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancel")
	}
	big := imagefilter.NewVolume([3]int{128, 128, 2}, [3]float64{1, 1, 1})
	for i := range big.Data {
		big.Data[i] = float32((i * 37) % 101)
	}
	before := big.Clone()
	// Deterministic Err-check cancellation at several progress points covers
	// validation, sort, component sweep and reconstruction without timed sleeps.
	for _, at := range []int{3, 20, 120, 240, 300} {
		ctx := &attributeCancelContext{Context: context.Background(), at: at}
		got, err := GrayscaleAttributeOpen(ctx, big, 7, GrayscaleAttributeOptions{})
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel checkpoint%d calls%d err%v", at, ctx.calls, err)
		}
	}
	if !reflect.DeepEqual(big.Data, before.Data) || big.Dims != before.Dims || big.SpacingMM != before.SpacingMM {
		t.Fatal("cancel mutated source")
	}
	identity, err := GrayscaleAttributeOpen(nil, v, 0, GrayscaleAttributeOptions{})
	if err != nil || !reflect.DeepEqual(identity.Data, v.Data) {
		t.Fatal("zero threshold")
	}
	identity.Data[0] = 100
	if v.Data[0] != 0 {
		t.Fatal("output aliases input")
	}
}
