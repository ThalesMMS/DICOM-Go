package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Peak of a VOI (#1116): the highest mean value of a small sphere, SUVpeak
// when the values are SUV. PERCIST 1.0 (Wahl et al., J Nucl Med 2009;50
// Suppl 1:122S) defines SUVpeak on a 1 cm³ sphere (radius 6.2 mm) placed in
// the lesion where its mean is highest; implementations differ on where the
// sphere may be centred, so PeakSearch makes the choice explicit.
//
// The sphere is centred on voxel centres. Because the grid is regular, one
// kernel (offsets and weights) rasterized around a voxel centre serves every
// candidate. By default the kernel has partial-volume weights, so a 1 cm³
// sphere keeps close to 1 cm³ on coarse PET voxels (with VOICentreInside a
// 4 mm grid would count whole voxels only). A candidate whose sphere leaves
// the grid or meets a voxel without a value is skipped; accepting it (mean
// over the available voxels) is opt-in through AllowIncompleteSphere and
// reported in PeakResult.

// DefaultPeakVolumeMM3 is the PERCIST peak sphere volume, 1 cm³.
const DefaultPeakVolumeMM3 = 1000

// MaxPeakVoxelReads bounds candidate centres × sphere voxels. A PET VOI of
// 5 cm radius on 2 mm voxels needs about 10⁷ reads; a large VOI on a CT grid
// can exceed the bound, and then PeakSearchAroundMaximum narrows the search.
const MaxPeakVoxelReads = 1 << 30

// PeakSearch selects the candidate centres of the peak sphere. Every
// candidate is a voxel whose centre lies inside the VOI and has a value.
type PeakSearch int

const (
	// PeakSearchVOI tries every candidate (PERCIST: the position of
	// highest mean in the lesion).
	PeakSearchVOI PeakSearch = iota
	// PeakSearchAroundMaximum tries the candidates within SearchRadiusMM of
	// the VOI's maximum voxel.
	PeakSearchAroundMaximum
	// PeakSearchAtMaximum centres the sphere on the maximum voxel.
	PeakSearchAtMaximum
)

func (s PeakSearch) String() string {
	switch s {
	case PeakSearchAroundMaximum:
		return "around the maximum"
	case PeakSearchAtMaximum:
		return "at the maximum"
	}
	return "whole VOI"
}

// PeakOptions configures VOIMask.Peak.
type PeakOptions struct {
	// VolumeMM3 is the sphere volume; 0 means DefaultPeakVolumeMM3.
	VolumeMM3 float64
	// CentreInsideKernel rasterizes the sphere with VOICentreInside instead
	// of partial-volume weights.
	CentreInsideKernel bool
	// Supersampling of the partial-volume kernel; 0 means
	// DefaultVOISupersampling.
	Supersampling int
	Search        PeakSearch
	// SearchRadiusMM bounds PeakSearchAroundMaximum; it must be positive
	// there and is ignored otherwise.
	SearchRadiusMM float64
	// AllowIncompleteSphere accepts spheres that leave the grid or include
	// voxels without a value, averaging the voxels available (opt-in,
	// reported by PeakResult.Incomplete).
	AllowIncompleteSphere bool
}

// PeakResult is the sphere of highest mean.
type PeakResult struct {
	// Mean is the weighted mean of the values inside the sphere.
	Mean float64
	// Center and CenterVoxel locate the sphere's centre.
	Center      render.Vec3
	CenterVoxel [3]int
	RadiusMM    float64
	// KernelVoxels and KernelVolumeMM3 describe the complete rasterized
	// sphere (Σ weight · voxel volume), to compare with VolumeMM3.
	KernelVoxels    int
	KernelVolumeMM3 float64
	VolumeMM3       float64
	Weighting       VOIWeighting
	Search          PeakSearch
	// Maximum and MaximumVoxel are the VOI maximum the search refers to.
	Maximum      float64
	MaximumVoxel [3]int
	// Candidates counts the centres evaluated; SkippedIncomplete those left
	// out because their sphere was incomplete.
	Candidates        int
	SkippedIncomplete int
	// Incomplete reports that the chosen sphere left the grid or met voxels
	// without a value (only with AllowIncompleteSphere); CoveredVolumeMM3 is
	// then the volume actually averaged.
	Incomplete       bool
	CoveredVolumeMM3 float64
}

// ErrNoPeak reports that no candidate centre yields a usable sphere.
var ErrNoPeak = errors.New("roi: no complete peak sphere is centred inside the VOI")

type kernelVoxel struct {
	di, dj, dk int
	weight     float64
}

// peakKernel rasterizes a sphere of radius around the centre of voxel
// (0,0,0) and returns index offsets and weights.
func peakKernel(ctx context.Context, t gridTransform, radius float64, weighting VOIWeighting, supersampling int) ([]kernelVoxel, error) {
	sphere := Sphere{Center: t.grid.Origin, RadiusMM: radius}
	lo, hi := primitiveIndexBox(t, sphere)
	box := indexBox{
		lo: [3]int{int(math.Floor(lo.X)), int(math.Floor(lo.Y)), int(math.Floor(lo.Z))},
		hi: [3]int{int(math.Ceil(hi.X)), int(math.Ceil(hi.Y)), int(math.Ceil(hi.Z))},
	}
	if box.count() > MaxVOIVoxels {
		return nil, fmt.Errorf("%w: the peak sphere covers more than %d voxels", ErrInvalidVOI, MaxVOIVoxels)
	}
	var kernel []kernelVoxel
	err := rasterizeBox(ctx, t, sphere, box, weighting, supersampling, func(i, j, k int, weight float64, _ bool) {
		kernel = append(kernel, kernelVoxel{di: i, dj: j, dk: k, weight: weight})
	})
	return kernel, err
}

type peakCandidate struct {
	voxel [3]int
	value float64
}

// Peak finds the sphere of highest mean value centred inside the VOI.
func (m *VOIMask) Peak(ctx context.Context, valueAt VoxelValueFunc, opts PeakOptions) (PeakResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || valueAt == nil {
		return PeakResult{}, fmt.Errorf("%w: peak needs a mask and a value accessor", ErrInvalidVOI)
	}
	t, err := m.Grid.transform()
	if err != nil {
		return PeakResult{}, err
	}
	volume := opts.VolumeMM3
	if volume == 0 {
		volume = DefaultPeakVolumeMM3
	}
	if !finitePositive(volume) {
		return PeakResult{}, fmt.Errorf("%w: peak volume %g mm³ must be finite and positive", ErrInvalidVOI, opts.VolumeMM3)
	}
	switch opts.Search {
	case PeakSearchVOI, PeakSearchAtMaximum:
	case PeakSearchAroundMaximum:
		if !finitePositive(opts.SearchRadiusMM) {
			return PeakResult{}, fmt.Errorf("%w: peak search radius %g mm must be finite and positive", ErrInvalidVOI, opts.SearchRadiusMM)
		}
	default:
		return PeakResult{}, fmt.Errorf("%w: unknown peak search %d", ErrInvalidVOI, opts.Search)
	}
	weighting := VOIPartialVolume
	if opts.CentreInsideKernel {
		weighting = VOICentreInside
	}
	supersampling, err := VOIRasterOptions{Weighting: weighting, Supersampling: opts.Supersampling}.supersampling()
	if err != nil {
		return PeakResult{}, err
	}
	radius := SphereRadiusForVolume(volume)
	kernel, err := peakKernel(ctx, t, radius, weighting, supersampling)
	if err != nil {
		return PeakResult{}, err
	}
	if len(kernel) == 0 {
		return PeakResult{}, fmt.Errorf("%w: the %g mm³ peak sphere contains no voxel centre; use partial-volume weights", ErrInvalidVOI, volume)
	}
	kernelWeight := 0.0
	for _, voxel := range kernel {
		kernelWeight += voxel.weight
	}
	voxelVolume := m.Grid.VoxelVolumeMM3()
	result := PeakResult{
		RadiusMM: radius, VolumeMM3: volume, Weighting: weighting, Search: opts.Search,
		KernelVoxels: len(kernel), KernelVolumeMM3: kernelWeight * voxelVolume,
	}

	candidates := make([]peakCandidate, 0, len(m.Voxels))
	maxIndex := -1
	for _, voxel := range m.Voxels {
		if !voxel.CentreInside {
			continue
		}
		v, ok := valueAt(voxel.Column, voxel.Row, voxel.Slice)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		candidates = append(candidates, peakCandidate{voxel: [3]int{voxel.Column, voxel.Row, voxel.Slice}, value: v})
		if maxIndex < 0 || v > candidates[maxIndex].value {
			maxIndex = len(candidates) - 1
		}
	}
	if maxIndex < 0 {
		return result, ErrEmptyVOI
	}
	maximum := candidates[maxIndex]
	result.Maximum, result.MaximumVoxel = maximum.value, maximum.voxel
	maxPoint := m.Grid.IndexToPoint(float64(maximum.voxel[0]), float64(maximum.voxel[1]), float64(maximum.voxel[2]))
	switch opts.Search {
	case PeakSearchAtMaximum:
		candidates = []peakCandidate{maximum}
	case PeakSearchAroundMaximum:
		near := candidates[:0:0]
		for _, c := range candidates {
			p := m.Grid.IndexToPoint(float64(c.voxel[0]), float64(c.voxel[1]), float64(c.voxel[2]))
			if p.Sub(maxPoint).Length() <= opts.SearchRadiusMM {
				near = append(near, c)
			}
		}
		candidates = near
	}

	if uint64(len(candidates))*uint64(len(kernel)) > MaxPeakVoxelReads {
		return result, fmt.Errorf("%w: %d candidate centres × %d sphere voxels exceed %d reads; search around the maximum", ErrInvalidVOI, len(candidates), len(kernel), MaxPeakVoxelReads)
	}
	found := false
	for n, c := range candidates {
		if n&255 == 0 {
			if err := ctx.Err(); err != nil {
				return PeakResult{}, err
			}
		}
		result.Candidates++
		sum, weight, complete := 0.0, 0.0, true
		for _, kv := range kernel {
			i, j, k := c.voxel[0]+kv.di, c.voxel[1]+kv.dj, c.voxel[2]+kv.dk
			if !t.inGrid(i, j, k) {
				complete = false
				continue
			}
			v, ok := valueAt(i, j, k)
			if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
				complete = false
				continue
			}
			sum += kv.weight * v
			weight += kv.weight
		}
		if !complete && !opts.AllowIncompleteSphere {
			result.SkippedIncomplete++
			continue
		}
		if weight == 0 {
			result.SkippedIncomplete++
			continue
		}
		mean := sum / weight
		if found && !(mean > result.Mean) {
			continue
		}
		found = true
		result.Mean = mean
		result.CenterVoxel = c.voxel
		result.Incomplete = !complete
		result.CoveredVolumeMM3 = weight * voxelVolume
	}
	if !found {
		return result, ErrNoPeak
	}
	result.Center = m.Grid.IndexToPoint(float64(result.CenterVoxel[0]), float64(result.CenterVoxel[1]), float64(result.CenterVoxel[2]))
	return result, nil
}
