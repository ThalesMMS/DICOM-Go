package mrdiffusion

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/linalg"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"sync/atomic"
	"unsafe"
)

var ErrTensor = errors.New("mrdiffusion: invalid tensor reconstruction")
var ErrTensorRank = errors.New("mrdiffusion: tensor design rank insufficient")

// TensorSource records one contributing image/plane, with explicit series and
// optional 1-based enhanced frame number (zero means entire single-frame SOP).
type TensorSource struct {
	StudyInstanceUID, SeriesInstanceUID, SOPClassUID, SOPInstanceUID string
	FrameNumber                                                      int
}
type TensorVolume struct {
	Geometry TensorGeometry
	Encoding TensorEncoding
	Signals  []float32
	Sources  []TensorSource
}
type TensorNegativePolicy uint8

const (
	TensorRejectNegative TensorNegativePolicy = iota
	TensorClampNegative
)

type TensorStatus uint8

const (
	TensorValid TensorStatus = iota
	TensorMasked
	TensorLowSignal
	TensorNonfinite
	TensorNegative
	TensorNumerical
)

type TensorOptions struct {
	Mask                                                     []bool
	NoiseFloor, MinSignal, NegativeTolerance, MaxDiffusivity float64
	NegativePolicy                                           TensorNegativePolicy
	Workers                                                  int
	MaxVolumes, MaxVoxels, MaxBytes                          uint64
}

// TensorVoxel holds LPS tensor quantities. RGB = FA*abs(principal LPS axes),
// quantized to [0,255]; repeated largest eigenvalues leave direction undefined.
type TensorVoxel struct {
	Tensor                    Tensor3
	Eigenvalues               [3]float64
	FA, MD                    float64
	Direction                 render.Vec3
	RGB                       [3]uint8
	Status                    TensorStatus
	DirectionDefined, Clamped bool
}

// TensorResult owns outputs and provenance. No borrowed signals/mask retained.
// Callers must not mutate Voxels concurrently with writing or reading maps.
type TensorResult struct {
	Voxels                      []TensorVoxel
	Counts                      [6]int
	DuplicateEncodings, Clamped int
	DesignDiagonalRatio         float64
	geometry                    TensorGeometry
	encodings                   []Tensor3
	sources                     [][]TensorSource
	options                     TensorOptions
	logicalBytes                uint64
}

func (r TensorResult) Geometry() TensorGeometry { return r.geometry }
func (r TensorResult) LogicalBytes() uint64     { return r.logicalBytes }

