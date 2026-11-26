package colorbar

import (
	"math"
	"strconv"
	"strings"
)

// maxDecimals bounds label precision so degenerate ranges cannot produce
// unreadable labels.
const maxDecimals = 6

// NiceTicks returns at most maxTicks values inside [low, high] that are
// multiples of a "nice" step (1, 2 or 5 times a power of ten), following the
// classic nice-numbers labelling rule. When the interval is too narrow to hold
// two nice ticks, the interval ends are returned instead. The returned step is
// the spacing the labels should be formatted for.
func NiceTicks(low, high float64, maxTicks int) (values []float64, step float64) {
	if !finite(low) || !finite(high) || high <= low {
		return nil, 0
	}
	if maxTicks < 2 {
		maxTicks = DefaultMaxTicks
	}
	span := niceNumber(high-low, false)
	step = niceNumber(span/float64(maxTicks-1), true)
	count := func(step float64) (first, last float64, n int) {
		first = math.Ceil(low/step - 1e-9)
		last = math.Floor(high/step + 1e-9)
		return first, last, int(last-first) + 1
	}
	// Coarsen until the budget holds, then refine while fewer than two ticks
	// fit (a narrow range between two coarse multiples).
	for guard := 0; guard < 64 && step > 0; guard++ {
		if _, _, n := count(step); n <= maxTicks {
			break
		}
		step = niceNumber(step*1.0001, false)
	}
	for guard := 0; guard < 64 && step > 0; guard++ {
		if _, _, n := count(step); n >= 2 {
			break
		}
		finer := previousNice(step)
		if _, _, n := count(finer); n > maxTicks {
			break
		}
		step = finer
	}
	if step <= 0 {
		return []float64{low, high}, high - low
	}
	first, last, n := count(step)
	if n < 2 || n > maxTicks {
		return []float64{low, high}, high - low
	}
	values = make([]float64, 0, n)
	for k := first; k <= last; k++ {
		value := k * step
		if value == 0 {
			value = 0 // normalize -0
		}
		values = append(values, value)
	}
	return values, step
}

// previousNice returns the next smaller 1-2-5 step.
func previousNice(step float64) float64 {
	exponent := math.Floor(math.Log10(step) + 1e-9)
	scale := math.Pow(10, exponent)
	switch fraction := math.Round(step / scale); {
	case fraction >= 5:
		return 2 * scale
	case fraction >= 2:
		return scale
	default:
		return scale / 2
	}
}

// FormatValue formats a tick value with the number of decimals its step needs
// (0 for steps of 1 or more, 1 for 0.5, 2 for 0.05, ...), never printing
// "-0".
func FormatValue(value, step float64) string {
	return formatDecimals(value, decimalsForStep(step))
}

func decimalsForStep(step float64) int {
	if !finite(step) || step <= 0 {
		return 0
	}
	decimals := int(-math.Floor(math.Log10(step) + 1e-9))
	return max(0, min(maxDecimals, decimals))
}

func formatDecimals(value float64, decimals int) string {
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	if strings.HasPrefix(text, "-") && strings.Trim(text[1:], "0.") == "" {
		text = text[1:]
	}
	return text
}

// niceNumber rounds x to 1, 2, 5 or 10 times a power of ten: to the nearest
// such value when round is true, otherwise to the smallest one >= x.
func niceNumber(x float64, round bool) float64 {
	if !finite(x) || x <= 0 {
		return 0
	}
	exponent := math.Floor(math.Log10(x))
	scale := math.Pow(10, exponent)
	fraction := x / scale
	var nice float64
	if round {
		switch {
		case fraction < 1.5:
			nice = 1
		case fraction < 3:
			nice = 2
		case fraction < 7:
			nice = 5
		default:
			nice = 10
		}
	} else {
		switch {
		case fraction <= 1:
			nice = 1
		case fraction <= 2:
			nice = 2
		case fraction <= 5:
			nice = 5
		default:
			nice = 10
		}
	}
	return nice * scale
}
