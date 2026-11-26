package roi

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func aa1542Mask(size [3]int, h [3]float64, kind string) *VolumeMask {
	m := NewVolumeMask(size[0], size[1], size[2], h)
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				dx, dy, dz := (float64(x)-float64(size[0]-1)/2)*h[0], (float64(y)-float64(size[1]-1)/2)*h[1], (float64(z)-float64(size[2]-1)/2)*h[2]
				radius := .3 * math.Min(float64(size[0])*h[0], float64(size[1])*h[1])
				inside := dx*dx+dy*dy+dz*dz < radius*radius
				switch kind {
				case "step":
					inside = x+y/2 < size[0]/2
				case "thin":
					inside = z == size[2]/2 && x > 1 && x < size[0]-2 && y > 1 && y < size[1]-2
				case "hollow":
					inside = inside && dx*dx+dy*dy+dz*dz > radius*radius/4
				}
				m.Voxels[(z*size[1]+y)*size[0]+x] = inside
			}
		}
	}
	return m
}
func TestBinaryAntiAliasInitialIndependent(t *testing.T) {
	m := aa1542Mask([3]int{5, 4, 3}, [3]float64{.7, 1.1, 1.6}, "sphere")
	size, h, _, _, e := antiAliasGeometry(context.Background(), m)
	if e != nil {
		t.Fatal(e)
	}
	phi, e := antiAliasInitial(context.Background(), m, size, h, .7)
	if e != nil {
		t.Fatal(e)
	}
	for i, v := range m.Voxels {
		x, y, z := i%size[0], (i/size[0])%size[1], i/(size[0]*size[1])
		best := math.Inf(1)
		for j, w := range m.Voxels {
			if v == w {
				continue
			}
			a, b, c := j%size[0], (j/size[0])%size[1], j/(size[0]*size[1])
			dx, dy, dz := float64(x-a)*h[0], float64(y-b)*h[1], float64(z-c)*h[2]
			best = math.Min(best, math.Sqrt(dx*dx+dy*dy+dz*dz))
		}
		want := best - .35
		if !v {
			want = -want
		}
		if math.Abs(float64(phi[i])-want) > 1e-6 {
			t.Fatal(i, phi[i], want)
		}
	}
}

