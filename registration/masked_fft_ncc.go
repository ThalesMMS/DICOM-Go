package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/cmplx"

	"github.com/ThalesMMS/dicom-go/internal/fft"
	"github.com/ThalesMMS/dicom-go/render"
)

// ErrMaskedFFT denotes invalid input, geometry or resource admission.
var ErrMaskedFFT = errors.New("registration: masked FFT admission")

// ErrMaskedFFTNoOverlap denotes no candidate with sufficient actual overlap.
var ErrMaskedFFTNoOverlap = errors.New("registration: masked FFT insufficient overlap")

// ErrMaskedFFTVariance denotes sufficient overlap but no numerically resolvable variance.
var ErrMaskedFFTVariance = errors.New("registration: masked FFT zero or unresolved variance")

// MaskedFFTSearch is an inclusive LPS box on the fixed-to-moving translation.
type MaskedFFTSearch struct{ Min, Max render.Vec3 }

// MaskedFFTOptions controls linear masked NCC. Zero limits use documented defaults.
// Workers bounds the goroutines of the FFT line transforms (zero: the module's
// shared concurrency budget); at most one works per 2^18 padded voxels. The
// result does not depend on it.
type MaskedFFTOptions struct {
	MinOverlap                                            int
	MinOverlapFraction                                    float64 // relative to the smaller included mask population
	Search                                                *MaskedFFTSearch
	AmbiguityTolerance                                    float64 // score distance to the best distinct integer shift
	Workers                                               int
	MaxInputVoxels, MaxPaddedVoxels, MaxBytes, MaxFFTWork uint64
}

// MaskedFFTResult is a diagnostic integer translation proposal, not confidence.
// MovingIndex = FixedIndex + IndexShift; TranslationLPS maps a fixed physical
// point to its corresponding moving point. No image or transform is modified.
type MaskedFFTResult struct {
	IndexShift                                [3]int
	TranslationLPS                            render.Vec3
	Score                                     float64
	Overlap                                   int
	OverlapFraction                           float64
	Ambiguous, SearchBoundary, LinearBoundary bool
	// Usable is false for repeated near-best peaks or an extreme linear border.
	Usable                bool
	PaddedSize            [3]int
	ScratchBytes, FFTWork uint64
}

// MaskedFFTWorkspace reports the kernel's admitted logical scratch and FFT-work
// proxy before execution. Batch callers can account even inconclusive kernels,
// whose zero result deliberately carries no successful proposal metadata.
// This validates geometry/limits, not sample finiteness or useful overlap.
func MaskedFFTWorkspace(f, m, fm, mm *Image, o MaskedFFTOptions) (scratchBytes, fftWork uint64, err error) {
	p, err := maskedFFTAdmission(f, m, fm, mm, o)
	return p.bytes, p.work, err
}

type maskedFFTPlan struct {
	size             [3]int
	n, line, workers int
	bytes, work      uint64
	options          MaskedFFTOptions
}

