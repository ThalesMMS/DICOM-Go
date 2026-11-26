package display

import "math"

// WindowDomain describes the values actually entering VOI, not a parallel RWVM
// readout. Known ranges may be metadata bounds or resolved calibrated values.
type WindowDomain struct {
	Min, Max, Quantum float64
	Unit              string
	Known             bool
}

func finiteWindowValue(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// RescaledWindowDomain resolves a stored integer interval through the already
// resolved linear modality transform. It does not choose or apply an RWVM.
func RescaledWindowDomain(bits int, signed bool, slope, intercept float64, unit string) WindowDomain {
	if bits < 1 || bits > 32 || !finiteWindowValue(slope) || !finiteWindowValue(intercept) {
		return WindowDomain{}
	}
	if slope == 0 {
		slope = 1
	}
	lo, hi := 0.0, math.Ldexp(1, bits)-1
	if signed {
		lo = -math.Ldexp(1, bits-1)
		hi = math.Ldexp(1, bits-1) - 1
	}
	lo, hi = lo*slope+intercept, hi*slope+intercept
	if lo > hi {
		lo, hi = hi, lo
	}
	if !finiteWindowValue(lo) || !finiteWindowValue(hi) || !finiteWindowValue(hi-lo) {
		return WindowDomain{}
	}
	return WindowDomain{Min: lo, Max: hi, Quantum: math.Abs(slope), Unit: unit, Known: true}
}

// MeasuredWindowDomain summarizes resolved float/calibrated values once at
// preparation time. Padding must already be represented by non-finite values,
// as parametricmap.FrameValues does. No conversion or calibration is performed.
func MeasuredWindowDomain(values []float64, unit string) WindowDomain {
	d := WindowDomain{Min: math.Inf(1), Max: math.Inf(-1), Unit: unit}
	for _, v := range values {
		if !finiteWindowValue(v) {
			continue
		}
		d.Min, d.Max = math.Min(d.Min, v), math.Max(d.Max, v)
		d.Known = true
	}
	if !d.Known || !finiteWindowValue(d.Max-d.Min) {
		return WindowDomain{}
	}
	return d
}

// ManualWindowMinimum is the DICOM VOI minimum: LINEAR >=1, the other
// functions strictly positive. The positive floor also preserves finite
// representable changes at the current domain magnitude.
func ManualWindowMinimum(function VOIFunction, domain WindowDomain) float64 {
	if function != VOILinearExact && function != VOISigmoid {
		return 1
	}
	magnitude := math.Max(math.Abs(domain.Min), math.Abs(domain.Max))
	ulp := math.Nextafter(magnitude, math.Inf(1)) - magnitude
	if !finiteWindowValue(ulp) || ulp <= 0 {
		ulp = math.SmallestNonzeroFloat64
	}
	span := domain.Max - domain.Min
	floor := math.Max(ulp*16, math.SmallestNonzeroFloat64*1024)
	if domain.Known && finiteWindowValue(span) && span > 0 {
		floor = math.Max(floor, span/1e6)
	}
	return floor
}