// Independent central-difference Hessian form: (tr(H)-gᵀHg/|g|²)/(d-1).
// Does not call the production level-set stencil or its mixed helper.
func aa1542DirectRate(data []float32, size [3]int, h [3]float64, i int) float64 {
	pos := [3]int{i % size[0], i / size[0] % size[1], i / (size[0] * size[1])}
	dims := 2
	if size[2] > 1 {
		dims = 3
	}
	at := func(a, b, c int) float64 {
		x := min(size[0]-1, max(0, a))
		y := min(size[1]-1, max(0, b))
		z := min(size[2]-1, max(0, c))
		return float64(data[(z*size[1]+y)*size[0]+x])
	}
	sample := func(a, b int, sa, sb int) float64 { p := pos; p[a] += sa; p[b] += sb; return at(p[0], p[1], p[2]) }
	center := float64(data[i])
	var gradient [3]float64
	var H [3][3]float64
	for a := 0; a < dims; a++ {
		p, q := pos, pos
		p[a]--
		q[a]++
		minus, plus := at(p[0], p[1], p[2]), at(q[0], q[1], q[2])
		gradient[a] = (plus - minus) / (2 * h[a])
		H[a][a] = (plus - 2*center + minus) / (h[a] * h[a])
		for b := 0; b < a; b++ {
			H[a][b] = (sample(a, b, 1, 1) - sample(a, b, 1, -1) - sample(a, b, -1, 1) + sample(a, b, -1, -1)) / (4 * h[a] * h[b])
			H[b][a] = H[a][b]
		}
	}
	norm, trace, product := 0., 0., 0.
	for a := 0; a < dims; a++ {
		norm += gradient[a] * gradient[a]
		trace += H[a][a]
		for b := 0; b < dims; b++ {
			product += gradient[a] * H[a][b] * gradient[b]
		}
	}
	if norm <= 1e-12 {
		return 0
	}
	return (trace - product/norm) / float64(dims-1)
}
func TestBinaryAntiAliasCurvatureAnalytical(t *testing.T) {
	size, h := [3]int{7, 7, 5}, [3]float64{.5, .8, 1.2}
	phi := make([]float32, size[0]*size[1]*size[2])
	radius := 10.
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				a, b, c := float64(x)*h[0], float64(y)*h[1], float64(z)*h[2]
				phi[(z*size[1]+y)*size[0]+x] = float32((radius*radius - a*a - b*b - c*c) / (2 * radius))
			}
		}
	}
	g := &levelSetGrid{nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, h: h, dims: 3}
	s := &levelSetState{levelSetGrid: g, phi: phi}
	i := (2*size[1]+3)*size[0] + 3
	got, _, _ := s.ratePotentialRegularized(i, levelSetTerms{curvature: 1}, false, 0, 0)
	if math.Abs(float64(got)+1/radius) > 2e-5 || math.Abs(float64(got)-aa1542DirectRate(phi, size, h, i)) > 2e-6 {
		t.Fatal(got)
	}
}
func TestBinaryAntiAliasOneStepIndependent(t *testing.T) {
	for _, size := range [][3]int{{7, 6, 1}, {7, 6, 4}} {
		m := aa1542Mask(size, [3]float64{.7, 1.1, 1.6}, "step")
		r, e := BinaryAntiAlias(nil, m, BinaryAntiAliasOptions{MaxIterations: 1, MaximumRMSChangeMM: 1e-12, Workers: 1})
		if e != nil {
			t.Fatal(e)
		}
		h := [3]float64{.7, 1.1, 1.6}
		if size[2] == 1 {
			h[2] = 1
		}
		squared, count := 0., 0
		for i, v := range r.InitialPhi {
			want := float64(v)
			if math.Abs(want) <= r.Report.Options.BandWidthMM {
				rate := float64(float32(aa1542DirectRate(r.InitialPhi, size, h, i)))
				want += r.Report.Options.TimeStepMM2 * rate
				if m.Voxels[i] {
					want = math.Max(want, r.Report.Options.SignMarginMM)
				} else {
					want = math.Min(want, -r.Report.Options.SignMarginMM)
				}
				d := float64(float32(want)) - float64(v)
				squared += d * d
				count++
			}
			if math.Abs(float64(r.Phi[i])-float64(float32(want))) > 2e-6 {
				t.Fatal(size, i, r.Phi[i], want)
			}
		}
		if math.Abs(r.Report.History[0].RMSChangeMM-math.Sqrt(squared/float64(count))) > 2e-6 {
			t.Fatal(r.Report)
		}
	}
}
func TestBinaryAntiAliasPhantomsConstraints(t *testing.T) {
	for _, kind := range []string{"sphere", "step", "thin", "hollow"} {
		m := aa1542Mask([3]int{17, 15, 9}, [3]float64{.6, 1.1, 1.8}, kind)
		before := m.Clone()
		r, e := BinaryAntiAlias(nil, m, BinaryAntiAliasOptions{MaxIterations: 30})
		if e != nil {
			t.Fatal(kind, e)
		}
		if !reflect.DeepEqual(before, m) || !reflect.DeepEqual(before, r.Original) || r.Report.Iterations < 1 || r.Report.Iterations > 30 || len(r.Report.History) != r.Report.Iterations {
			t.Fatal(kind, r.Report)
		}
		for i, v := range m.Voxels {
			if !antiAliasFinite(float64(r.Phi[i])) || (r.Phi[i] > 0) != v {
				t.Fatal(kind, i, r.Phi[i], v)
			}
		}
		t.Logf("%s iterations=%d rms=%g converged=%v bytes=%d band=%d", kind, r.Report.Iterations, r.Report.History[len(r.Report.History)-1].RMSChangeMM, r.Report.Converged, r.Report.LogicalBytes, r.Report.BandVoxels)
	}

	// Scaling every physical coordinate by two scales phi/RMS by two and
	// the default timestep by four, with the same constrained iterations.
	base := aa1542Mask([3]int{13, 11, 7}, [3]float64{.7, 1.1, 1.6}, "sphere")
	scaled := base.Clone()
	for _, axis := range [][]float64{scaled.X, scaled.Y, scaled.Z} {
		for i := range axis {
			axis[i] *= 2
		}
	}
	a, e := BinaryAntiAlias(nil, base, BinaryAntiAliasOptions{Workers: 1, MaxIterations: 8})
	if e != nil {
		t.Fatal(e)
	}
	b, e := BinaryAntiAlias(nil, scaled, BinaryAntiAliasOptions{Workers: 1, MaxIterations: 8})
	if e != nil {
		t.Fatal(e)
	}
	if a.Report.Iterations != b.Report.Iterations {
		t.Fatal(a.Report, b.Report)
	}
	for i, v := range a.Phi {
		if math.Abs(float64(b.Phi[i])-2*float64(v)) > 2e-6 {
			t.Fatal(i, v, b.Phi[i])
		}
	}
	// A planar interface is a steady solution under the reflecting stencil.
	m := aa1542Mask([3]int{12, 9, 1}, [3]float64{1, 1, 1}, "step")
	for y := 0; y < m.Rows; y++ {
		for x := 0; x < m.Columns; x++ {
			m.Voxels[y*m.Columns+x] = x < 6
		}
	}
	r, e := BinaryAntiAlias(nil, m, BinaryAntiAliasOptions{})
	if e != nil || !r.Report.Converged || r.Report.Iterations != 1 || r.Report.History[0].RMSChangeMM > 1e-7 {
		t.Fatal(r, e)
	}
}
func TestBinaryAntiAliasAdmission(t *testing.T) {
	for _, kind := range []string{"empty", "full", "voxels", "bytes", "work", "iterations", "dt", "band", "margin", "workers", "irregular", "nan", "shape"} {
		t.Run(kind, func(t *testing.T) {
			m := aa1542Mask([3]int{7, 6, 4}, [3]float64{.7, 1.1, 1.6}, "sphere")
			o := BinaryAntiAliasOptions{}
			switch kind {
			case "empty":
				clear(m.Voxels)
			case "full":
				for i := range m.Voxels {
					m.Voxels[i] = true
				}
			case "voxels":
				o.MaxVoxels = 1
			case "bytes":
				o.MaxBytes = 1
			case "work":
				o.MaxVoxelIterations = 1
			case "iterations":
				o.MaxIterations = -1
			case "dt":
				o.TimeStepMM2 = 100
			case "band":
				o.BandWidthMM = .1
			case "margin":
				o.SignMarginMM = 1
			case "workers":
				o.Workers = -1
			case "irregular":
				m.X[2] += .1
			case "nan":
				m.Y[0] = math.NaN()
			case "shape":
				m.Voxels = m.Voxels[:1]
			}
			r, e := BinaryAntiAlias(nil, m, o)
			if e == nil || r != nil || !errors.Is(e, ErrBinaryAntiAlias) && !errors.Is(e, ErrBinaryAntiAliasLimit) {
				t.Fatal(r, e)
			}
		})
	}
}

