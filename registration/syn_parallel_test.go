package registration

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// synCorrelationScatter is the serial reference of synCorrelation: each
// eligible centre, in index order, scatters its derivative terms onto its
// neighbourhood.
func synCorrelationScatter(w synWarped, size, radius [3]int, variance float64) (float64, []float64, []float64, int) {
	df, dm := make([]float64, len(w.fixed)), make([]float64, len(w.fixed))
	var sum float64
	eligible := 0
	for i := range w.fixed {
		if !w.valid[i] {
			continue
		}
		x, y, z := i%size[0], i/size[0]%size[1], i/(size[0]*size[1])
		var indices []int
		var meanF, meanM float64
		for k := max(0, z-radius[2]); k <= min(size[2]-1, z+radius[2]); k++ {
			for j := max(0, y-radius[1]); j <= min(size[1]-1, y+radius[1]); j++ {
				for h := max(0, x-radius[0]); h <= min(size[0]-1, x+radius[0]); h++ {
					at := (k*size[1]+j)*size[0] + h
					if !w.valid[at] {
						continue
					}
					indices = append(indices, at)
					n := float64(len(indices))
					meanF += (w.fixed[at] - meanF) / n
					meanM += (w.moving[at] - meanM) / n
				}
			}
		}
		if len(indices) < 2 {
			continue
		}
		var ff, mm, fm float64
		for _, at := range indices {
			f, m := w.fixed[at]-meanF, w.moving[at]-meanM
			ff += f * f
			mm += m * m
			fm += f * m
		}
		n := float64(len(indices))
		if ff/n <= variance || mm/n <= variance {
			continue
		}
		rho := (fm / math.Sqrt(ff)) / math.Sqrt(mm)
		sum -= rho * rho
		eligible++
		sf, sm := -2*(fm/mm)/ff, -2*(fm/ff)/mm
		for _, at := range indices {
			f, m := w.fixed[at]-meanF, w.moving[at]-meanM
			df[at] += sf * (m - fm/ff*f)
			dm[at] += sm * (f - fm/mm*m)
		}
	}
	for i := range df {
		df[i] /= float64(eligible)
		dm[i] /= float64(eligible)
	}
	return sum / float64(eligible), df, dm, eligible
}

// TestSyNCorrelationGatherMatchesScatter: the parallel gather of the local
// correlation derivative equals the serial scatter bit for bit, with
// invalid voxels, a constant patch and an anisotropic radius.
func TestSyNCorrelationGatherMatchesScatter(t *testing.T) {
	size := [3]int{23, 17, 11}
	n := size[0] * size[1] * size[2]
	w := synWarped{fixed: make([]float64, n), moving: make([]float64, n), valid: make([]bool, n)}
	for i := range w.fixed {
		w.fixed[i] = math.Sin(float64(i)*.37) + float64(i%7)
		w.moving[i] = 2*math.Cos(float64(i)*.11) - float64(i%5)
		w.valid[i] = i%13 != 0
		if i%size[0] < 4 && i/size[0]%size[1] < 4 {
			w.fixed[i] = 3 // a constant corner some centres find degenerate
		}
	}
	for _, radius := range [][3]int{{2, 2, 2}, {1, 3, 0}} {
		want, wantF, wantM, wantCount := synCorrelationScatter(w, size, radius, 1e-6)
		value, _, _, count, err := synCorrelation(context.Background(), w, size, radius, 1e-6, false)
		if err != nil || value != want || count != wantCount {
			t.Fatalf("radius %v value %g/%d, want %g/%d, err %v", radius, value, count, want, wantCount, err)
		}
		value, df, dm, count, err := synCorrelation(context.Background(), w, size, radius, 1e-6, true)
		if err != nil || value != want || count != wantCount {
			t.Fatalf("radius %v derivative value %g/%d, want %g/%d, err %v", radius, value, count, want, wantCount, err)
		}
		if !reflect.DeepEqual(df, wantF) || !reflect.DeepEqual(dm, wantM) {
			t.Fatalf("radius %v: gathered derivatives differ from the serial scatter", radius)
		}
	}
}

// TestSyNResultIndependentOfWorkers runs a two-level SyN on the calling
// goroutine alone and with the shared budget: every field, validity mask and
// diagnostic is identical.
func TestSyNResultIndependentOfWorkers(t *testing.T) {
	f, m := synPhantomPair(t, 16, .8)
	for _, regularization := range []float64{.1, 0} {
		o := SyNOptions{Levels: []SyNLevel{{Shrink: 2, Iterations: 5}, {Shrink: 1, Iterations: 10}}, Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}}, LearningRateMM: .3, UpdateSigmaMM: .8, Regularization: regularization, MinimumImprovement: 1e-9}
		previous := parallel.SetBudget(0)
		serial, err := SyN(context.Background(), f, m, o)
		parallel.SetBudget(previous)
		if err != nil {
			t.Fatal(err)
		}
		shared, err := SyN(context.Background(), f, m, o)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(serial, shared) {
			t.Fatalf("regularization %g: SyN result depends on the workers", regularization)
		}
	}
}

// TestSyNComposeInverseMatchesInvertPoint: the prepared, parallel
// composition gives every voxel the vector the registration's own
// InvertPoint gives.
func TestSyNComposeInverseMatchesInvertPoint(t *testing.T) {
	f, m := synPhantomPair(t, 12, .6)
	half, other := newField(f), newField(f)
	for i := range half.Vectors {
		x, y, z := float64(i%12), float64(i/12%12), float64(i/144)
		half.Vectors[i] = render.Vec3{X: .3 * math.Sin(x*.5) * math.Sin(y*.4), Y: .2 * math.Cos(z*.3)}
		other.Vectors[i] = render.Vec3{Z: .25 * math.Sin(x*.2+y*.3)}
	}
	synZeroBoundary(half.Vectors, half.Size)
	synZeroBoundary(other.Vectors, other.Size)
	o, _, err := prepareSyN(context.Background(), f, m, SyNOptions{Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}}})
	if err != nil {
		t.Fatal(err)
	}
	field, valid, maximum, err := synComposeInverse(context.Background(), half, other, m, f, o.Inverse)
	if err != nil {
		t.Fatal(err)
	}
	g, err := half.Grid()
	if err != nil {
		t.Fatal(err)
	}
	h, err := other.Grid()
	if err != nil {
		t.Fatal(err)
	}
	identity := spatialreg.Matrix{Values: render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}}
	r := &spatialreg.Registration{Pre: identity, Post: identity, Grid: g}
	wantMaximum := 0.0
	for i := range field.Vectors {
		p := synPoint(m, i)
		inverse, err := r.InvertPoint(p, o.Inverse)
		if err != nil {
			if valid[i] {
				t.Fatalf("voxel %d valid, InvertPoint failed: %v", i, err)
			}
			continue
		}
		u, err := h.Sample(inverse.Registered)
		mapped := inverse.Registered.Add(u)
		if err != nil || !f.inside(f.ContinuousIndex(mapped)) {
			if valid[i] {
				t.Fatalf("voxel %d valid outside the target", i)
			}
			continue
		}
		if !valid[i] || field.Vectors[i] != mapped.Sub(p) {
			t.Fatalf("voxel %d: %v (valid %v), want %v", i, field.Vectors[i], valid[i], mapped.Sub(p))
		}
		wantMaximum = math.Max(wantMaximum, inverse.Residual)
	}
	if maximum != wantMaximum {
		t.Fatalf("maximum residual %g, want %g", maximum, wantMaximum)
	}
}
