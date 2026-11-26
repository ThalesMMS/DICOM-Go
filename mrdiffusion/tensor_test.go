package mrdiffusion

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"reflect"
	"sync"
	"testing"
)

func tensorTestGrid() TensorGeometry {
	return TensorGeometry{Size: [3]int{4, 3, 2}, Origin: render.Vec3{X: 4, Y: -2, Z: 10}, Spacing: render.Vec3{X: .8, Y: 1.1, Z: 2}, Direction: [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, FrameOfReferenceUID: "2.25.1465.4"}
}
func tensorTestFixture(g TensorGeometry, d Tensor3, axes TensorAxes) []TensorVolume {
	directions := [][3]float64{{}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {math.Sqrt(.5), math.Sqrt(.5), 0}, {math.Sqrt(.5), 0, math.Sqrt(.5)}, {0, math.Sqrt(.5), math.Sqrt(.5)}}
	out := make([]TensorVolume, len(directions))
	n := g.Size[0] * g.Size[1] * g.Size[2]
	for i, v := range directions {
		e := TensorEncoding{Axes: axes}
		if i > 0 {
			e.BValue = 1000
			e.HasGradient = true
			e.Gradient = v
		}
		p := v
		if axes == TensorImageAxes {
			for j := 0; j < 3; j++ {
				p[j] = 0
				for k := 0; k < 3; k++ {
					p[j] += g.Direction[j][k] * v[k]
				}
			}
		}
		bD := e.BValue * (d[0]*p[0]*p[0] + d[1]*p[1]*p[1] + d[2]*p[2]*p[2] + 2*d[3]*p[0]*p[1] + 2*d[4]*p[0]*p[2] + 2*d[5]*p[1]*p[2])
		signals := make([]float32, n)
		for j := range signals {
			signals[j] = float32((900 + float64(j)) * math.Exp(-bD))
		}
		sources := make([]TensorSource, g.Size[2])
		for z := range sources {
			sources[z] = TensorSource{StudyInstanceUID: "2.25.1465.1", SeriesInstanceUID: "2.25.1465.10." + string(rune('1'+i)), SOPClassUID: "1.2.840.10008.5.1.4.1.1.4.1", SOPInstanceUID: "2.25.1465.20." + string(rune('1'+i)), FrameNumber: z + 1}
		}
		out[i] = TensorVolume{Geometry: g, Encoding: e, Signals: signals, Sources: sources}
	}
	return out
}
func tensorNear(t testing.TB, a, b, tol float64) {
	t.Helper()
	if !tensorFinite(a) || math.Abs(a-b) > tol {
		t.Fatalf("got%.15g want%.15g tolerance%g", a, b, tol)
	}
}
func TestTensorAnalyticalIsotropicAnisotropic(t *testing.T) {
	for _, d := range []Tensor3{{.001, .001, .001, 0, 0, 0}, {.0018, .0007, .0004, .0001, -.00006, .00004}} {
		v := tensorTestFixture(tensorTestGrid(), d, TensorLPS)
		r, e := FitTensor(nil, v, TensorOptions{})
		if e != nil {
			t.Fatal(e)
		}
		tr := d[0] + d[1] + d[2]
		fro := d[0]*d[0] + d[1]*d[1] + d[2]*d[2] + 2*(d[3]*d[3]+d[4]*d[4]+d[5]*d[5])
		fa := math.Sqrt(math.Max(0, 1.5*(fro-tr*tr/3)/fro))
		for _, x := range r.Voxels {
			if x.Status != TensorValid {
				t.Fatal(x)
			}
			for k, a := range x.Tensor {
				tensorNear(t, a, d[k], 3e-10)
			}
			tensorNear(t, x.MD, tr/3, 3e-10)
			tensorNear(t, x.FA, fa, 2e-6)
		}
		if r.Counts[TensorValid] != 24 {
			t.Fatal(r.Counts)
		}
	}
}
func TestTensorObliqueImagePatientInvariant(t *testing.T) {
	g := tensorTestGrid()
	c, s := math.Cos(.7), math.Sin(.7)
	g.Direction = [3][3]float64{{c, -s, 0}, {s, c, 0}, {0, 0, 1}}
	eigen := [3]float64{.0018, .0007, .0004}
	var a [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				a[i][j] += g.Direction[i][k] * eigen[k] * g.Direction[j][k]
			}
		}
	}
	d := Tensor3{a[0][0], a[1][1], a[2][2], a[0][1], a[0][2], a[1][2]}
	for _, axes := range []TensorAxes{TensorLPS, TensorImageAxes} {
		for _, matrixOnly := range []bool{false, true} {
			volumes := tensorTestFixture(g, d, axes)
			if matrixOnly {
				for i := range volumes {
					e := &volumes[i].Encoding
					x, y, z := e.Gradient[0], e.Gradient[1], e.Gradient[2]
					e.BMatrix = Tensor3{e.BValue * x * x, e.BValue * y * y, e.BValue * z * z, e.BValue * x * y, e.BValue * x * z, e.BValue * y * z}
					e.HasGradient = false
					e.HasBMatrix = true
				}
			}
			r, e := FitTensor(nil, volumes, TensorOptions{})
			if e != nil {
				t.Fatal(e)
			}
			for _, v := range r.Voxels {
				for k, x := range v.Tensor {
					tensorNear(t, x, d[k], 3e-10)
				}
				if !v.DirectionDefined {
					t.Fatal(v)
				}
				u := render.Vec3{X: c, Y: s}
				if math.Abs(v.Direction.Dot(u)) < 1-1e-8 {
					t.Fatal(v.Direction, u)
				}
				for k, x := range v.Eigenvalues {
					tensorNear(t, x, eigen[k], 3e-10)
				}
			}
		}
	}
}
func TestTensorBMatrixRankAndAdmission(t *testing.T) {
	for _, kind := range []string{"rank", "b0", "unknown", "gradient", "inconsistent", "negativeb", "trace", "nonfiniteb", "geometry", "reflection", "degenerate", "spacing", "channels", "voxels", "bytes", "mask", "options"} {
		t.Run(kind, func(t *testing.T) {
			v := tensorTestFixture(tensorTestGrid(), Tensor3{.001, .0007, .0004}, TensorLPS)
			o := TensorOptions{}
			want := ErrTensor
			switch kind {
			case "rank":
				for i := 1; i < len(v); i++ {
					v[i].Encoding = v[1].Encoding
				}
				want = ErrTensorRank
			case "b0":
				v[0].Encoding = v[1].Encoding
			case "unknown":
				v[1].Encoding.Axes = "unknown"
			case "gradient":
				v[1].Encoding.Gradient = [3]float64{2, 0, 0}
			case "inconsistent":
				v[1].Encoding.HasBMatrix = true
				v[1].Encoding.BMatrix = Tensor3{0, 1000, 0}
			case "negativeb":
				v[1].Encoding.BValue = -1
			case "trace":
				v[1].Encoding = TensorEncoding{BValue: 1000, HasBMatrix: true, BMatrix: Tensor3{100, 0, 0}, Axes: TensorLPS}
			case "nonfiniteb":
				v[1].Encoding.BValue = math.NaN()
			case "geometry":
				v[1].Geometry.Origin.X++
			case "reflection":
				for i := range v {
					v[i].Geometry.Direction[0][0] = -1
				}
			case "degenerate":
				for i := range v {
					v[i].Geometry.Direction[0][0] = 0
				}
			case "spacing":
				for i := range v {
					v[i].Geometry.Spacing.Z = 0
				}
			case "channels":
				o.MaxVolumes = 6
			case "voxels":
				o.MaxVoxels = 1
			case "bytes":
				o.MaxBytes = 1
			case "mask":
				o.Mask = []bool{true}
			case "options":
				o.NoiseFloor = -1
			}
			r, e := FitTensor(nil, v, o)
			if !errors.Is(e, want) || len(r.Voxels) != 0 {
				t.Fatal(kind, r, e, want)
			}
		})
	}
	v := tensorTestFixture(tensorTestGrid(), Tensor3{.001, .0007, .0004}, TensorLPS)
	for i := 1; i < len(v); i++ {
		g := v[i].Encoding.Gradient
		b := v[i].Encoding.BValue
		v[i].Encoding.HasBMatrix = true
		v[i].Encoding.BMatrix = Tensor3{b * g[0] * g[0], b * g[1] * g[1], b * g[2] * g[2], b * g[0] * g[1], b * g[0] * g[2], b * g[1] * g[2]}
		v[i].Encoding.HasGradient = false
	}
	v = append(v, v[1])
	r, e := FitTensor(nil, v, TensorOptions{})
	if e != nil || r.DuplicateEncodings != 1 || r.Counts[TensorValid] != 24 {
		t.Fatal(r, e)
	}
}
func TestTensorNoiseNegativeAndUndefinedDirections(t *testing.T) {
	v := tensorTestFixture(tensorTestGrid(), Tensor3{.001, .0007, .0004}, TensorLPS)
	v[1].Signals[0] = float32(math.NaN())
	v[2].Signals[1] = 0
	mask := make([]bool, 24)
	for i := range mask {
		mask[i] = true
	}
	mask[2] = false
	r, e := FitTensor(nil, v, TensorOptions{Mask: mask})
	if e != nil || r.Counts[TensorNonfinite] != 1 || r.Counts[TensorLowSignal] != 1 || r.Counts[TensorMasked] != 1 || r.Counts[TensorValid] != 21 {
		t.Fatal(r.Counts, e)
	}
	negative := tensorTestFixture(tensorTestGrid(), Tensor3{.001, .0007, -.0004}, TensorLPS)
	r, e = FitTensor(nil, negative, TensorOptions{})
	if e != nil || r.Counts[TensorNegative] != 24 {
		t.Fatal(r.Counts, e)
	}
	r, e = FitTensor(nil, negative, TensorOptions{NegativePolicy: TensorClampNegative})
	if e != nil || r.Clamped != 24 || r.Counts[TensorValid] != 24 {
		t.Fatal(r, e)
	}
	for _, x := range r.Voxels {
		if !x.Clamped || x.Eigenvalues[2] != 0 {
			t.Fatal(x)
		}
	}
	v = tensorTestFixture(tensorTestGrid(), Tensor3{.001, .001, .001}, TensorLPS)
	r, e = FitTensor(nil, v, TensorOptions{})
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range r.Voxels {
		if x.DirectionDefined || x.RGB != [3]uint8{} {
			t.Fatal("isotropic direction manufactured", x)
		}
	}
	v = tensorTestFixture(tensorTestGrid(), Tensor3{.0018, .0007, .0004}, TensorLPS)
	for i := range v {
		for j := range v[i].Signals {
			v[i].Signals[j] += 50
		}
	}
	r, e = FitTensor(nil, v, TensorOptions{NoiseFloor: 50})
	if e != nil {
		t.Fatal(e)
	}
	tensorNear(t, r.Voxels[0].MD, (.0018+.0007+.0004)/3, 3e-10)
}
func TestTensorConcurrentOwnershipDeterminism(t *testing.T) {
	g := tensorTestGrid()
	g.Size = [3]int{16, 16, 2}
	v := tensorTestFixture(g, Tensor3{.0018, .0007, .0004}, TensorLPS)
	before := make([][]float32, len(v))
	for i := range v {
		before[i] = append([]float32(nil), v[i].Signals...)
	}
	want, e := FitTensor(nil, v, TensorOptions{Workers: 1})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := FitTensor(nil, v, TensorOptions{Workers: 4})
			if e != nil || !reflect.DeepEqual(r.Voxels, want.Voxels) || r.Counts != want.Counts {
				t.Error(r, e)
			}
		}()
	}
	wg.Wait()
	for i := range v {
		if !reflect.DeepEqual(v[i].Signals, before[i]) {
			t.Fatal("modified inputs")
		}
	}
	v[0].Signals[0] = 0
	v[0].Sources[0].SOPInstanceUID = "mutated"
	if want.sources[0][0].SOPInstanceUID == "mutated" || want.Voxels[0].Status != TensorValid {
		t.Fatal("retained input ownership")
	}
}