func maskedFFTFail(s string) error { return fmt.Errorf("%w: %s", ErrMaskedFFT, s) }
func maskedFFTProduct(size [3]int) (uint64, error) {
	n := uint64(1)
	for _, d := range size {
		if d < 1 || uint64(d) > math.MaxUint64/n {
			return 0, maskedFFTFail("size overflow")
		}
		n *= uint64(d)
	}
	return n, nil
}
func maskedFFTGeometry(a, b *Image, origin bool) bool {
	if a == nil || b == nil {
		return false
	}
	if origin && a.origin != b.origin {
		return false
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			x, y := a.indexToPhysical[r][c], b.indexToPhysical[r][c]
			if !finite(x) || !finite(y) || math.Abs(x-y) > 1e-10*math.Max(math.Abs(x), math.Abs(y)) {
				return false
			}
		}
	}
	return finiteVec(a.origin) && finiteVec(b.origin)
}
func maskedFFTAdmission(f, m, fm, mm *Image, options MaskedFFTOptions) (maskedFFTPlan, error) {
	p := maskedFFTPlan{options: options}
	o := &p.options
	if f == nil || m == nil || fm == nil || mm == nil {
		return p, maskedFFTFail("two images and two masks required")
	}
	if !maskedFFTGeometry(f, m, false) || !maskedFFTGeometry(f, fm, true) || !maskedFFTGeometry(m, mm, true) || f.size != fm.size || m.size != mm.size || (f.size[2] == 1) != (m.size[2] == 1) {
		return p, maskedFFTFail("incompatible image/mask grids")
	}
	if o.MinOverlap == 0 {
		o.MinOverlap = 3
	}
	if o.MinOverlapFraction == 0 {
		o.MinOverlapFraction = .1
	}
	if o.AmbiguityTolerance == 0 {
		o.AmbiguityTolerance = 1e-8
	}
	if o.MinOverlap < 3 || !finite(o.MinOverlapFraction) || o.MinOverlapFraction < 0 || o.MinOverlapFraction > 1 || !finite(o.AmbiguityTolerance) || o.AmbiguityTolerance < 0 || o.AmbiguityTolerance > 2 || o.Workers < 0 {
		return p, maskedFFTFail("invalid options")
	}
	if o.Search != nil {
		if !finiteVec(o.Search.Min) || !finiteVec(o.Search.Max) || o.Search.Min.X > o.Search.Max.X || o.Search.Min.Y > o.Search.Max.Y || o.Search.Min.Z > o.Search.Max.Z {
			return p, maskedFFTFail("invalid physical search")
		}
		v := *o.Search
		o.Search = &v
	}
	if o.MaxInputVoxels == 0 {
		o.MaxInputVoxels = 1 << 22
	}
	if o.MaxPaddedVoxels == 0 {
		o.MaxPaddedVoxels = 1 << 22
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 768 << 20
	}
	if o.MaxFFTWork == 0 {
		o.MaxFFTWork = 1 << 32
	}
	fn, e := maskedFFTProduct(f.size)
	if e != nil {
		return p, e
	}
	mn, e := maskedFFTProduct(m.size)
	if e != nil {
		return p, e
	}
	if fn > o.MaxInputVoxels || mn > o.MaxInputVoxels-fn || fn > uint64(len(f.data)) || mn > uint64(len(m.data)) || fn != uint64(len(f.data)) || mn != uint64(len(m.data)) || fn != uint64(len(fm.data)) || mn != uint64(len(mm.data)) {
		return p, maskedFFTFail("input voxel budget or sample lengths")
	}
	n := uint64(1)
	lg := uint64(0)
	maxInt := uint64(^uint(0) >> 1)
	for axis := 0; axis < 3; axis++ {
		need := uint64(f.size[axis]) + uint64(m.size[axis]) - 1
		d := uint64(1)
		for d < need {
			if d > maxInt/2 {
				return p, maskedFFTFail("padding overflow")
			}
			d *= 2
			lg++
		}
		if d > o.MaxPaddedVoxels || n > o.MaxPaddedVoxels/d {
			return p, maskedFFTFail("padded voxel budget")
		}
		n *= d
		p.size[axis] = int(d)
		p.line = max(p.line, int(d))
	}
	// Six complex spectra + complex inverse scratch + six real moment maps,
	// and one complex axis line per FFT worker. Slice/object headers and
	// runtime excluded.
	if n > maxInt {
		return p, maskedFFTFail("scratch overflow")
	}
	p.workers = fft.AxisWorkers(int(n), o.Workers)
	lines := uint64(p.line) * uint64(p.workers)
	if n > math.MaxUint64/160 || lines/uint64(p.workers) != uint64(p.line) || lines > (math.MaxUint64-160*n)/16 {
		return p, maskedFFTFail("scratch overflow")
	}
	p.bytes = 160*n + 16*lines
	if lg != 0 && n > math.MaxUint64/(12*lg) {
		return p, maskedFFTFail("FFT work overflow")
	}
	p.work = 12 * n * lg
	if p.bytes > o.MaxBytes || p.work > o.MaxFFTWork {
		return p, maskedFFTFail("scratch or FFT work budget")
	}
	p.n = int(n)
	return p, nil
}

