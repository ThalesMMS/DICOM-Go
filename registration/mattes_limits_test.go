package registration

import (
	"errors"
	"math"
	"math/bits"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

type mattesParameterCount struct {
	Transform
	n int
}

func (t mattesParameterCount) NumberOfParameters() int { return t.n }

type mattesLocalParameterCount struct{ mattesParameterCount }

func (mattesLocalParameterCount) localSupport() bool { return true }

func (mattesLocalParameterCount) sparseJacobianAt(render.Vec3, *sparseJacobian) {
	panic("oversized local transform reached Jacobian evaluation")
}

func TestMattesRejectsOversizedWorkspace(t *testing.T) {
	fixed, moving := itkGaussians(t)
	transform := NewTranslation3D()
	for _, tc := range []struct {
		name string
		bins int
	}{
		{"bin_product_overflow", math.MaxInt},
		{"histogram_bytes_overflow", 1 << (bits.UintSize/2 - 1)},
		{"histogram_over_limit", 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{Workers: 1}, tc.bins)
			if metric != nil || !errors.Is(err, ErrMattesWorkspaceLimit) {
				t.Fatalf("expected a rejected histogram, got metric %v, err %v", metric, err)
			}
		})
	}

	metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{Workers: 1}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		transform Transform
	}{
		{"dense_product_overflow", mattesParameterCount{transform, math.MaxInt}},
		{"dense_negative_count", mattesParameterCount{transform, -1}},
		{"dense_table_over_limit", mattesParameterCount{transform, maxMattesArrayBytes/8/25 + 1}},
		{"local_bytes_overflow", mattesLocalParameterCount{mattesParameterCount{transform, math.MaxInt}}},
		{"local_negative_count", mattesLocalParameterCount{mattesParameterCount{transform, -1}}},
		{"local_vector_over_limit", mattesLocalParameterCount{mattesParameterCount{transform, maxMattesArrayBytes/8 + 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, gradient, err := metric.ValueAndDerivative(tc.transform)
			if !errors.Is(err, ErrMattesWorkspaceLimit) || value != 0 || gradient != nil {
				t.Fatalf("expected a rejected workspace, got value %g, gradient %v, err %v", value, gradient, err)
			}
		})
	}
	want, err := metric.Value(transform)
	if err != nil {
		t.Fatal(err)
	}
	got, err := metric.Value(mattesParameterCount{transform, math.MaxInt})
	if err != nil || got != want {
		t.Fatalf("value-only evaluation with an unused derivative count: value %g, want %g, err %v", got, want, err)
	}
}
