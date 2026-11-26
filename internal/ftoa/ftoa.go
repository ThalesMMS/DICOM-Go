// Package ftoa formats floating-point numbers the way C's printf family does,
// with the flags, width and precision of DCMTK's OFStandard::ftoa. It backs
// the Decimal String formatting of package object.
package ftoa

import (
	"math"
	"strconv"
	"strings"
)

// Flags select the conversion and padding.
type Flags uint

const (
	// ZeroPad pads to the width with zeros after the sign ('0').
	ZeroPad Flags = 1 << iota
	// LeftAdjust pads on the right ('-').
	LeftAdjust
	// Alternate keeps the decimal point and, for %g, trailing zeros ('#').
	Alternate
	// FormatE selects %e; FormatF selects %f; neither is %g.
	FormatE
	FormatF
	// Uppercase writes E, INF and NAN.
	Uppercase
)

const (
	// DefaultPrecision is C's default of six digits.
	DefaultPrecision = -1
	// ShortestPrecision uses the fewest digits that read back as value.
	ShortestPrecision = -2
)

// Format formats value in a field of at least width characters. precision
// is the number of significant digits (%g) or of digits after the point (%e,
// %f); DefaultPrecision is 6 and ShortestPrecision the shortest round trip.
// Infinities and NaN are "inf", "-inf" and "nan" (NaN has no sign).
func Format(value float64, flags Flags, width, precision int) string {
	var body string
	switch {
	case math.IsNaN(value):
		body = "nan"
	case math.IsInf(value, 1):
		body = "inf"
	case math.IsInf(value, -1):
		body = "-inf"
	default:
		body = number(value, flags, precision)
	}
	if flags&Uppercase != 0 {
		body = strings.ToUpper(body)
	}
	return pad(body, flags, width, !math.IsNaN(value) && !math.IsInf(value, 0))
}

func number(value float64, flags Flags, precision int) string {
	verb := byte('g')
	switch {
	case flags&FormatE != 0:
		verb = 'e'
	case flags&FormatF != 0:
		verb = 'f'
	}
	switch precision {
	case ShortestPrecision:
		precision = -1
	case DefaultPrecision:
		precision = 6
	default:
		if precision < 0 {
			precision = 6
		}
	}
	if verb == 'g' && precision == 0 {
		precision = 1
	}
	body := strconv.FormatFloat(value, verb, precision, 64)
	if flags&Alternate == 0 {
		return body
	}
	return alternate(body, verb, value, precision)
}

// alternate applies '#': a decimal point always, and for %g the trailing
// zeros up to the precision.
func alternate(body string, verb byte, value float64, precision int) string {
	mantissa, exponent := body, ""
	if i := strings.IndexAny(body, "e"); i >= 0 {
		mantissa, exponent = body[:i], body[i:]
	}
	if verb == 'g' && precision > 0 {
		// Keep precision significant digits.
		digits := 0
		seen := false
		for _, c := range mantissa {
			if c >= '1' && c <= '9' {
				seen = true
			}
			if c >= '0' && c <= '9' && (seen || value == 0) {
				digits++
			}
		}
		if !strings.Contains(mantissa, ".") {
			mantissa += "."
		}
		if digits < precision {
			mantissa += strings.Repeat("0", precision-digits)
		}
	} else if !strings.Contains(mantissa, ".") {
		mantissa += "."
	}
	return mantissa + exponent
}

func pad(body string, flags Flags, width int, finite bool) string {
	if len(body) >= width {
		return body
	}
	fill := width - len(body)
	switch {
	case flags&LeftAdjust != 0:
		return body + strings.Repeat(" ", fill)
	case flags&ZeroPad != 0 && finite:
		sign := ""
		if strings.HasPrefix(body, "-") || strings.HasPrefix(body, "+") {
			sign, body = body[:1], body[1:]
		}
		return sign + strings.Repeat("0", fill) + body
	default:
		return strings.Repeat(" ", fill) + body
	}
}
