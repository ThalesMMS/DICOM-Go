package roi

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// PCAShapeTrainingSample is a registered canonical negative-inside mm field.
// Training does not estimate pose or silently regrid/normalize any sample.
type PCAShapeTrainingSample struct {
	ID       string
	Geometry PCAShapeGeometry
	FieldMM  []float64
}
type PCAShapeTrainOptions struct {
	ID, Provenance string
	// Components is the largest retained rank; zero uses the component limit.
	// Numerically zero eigenvalues are discarded, not amplified as components.
	Components int
	Limits     PCAShapeLimits
}

// TrainPCAShapeModel uses sample covariance (M-1), a bounded symmetric Jacobi
// eigensolve on the M×M dual Gram matrix, then unit-L2 voxel-space components.
// The sign is fixed by the largest-magnitude component entry. No labels,
// patients, automatic registration or model selection are inferred here.
func TrainPCAShapeModel(ctx context.Context, samples []PCAShapeTrainingSample, o PCAShapeTrainOptions) (*PCAShapeModel, error) {
	ctx = pcaContext(ctx)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	l, e := o.Limits.normalize()
	if e != nil {
		return nil, e
	}
	m := len(samples)
	if m < 2 || o.ID == "" || o.Provenance == "" || len(o.ID) > 1024 || len(o.Provenance) > 4096 || o.Components < 0 {
		return nil, ErrPCAShapeInvalid
	}
	if m > l.MaxSamples || o.Components > l.MaxComponents {
		return nil, ErrPCAShapeLimit
	}
	k := o.Components
	if k == 0 {
		k = l.MaxComponents
	}
	k = min(k, m-1)
	g := samples[0].Geometry
	n, e := g.count(l)
	if e != nil {
		return nil, e
	}
	// Include the constructor's owned clone and retained Gram/eigenvector
	// matrices. Count validation, covariance, component and orthogonality work.
	bytes := int64(n)*int64(k+1)*16 + int64(m*m)*16 + int64(m)*64 + 16384
	work := int64(n)*int64(m*m+m*k+k*k+2*m+2*k+4) + int64(m*m*m)*64
	if bytes > l.MaxBytes || work > l.MaxWork {
		return nil, ErrPCAShapeLimit
	}
	ids := map[string]bool{}
	for _, s := range samples {
		if s.ID == "" || len(s.ID) > 1024 || ids[s.ID] || s.Geometry != g {
			return nil, fmt.Errorf("%w: sample identity/geometry", ErrPCAShapeInvalid)
		}
		ids[s.ID] = true
		if e = pcaField(ctx, s.FieldMM, n, true); e != nil {
			return nil, e
		}
	}
	mean := make([]float64, n)
	for i := range mean {
		if i%4096 == 0 {
			if e = ctx.Err(); e != nil {
				return nil, e
			}
		}
		for _, s := range samples {
			mean[i] += s.FieldMM[i] / float64(m)
		}
	}
	gram := make([]float64, m*m)
	for a := 0; a < m; a++ {
		for b := 0; b <= a; b++ {
			sum := 0.0
			for i, mu := range mean {
				if i%4096 == 0 {
					if e = ctx.Err(); e != nil {
						return nil, e
					}
				}
				sum += (samples[a].FieldMM[i] - mu) * (samples[b].FieldMM[i] - mu)
			}
			gram[a*m+b] = sum / float64(m-1)
			gram[b*m+a] = gram[a*m+b]
		}
	}
	values, vectors, e := pcaJacobi(ctx, gram, m)
	if e != nil {
		return nil, e
	}
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return values[order[a]] > values[order[b]] })
	d := PCAShapeModelData{Version: 1, ID: o.ID, Provenance: o.Provenance, Convention: PCAShapeConvention, Geometry: g, TrainingSamples: m, MeanMM: mean}
	maximum := math.Max(0, values[order[0]])
	for _, a := range order {
		lambda := values[a]
		if len(d.Components) >= k || lambda <= math.Max(1e-18, maximum*1e-12) {
			break
		}
		u := make([]float64, n)
		denom := math.Sqrt(float64(m-1) * lambda)
		norm := 0.0
		largest := 0
		for i, mu := range mean {
			if i%4096 == 0 {
				if e = ctx.Err(); e != nil {
					return nil, e
				}
			}
			for j, s := range samples {
				u[i] += (s.FieldMM[i] - mu) * vectors[j*m+a] / denom
			}
			norm += u[i] * u[i]
			if math.Abs(u[i]) > math.Abs(u[largest]) {
				largest = i
			}
		}
		if !(norm > 0) || !pcaFinite(norm) {
			return nil, ErrPCAShapeInvalid
		}
		scale := 1 / math.Sqrt(norm)
		if u[largest] < 0 {
			scale = -scale
		}
		for i := range u {
			if i%4096 == 0 {
				if e = ctx.Err(); e != nil {
					return nil, e
				}
			}
			u[i] *= scale
		}
		d.Components = append(d.Components, u)
		d.StandardDeviationsMM = append(d.StandardDeviationsMM, math.Sqrt(lambda))
	}
	return NewPCAShapeModel(ctx, d, l)
}

// Cyclic Jacobi rotations diagonalize a small real symmetric matrix. The
// fixed 64-sweep bound and per-rotation cancellation prevent unbounded fitting.
func pcaJacobi(ctx context.Context, a []float64, n int) ([]float64, []float64, error) {
	v := make([]float64, n*n)
	for i := 0; i < n; i++ {
		v[i*n+i] = 1
	}
	for sweep := 0; sweep < 64; sweep++ {
		if e := ctx.Err(); e != nil {
			return nil, nil, e
		}
		off, diag := 0.0, 0.0
		for i := 0; i < n; i++ {
			diag = math.Max(diag, math.Abs(a[i*n+i]))
			for j := i + 1; j < n; j++ {
				off = math.Max(off, math.Abs(a[i*n+j]))
			}
		}
		if off <= math.Max(1e-20, diag*1e-13) {
			values := make([]float64, n)
			for i := range values {
				values[i] = a[i*n+i]
			}
			return values, v, nil
		}
		for p := 0; p < n; p++ {
			for q := p + 1; q < n; q++ {
				if e := ctx.Err(); e != nil {
					return nil, nil, e
				}
				apq := a[p*n+q]
				if math.Abs(apq) <= math.Max(1e-22, diag*1e-15) {
					continue
				}
				tau := (a[q*n+q] - a[p*n+p]) / (2 * apq)
				t := math.Copysign(1/(math.Abs(tau)+math.Hypot(1, tau)), tau)
				c := 1 / math.Hypot(1, t)
				s := t * c
				app, aqq := a[p*n+p], a[q*n+q]
				a[p*n+p] = app - t*apq
				a[q*n+q] = aqq + t*apq
				a[p*n+q] = 0
				a[q*n+p] = 0
				for k := 0; k < n; k++ {
					if k != p && k != q {
						akp, akq := a[k*n+p], a[k*n+q]
						a[k*n+p] = c*akp - s*akq
						a[p*n+k] = a[k*n+p]
						a[k*n+q] = s*akp + c*akq
						a[q*n+k] = a[k*n+q]
					}
					vkp, vkq := v[k*n+p], v[k*n+q]
					v[k*n+p] = c*vkp - s*vkq
					v[k*n+q] = s*vkp + c*vkq
				}
			}
		}
	}
	return nil, nil, fmt.Errorf("%w: PCA eigensolve did not converge in 64 sweeps", ErrPCAShapeLimit)
}
