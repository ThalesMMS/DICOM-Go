package display

import (
	"math"
	"testing"
)

func TestWindowDomainRescaleAndFiniteMeasuredValues(t *testing.T) {
	d := RescaledWindowDomain(12, true, -.001, 1, "mm2/s")
	if !d.Known || math.Abs(d.Min-(-1.047)) > 1e-12 || math.Abs(d.Max-3.048) > 1e-12 || d.Quantum != .001 {
		t.Fatalf("negative rescale: %+v", d)
	}
	d = MeasuredWindowDomain([]float64{math.NaN(), math.Inf(1), math.Inf(-1), .0008, .0012}, "mm2/s")
	if !d.Known || d.Min != .0008 || d.Max != .0012 || d.Unit != "mm2/s" {
		t.Fatalf("float domain: %+v", d)
	}
	if MeasuredWindowDomain([]float64{math.NaN(), math.Inf(1)}, "").Known {
		t.Fatal("unmeasured interval became valid")
	}
	if MeasuredWindowDomain([]float64{-1e308, 1e308}, "").Known {
		t.Fatal("overflowed finite span accepted")
	}
	if RescaledWindowDomain(0, false, 1, 0, "").Known {
		t.Fatal("missing bits became known")
	}
	for _, fn := range []VOIFunction{VOILinear, VOILinearExact, VOISigmoid} {
		minimum := ManualWindowMinimum(fn, d)
		if fn == VOILinear && minimum != 1 {
			t.Fatal("LINEAR below normative1")
		}
		if fn != VOILinear && !(minimum > 0 && minimum < .0004) {
			t.Fatalf("fractional function floor %v", minimum)
		}
	}
}
