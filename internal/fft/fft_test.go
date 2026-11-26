package fft

import (
	"math"
	"math/cmplx"
	"math/rand"
	"testing"
)

func TestFFTMatchesDFT(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	data := make([]complex128, 16)
	for i := range data {
		data[i] = complex(random.NormFloat64(), random.NormFloat64())
	}
	want := make([]complex128, len(data))
	for k := range want {
		for n, value := range data {
			want[k] += value * cmplx.Exp(complex(0, -2*math.Pi*float64(k*n)/float64(len(data))))
		}
	}
	got := append([]complex128(nil), data...)
	Transform(got, false)
	for k := range got {
		if cmplx.Abs(got[k]-want[k]) > 1e-9 {
			t.Fatalf("bin %d: %v, want %v", k, got[k], want[k])
		}
	}
	Transform(got, true)
	for i := range got {
		if cmplx.Abs(got[i]/complex(float64(len(got)), 0)-data[i]) > 1e-12 {
			t.Fatalf("inverse %d: %v, want %v", i, got[i], data[i])
		}
	}
}

func TestTransform2DInverts(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	data := make([]complex128, 8*4)
	for i := range data {
		data[i] = complex(random.NormFloat64(), 0)
	}
	got := append([]complex128(nil), data...)
	Transform2D(got, 8, 4, false)
	Transform2D(got, 8, 4, true)
	for i := range got {
		if cmplx.Abs(got[i]-data[i]) > 1e-12 {
			t.Fatalf("sample %d: %v, want %v", i, got[i], data[i])
		}
	}
	for n, want := range map[int]int{0: 1, 1: 1, 2: 2, 3: 4, 200: 256, 512: 512} {
		if got := NextPowerOfTwo(n); got != want {
			t.Errorf("NextPowerOfTwo(%d) = %d, want %d", n, got, want)
		}
	}
}