type aa1542CountingContext struct {
	context.Context
	left atomic.Int64
}

func (c *aa1542CountingContext) Err() error {
	if c.left.Add(-1) <= 0 {
		return context.Canceled
	}
	return nil
}
func TestBinaryAntiAliasCancellationAtomic(t *testing.T) {
	m := aa1542Mask([3]int{15, 13, 7}, [3]float64{.6, 1.1, 1.8}, "sphere")
	for _, at := range []int64{1, 20, 100} {
		c := &aa1542CountingContext{Context: context.Background()}
		c.left.Store(at)
		if r, e := BinaryAntiAlias(c, m, BinaryAntiAliasOptions{Workers: 1, MaxIterations: 20}); r != nil || !errors.Is(e, context.Canceled) {
			t.Fatal(at, r, e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	before := parallel.InUse()
	r, e := BinaryAntiAlias(ctx, m, BinaryAntiAliasOptions{Progress: func(BinaryAntiAliasIteration) { cancel() }})
	if r != nil || !errors.Is(e, context.Canceled) || parallel.InUse() != before {
		t.Fatal(r, e, parallel.InUse(), before)
	}
}
func TestBinaryAntiAliasOwnershipWorkers(t *testing.T) {
	m := aa1542Mask([3]int{32, 24, 12}, [3]float64{.6, 1.1, 1.8}, "sphere")
	before := m.Clone()
	one, e := BinaryAntiAlias(nil, m, BinaryAntiAliasOptions{Workers: 1, MaxIterations: 6})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := BinaryAntiAlias(nil, m, BinaryAntiAliasOptions{Workers: 4, MaxIterations: 6})
			if e != nil || !reflect.DeepEqual(one.Phi, r.Phi) || !reflect.DeepEqual(one.Report.History, r.Report.History) {
				t.Error(r, e)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(m, before) {
		t.Fatal("modified source")
	}
	m.X[0] = -999
	m.Voxels[0] = !m.Voxels[0]
	if !reflect.DeepEqual(one.Original, before) {
		t.Fatal("borrowed source retained")
	}
}
