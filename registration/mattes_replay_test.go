package registration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestMattesConcurrentReplay(t *testing.T) {
	fixed, moving, fixedMask, movingMask, points := mattesBenchmarkFixture(t)
	hashVoxels := func(image *Image) []byte {
		hash := sha256.New()
		if err := binary.Write(hash, binary.LittleEndian, image.Data()); err != nil {
			t.Fatal(err)
		}
		return hash.Sum(nil)
	}
	t.Logf("fixed_sha256=%x moving_sha256=%x", hashVoxels(fixed), hashVoxels(moving))
	domain := MetricDomain{FixedMask: fixedMask, MovingMask: movingMask, Points: points, Workers: 1}
	serial, err := NewMattesMutualInformation(fixed, moving, domain, 32)
	if err != nil {
		t.Fatal(err)
	}
	domain.Workers = 4
	parallel, err := NewMattesMutualInformation(fixed, moving, domain, 32)
	if err != nil {
		t.Fatal(err)
	}
	for index, parameters := range mattesReplayParameters {
		transform := mattesBenchmarkTransform(t)
		if err := transform.SetParameters(parameters[:]); err != nil {
			t.Fatal(err)
		}
		value, gradient, err := serial.ValueAndDerivative(transform)
		if err != nil {
			t.Fatal(err)
		}
		overlap := 0
		for i := range points {
			s, ok := serial.evaluator.sampleAt(i, transform, false)
			if ok && s.moving >= serial.movingMin && s.moving <= serial.movingMax {
				overlap++
			}
		}
		if overlap != 6720 {
			t.Fatalf("%d: overlap %d/%d, want 6720 valid points", index, overlap, len(points))
		}
		t.Run(fmt.Sprintf("transform_%d", index), func(t *testing.T) {
			t.Logf("overlap=%d value=%.17g gradient=%v", overlap, value, gradient)
			t.Parallel()
			for range 3 {
				gotValue, gotGradient, err := parallel.ValueAndDerivative(transform)
				if err != nil || gotValue != value || !slices.Equal(gotGradient, gradient) {
					t.Fatalf("value/gradient %g %v (%v); want %g %v", gotValue, gotGradient, err, value, gradient)
				}
			}
		})
	}
}

func TestLinearWithGradientMatchesGeneralSampler(t *testing.T) {
	fixed, _, _, _, _ := mattesBenchmarkFixture(t)
	for _, position := range []render.Vec3{
		{X: 4.125, Y: 3.25, Z: 2.75},
		{X: 0.47, Y: 30.12, Z: 13.85},
		{X: 4, Y: 3.25, Z: 2.75},
		{X: -.25, Y: 31.25, Z: 13.85},
		{X: math.SmallestNonzeroFloat64, Y: 3.25, Z: 2.75},
	} {
		value, gradient := fixed.linearWithGradientAt(position)
		wantValue, wantGradient := fixed.linearAt(position), fixed.gradientAt(position)
		if math.Abs(value-wantValue) > 1e-12 || gradient.Sub(wantGradient).Length() > 1e-12 {
			t.Fatalf("%v: value/gradient %g %v; want %g %v", position, value, gradient, wantValue, wantGradient)
		}
	}
}

func TestMattesRegistrationCanceledAtFinalEvaluation(t *testing.T) {
	fixed, moving, _, _, _ := mattesBenchmarkFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := Method{
		Fixed: fixed, Moving: moving, Transform: mattesBenchmarkTransform(t),
		Levels:    []Level{{Shrink: 1}},
		Optimizer: optimize.RegularStepGradientDescent{MaximumIterations: 1},
		Progress:  func(Progress) { cancel() },
	}
	result, err := method.Run(ctx)
	if !errors.Is(err, context.Canceled) || result.Transform != nil {
		t.Fatalf("canceled registration returned transform present = %t, error = %v", result.Transform != nil, err)
	}
}
