package render

import (
	"context"
	"math"
)

// physicalGradientBounds bounds every central-difference sample of a block,
// including trilinear neighbours and the one-voxel gradient halo. It computes
// metadata only, on demand, for an immutable prepared source generation.
// Covariant axis norms make the triangle-inequality bound conservative for
// anisotropic and non-orthogonal patient geometry as well as regular grids.
func (g *vrBlockGrid) physicalGradientBounds(ctx context.Context, s *volumeSampler) ([]float64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.gradientMu.Lock()
		if g.gradientMax != nil {
			result := g.gradientMax
			g.gradientMu.Unlock()
			return result, nil
		}
		if done := g.gradientBuilding; done != nil {
			g.gradientMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		g.gradientBuilding = done
		g.gradientMu.Unlock()
		result, err := g.buildPhysicalGradientBounds(ctx, s)
		g.gradientMu.Lock()
		if err == nil {
			g.gradientMax = result
		}
		g.gradientBuilding = nil
		close(done)
		g.gradientMu.Unlock()
		return result, err
	}
}
func (g *vrBlockGrid) buildPhysicalGradientBounds(ctx context.Context, s *volumeSampler) ([]float64, error) {
	bounds := make([]float64, len(g.min))
	span := func(b, n int) (int, int) { return max(0, b*vrBlockSize-1), min(n-1, b*vrBlockSize+vrBlockSize+1) }
	factor := 0.0
	for _, axis := range []struct {
		direction Vec3
		spacing   float64
	}{{s.vol.AxisX, s.vol.ColSpacing}, {s.vol.AxisY, s.vol.RowSpacing}, {s.vol.Normal, s.vol.SliceSpacing}} {
		if axis.spacing > 0 {
			factor += .5 * axis.direction.Length() / axis.spacing
		}
	}
	err := parallelRowsContext(ctx, g.nz, func(bz int) {
		z0, z1 := span(bz, s.depth)
		for by := 0; by < g.ny; by++ {
			y0, y1 := span(by, s.rows)
			for bx := 0; bx < g.nx; bx++ {
				if ctx.Err() != nil {
					return
				}
				x0, x1 := span(bx, s.cols)
				lo, hi := math.Inf(1), math.Inf(-1)
				for z := z0; z <= z1; z++ {
					for y := y0; y <= y1; y++ {
						for x := x0; x <= x1; x++ {
							value := s.valueOrZero(x, y, z)
							if !finite(value) {
								lo, hi = math.Inf(-1), math.Inf(1)
								break
							}
							lo, hi = math.Min(lo, value), math.Max(hi, value)
						}
					}
				}
				index := (bz*g.ny+by)*g.nx + bx
				if lo == hi {
					bounds[index] = 0
					continue
				}
				lo, hi = widen(lo, hi)
				bound := (hi - lo) * factor
				if !finite(bound) || factor <= 0 {
					bound = math.Inf(1)
				}
				if bound == 0 {
					bound = math.SmallestNonzeroFloat64
				}
				bounds[index] = bound
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return bounds, nil
}

// A nonmonotone opacity curve may be visible between two zero endpoints.
func gradientOpacityZeroUntil(function VRGradientOpacity, maximum float64) bool {
	if !finite(maximum) || maximum < 0 || function.At(0) != 0 || function.At(maximum) != 0 {
		return false
	}
	for _, point := range function {
		if point.Magnitude >= 0 && point.Magnitude <= maximum && point.Opacity != 0 {
			return false
		}
	}
	return true
}
func (l *vrSpaceLeap) limitGradientOpacity(preset VRPreset, bounds []float64) {
	if len(bounds) != len(l.empty) {
		return
	}
	for i, maximum := range bounds {
		if len(preset.GradientOpacity) > 0 {
			if gradientOpacityZeroUntil(preset.GradientOpacity, maximum) {
				l.empty[i] = true
			}
		} else if preset.GradientOpacityScale > 0 && maximum == 0 {
			l.empty[i] = true
		}
	}
}

// Prefix sums count nonzero alpha table corners inside a bounded intensity x
// gradient rectangle. Bilinear interpolation of four zero corners is zero.
func (l *vrSpaceLeap) limitTransfer2DGradients(t *VRTransfer2D, bounds []float64) {
	if len(bounds) != len(l.empty) {
		return
	}
	stride := t.width + 1
	prefix := make([]int, (t.height+1)*stride)
	for y := 0; y < t.height; y++ {
		row := 0
		for x := 0; x < t.width; x++ {
			if t.table[y*t.width+x].A != 0 {
				row++
			}
			prefix[(y+1)*stride+x+1] = prefix[y*stride+x+1] + row
		}
	}
	for i, maximum := range bounds {
		if !finite(maximum) || !finite(float64(l.grid.min[i])) || !finite(float64(l.grid.max[i])) {
			continue
		}
		lo, hi := interpolationBounds(float64(l.grid.min[i]), float64(l.grid.max[i]))
		first := int(t.columnPosition(lo))
		last := min(int(math.Ceil(t.columnPosition(hi))), t.width-1)
		row := min(int(math.Ceil(clampUnit(maximum/t.gradientMax)*float64(t.height-1))), t.height-1)
		count := prefix[(row+1)*stride+last+1] - prefix[(row+1)*stride+first]
		if count == 0 {
			l.empty[i] = true
		}
	}
}
