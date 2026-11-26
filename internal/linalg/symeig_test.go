package linalg

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// checkDecomposition verifies A·v = λ·v, orthonormal columns and ascending
// eigenvalues.
func checkDecomposition(t *testing.T, a [][]float64, values []float64, vectors [][]float64, tol float64) {
	t.Helper()
	n := len(a)
	for k := 0; k < n; k++ {
		if k > 0 && values[k] < values[k-1] {
			t.Fatalf("eigenvalues not ascending: %v", values)
		}
		for i := 0; i < n; i++ {
			av := 0.0
			for j := 0; j < n; j++ {
				av += a[i][j] * vectors[j][k]
			}
			if math.Abs(av-values[k]*vectors[i][k]) > tol {
				t.Fatalf("A·v%d[%d] = %g, λv = %g", k, i, av, values[k]*vectors[i][k])
			}
		}
		for l := 0; l < n; l++ {
			dot := 0.0
			for i := 0; i < n; i++ {
				dot += vectors[i][k] * vectors[i][l]
			}
			want := 0.0
			if k == l {
				want = 1
			}
			if math.Abs(dot-want) > 1e-12 {
				t.Fatalf("v%d·v%d = %g, want %g", k, l, dot, want)
			}
		}
	}
}

func TestSymmetricEigenKnownMatrices(t *testing.T) {
	tests := []struct {
		name string
		a    [][]float64
		want []float64
	}{
		{"zero", [][]float64{{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}, []float64{0, 0, 0}},
		{"diagonal", [][]float64{{3, 0, 0}, {0, 1, 0}, {0, 0, 2}}, []float64{1, 2, 3}},
		{"triple", [][]float64{{5, 0, 0}, {0, 5, 0}, {0, 0, 5}}, []float64{5, 5, 5}},
		// 2I + 3·uuᵀ with u = (1,1,1)/√3: eigenvalues 2, 2, 5.
		{"double", [][]float64{{3, 1, 1}, {1, 3, 1}, {1, 1, 3}}, []float64{2, 2, 5}},
		{"2x2", [][]float64{{2, 1}, {1, 2}}, []float64{1, 3}},
		{"negative", [][]float64{{0, 2}, {2, 0}}, []float64{-2, 2}},
		{"4x4", [][]float64{{4, 1, 0, 0}, {1, 4, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 7}}, []float64{1, 3, 5, 7}},
		{"1x1", [][]float64{{-4}}, []float64{-4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, vectors, err := SymmetricEigen(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tt.want {
				if math.Abs(values[i]-tt.want[i]) > 1e-12 {
					t.Fatalf("values = %v, want %v", values, tt.want)
				}
			}
			checkDecomposition(t, tt.a, values, vectors, 1e-12)
		})
	}
}

func TestSymmetricEigenRandomRotations(t *testing.T) {
	rng := rand.New(rand.NewSource(1097))
	for trial := 0; trial < 200; trial++ {
		n := 2 + trial%3
		// Random orthonormal basis by Gram-Schmidt, eigenvalues with
		// deliberate repeats and wide dynamic range.
		q := make([][]float64, n)
		for k := 0; k < n; k++ {
			vec := make([]float64, n)
			for {
				for i := range vec {
					vec[i] = rng.NormFloat64()
				}
				for j := 0; j < k; j++ {
					dot := 0.0
					for i := range vec {
						dot += vec[i] * q[j][i]
					}
					for i := range vec {
						vec[i] -= dot * q[j][i]
					}
				}
				norm := 0.0
				for _, x := range vec {
					norm += x * x
				}
				if norm > 1e-6 {
					for i := range vec {
						vec[i] /= math.Sqrt(norm)
					}
					break
				}
			}
			q[k] = vec
		}
		lambda := make([]float64, n)
		for i := range lambda {
			lambda[i] = math.Pow(10, float64(rng.Intn(9)-4))
			if i > 0 && rng.Intn(3) == 0 {
				lambda[i] = lambda[i-1]
			}
		}
		a := make([][]float64, n)
		for i := range a {
			a[i] = make([]float64, n)
			for j := range a[i] {
				for k := 0; k < n; k++ {
					a[i][j] += lambda[k] * q[k][i] * q[k][j]
				}
			}
		}
		for i := range a { // exact symmetry
			for j := 0; j < i; j++ {
				a[i][j] = a[j][i]
			}
		}
		values, vectors, err := SymmetricEigen(a)
		if err != nil {
			t.Fatal(err)
		}
		checkDecomposition(t, a, values, vectors, 1e-9)
	}
}

func TestSymmetricEigen3And4Wrappers(t *testing.T) {
	values, vectors, err := SymmetricEigen3([3][3]float64{{2, 0, 0}, {0, 0, 0}, {0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if values != [3]float64{0, 1, 2} {
		t.Fatalf("values = %v", values)
	}
	// Column 0 is the y axis.
	if math.Abs(math.Abs(vectors[1][0])-1) > 1e-15 {
		t.Fatalf("vectors = %v", vectors)
	}
	v4, _, err := SymmetricEigen4([4][4]float64{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}})
	if err != nil || v4 != [4]float64{1, 1, 1, 1} {
		t.Fatalf("SymmetricEigen4 = %v, %v", v4, err)
	}
}

func TestSymmetricEigenRejectsInvalidInput(t *testing.T) {
	for name, a := range map[string][][]float64{
		"ragged":     {{1, 2}, {3}},
		"asymmetric": {{1, 2}, {3, 1}},
		"nan":        {{math.NaN(), 0}, {0, 1}},
		"inf":        {{math.Inf(1), 0}, {0, 1}},
	} {
		if _, _, err := SymmetricEigen(a); !errors.Is(err, ErrInvalidMatrix) {
			t.Fatalf("%s: err = %v, want ErrInvalidMatrix", name, err)
		}
	}
}