type tensorCancelContext struct {
	context.Context
	left int
}

func (c *tensorCancelContext) Err() error {
	c.left--
	if c.left <= 0 {
		return context.Canceled
	}
	return nil
}
func TestTensorCancellationAtomic(t *testing.T) {
	v := tensorTestFixture(tensorTestGrid(), Tensor3{.001, .0007, .0004}, TensorLPS)
	for _, n := range []int{1, 4, 20, 60} {
		ctx := &tensorCancelContext{Context: context.Background(), left: n}
		r, e := FitTensor(ctx, v, TensorOptions{Workers: 1})
		if !errors.Is(e, context.Canceled) || len(r.Voxels) != 0 {
			t.Fatal(n, r, e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := FitTensor(ctx, v, TensorOptions{}); !errors.Is(e, context.Canceled) || len(r.Voxels) > 0 {
		t.Fatal(r, e)
	}
}
func TestTensorDICOMClassicEnhancedExtraction(t *testing.T) {
	ds := newDataset("")
	ds.SetFD(tagDiffusionBValue, 1000)
	ds.SetFD(tagDiffusionGradientOrient, 1, 0, 0)
	e, err := ReadTensorEncoding(nil, ds, 0)
	if err != nil || !e.HasGradient || e.BValue != 1000 || e.Axes != TensorLPS {
		t.Fatal(e, err)
	}
	matrix := newDataset("")
	for i, tag := range []uint16{0x9602, 0x9605, 0x9607, 0x9603, 0x9604, 0x9606} {
		value := 0.
		if i == 0 {
			value = 1000
		}
		matrix.SetFD(core.NewTag(0x0018, tag), value)
	}
	macro := macroItem(1000, "BMATRIX", nil)
	macro.SetSequence(core.NewTag(0x0018, 0x9601), matrix)
	fg := newDataset("")
	fg.SetSequence(tagMRDiffusionSequence, macro)
	enh := newDataset("")
	enh.SetStrings(core.NewTag(0x0028, 0x0008), core.VRIS, "2")
	enh.SetSequence(core.NewTag(0x5200, 0x9230), fg, fg)
	for _, pair := range []struct {
		tag  core.Tag
		text string
	}{{core.NewTag(0x0020, 0x000d), "2.25.1465.1"}, {core.NewTag(0x0020, 0x000e), "2.25.1465.2"}, {core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.4.1"}, {core.NewTag(0x0008, 0x0018), "2.25.1465.3"}} {
		enh.SetStrings(pair.tag, core.VRUI, pair.text)
	}
	e, err = ReadTensorEncoding(nil, enh, 1)
	if err != nil || !e.HasBMatrix || e.BMatrix != (Tensor3{1000}) {
		t.Fatal(e, err)
	}
	source, err := ReadTensorSource(enh, 1)
	if err != nil || source.FrameNumber != 2 || source.SeriesInstanceUID != "2.25.1465.2" {
		t.Fatal(source, err)
	}
	if _, err = ReadTensorEncoding(nil, enh, 2); !errors.Is(err, ErrTensor) {
		t.Fatal(err)
	}
	matrix.SetFD(core.NewTag(0x0018, 0x9602), -1000)
	macro.SetSequence(core.NewTag(0x0018, 0x9601), matrix)
	fg.SetSequence(tagMRDiffusionSequence, macro)
	enh.SetSequence(core.NewTag(0x5200, 0x9230), fg, fg)
	if _, err = ReadTensorEncoding(nil, enh, 0); !errors.Is(err, ErrTensor) {
		t.Fatal(err)
	}
}