// FitTensor reconstructs S_i=S0*exp(-B_i:D) by unweighted log-linear least
// squares anchored to arithmetic mean corrected b=0 signal. No Rician fitting.
func FitTensor(ctx context.Context, volumes []TensorVolume, o TensorOptions) (TensorResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return TensorResult{}, e
	}
	if o.MaxVolumes == 0 {
		o.MaxVolumes = 256
	}
	if o.MaxVoxels == 0 {
		o.MaxVoxels = 1 << 24
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 512 << 20
	}
	if o.MaxDiffusivity == 0 {
		o.MaxDiffusivity = .1
	}
	if o.NegativeTolerance == 0 {
		o.NegativeTolerance = 1e-10
	}
	if len(volumes) < 7 || uint64(len(volumes)) > o.MaxVolumes || len(volumes) > 4096 || o.Workers < 0 || !tensorFinite(o.NoiseFloor) || o.NoiseFloor < 0 || !tensorFinite(o.MinSignal) || o.MinSignal < 0 || !tensorFinite(o.NegativeTolerance) || o.NegativeTolerance < 0 || !tensorFinite(o.MaxDiffusivity) || o.MaxDiffusivity <= 0 || o.NegativePolicy > TensorClampNegative {
		return TensorResult{}, tensorError("options or channel count")
	}
	n, e := tensorGrid(volumes[0].Geometry)
	if e != nil {
		return TensorResult{}, e
	}
	if uint64(n) > o.MaxVoxels || o.Mask != nil && len(o.Mask) != n {
		return TensorResult{}, tensorError("voxel/mask admission")
	}
	// Includes borrowed input sample payload in the request admission, output
	// struct payload, design/QR/provenance payload and conservative worker scratch.
	workers := 1 + min(63, parallel.Budget())
	if o.Workers > 0 {
		workers = min(workers, o.Workers)
	}
	workers = min(workers, max(1, (n+127)/128))
	bytesPerVoxel := uint64(len(volumes))*4 + uint64(unsafe.Sizeof(TensorVoxel{})) + 1
	if uint64(n) > math.MaxUint64/bytesPerVoxel {
		return TensorResult{}, tensorError("byte overflow")
	}
	budget := uint64(n) * bytesPerVoxel
	if !tensorBudgetAdd(&budget, uint64(len(volumes))*256+uint64(workers)*4096, o.MaxBytes) {
		return TensorResult{}, tensorError("byte budget")
	}
	for _, v := range volumes {
		if !tensorSameGrid(v.Geometry, volumes[0].Geometry) || len(v.Signals) != n || len(v.Sources) != 0 && len(v.Sources) != v.Geometry.Size[2] {
			return TensorResult{}, tensorError("incompatible volume/source geometry")
		}
		for i, s := range v.Sources {
			if i&63 == 0 {
				if e := ctx.Err(); e != nil {
					return TensorResult{}, e
				}
			}
			if len(s.StudyInstanceUID) > 64 || len(s.SeriesInstanceUID) > 64 || len(s.SOPClassUID) > 64 || len(s.SOPInstanceUID) > 64 || s.FrameNumber < 0 {
				return TensorResult{}, tensorError("source metadata")
			}
			if !tensorBudgetAdd(&budget, 384, o.MaxBytes) {
				return TensorResult{}, tensorError("source byte budget")
			}
		}
	}
	if budget > o.MaxBytes {
		return TensorResult{}, tensorError("byte budget")
	}
	rows := make([]Tensor3, len(volumes))
	b0 := []int{}
	weighted := []int{}
	maxB := 0.
	duplicates := 0
	for i, v := range volumes {
		if e := ctx.Err(); e != nil {
			return TensorResult{}, e
		}
		b, e := tensorEncodingLPS(v.Encoding, v.Geometry)
		if e != nil {
			return TensorResult{}, fmt.Errorf("volume%d: %w", i, e)
		}
		rows[i] = b
		if v.Encoding.BValue == 0 {
			b0 = append(b0, i)
		} else {
			for index, previous := range weighted {
				if index&63 == 0 {
					if e := ctx.Err(); e != nil {
						return TensorResult{}, e
					}
				}
				same := true
				for k, x := range b {
					if math.Abs(x-rows[previous][k]) > 1e-8*math.Max(v.Encoding.BValue, volumes[previous].Encoding.BValue) {
						same = false
						break
					}
				}
				if same {
					duplicates++
					break
				}
			}
			weighted = append(weighted, i)
			maxB = math.Max(maxB, v.Encoding.BValue)
		}
	}
	if len(b0) == 0 {
		return TensorResult{}, tensorError("b=0 reference missing")
	}
	if len(weighted) < 6 {
		return TensorResult{}, ErrTensorRank
	}
	q := make([][6]float64, len(weighted))
	for i, j := range weighted {
		for c := 0; c < 6; c++ {
			q[i][c] = rows[j][c] / maxB
			if c >= 3 {
				q[i][c] *= 2
			}
		}
	}
	var r [6][6]float64
	small, large := math.Inf(1), 0.
	// Reorthogonalized modified Gram-Schmidt; rank cutoff on normalized design.
	for col := 0; col < 6; col++ {
		for pass := 0; pass < 2; pass++ {
			for prev := 0; prev < col; prev++ {
				dot := 0.
				for i := range q {
					if i&63 == 0 {
						if e := ctx.Err(); e != nil {
							return TensorResult{}, e
						}
					}
					dot += q[i][col] * q[i][prev]
				}
				r[prev][col] += dot
				for i := range q {
					q[i][col] -= dot * q[i][prev]
				}
			}
		}
		norm := 0.
		for i := range q {
			norm += q[i][col] * q[i][col]
		}
		norm = math.Sqrt(norm)
		if !tensorFinite(norm) || norm <= 1e-10*math.Sqrt(float64(len(q))) {
			return TensorResult{}, ErrTensorRank
		}
		r[col][col] = norm
		small = math.Min(small, norm)
		large = math.Max(large, norm)
		for i := range q {
			q[i][col] /= norm
		}
	}
	result := TensorResult{geometry: volumes[0].Geometry, encodings: append([]Tensor3(nil), rows...), sources: make([][]TensorSource, len(volumes)), options: o, logicalBytes: budget, DuplicateEncodings: duplicates, DesignDiagonalRatio: large / small, Voxels: make([]TensorVoxel, n)}
	result.options.Mask = nil
	for i, v := range volumes {
		result.sources[i] = append([]TensorSource(nil), v.Sources...)
	}
	var counts [6]atomic.Int64
	var clamped atomic.Int64
	err := parallel.Run(ctx, (n+127)/128, workers, 1, func() func(int) {
		return func(chunk int) {
			for voxel := chunk * 128; voxel < min(n, (chunk+1)*128); voxel++ {
				if ctx.Err() != nil {
					return
				}
				out := TensorVoxel{}
				if o.Mask != nil && !o.Mask[voxel] {
					out.Status = TensorMasked
				} else {
					s0 := 0.
					status := TensorValid
					for i, v := range volumes {
						if i&63 == 0 && ctx.Err() != nil {
							return
						}
						s := float64(v.Signals[voxel])
						if !tensorFinite(s) {
							status = TensorNonfinite
							break
						}
						if s-o.NoiseFloor <= o.MinSignal {
							status = TensorLowSignal
							break
						}
					}
					if status == TensorValid {
						for index, i := range b0 {
							if index&63 == 0 && ctx.Err() != nil {
								return
							}
							s0 += float64(volumes[i].Signals[voxel]) - o.NoiseFloor
						}
						s0 /= float64(len(b0))
						var rhs [6]float64
						for row, i := range weighted {
							if row&63 == 0 && ctx.Err() != nil {
								return
							}
							y := math.Log(s0) - math.Log(float64(volumes[i].Signals[voxel])-o.NoiseFloor)
							for c := 0; c < 6; c++ {
								rhs[c] += q[row][c] * y
							}
						}
						var d Tensor3
						for col := 5; col >= 0; col-- {
							v := rhs[col]
							for j := col + 1; j < 6; j++ {
								v -= r[col][j] * d[j]
							}
							d[col] = v / r[col][col]
						}
						for c := range d {
							d[c] /= maxB
						}
						out = tensorQuantities(d, o)
						status = out.Status
					}
					out.Status = status
				}
				result.Voxels[voxel] = out
				counts[out.Status].Add(1)
				if out.Clamped {
					clamped.Add(1)
				}
			}
		}
	})
	if err != nil {
		return TensorResult{}, err
	}
	if e := ctx.Err(); e != nil {
		return TensorResult{}, e
	}
	for i := range counts {
		result.Counts[i] = int(counts[i].Load())
	}
	result.Clamped = int(clamped.Load())
	if e := ctx.Err(); e != nil {
		return TensorResult{}, e
	}
	return result, nil
}
func tensorQuantities(d Tensor3, o TensorOptions) TensorVoxel {
	out := TensorVoxel{Status: TensorNumerical}
	scale := 0.
	for _, v := range d {
		if !tensorFinite(v) {
			return out
		}
		scale = math.Max(scale, math.Abs(v))
	}
	if scale == 0 {
		out.Tensor = d
		out.Status = TensorValid
		return out
	}
	normalized := d
	for i := range normalized {
		normalized[i] /= scale
	}
	w, v, e := linalg.SymmetricEigen3(tensorMatrix(normalized))
	if e != nil {
		return out
	}
	for i := range w {
		w[i] *= scale
		if !tensorFinite(w[i]) || math.Abs(w[i]) > o.MaxDiffusivity {
			return out
		}
	}
	if w[0] < -o.NegativeTolerance && o.NegativePolicy == TensorRejectNegative {
		out.Status = TensorNegative
		return out
	}
	for i := range w {
		if w[i] < 0 {
			w[i] = 0
			out.Clamped = true
		}
	}
	if out.Clamped {
		var m [3][3]float64
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				for k := 0; k < 3; k++ {
					m[i][j] += v[i][k] * w[k] * v[j][k]
				}
			}
		}
		d = tensorPack(m)
	}
	out.Tensor = d
	out.Eigenvalues = [3]float64{w[2], w[1], w[0]}
	out.MD = w[0]/3 + w[1]/3 + w[2]/3
	if w[2] > 0 {
		a, b, c := w[0]/w[2], w[1]/w[2], 1.
		mean := (a + b + c) / 3
		out.FA = math.Min(1, math.Sqrt(1.5*((a-mean)*(a-mean)+(b-mean)*(b-mean)+(c-mean)*(c-mean))/(a*a+b*b+c*c)))
	}
	if !tensorFinite(out.MD) || !tensorFinite(out.FA) {
		return TensorVoxel{Status: TensorNumerical}
	}
	out.Status = TensorValid
	if w[2] > 0 && w[2]-w[1] > 1e-8*w[2] {
		out.DirectionDefined = true
		components := [3]float64{v[0][2], v[1][2], v[2][2]}
		pivot := 0
		for i := 1; i < 3; i++ {
			if math.Abs(components[i]) > math.Abs(components[pivot]) {
				pivot = i
			}
		}
		if components[pivot] < 0 {
			for i := range components {
				components[i] = -components[i]
			}
		}
		out.Direction = render.Vec3{X: components[0], Y: components[1], Z: components[2]}
		for i, x := range components {
			out.RGB[i] = uint8(math.Round(255 * math.Min(1, out.FA) * math.Abs(x)))
		}
	}
	return out
}

func tensorBudgetAdd(total *uint64, more, limit uint64) bool {
	if *total > limit || more > limit-*total {
		return false
	}
	*total += more
	return true
}
