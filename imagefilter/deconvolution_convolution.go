package imagefilter

import "context"

// PSFBoundary fixes the forward operator, including its finite field of view.
type PSFBoundary uint8

const (
	PSFZeroBoundary     PSFBoundary = iota // zero intensity outside the source grid
	PSFCircularBoundary                    // periodic grid, explicitly wraps all axes
)

// psfConvolve implements A or its exact transpose. Scatter, rather than a
// flipped boundary sampler, preserves the adjoint of the cropped zero model.
func psfConvolve(ctx context.Context, src, dst, k []float64, dims, kdims [3]int, border PSFBoundary, adjoint bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range dst {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		dst[i] = 0
	}
	center := [3]int{kdims[0] / 2, kdims[1] / 2, kdims[2] / 2}
	visits := 0
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				oi := (z*dims[1]+y)*dims[0] + x
				for kz := 0; kz < kdims[2]; kz++ {
					for ky := 0; ky < kdims[1]; ky++ {
						for kx := 0; kx < kdims[0]; kx++ {
							visits++
							if visits&1023 == 0 {
								if err := ctx.Err(); err != nil {
									return err
								}
							}
							ki := (kz*kdims[1]+ky)*kdims[0] + kx
							weight := k[ki]
							if weight == 0 {
								continue
							}
							xx, yy, zz := x-kx+center[0], y-ky+center[1], z-kz+center[2]
							if border == PSFCircularBoundary {
								xx = psfWrap(xx, dims[0])
								yy = psfWrap(yy, dims[1])
								zz = psfWrap(zz, dims[2])
							} else if xx < 0 || xx >= dims[0] || yy < 0 || yy >= dims[1] || zz < 0 || zz >= dims[2] {
								continue
							}
							si := (zz*dims[1]+yy)*dims[0] + xx
							if adjoint {
								dst[si] += weight * src[oi]
							} else {
								dst[oi] += weight * src[si]
							}
						}
					}
				}
			}
		}
	}
	return ctx.Err()
}
func psfWrap(x, n int) int {
	x %= n
	if x < 0 {
		x += n
	}
	return x
}
