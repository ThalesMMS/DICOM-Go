package compare

import "math"

func ssimWeights(s SSIMOptions) []float64 {
	out := make([]float64, s.Window*s.Window)
	r := s.Window / 2
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			w := float64(1)
			if s.Kernel == "gaussian" {
				w = math.Exp(-.5 * (math.Pow(float64(x)/s.Sigma, 2) + math.Pow(float64(y)/s.Sigma, 2)))
			}
			out[(y+r)*s.Window+x+r] = w
		}
	}
	return out
}
func reflected(i, n int) int {
	if n == 1 {
		return 0
	}
	period := 2*n - 2
	i %= period
	if i < 0 {
		i += period
	}
	if i >= n {
		i = period - i
	}
	return i
}
func localSSIM(a, b PresentedFrame, o PresentedOptions, w []float64, x, y, c int) float64 {
	radius := o.SSIM.Window / 2
	sum, mx, my := float64(0), float64(0), float64(0)
	// Centred two-pass population moments avoid catastrophic subtraction on
	// constant/nearly constant planes; this is intentionally a direct kernel.
	visit := func(emit func(float64, float64, float64)) {
		for dy := -radius; dy <= radius; dy++ {
			for dx := -radius; dx <= radius; dx++ {
				xx, yy := x+dx, y+dy
				if o.SSIM.Border == "reflect" {
					xx = reflected(xx, a.Width)
					yy = reflected(yy, a.Height)
				} else if xx < 0 || yy < 0 || xx >= a.Width || yy >= a.Height {
					continue
				}
				p := yy*a.Width + xx
				if len(o.Mask) > 0 && !o.Mask[p] {
					continue
				}
				weight := w[(dy+radius)*o.SSIM.Window+dx+radius]
				i := p*a.Components + c
				emit(a.Values[i], b.Values[i], weight)
			}
		}
	}
	visit(func(v, u, weight float64) { sum += weight; mx += weight * v; my += weight * u })
	mx /= sum
	my /= sum
	vx, vy, cov := float64(0), float64(0), float64(0)
	visit(func(v, u, weight float64) {
		dx, dy := v-mx, u-my
		vx += weight * dx * dx
		vy += weight * dy * dy
		cov += weight * dx * dy
	})
	vx /= sum
	vy /= sum
	cov /= sum
	c1 := o.SSIM.K1 * o.SSIM.Range
	c1 *= c1
	c2 := o.SSIM.K2 * o.SSIM.Range
	c2 *= c2
	value := ((2*mx*my + c1) * (2*cov + c2)) / ((mx*mx + my*my + c1) * (vx + vy + c2))
	// Roundoff can exceed the analytic [-1,1] bound by a few ulps.
	return math.Max(-1, math.Min(1, value))
}
