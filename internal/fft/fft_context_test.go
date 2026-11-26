package fft

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
)

type fft1505CancelContext struct {
	context.Context
	checks atomic.Int64
	after  int64
	cancel context.CancelFunc
}

func (c *fft1505CancelContext) Err() error {
	if c.checks.Add(1) >= c.after {
		c.cancel()
	}
	return c.Context.Err()
}

func TestTransformContextExactAndCancel(t *testing.T) {
	for _, inverse := range []bool{false, true} {
		data := make([]complex128, 4096)
		for i := range data {
			data[i] = complex(math.Sin(float64(i)), math.Cos(float64(i)*0.3))
		}
		want := append([]complex128(nil), data...)
		Transform(want, inverse)
		if err := TransformContext(context.Background(), data, inverse); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatal("context transform changed arithmetic")
		}
	}
	for _, after := range []int64{1, 3, 70} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &fft1505CancelContext{Context: ctx, after: after, cancel: cancel}
		if err := TransformContext(c, make([]complex128, 65536), false); !errors.Is(err, context.Canceled) {
			t.Fatalf("check%d: %v", after, err)
		}
	}
}
