package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
)

func requireMatrix(t *testing.T, name string, got [][]float64, want [][]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", name, len(got), len(want))
	}
	for r := range want {
		if len(got[r]) != len(want[r]) {
			t.Fatalf("%s: row %d has %d columns, want %d", name, r, len(got[r]), len(want[r]))
		}
		for c := range want[r] {
			if math.Abs(got[r][c]-want[r][c]) > 1e-6 {
				t.Fatalf("%s: (%d,%d) = %g, want %g", name, r, c, got[r][c], want[r][c])
			}
		}
	}
}

// The cases below port itkMultiLabelSTAPLEImageFilterGTest.cxx (ITK
// 35b196e13c, Apache-2.0): the acceptance criterion of #1101.
func TestMultiLabelSTAPLEITKCases(t *testing.T) {
	t.Run("voting ties do not corrupt the seeded matrices", func(t *testing.T) {
		result, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{0, 1, 0, 1}, {0, 1, 1, 0}}, MultiLabelSTAPLEOptions{MaxIterations: STAPLESeedOnly})
		if err != nil {
			t.Fatal(err)
		}
		if result.Iterations != 0 {
			t.Fatalf("iterations %d", result.Iterations)
		}
		for rater, matrix := range result.ConfusionMatrices {
			requireMatrix(t, "rater "+string(rune('0'+rater)), matrix, [][]float64{{1, 0}, {0, 1}, {0, 0}})
		}
		if want := []int{0, 1, 2, 2}; !equalInts(result.Labels, want) {
			t.Fatalf("labels %v, want %v", result.Labels, want)
		}
	})
	t.Run("default undecided label", func(t *testing.T) {
		result, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{0, 0, 0, 0}, {255, 255, 255, 255}}, MultiLabelSTAPLEOptions{MaxIterations: STAPLESeedOnly})
		if err != nil || result.UndecidedLabel != 256 {
			t.Fatalf("undecided %d: %v", result.UndecidedLabel, err)
		}
	})
	t.Run("saturated label count terminates", func(t *testing.T) {
		_, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{255, 0, 0, 0}, {255, 1, 1, 1}}, MultiLabelSTAPLEOptions{MaxIterations: 1, HasUndecidedLabel: true})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("saturated consensus on label zero seeds the matrix", func(t *testing.T) {
		result, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{255, 0, 0, 0}, {255, 0, 0, 0}}, MultiLabelSTAPLEOptions{MaxIterations: STAPLESeedOnly, HasUndecidedLabel: true, UndecidedLabel: 0})
		if err != nil {
			t.Fatal(err)
		}
		for rater, matrix := range result.ConfusionMatrices {
			if math.Abs(matrix[0][0]-1) > 1e-6 || math.Abs(matrix[255][255]-1) > 1e-6 {
				t.Fatalf("rater %d: (0,0)=%g (255,255)=%g", rater, matrix[0][0], matrix[255][255])
			}
		}
	})
	t.Run("consensus on label zero with undecided zero", func(t *testing.T) {
		result, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{0, 1, 0, 0}, {0, 0, 0, 0}}, MultiLabelSTAPLEOptions{MaxIterations: STAPLESeedOnly, HasUndecidedLabel: true, UndecidedLabel: 0})
		if err != nil {
			t.Fatal(err)
		}
		for rater, matrix := range result.ConfusionMatrices {
			requireMatrix(t, "rater "+string(rune('0'+rater)), matrix, [][]float64{{1, 0}, {0, 0}, {0, 0}})
		}
	})
}

