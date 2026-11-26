package mesh

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestVertexScalarAttributeValidation(t *testing.T) {
	good := VertexScalarAttribute{Name: "CURVATURE_MEAN", Values: []float64{.1, -.2, 0}, Units: "mm^-1"}
	for _, single := range []bool{false, true} {
		if e := validateVertexScalars([]VertexScalarAttribute{good}, 3, single); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"", "x", "NY", "_MEAN", "1MEAN", "a b", "a\nb", "é", strings.Repeat("a", 65)} {
		bad := good
		bad.Name = name
		if e := validateVertexScalars([]VertexScalarAttribute{bad}, 3, false); !errors.Is(e, ErrInvalidField) {
			t.Fatalf("name %q: %v", name, e)
		}
	}
	for _, unit := range []string{"mm\nheader", "mm per m", "µm", strings.Repeat("a", 65)} {
		bad := good
		bad.Units = unit
		if e := validateVertexScalars([]VertexScalarAttribute{bad}, 3, false); !errors.Is(e, ErrInvalidField) {
			t.Fatalf("units %q: %v", unit, e)
		}
	}
	for _, bad := range [][]VertexScalarAttribute{{good, good}, {{Name: "MEAN", Values: []float64{1}}}, make([]VertexScalarAttribute, 33), {{Name: "MEAN", Values: []float64{0, math.Inf(1), 0}}}, {{Name: "MEAN", Values: []float64{0, math.NaN(), 0}}}} {
		if e := validateVertexScalars(bad, 3, false); !errors.Is(e, ErrInvalidField) {
			t.Fatalf("bad attributes: %v", e)
		}
	}
	large := []VertexScalarAttribute{{Name: "LARGE", Values: []float64{math.MaxFloat64}}}
	if e := validateVertexScalars(large, 1, false); e != nil {
		t.Fatal("PLY double rejected", e)
	}
	if e := validateVertexScalars(large, 1, true); !errors.Is(e, ErrInvalidField) {
		t.Fatal("GLB overflow accepted")
	}
	largeVertices := uint64(math.MaxUint32)
	if largeVertices <= uint64(^uint(0)>>1) {
		if e := validateVertexScalars([]VertexScalarAttribute{good}, int(largeVertices), true); !errors.Is(e, ErrInvalidField) {
			t.Fatal("index bound accepted")
		}
	}
}
