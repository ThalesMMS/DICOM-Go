// Package fft holds the radix-2 fast Fourier transform shared by the
// packages that need one: dsa's phase correlation and imagefilter's N4
// histogram sharpening.
package fft

import (
	"context"
	"math"
)

// NextPowerOfTwo returns the smallest power of two not below n (1 for n ≤ 1).
func NextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// Transform is an in-place iterative radix-2 transform of a power-of-two
// length: X[k] = Σ x[n]·exp(∓2πi·kn/N), the minus sign forward. The inverse
// is unscaled.
func Transform(data []complex128, inverse bool) { _ = transformContext(nil, data, inverse) }

// TransformContext leaves data partially transformed on cancellation. Callers
// must discard that private scratch; it never publishes a complete result.
func TransformContext(ctx context.Context, data []complex128, inverse bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return transformContext(ctx, data, inverse)
}

func transformContext(ctx context.Context, data []complex128, inverse bool) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	n := len(data)
	for i, j := 1, 0; i < n; i++ {
		if ctx != nil && i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			data[i], data[j] = data[j], data[i]
		}
	}
	sign := -1.0
	if inverse {
		sign = 1
	}
	for size := 2; size <= n; size <<= 1 {
		angle := sign * 2 * math.Pi / float64(size)
		step := complex(math.Cos(angle), math.Sin(angle))
		half := size / 2
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < half; k++ {
				if ctx != nil && (start+k)&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				even, odd := data[start+k], data[start+k+half]*w
				data[start+k] = even + odd
				data[start+k+half] = even - odd
				w *= step
			}
		}
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// Transform2D transforms the width × height row-major data in place (both
// powers of two); the inverse is scaled by 1/(width·height).
func Transform2D(data []complex128, width, height int, inverse bool) {
	for y := 0; y < height; y++ {
		Transform(data[y*width:(y+1)*width], inverse)
	}
	column := make([]complex128, height)
	for x := 0; x < width; x++ {
		for y := range column {
			column[y] = data[y*width+x]
		}
		Transform(column, inverse)
		for y, value := range column {
			data[y*width+x] = value
		}
	}
	if inverse {
		scale := complex(1/float64(width*height), 0)
		for i := range data {
			data[i] *= scale
		}
	}
}
