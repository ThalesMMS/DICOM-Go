package registration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"
)

func maskedFFTImage(t testing.TB, size [3]int, data []float32, origin, spacing render.Vec3, direction Matrix3) *Image {
	t.Helper()
	im, e := NewImage(size, data, origin, spacing, direction)
	if e != nil {
		t.Fatal(e)
	}
	return im
}
func maskedFFTFixture(t testing.TB, depth int) (*Image, *Image, *Image, *Image) {
	t.Helper()
	fs, ms := [3]int{7, 6, depth}, [3]int{9, 7, depth}
	if depth > 1 {
		ms[2] = depth + 1
	}
	fv, fmask := make([]float32, fs[0]*fs[1]*fs[2]), make([]float32, fs[0]*fs[1]*fs[2])
	mv, mmask := make([]float32, ms[0]*ms[1]*ms[2]), make([]float32, ms[0]*ms[1]*ms[2])
	for z := 0; z < fs[2]; z++ {
		for y := 0; y < fs[1]; y++ {
			for x := 0; x < fs[0]; x++ {
				i := (z*fs[1]+y)*fs[0] + x
				v := float32((i*37+i*i*13)%101 - 50)
				fv[i] = v
				if (x+2*y+z)%5 != 0 {
					fmask[i] = 1
				}
				j := ((z)*ms[1]+y+1)*ms[0] + x + 1
				mv[j] = 3*v + 17
				if (2*x+y+z)%7 != 0 {
					mmask[j] = 1
				}
			}
		}
	}
	origin := render.Vec3{X: 4, Y: -3, Z: 8}
	spacing := render.Vec3{X: 1.25, Y: 2.5, Z: 3}
	d := Identity3()
	return maskedFFTImage(t, fs, fv, origin, spacing, d), maskedFFTImage(t, ms, mv, origin, spacing, d), maskedFFTImage(t, fs, fmask, origin, spacing, d), maskedFFTImage(t, ms, mmask, origin, spacing, d)
}

