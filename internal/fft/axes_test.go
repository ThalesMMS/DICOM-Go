package fft

import (
	"context"
	"errors"
	"testing"
)

func TestAxisWorkers(t *testing.T) {
	for _, tc := range []struct{ n, workers, want int }{
		{ElementsPerWorker - 1, 24, 1},
		{ElementsPerWorker, 24, 1},
		{2 * ElementsPerWorker, 24, 2},
		{2 * ElementsPerWorker, 1, 1},
		{1024 * 1024, 24, 4},
		{128 * 128 * 128, 24, 8},
		{128 * 128 * 128, 3, 3},
		{128 * 128 * 128, 0, 8},
		{64, 0, 1},
	} {
		if got := AxisWorkers(tc.n, tc.workers); got != tc.want {
			t.Errorf("AxisWorkers(%d, %d) = %d, want %d", tc.n, tc.workers, got, tc.want)
		}
	}
	if got := GatherLength([3]int{512, 1, 1}); got != 0 {
		t.Errorf("x-only gather length %d", got)
	}
	if got := GatherLength([3]int{8, 64, 32}); got != 64 {
		t.Errorf("gather length %d, want 64", got)
	}
}

// serialAxes is the reference: every line of every axis gathered and
// transformed in turn on the calling goroutine.
func serialAxes(a []complex128, size [3]int, inverse bool) {
	stride := 1
	for axis := 0; axis < 3; axis++ {
		block := stride * size[axis]
		line := make([]complex128, size[axis])
		for start := 0; start < len(a); start += block {
			for offset := 0; offset < stride; offset++ {
				for k := range line {
					line[k] = a[start+offset+k*stride]
				}
				Transform(line, inverse)
				for k := range line {
					a[start+offset+k*stride] = line[k]
				}
			}
		}
		stride = block
	}
}

// TestTransformAxesMatchesSerialForAnyWorkers transforms arrays large enough
// to use several workers: the result is bit-identical to the serial line
// order for every worker bound, in 2-D and 3-D, forward and inverse.
func TestTransformAxesMatchesSerialForAnyWorkers(t *testing.T) {
	for _, size := range [][3]int{{64, 32, 512}, {1024, 1024, 1}, {1, 512, 1024}, {8, 4, 2}} {
		n := size[0] * size[1] * size[2]
		input := make([]complex128, n)
		for i := range input {
			input[i] = complex(float64((i*37)%101)-50, float64((i*13)%17))
		}
		for _, inverse := range []bool{false, true} {
			want := append([]complex128(nil), input...)
			serialAxes(want, size, inverse)
			for _, workers := range []int{1, 2, 3, 0} {
				got := append([]complex128(nil), input...)
				if err := TransformAxes(context.Background(), got, size, inverse, workers); err != nil {
					t.Fatal(err)
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("size %v inverse %v workers %d: element %d = %v, want %v", size, inverse, workers, i, got[i], want[i])
					}
				}
			}
		}
	}
}

func TestTransformAxesStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := make([]complex128, 4*ElementsPerWorker)
	if err := TransformAxes(ctx, data, [3]int{1024, 1024, 1}, false, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
}