func TestMultiLabelSTAPLESeedOnlyNormalizesTrueLabelColumns(t *testing.T) {
	// The majority labels are 0,0,0,1,1. Rater 0's counts are
	// {{2,1},{1,1}}, so the true-label columns have totals 3 and 2.
	raters := [][]uint16{{0, 0, 1, 0, 1}, {0, 0, 0, 1, 1}, {0, 0, 0, 1, 1}}
	result, err := MultiLabelSTAPLE(context.Background(), raters, MultiLabelSTAPLEOptions{MaxIterations: STAPLESeedOnly})
	if err != nil {
		t.Fatal(err)
	}
	if result.Iterations != 0 {
		t.Fatalf("iterations %d, want seed only", result.Iterations)
	}
	requireMatrix(t, "rater 0", result.ConfusionMatrices[0], [][]float64{{2.0 / 3, 0.5}, {1.0 / 3, 0.5}, {0, 0}})
	for _, matrix := range result.ConfusionMatrices[1:] {
		requireMatrix(t, "agreeing rater", matrix, [][]float64{{1, 0}, {0, 1}, {0, 0}})
	}
	if want := []int{0, 0, 0, 1, 1}; !equalInts(result.Labels, want) {
		t.Fatalf("labels %v, want %v", result.Labels, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// raterMasks derives raters with known error rates from a ground truth.
func raterMasks(seed int64, n int, sensitivity, specificity []float64) ([]bool, [][]bool) {
	rng := rand.New(rand.NewSource(seed))
	truth := make([]bool, n)
	for i := range truth {
		truth[i] = i%10 < 3 // 30% foreground
	}
	raters := make([][]bool, len(sensitivity))
	for k := range raters {
		raters[k] = make([]bool, n)
		for i, fg := range truth {
			if fg {
				raters[k][i] = rng.Float64() < sensitivity[k]
			} else {
				raters[k][i] = rng.Float64() >= specificity[k]
			}
		}
	}
	return truth, raters
}

func TestBinarySTAPLERecoversRaterPerformance(t *testing.T) {
	sensitivity := []float64{0.95, 0.85, 0.7, 0.9}
	specificity := []float64{0.98, 0.9, 0.97, 0.8}
	truth, raters := raterMasks(1101, 200000, sensitivity, specificity)
	result, err := BinarySTAPLE(context.Background(), raters, BinarySTAPLEOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Converged || result.Iterations < 2 {
		t.Fatalf("converged %v after %d iterations", result.Converged, result.Iterations)
	}
	for k := range raters {
		if math.Abs(result.Sensitivity[k]-sensitivity[k]) > 0.02 || math.Abs(result.Specificity[k]-specificity[k]) > 0.02 {
			t.Errorf("rater %d: sensitivity %.3f specificity %.3f, want %.2f %.2f", k, result.Sensitivity[k], result.Specificity[k], sensitivity[k], specificity[k])
		}
	}
	// The consensus beats the best single rater and plain majority voting.
	errorsOf := func(mask []bool) int {
		count := 0
		for i := range mask {
			if mask[i] != truth[i] {
				count++
			}
		}
		return count
	}
	consensus := errorsOf(result.Mask(0.5))
	best := len(truth)
	for _, r := range raters {
		best = min(best, errorsOf(r))
	}
	labels := make([][]uint16, len(raters))
	for k, r := range raters {
		labels[k] = make([]uint16, len(r))
		for i, v := range r {
			if v {
				labels[k][i] = 1
			}
		}
	}
	votes, err := LabelVoting(labels, 0)
	if err != nil {
		t.Fatal(err)
	}
	majority := make([]bool, len(votes))
	for i, v := range votes {
		majority[i] = v == 1
	}
	if consensus >= best || consensus >= errorsOf(majority) {
		t.Fatalf("STAPLE %d errors, best rater %d, voting %d", consensus, best, errorsOf(majority))
	}
}

func TestBinarySTAPLEAgreementAndMultiLabelRecovery(t *testing.T) {
	mask := []bool{true, false, true, true, false}
	result, err := BinarySTAPLE(context.Background(), [][]bool{mask, mask, mask}, BinarySTAPLEOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range result.Probability {
		if want := map[bool]float64{true: 1, false: 0}[mask[i]]; math.Abs(p-want) > 1e-9 {
			t.Fatalf("voxel %d probability %g", i, p)
		}
	}
	if result.Sensitivity[0] != 1 || result.Specificity[2] != 1 {
		t.Fatalf("perfect raters: %v %v", result.Sensitivity, result.Specificity)
	}
	// Multi-label: three labels, three raters with different confusions.
	rng := rand.New(rand.NewSource(7))
	n := 30000
	truth := make([]uint16, n)
	for i := range truth {
		truth[i] = uint16(i % 3)
	}
	flip := []float64{0.05, 0.2, 0.1}
	raters := make([][]uint16, 3)
	for k := range raters {
		raters[k] = make([]uint16, n)
		for i, label := range truth {
			raters[k][i] = label
			if rng.Float64() < flip[k] {
				raters[k][i] = uint16((int(label) + 1 + rng.Intn(2)) % 3)
			}
		}
	}
	staple, err := MultiLabelSTAPLE(context.Background(), raters, MultiLabelSTAPLEOptions{})
	if err != nil || !staple.Converged {
		t.Fatalf("converged %v: %v", staple.Converged, err)
	}
	for k := range raters {
		for c := 0; c < 3; c++ {
			if got := staple.ConfusionMatrices[k][c][c]; math.Abs(got-(1-flip[k])) > 0.02 {
				t.Errorf("rater %d label %d agreement %.3f, want %.2f", k, c, got, 1-flip[k])
			}
		}
	}
	mistakes := 0
	for i, label := range staple.Labels {
		if label != int(truth[i]) {
			mistakes++
		}
	}
	votes, err := LabelVoting(raters, -1)
	if err != nil {
		t.Fatal(err)
	}
	voteMistakes := 0
	for i, label := range votes {
		if label != int(truth[i]) {
			voteMistakes++
		}
	}
	// Two raters wrong in the same way cannot be recovered: ~2.5% here.
	if float64(mistakes)/float64(n) > 0.03 || mistakes >= voteMistakes {
		t.Fatalf("multi-label consensus wrong on %d of %d voxels (voting %d)", mistakes, n, voteMistakes)
	}
}

func TestConsensusInputErrors(t *testing.T) {
	for name, err := range map[string]error{
		"one rater": func() error {
			_, err := BinarySTAPLE(context.Background(), [][]bool{{true}}, BinarySTAPLEOptions{})
			return err
		}(),
		"sizes": func() error { _, err := LabelVoting([][]uint16{{1}, {1, 2}}, -1); return err }(),
		"empty": func() error {
			_, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{}, {}}, MultiLabelSTAPLEOptions{})
			return err
		}(),
		"too many": func() error {
			_, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{2000}, {0}}, MultiLabelSTAPLEOptions{})
			return err
		}(),
		"few priors": func() error {
			_, err := MultiLabelSTAPLE(context.Background(), [][]uint16{{2}, {0}}, MultiLabelSTAPLEOptions{Priors: []float64{1}})
			return err
		}(),
	} {
		if !errors.Is(err, ErrConsensusInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MultiLabelSTAPLE(ctx, [][]uint16{{0, 1}, {1, 1}}, MultiLabelSTAPLEOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
