package render

import (
	"context"
	"math"
)

// Request-owned bases retain the original arithmetic, without a path/volume
// cache. Output validation bounds the column count before this allocation.
const cprPreparedBasisBytes = 48
const maxCPRPreparedBasisBytes = cprPreparedBasisBytes * MaxCPROutputDimension

type cprPreparedBasis struct{ cross, slab Vec3 }

func prepareCPRBases(ctx context.Context, req CPRRequest, samples []CPRPathSample, stretched bool) ([]cprPreparedBasis, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := checkedCPRProduct("prepared_basis_bytes", maxCPRPreparedBasisBytes, uint64(len(samples)), cprPreparedBasisBytes); err != nil {
		return nil, err
	}
	bases := make([]cprPreparedBasis, len(samples))
	ref := req.StretchDir
	if stretched && ref == (Vec3{}) && len(samples) > 0 {
		ref = samples[0].Normal
	}
	for c, s := range samples {
		if c&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		cross := s.Normal
		if stretched {
			cross = ref.Sub(s.Tangent.Scale(ref.Dot(s.Tangent))).Normalize()
			if cross == (Vec3{}) {
				cross = s.Normal
			}
		}
		bases[c].cross, bases[c].slab = rotateCPRBasis(cross, s.Tangent, req.RotationDegrees)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bases, nil
}

// The mapper borrows the sampler volume only while its immutable generation
// lease is held. Public input geometry must not be concurrently mutated, as
// for PatientToVoxel. Corrected/irregular/tilted source geometry uses reference
// mapping even if the sampler obtained a regularized replacement.
type cprPreparedMapping struct {
	vol     *Volume
	regular bool
}

func prepareCPRMapping(source, sampled *Volume) cprPreparedMapping {
	regular := source == sampled && sampled != nil && sampled.regular && !sampled.tilt.Applied && !sampled.geometry.RequiresResampling && finiteVec3(sampled.Origin) && finiteVec3(sampled.AxisX) && finiteVec3(sampled.AxisY) && finiteVec3(sampled.Normal) && sampled.ColSpacing > 0 && sampled.RowSpacing > 0 && !math.IsInf(sampled.ColSpacing, 0) && !math.IsInf(sampled.RowSpacing, 0)
	return cprPreparedMapping{vol: sampled, regular: regular}
}

func (m cprPreparedMapping) at(p Vec3, interval *int) Vec3 {
	if !m.regular {
		return m.vol.PatientToVoxel(p)
	}
	v := m.vol
	d := p.Sub(v.Origin)
	z := d.Dot(v.Normal) / v.effectiveSliceSpacing()
	if len(v.positions) > 0 {
		z = m.positionToIndex(d.Dot(v.Normal), interval)
	}
	return Vec3{X: d.Dot(v.AxisX) / v.ColSpacing, Y: d.Dot(v.AxisY) / v.RowSpacing, Z: z}
}

// Only reuse a confirmed interior interval. Misses, endpoint/extrapolation
// cases and duplicate positions use the existing search verbatim. In
// particular, do not approximate recorded positions by a uniform slice step.
func (m cprPreparedMapping) positionToIndex(pos float64, interval *int) float64 {
	v := m.vol
	n := len(v.positions)
	lo := *interval
	if n > 1 && lo >= 0 && lo < n-1 && pos > v.positions[0] && pos < v.positions[n-1] && v.positions[lo] <= pos && pos < v.positions[lo+1] {
		span := v.positions[lo+1] - v.positions[lo]
		if span != 0 {
			return float64(lo) + (pos-v.positions[lo])/span
		}
	}
	z := v.positionToIndex(pos)
	*interval = -1
	if z >= 0 && z < float64(n-1) {
		*interval = int(math.Floor(z))
	}
	return z
}
