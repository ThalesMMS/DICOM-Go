package roi

import (
	"context"
	"fmt"
	"math"
)

func antiAliasGeometry(ctx context.Context, m *VolumeMask) (size [3]int, h, origin [3]float64, n int, err error) {
	if m == nil {
		return size, h, origin, 0, ErrBinaryAntiAlias
	}
	size = [3]int{m.Columns, m.Rows, m.Slices}
	axes := [3][]float64{m.X, m.Y, m.Z}
	product := uint64(1)
	for a, d := range size {
		if d < 1 || uint64(d) > uint64(math.MaxInt)/product || len(axes[a]) != d {
			return size, h, origin, 0, ErrBinaryAntiAlias
		}
		product *= uint64(d)
	}
	if size[0] < 2 || size[1] < 2 || uint64(len(m.Voxels)) != product {
		return size, h, origin, 0, ErrBinaryAntiAlias
	}
	n = int(product)
	for a, axis := range axes {
		origin[a] = axis[0]
		h[a] = 1
		if len(axis) > 1 {
			h[a] = axis[1] - axis[0]
		}
		if !antiAliasFinite(origin[a]) || !antiAliasFinite(h[a]) || h[a] <= 0 {
			return size, h, origin, 0, ErrBinaryAntiAlias
		}
		for i, v := range axis {
			if i&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return size, h, origin, 0, e
				}
			}
			expected := origin[a] + float64(i)*h[a]
			if !antiAliasFinite(v) || !antiAliasFinite(expected) || i > 0 && v <= axis[i-1] || math.Abs((v-origin[a])-float64(i)*h[a]) > 1e-6*h[a] {
				return size, h, origin, 0, fmt.Errorf("%w: regular finite axes required", ErrBinaryAntiAlias)
			}
		}
		length := float64(len(axis)-1) * h[a]
		if !antiAliasFinite(length) || length > float64(math.MaxFloat32)/16 || !antiAliasFinite(length*length) || !antiAliasFinite(h[a]*h[a]) || h[a]*h[a] == 0 {
			return size, h, origin, 0, ErrBinaryAntiAlias
		}
	}
	return size, h, origin, n, nil
}

// Sequential separable exact EDT reuses the package's context-aware lower
// parabola envelope. Coordinates are shifted to zero to avoid origin² overflow.
// Initial phi is opposite-centre distance minus half the minimum spacing;
// it is an initialization in mm, not exact distance to voxel-box faces.
func antiAliasInitial(ctx context.Context, m *VolumeMask, size [3]int, h [3]float64, hmin float64) ([]float32, error) {
	n := len(m.Voxels)
	d := make([]float64, n)
	phi := make([]float32, n)
	maxAxis := max(size[0], size[1], size[2])
	f := make([]float64, maxAxis)
	out := make([]float64, maxAxis)
	coords := make([]float64, maxAxis)
	hull := make([]int, maxAxis)
	bounds := make([]float64, maxAxis+1)
	for _, target := range []bool{false, true} {
		for i, v := range m.Voxels {
			if i&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
			}
			if v == target {
				d[i] = 0
			} else {
				d[i] = math.Inf(1)
			}
		}
		for axis, length := range size {
			for k := 0; k < length; k++ {
				coords[k] = float64(k) * h[axis]
			}
			lines := n / length
			stride := [3]int{1, size[0], size[0] * size[1]}[axis]
			for line := 0; line < lines; line++ {
				if line&63 == 0 {
					if e := ctx.Err(); e != nil {
						return nil, e
					}
				}
				base := line * length
				if axis == 1 {
					base = (line/size[0])*size[0]*size[1] + line%size[0]
				}
				if axis == 2 {
					base = line
				}
				for k := 0; k < length; k++ {
					if k&4095 == 0 {
						if e := ctx.Err(); e != nil {
							return nil, e
						}
					}
					f[k] = d[base+k*stride]
				}
				if e := squaredDistance1DContext(ctx, out[:length], f[:length], coords[:length], hull[:length], bounds[:length+1]); e != nil {
					return nil, e
				}
				for k := 0; k < length; k++ {
					if k&4095 == 0 {
						if e := ctx.Err(); e != nil {
							return nil, e
						}
					}
					d[base+k*stride] = out[k]
				}
			}
		}
		for i, v := range m.Voxels {
			if i&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
			}
			if v != target {
				value := math.Sqrt(d[i]) - .5*hmin
				if target {
					value = -value
				}
				if !antiAliasFinite(value) || math.IsInf(float64(float32(value)), 0) || value == 0 {
					return nil, ErrBinaryAntiAlias
				}
				phi[i] = float32(value)
			}
		}
	}
	return phi, ctx.Err()
}
