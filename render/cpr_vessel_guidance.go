package render

import (
	"context"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// These limits bound derived path-analysis data, separately from the source
// volume's canonical store. The response is never rendered as a 3D image.
const (
	MaxCPRVesselVoxels          = 8_000_000
	cprVesselWorkingBytes int64 = 64 << 20
	CPRVesselMinSigmaMM         = .75
	CPRVesselMaxSigmaMM         = 4.0
)

func prepareCPRVesselSpeed(ctx context.Context, volume *Volume, waypoints []Vec3, options CPRPathAssistantOptions) (*Volume, error) {
	// Prepare lazy source decoding with the caller's cancellation before the
	// reader takes its own immutable generation lease.
	lease, err := volume.AcquireSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	reader, err := volume.AcquireReader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	source := reader.sampler.vol
	crop, err := planCPRVesselCrop(source, waypoints, options.MarginMM)
	if err != nil {
		return nil, err
	}
	low, high, dims := crop.low, crop.high, crop.dims
	spacing, nativeSpacing := crop.spacing, crop.nativeSpacing
	input := imagefilter.NewVolume(dims, spacing)
	for z := 0; z < dims[2]; z++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for y := 0; y < dims[1]; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := 0; x < dims[0]; x++ {
				value, valid := reader.sampler.trilinearAt(Vec3{
					X: math.Min(float64(high[0]), float64(low[0])+float64(x)*spacing[0]/nativeSpacing[0]),
					Y: math.Min(float64(high[1]), float64(low[1])+float64(y)*spacing[1]/nativeSpacing[1]),
					Z: math.Min(float64(high[2]), float64(low[2])+float64(z)*spacing[2]/nativeSpacing[2]),
				})
				if !valid || math.IsNaN(value) || math.IsInf(value, 0) {
					return nil, ErrCPRPathAssistantInput
				}
				input.Data[(z*dims[1]+y)*dims[0]+x] = float32(value)
			}
		}
	}
	response, err := imagefilter.Vesselness(ctx, input, imagefilter.VesselnessOptions{
		SigmaMinMM: CPRVesselMinSigmaMM, SigmaMaxMM: CPRVesselMaxSigmaMM, SigmaSteps: 6,
		// Shape-only response prevents dense calcium from setting the global
		// intensity normalization for a much fainter contrast-filled lumen.
		Gamma: -1, MaxWorkingBytes: cprVesselWorkingBytes,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	origin := source.VoxelToPatient(Vec3{X: float64(low[0]), Y: float64(low[1]), Z: float64(low[2])})
	return newDerivedFloat32Volume(source, dims[0], dims[1], dims[2], origin, spacing[0], spacing[1], spacing[2], response.Response.Data)
}

type cprVesselCrop struct {
	low, high, dims        [3]int
	spacing, nativeSpacing [3]float64
}

func planCPRVesselCrop(source *Volume, waypoints []Vec3, searchMarginMM float64) (cprVesselCrop, error) {
	nativeSpacing := [3]float64{source.ColSpacing, source.RowSpacing, source.SliceSpacing}
	spacing := nativeSpacing
	dimensions := [3]int{source.Cols, source.Rows, source.Depth}
	minimum := [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
	maximum := [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
	for _, patient := range waypoints {
		voxel := source.PatientToVoxel(patient)
		for axis, value := range [3]float64{voxel.X, voxel.Y, voxel.Z} {
			minimum[axis] = math.Min(minimum[axis], value)
			maximum[axis] = math.Max(maximum[axis], value)
		}
	}
	// Keep the search margin plus four maximum-scale sigmas as filter context.
	margin := searchMarginMM + 4*CPRVesselMaxSigmaMM
	var low, high, dims [3]int
	for axis := range dims {
		low[axis] = int(math.Max(0, math.Floor(minimum[axis]-margin/nativeSpacing[axis])))
		high[axis] = int(math.Min(float64(dimensions[axis]-1), math.Ceil(maximum[axis]+margin/nativeSpacing[axis])))
		// The smallest analysed sigma bounds the useful analysis resolution.
		// Cover both crop endpoints exactly, including source-edge waypoints.
		spanMM := float64(high[axis]-low[axis]) * nativeSpacing[axis]
		dims[axis] = int(math.Ceil(spanMM/math.Max(nativeSpacing[axis], CPRVesselMinSigmaMM))) + 1
		if dims[axis] > 1 {
			spacing[axis] = spanMM / float64(dims[axis]-1)
		}
	}
	_, ok := boundedCPRAssistantProduct(dims[0], dims[1], dims[2], MaxCPRVesselVoxels)
	if !ok {
		return cprVesselCrop{}, &CPRPathAssistantLimitError{Resource: "vessel_voxels", Value: MaxCPRVesselVoxels + 1, Limit: MaxCPRVesselVoxels}
	}
	return cprVesselCrop{low: low, high: high, dims: dims, spacing: spacing, nativeSpacing: nativeSpacing}, nil
}