// Independent direct two-pass centered covariance. No spectra, global
// intensity scaling, FFT moments or production candidate scorer are reused.
func maskedFFTDirectAt(f, m, fm, mm *Image, s [3]int) (float64, int, bool) {
	n := 0
	sf, sm := 0., 0.
	for z := 0; z < f.size[2]; z++ {
		for y := 0; y < f.size[1]; y++ {
			for x := 0; x < f.size[0]; x++ {
				j, k, l := x+s[0], y+s[1], z+s[2]
				if j < 0 || k < 0 || l < 0 || j >= m.size[0] || k >= m.size[1] || l >= m.size[2] || fm.At(x, y, z) <= 0 || mm.At(j, k, l) <= 0 {
					continue
				}
				n++
				sf += float64(f.At(x, y, z))
				sm += float64(m.At(j, k, l))
			}
		}
	}
	if n < 3 {
		return 0, n, false
	}
	af, am := sf/float64(n), sm/float64(n)
	vf, vm, c := 0., 0., 0.
	for z := 0; z < f.size[2]; z++ {
		for y := 0; y < f.size[1]; y++ {
			for x := 0; x < f.size[0]; x++ {
				j, k, l := x+s[0], y+s[1], z+s[2]
				if j < 0 || k < 0 || l < 0 || j >= m.size[0] || k >= m.size[1] || l >= m.size[2] || fm.At(x, y, z) <= 0 || mm.At(j, k, l) <= 0 {
					continue
				}
				a, b := float64(f.At(x, y, z))-af, float64(m.At(j, k, l))-am
				vf += a * a
				vm += b * b
				c += a * b
			}
		}
	}
	if vf == 0 || vm == 0 {
		return 0, n, false
	}
	return c / math.Sqrt(vf*vm), n, true
}
func maskedFFTDirectSearch(f, m, fm, mm *Image, o MaskedFFTOptions) ([3]int, float64, int) {
	count := [2]int{}
	for k, mask := range []*Image{fm, mm} {
		for _, v := range mask.data {
			if v > 0 {
				count[k]++
			}
		}
	}
	minimum := max(o.MinOverlap, int(math.Ceil(o.MinOverlapFraction*float64(min(count[0], count[1])))))
	best := -math.MaxFloat64
	var shift [3]int
	overlap := 0
	for z := 1 - f.size[2]; z < m.size[2]; z++ {
		for y := 1 - f.size[1]; y < m.size[1]; y++ {
			for x := 1 - f.size[0]; x < m.size[0]; x++ {
				s := [3]int{x, y, z}
				p := m.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}).Sub(f.origin)
				if o.Search != nil && (p.X < o.Search.Min.X || p.X > o.Search.Max.X || p.Y < o.Search.Min.Y || p.Y > o.Search.Max.Y || p.Z < o.Search.Min.Z || p.Z > o.Search.Max.Z) {
					continue
				}
				score, n, ok := maskedFFTDirectAt(f, m, fm, mm, s)
				if ok && n >= minimum && score > best+1e-12 {
					best, shift, overlap = score, s, n
				}
			}
		}
	}
	return shift, best, overlap
}
func TestMaskedFFTSpatialOracle2D3D(t *testing.T) {
	for _, depth := range []int{1, 3} {
		f, m, fm, mm := maskedFFTFixture(t, depth)
		o := MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2}
		s, score, n := maskedFFTDirectSearch(f, m, fm, mm, o)
		r, e := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, o)
		if e != nil {
			t.Fatal(e)
		}
		if r.IndexShift != s || s != [3]int{1, 1, 0} || r.Overlap != n || math.Abs(r.Score-score) > 1e-9 || !r.Usable {
			t.Fatalf("depth%d got%+v oracle%v/%g/%d", depth, r, s, score, n)
		}
		want := render.Vec3{X: 1.25, Y: 2.5}
		if r.TranslationLPS.Sub(want).Length() > 1e-10 {
			t.Fatal(r.TranslationLPS)
		}
	}
}
func TestMaskedFFTIndependentMasksSmallOverlap(t *testing.T) {
	f, m, fm, mm := maskedFFTFixture(t, 1)
	for i := range fm.data {
		fm.data[i] = 0
	}
	for _, i := range []int{2, 3, 4, 10, 11, 12} {
		fm.data[i] = 1
	}
	o := MaskedFFTOptions{MinOverlap: 3, MinOverlapFraction: .5}
	s, score, n := maskedFFTDirectSearch(f, m, fm, mm, o)
	r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
	if e != nil {
		t.Fatal(e)
	}
	if r.IndexShift != s || r.Overlap != n || math.Abs(r.Score-score) > 1e-9 {
		t.Fatalf("got%+v oracle%v/%g/%d", r, s, score, n)
	}
	direct, n, ok := maskedFFTDirectAt(f, m, fm, mm, [3]int{1, 1, 0})
	if !ok || n < 3 || math.Abs(direct-1) > 1e-12 {
		t.Fatal(direct, n)
	}
	for i := range f.data {
		if fm.data[i] <= 0 {
			f.data[i] = 1e6
		}
	}
	r2, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
	if e != nil || r2 != r {
		t.Fatal("excluded finite samples changed result", r, r2, e)
	}
}
func TestMaskedFFTObliquePhysicalSearch(t *testing.T) {
	f, m, fm, mm := maskedFFTFixture(t, 3)
	c, s := math.Cos(.6), math.Sin(.6)
	d := Matrix3{{c, -s, 0}, {s, c, 0}, {0, 0, 1}}
	for _, im := range []*Image{f, m, fm, mm} {
		v := maskedFFTImage(t, im.size, im.data, im.origin, im.spacing, d)
		*im = *v
	}
	delta := render.Vec3{X: 10, Y: -7, Z: 2}
	m.origin = m.origin.Add(delta)
	mm.origin = mm.origin.Add(delta)
	want := delta.Add(d.MulVec(render.Vec3{X: 1.25, Y: 2.5}))
	o := MaskedFFTOptions{MinOverlap: 12, Search: &MaskedFFTSearch{Min: want.Sub(render.Vec3{X: .01, Y: .01, Z: .01}), Max: want.Add(render.Vec3{X: .01, Y: .01, Z: .01})}}
	r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
	if e != nil || r.IndexShift != [3]int{1, 1, 0} || r.TranslationLPS.Sub(want).Length() > 1e-10 || !r.SearchBoundary {
		t.Fatal(r, e)
	}
	o.Search = &MaskedFFTSearch{Min: want.Add(render.Vec3{X: 100}), Max: want.Add(render.Vec3{X: 101})}
	if _, e = EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o); !errors.Is(e, ErrMaskedFFTNoOverlap) {
		t.Fatal(e)
	}
}
func TestMaskedFFTAdmissionAndDegeneracy(t *testing.T) {
	for _, kind := range []string{"empty", "variance", "nonfinite", "masknonfinite", "geometry", "maskgeometry", "bytes", "padding", "work", "input", "search", "options"} {
		t.Run(kind, func(t *testing.T) {
			f, m, fm, mm := maskedFFTFixture(t, 1)
			o := MaskedFFTOptions{}
			want := ErrMaskedFFT
			switch kind {
			case "empty":
				clear(fm.data)
				want = ErrMaskedFFTNoOverlap
			case "variance":
				for i := range f.data {
					f.data[i] = 7
				}
				want = ErrMaskedFFTVariance
			case "nonfinite":
				f.data[0] = float32(math.NaN())
			case "masknonfinite":
				fm.data[0] = float32(math.Inf(1))
			case "geometry":
				m.indexToPhysical[0][0] *= 2
			case "maskgeometry":
				fm.origin.X++
			case "bytes":
				o.MaxBytes = 1
			case "padding":
				o.MaxPaddedVoxels = 1
			case "work":
				o.MaxFFTWork = 1
			case "input":
				o.MaxInputVoxels = 1
			case "search":
				o.Search = &MaskedFFTSearch{Min: render.Vec3{X: 2}, Max: render.Vec3{X: 1}}
			case "options":
				o.MinOverlap = 2
			}
			r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
			if !errors.Is(e, want) || r != (MaskedFFTResult{}) {
				t.Fatal(r, e, want)
			}
		})
	}
	f, m, fm, mm := maskedFFTFixture(t, 1)
	f.size[0] = int(^uint(0) >> 1)
	fm.size = f.size
	if _, e := maskedFFTAdmission(f, m, fm, mm, MaskedFFTOptions{}); !errors.Is(e, ErrMaskedFFT) {
		t.Fatal(e)
	}
}
func TestMaskedFFTAmbiguityAndLinearBoundary(t *testing.T) {
	size := [3]int{6, 3, 1}
	a, mask := make([]float32, 18), make([]float32, 18)
	for i := range a {
		a[i] = float32(i % 2)
		mask[i] = 1
	}
	f := maskedFFTImage(t, size, a, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	fm := maskedFFTImage(t, size, mask, f.origin, f.spacing, f.direction)
	r, e := EstimateMaskedFFTTranslation(nil, f, f, fm, fm, MaskedFFTOptions{MinOverlap: 3})
	if e != nil || !r.Ambiguous || r.Usable {
		t.Fatal(r, e)
	}
	// A search restricted to an extreme x shift has at least three y samples;
	// diagnostic peak is retained, but cannot be a usable border proposal.
	for i := range a {
		a[i] = float32((i*i + 3*i) % 17)
	}
	o := MaskedFFTOptions{MinOverlap: 3, Search: &MaskedFFTSearch{Min: render.Vec3{X: 5}, Max: render.Vec3{X: 5}}}
	r, e = EstimateMaskedFFTTranslation(nil, f, f, fm, fm, o)
	if e != nil || !r.LinearBoundary || r.Usable {
		t.Fatal(r, e)
	}
}

// Err counter tests cancellation inside preparation, axes and publication without sleeps.
type maskedFFTCancelContext struct {
	context.Context
	calls, cancelAt int
}

func (c *maskedFFTCancelContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func TestMaskedFFTCancellationAtomic(t *testing.T) {
	f, m, fm, mm := maskedFFTFixture(t, 3)
	c := &maskedFFTCancelContext{Context: context.Background(), cancelAt: math.MaxInt}
	_, e := EstimateMaskedFFTTranslation(c, f, m, fm, mm, MaskedFFTOptions{})
	if e != nil {
		t.Fatal(e)
	}
	for _, at := range []int{1, 5, 30, c.calls / 2, c.calls} {
		ctx := &maskedFFTCancelContext{Context: context.Background(), cancelAt: at}
		r, e := EstimateMaskedFFTTranslation(ctx, f, m, fm, mm, MaskedFFTOptions{})
		if !errors.Is(e, context.Canceled) || r != (MaskedFFTResult{}) {
			t.Fatal(at, r, e)
		}
	}
}
func TestMaskedFFTConcurrentDeterminism(t *testing.T) {
	f, m, fm, mm := maskedFFTFixture(t, 3)
	want, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, MaskedFFTOptions{Workers: 1})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, MaskedFFTOptions{Workers: 8})
			if e != nil || r != want {
				t.Error(r, e)
			}
		}()
	}
	wg.Wait()
}
func TestMaskedFFTAxesRoundTrip(t *testing.T) {
	size := [3]int{8, 4, 2}
	v := make([]complex128, 64)
	for i := range v {
		v[i] = complex(float64(i%7), float64(i%3))
	}
	want := append([]complex128(nil), v...)
	if e := maskedFFTAxes(context.Background(), v, size, false, 0); e != nil {
		t.Fatal(e)
	}
	if e := maskedFFTAxes(context.Background(), v, size, true, 0); e != nil {
		t.Fatal(e)
	}
	for i := range v {
		if math.Hypot(real(v[i]-want[i]), imag(v[i]-want[i])) > 1e-12 {
			t.Fatal(i, v[i], want[i])
		}
	}
}

