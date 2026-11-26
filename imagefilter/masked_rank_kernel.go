package imagefilter

import "context"

// Exactly 12 bytes per retained convex row, independent of machine int width.
type rankSpan struct{ dy, dz, half int32 }

// The declared binary64 expression is also used at admission. No epsilon,
// dimension cap or half-voxel inflation changes this physical membership.
func (r *rankRequest) inside(dx, dy, dz int) bool {
	sum := float64(0)
	offsets := [3]int{dx, dy, dz}
	for a, d := range offsets {
		if r.src.Dims[a] == 1 {
			if d != 0 {
				return false
			}
			continue
		}
		radius := r.opts.RadiusMM[a]
		if radius == 0 {
			if d != 0 {
				return false
			}
			continue
		}
		distance := float64(d) * r.src.SpacingMM[a]
		if r.opts.Neighborhood == PhysicalBox {
			if distance > radius || -distance > radius {
				return false
			}
			continue
		}
		q := distance / radius
		sum += q * q
	}
	return sum <= 1
}

func (r *rankRequest) eachKernelRow(ctx context.Context, fn func(dy, dz, half int)) error {
	p := rankPoll{ctx: ctx}
	for dz := -r.radius[2]; dz <= r.radius[2]; dz++ {
		for dy := -r.radius[1]; dy <= r.radius[1]; dy++ {
			half := -1
			for dx := 0; dx <= r.radius[0]; dx++ {
				if err := p.step(); err != nil {
					return err
				}
				if r.inside(dx, dy, dz) {
					half = dx
				}
			}
			if half >= 0 {
				fn(dy, dz, half)
			}
		}
	}
	return p.check()
}
func rankCountKernel(ctx context.Context, r *rankRequest) (rows, samples int, err error) {
	err = r.eachKernelRow(ctx, func(_, _, half int) { rows++; samples += 2*half + 1 })
	return
}
func (r *rankRequest) makeKernel(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.kernel = make([]rankSpan, r.rows)
	i := 0
	return r.eachKernelRow(ctx, func(dy, dz, half int) { r.kernel[i] = rankSpan{int32(dy), int32(dz), int32(half)}; i++ })
}