// EstimateMaskedFFTTranslation computes six linear cross-correlations of the
// two independent masks and their first/second intensity moments. Inputs must
// remain unchanged for the duration of the call. Cancellation publishes zero.
func EstimateMaskedFFTTranslation(ctx context.Context, f, m, fm, mm *Image, o MaskedFFTOptions) (MaskedFFTResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return MaskedFFTResult{}, e
	}
	p, e := maskedFFTAdmission(f, m, fm, mm, o)
	if e != nil {
		return MaskedFFTResult{}, e
	}
	counts := [2]int{}
	centers := [2]float64{}
	scales := [2]float64{}
	for side, im := range []*Image{f, m} {
		mask := []*Image{fm, mm}[side]
		// Scan all values, including excluded samples: nonfinite never hides in a mask.
		for i, v := range im.data {
			if i&1023 == 0 {
				if e := ctx.Err(); e != nil {
					return MaskedFFTResult{}, e
				}
			}
			if !finite(float64(v)) || !finite(float64(mask.data[i])) {
				return MaskedFFTResult{}, maskedFFTFail("nonfinite sample or mask")
			}
			if mask.data[i] > 0 {
				counts[side]++
				centers[side] += float64(v)
			}
		}
		if counts[side] == 0 {
			return MaskedFFTResult{}, ErrMaskedFFTNoOverlap
		}
		centers[side] /= float64(counts[side])
		for i, v := range im.data {
			if i&1023 == 0 {
				if e := ctx.Err(); e != nil {
					return MaskedFFTResult{}, e
				}
			}
			if mask.data[i] > 0 {
				scales[side] = math.Max(scales[side], math.Abs(float64(v)-centers[side]))
			}
		}
		if scales[side] == 0 {
			return MaskedFFTResult{}, ErrMaskedFFTVariance
		}
	}
	if e := ctx.Err(); e != nil {
		return MaskedFFTResult{}, e
	}
	var spectra [6][]complex128
	for i := range spectra {
		spectra[i] = make([]complex128, p.n)
	}
	scratch := make([]complex128, p.n)
	var moments [6][]float64
	for i := range moments {
		moments[i] = make([]float64, p.n)
	}
	for side, im := range []*Image{f, m} {
		mask := []*Image{fm, mm}[side]
		for z := 0; z < im.size[2]; z++ {
			for y := 0; y < im.size[1]; y++ {
				if e := ctx.Err(); e != nil {
					return MaskedFFTResult{}, e
				}
				for x := 0; x < im.size[0]; x++ {
					if x&1023 == 0 {
						if e := ctx.Err(); e != nil {
							return MaskedFFTResult{}, e
						}
					}
					i := (z*im.size[1]+y)*im.size[0] + x
					if mask.data[i] <= 0 {
						continue
					}
					j := (z*p.size[1]+y)*p.size[0] + x
					v := (float64(im.data[i]) - centers[side]) / scales[side]
					spectra[3*side][j] = 1
					spectra[3*side+1][j] = complex(v, 0)
					spectra[3*side+2][j] = complex(v*v, 0)
				}
			}
		}
	}
	for i := range spectra {
		if e := maskedFFTAxes(ctx, spectra[i], p.size, false, p.workers); e != nil {
			return MaskedFFTResult{}, e
		}
	}
	pairs := [6][2]int{{0, 3}, {1, 3}, {0, 4}, {2, 3}, {0, 5}, {1, 4}}
	for k, pair := range pairs {
		for i := range scratch {
			if i&1023 == 0 {
				if e := ctx.Err(); e != nil {
					return MaskedFFTResult{}, e
				}
			}
			scratch[i] = cmplx.Conj(spectra[pair[0]][i]) * spectra[pair[1]][i]
		}
		if e := maskedFFTAxes(ctx, scratch, p.size, true, p.workers); e != nil {
			return MaskedFFTResult{}, e
		}
		for i, v := range scratch {
			if i&1023 == 0 {
				if e := ctx.Err(); e != nil {
					return MaskedFFTResult{}, e
				}
			}
			if !finite(real(v)) || !finite(imag(v)) {
				return MaskedFFTResult{}, maskedFFTFail("nonfinite FFT")
			}
			moments[k][i] = real(v)
		}
	}
	minimum := max(p.options.MinOverlap, int(math.Ceil(p.options.MinOverlapFraction*float64(min(counts[0], counts[1])))))
	result := MaskedFFTResult{PaddedSize: p.size, ScratchBytes: p.bytes, FFTWork: p.work}
	found, overlapSeen := false, false
	// Two deterministic z/y/x passes avoid an unbounded candidate list.
	ties := 0
	for pass := 0; pass < 2; pass++ {
		for z := 1 - f.size[2]; z < m.size[2]; z++ {
			for y := 1 - f.size[1]; y < m.size[1]; y++ {
				for x := 1 - f.size[0]; x < m.size[0]; x++ {
					if e := ctx.Err(); e != nil {
						return MaskedFFTResult{}, e
					}
					s := [3]int{x, y, z}
					t := m.origin.Sub(f.origin).Add(f.indexToPhysical.MulVec(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
					if !finiteVec(t) {
						return MaskedFFTResult{}, maskedFFTFail("nonfinite physical translation")
					}
					if !maskedFFTInSearch(t, p.options.Search) {
						continue
					}
					ix, iy, iz := x, y, z
					if ix < 0 {
						ix += p.size[0]
					}
					if iy < 0 {
						iy += p.size[1]
					}
					if iz < 0 {
						iz += p.size[2]
					}
					i := (iz*p.size[1]+iy)*p.size[0] + ix
					nf := math.Round(moments[0][i])
					if nf < float64(minimum) {
						continue
					}
					overlapSeen = true
					if nf > float64(min(counts[0], counts[1])) || math.Abs(moments[0][i]-nf) > 1e-5 {
						return MaskedFFTResult{}, maskedFFTFail("unresolved FFT overlap")
					}
					vf := moments[3][i] - moments[1][i]*moments[1][i]/nf
					vm := moments[4][i] - moments[2][i]*moments[2][i]/nf
					if vf <= 1e-10*nf || vm <= 1e-10*nf {
						continue
					}
					score := (moments[5][i] - moments[1][i]*moments[2][i]/nf) / math.Sqrt(vf*vm)
					if !finite(score) || score < -1-1e-6 || score > 1+1e-6 {
						return MaskedFFTResult{}, maskedFFTFail("unresolved NCC")
					}
					score = math.Max(-1, math.Min(1, score))
					if pass == 0 {
						if !found || score > result.Score+1e-12 {
							found = true
							result.IndexShift = s
							result.TranslationLPS = t
							result.Score = score
							result.Overlap = int(nf)
							result.OverlapFraction = nf / float64(min(counts[0], counts[1]))
						}
					} else if found && score >= result.Score-p.options.AmbiguityTolerance {
						ties++
					}
				}
			}
		}
	}
	if !found {
		if overlapSeen {
			return MaskedFFTResult{}, ErrMaskedFFTVariance
		}
		return MaskedFFTResult{}, ErrMaskedFFTNoOverlap
	}
	result.Ambiguous = ties > 1
	for axis, d := range result.IndexShift {
		if min(f.size[axis], m.size[axis]) > 1 && (d == 1-f.size[axis] || d == m.size[axis]-1) {
			result.LinearBoundary = true
		}
		if p.options.Search != nil && (f.size[axis] > 1 || m.size[axis] > 1) {
			for _, step := range []int{-1, 1} {
				s := result.IndexShift
				s[axis] += step
				t := m.origin.Sub(f.origin).Add(f.indexToPhysical.MulVec(render.Vec3{X: float64(s[0]), Y: float64(s[1]), Z: float64(s[2])}))
				if !maskedFFTInSearch(t, p.options.Search) {
					result.SearchBoundary = true
				}
			}
		}
	}
	result.Usable = !result.Ambiguous && !result.LinearBoundary
	if e := ctx.Err(); e != nil {
		return MaskedFFTResult{}, e
	}
	return result, nil
}
func maskedFFTInSearch(t render.Vec3, s *MaskedFFTSearch) bool {
	return s == nil || (t.X >= s.Min.X && t.X <= s.Max.X && t.Y >= s.Min.Y && t.Y <= s.Max.Y && t.Z >= s.Min.Z && t.Z <= s.Max.Z)
}