// Optional externally produced ITK artifact, not a provider-authentication claim.
func TestMaskedFFTITKOptIn(t *testing.T) {
	path := os.Getenv("DICOM_MASKED_FFT_ITK_ORACLE")
	if path == "" {
		t.Skip("external ITK artifact not supplied")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var a struct {
		Provider, Version, Convention string
		Shift                         [3]int
		Score                         float64
		Overlap                       int
	}
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	if a.Provider != "ITK" || a.Version == "" || a.Convention != "movingIndex=fixedIndex+shift" || !finite(a.Score) {
		t.Fatal("invalid oracle provenance/convention")
	}
	f, m, fm, mm := maskedFFTFixture(t, 3)
	r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2})
	if e != nil || !reflect.DeepEqual(r.IndexShift, a.Shift) || r.Overlap != a.Overlap || math.Abs(r.Score-a.Score) > 1e-6 {
		t.Fatal(r, a, e)
	}
}
func TestMaskedFFTScoreAndReverseConvention(t *testing.T) {
	f, m, fm, mm := maskedFFTFixture(t, 3)
	for _, s := range [][3]int{{1, 1, 0}, {0, 0, 0}, {-1, 2, 0}, {2, -1, 1}} {
		v, n, ok := maskedFFTDirectAt(f, m, fm, mm, s)
		if !ok || n < 3 {
			t.Fatal(s, n)
		}
		p := m.Point(render.Vec3{X: float64(s[0]), Y: float64(s[1]), Z: float64(s[2])}).Sub(f.origin)
		o := MaskedFFTOptions{MinOverlap: 3, Search: &MaskedFFTSearch{Min: p, Max: p}}
		r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
		if e != nil || r.IndexShift != s || r.Overlap != n || math.Abs(r.Score-v) > 1e-9 {
			t.Fatal(s, r, v, n, e)
		}
	}
	r, e := EstimateMaskedFFTTranslation(nil, m, f, mm, fm, MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2})
	if e != nil || r.IndexShift != [3]int{-1, -1, 0} || r.TranslationLPS.Sub(render.Vec3{X: -1.25, Y: -2.5}).Length() > 1e-10 {
		t.Fatal(r, e)
	}
}
