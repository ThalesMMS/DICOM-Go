package linalg

import (
	"math"
	"math/rand"
	"testing"
)

func TestSymmetricEigenvalues3Known(t *testing.T) {
	tests := []struct {
		name string
		a    [6]float64 // a00 a01 a02 a11 a12 a22
		want [3]float64
	}{
		{"zero", [6]float64{}, [3]float64{0, 0, 0}},
		{"diagonal", [6]float64{3, 0, 0, 1, 0, 2}, [3]float64{1, 2, 3}},
		{"triple", [6]float64{5, 0, 0, 5, 0, 5}, [3]float64{5, 5, 5}},
		{"double", [6]float64{3, 1, 1, 3, 1, 3}, [3]float64{2, 2, 5}},
		// A bright tube along (1,1,0)/√2: eigenvalues 0 along it and −1, −1
		// across it, rotated off the axes.
		{"tube", [6]float64{-0.5, 0.5, 0, -0.5, 0, -1}, [3]float64{-1, -1, 0}},
		{"tiny", [6]float64{3e-300, 1e-300, 1e-300, 3e-300, 1e-300, 3e-300}, [3]float64{2e-300, 2e-300, 5e-300}},
		{"huge", [6]float64{3e300, 1e300, 1e300, 3e300, 1e300, 3e300}, [3]float64{2e300, 2e300, 5e300}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SymmetricEigenvalues3(tt.a[0], tt.a[1], tt.a[2], tt.a[3], tt.a[4], tt.a[5])
			scale := math.Max(math.Abs(tt.want[0]), math.Abs(tt.want[2]))
			for i := range got {
				if math.Abs(got[i]-tt.want[i]) > 1e-7*scale+1e-300 {
					t.Fatalf("eigenvalues = %v, want %v", got, tt.want)
				}
			}
		})
	}
	if got := SymmetricEigenvalues3(1, math.NaN(), 0, 1, 0, 1); !math.IsNaN(got[0]) {
		t.Fatalf("NaN entry gave %v", got)
	}
}

// TestSymmetricEigenvalues3MatchesJacobi compares the closed form with the
// Jacobi solver on random matrices, including near-repeated spectra.
func TestSymmetricEigenvalues3MatchesJacobi(t *testing.T) {
	rng := rand.New(rand.NewSource(1099))
	for trial := 0; trial < 20000; trial++ {
		var a [3][3]float64
		for i := 0; i < 3; i++ {
			for j := i; j < 3; j++ {
				v := rng.NormFloat64()
				a[i][j], a[j][i] = v, v
			}
		}
		if trial%4 == 1 {
			// Near-degenerate: λ·I plus a small perturbation.
			for i := 0; i < 3; i++ {
				for j := 0; j < 3; j++ {
					a[i][j] *= 1e-6
				}
				a[i][i] += 2
			}
		}
		want, _, err := SymmetricEigen3(a)
		if err != nil {
			t.Fatal(err)
		}
		got := SymmetricEigenvalues3(a[0][0], a[0][1], a[0][2], a[1][1], a[1][2], a[2][2])
		scale := math.Max(math.Abs(want[0]), math.Abs(want[2]))
		for i := range got {
			if math.Abs(got[i]-want[i]) > 1e-7*scale {
				t.Fatalf("trial %d: eigenvalues %v, Jacobi %v", trial, got, want)
			}
		}
		if got[0] > got[1] || got[1] > got[2] {
			t.Fatalf("not ascending: %v", got)
		}
	}
}

func TestSymmetricEigenvalues2(t *testing.T) {
	tests := []struct {
		a00, a01, a11 float64
		want          [2]float64
	}{
		{2, 1, 2, [2]float64{1, 3}},
		{0, 2, 0, [2]float64{-2, 2}},
		{3, 0, -1, [2]float64{-1, 3}},
		{-4, 1e-9, -4, [2]float64{-4 - 1e-9, -4 + 1e-9}},
		// Nearly singular: det = 1e-12, the small eigenvalue ≈ 5e-13.
		{1, 1 - 1e-12, 1, [2]float64{1e-12, 2 - 1e-12}},
	}
	for _, tt := range tests {
		got := SymmetricEigenvalues2(tt.a00, tt.a01, tt.a11)
		for i := range got {
			if math.Abs(got[i]-tt.want[i]) > 1e-12*math.Max(1, math.Abs(tt.want[i])) {
				t.Fatalf("eigenvalues(%v, %v, %v) = %v, want %v", tt.a00, tt.a01, tt.a11, got, tt.want)
			}
		}
	}
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 5000; trial++ {
		a, b, c := rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()
		want, _, err := SymmetricEigen([][]float64{{a, b}, {b, c}})
		if err != nil {
			t.Fatal(err)
		}
		got := SymmetricEigenvalues2(a, b, c)
		for i := range got {
			if math.Abs(got[i]-want[i]) > 1e-12*math.Max(1, math.Abs(want[1])) {
				t.Fatalf("eigenvalues %v, Jacobi %v", got, want)
			}
		}
	}
}

func BenchmarkSymmetricEigenvalues3(b *testing.B) {
	var sink float64
	for i := 0; i < b.N; i++ {
		x := float64(i&1023) * 1e-3
		e := SymmetricEigenvalues3(-1+x, 0.3, 0.1, -0.9, 0.2*x, 0.05)
		sink += e[0]
	}
	_ = sink
}
