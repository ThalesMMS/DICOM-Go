package imagefilter

import (
	"context"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

func TestN41505ExactFrozenBaseline(t *testing.T) {
	for _, dims := range [][3]int{{23, 19, 11}, {31, 23, 1}, {23, 19, 3}} {
		for _, sparse := range []bool{false, true} {
			for _, shrink := range []int{1, 3} {
				in := NewVolume(dims, [3]float64{0.7, 1.8, 4.2})
				mask := make([]bool, len(in.Data))
				for i := range in.Data {
					in.Data[i] = float32(90 + i%31)
					mask[i] = !sparse || i%64 == 0
				}
				original := append([]float32(nil), in.Data...)
				oldMask := append([]bool(nil), mask...)
				opts := N4Options{Mask: mask, ShrinkFactor: shrink, Iterations: []int{3, 3}, Workers: 2}
				want, err := N4BiasFieldCorrectionReference1505(context.Background(), in, opts)
				if err != nil {
					t.Fatal(err)
				}
				got, err := N4BiasFieldCorrection(context.Background(), in, opts)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					for i := range got.Corrected.Data {
						if got.Corrected.Data[i] != want.Corrected.Data[i] || got.LogBiasField.Data[i] != want.LogBiasField.Data[i] {
							t.Fatalf("dims%v sparse%v shrink%d pixel%d: corrected %.9g/%.9g log %.9g/%.9g", dims, sparse, shrink, i, got.Corrected.Data[i], want.Corrected.Data[i], got.LogBiasField.Data[i], want.LogBiasField.Data[i])
						}
					}
					t.Fatalf("metadata/convergence differs dims%v sparse%v shrink%d", dims, sparse, shrink)
				}
				if !reflect.DeepEqual(in.Data, original) || !reflect.DeepEqual(mask, oldMask) {
					t.Fatal("input/mask modified")
				}
			}
		}
	}
}

// Test-only phase-aware context injects cancellation when a real kernel polls.
// Done is closed consistently and concurrently; no production hook is added.
type n41505PhaseContext struct {
	context.Context
	target, exclude string
	calls           atomic.Int64
	after           int64
	done            chan struct{}
	once            sync.Once
}

func n41505Context(target, exclude string, after int64) *n41505PhaseContext {
	return &n41505PhaseContext{Context: context.Background(), target: target, exclude: exclude, after: after, done: make(chan struct{})}
}
func (c *n41505PhaseContext) Done() <-chan struct{} { return c.done }
func (c *n41505PhaseContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
	}
	pcs := make([]uintptr, 24)
	n := runtime.Callers(2, pcs)
	found, rejected := false, false
	for _, pc := range pcs[:n] {
		f := runtime.FuncForPC(pc)
		if f != nil {
			name := f.Name()
			found = found || strings.Contains(name, c.target)
			rejected = rejected || (c.exclude != "" && strings.Contains(name, c.exclude))
		}
	}
	if found && !rejected && c.calls.Add(1) >= c.after {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func TestN41505CancellationInsideKernels(t *testing.T) {
	g := &n4Grid{dims: [3]int{64, 48, 32}}
	axes := g.axes()
	l := &bsplineLattice{n: [3]int{16, 12, 8}, phi: make([]float64, 16*12*8)}
	for i := range l.phi {
		l.phi[i] = math.Sin(float64(i))
	}
	samples := make([]int, 64*48*32)
	values := make([]float64, len(samples))
	for i := range samples {
		samples[i] = i
		values[i] = math.Sin(float64(i)*0.01) + 5
	}
	for _, name := range []string{"fit", "dense", "sparse", "refine", "sharpen", "convergence", "shrink", "full axes", "axes", "validation"} {
		t.Run(name, func(t *testing.T) {
			before := parallel.InUse()
			var err error
			switch name {
			case "fit":
				c := n41505Context("fitBSplineContext.func", "", 2)
				var out *bsplineLattice
				out, err = fitBSplineContext(c, l.n, axes, samples, values, 2)
				if out != nil {
					t.Fatal("partial lattice")
				}
			case "dense":
				c := n41505Context("evaluateToContext.func", "", 4)
				_, err = l.evaluateAtContext(c, axes, samples, 2, nil)
			case "sparse":
				c := n41505Context("evaluateAtContext.func", "", 4)
				_, err = l.evaluateAtContext(c, axes, []int{0, 2048, 4096, 8192, 16384, 32768, 65536}, 2, nil)
			case "refine":
				c := n41505Context("refineAxisContext", "", 2)
				var out *bsplineLattice
				out, err = l.refineAxisContext(c, 0)
				if out != nil {
					t.Fatal("partial refinement")
				}
			case "sharpen":
				c := n41505Context("sharpenN4Context", "", 3)
				opts, _ := N4Options{}.normalized(&Volume{Data: make([]float32, len(values))})
				err = sharpenN4Context(c, values, make([]float64, len(values)), opts)
			case "convergence":
				c := n41505Context("n4ConvergenceContext", "", 3)
				_, err = n4ConvergenceContext(c, values, values)
			case "shrink":
				c := n41505Context("shrinkForN4Context", "", 7)
				in := NewVolume(g.dims, [3]float64{1, 2, 3})
				_, err = shrinkForN4Context(c, in, nil, 1)
			case "full axes":
				c := n41505Context("fullAxesContext", "", 2)
				gg := &n4Grid{centres: [3][]float64{{0, 1}, {0, 1}, {0, 1}}}
				_, err = gg.fullAxesContext(c, [3]int{4096, 2, 2})
			case "axes":
				c := n41505Context("axesContext", "", 2)
				gg := &n4Grid{dims: [3]int{4096, 2, 2}}
				_, err = gg.axesContext(c)
			case "validation":
				c := n41505Context("validateN4Context", "", 3)
				in := NewVolume(g.dims, [3]float64{1, 2, 3})
				err = validateN4Context(c, in)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("phase %s: %v", name, err)
			}
			if parallel.InUse() != before {
				t.Fatalf("helpers not joined: %d -> %d", before, parallel.InUse())
			}
		})
	}
}

func TestN41505PublicCancellationNoPartialOutputs(t *testing.T) {
	in := NewVolume([3]int{64, 48, 32}, [3]float64{0.7, 1.8, 4.2})
	for i := range in.Data {
		in.Data[i] = float32(100 + i%17)
	}
	for _, phase := range []struct{ target, exclude string }{{"fitBSplineContext.func", ""}, {"evaluateToContext.func", "evaluateAtContext"}, {"sharpenN4Context", ""}} {
		c := n41505Context(phase.target, phase.exclude, 2)
		before := parallel.InUse()
		out, err := N4BiasFieldCorrection(c, in, N4Options{ShrinkFactor: 1, Iterations: []int{2}, Workers: 2})
		if !errors.Is(err, context.Canceled) || out != nil {
			t.Fatalf("%s: out %v err %v", phase.target, out, err)
		}
		if parallel.InUse() != before {
			t.Fatal("public returned before join")
		}
	}
}
