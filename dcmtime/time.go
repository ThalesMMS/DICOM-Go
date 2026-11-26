package dcmtime

import (
	"strings"
	"time"
)

// Time holds data for a parsed DICOM time (TM) value.
type Time struct {
	// Time is the underlying time.Time value.
	Time time.Time
	// Precision with which the raw TM value was stored. For instance, a Date value
	// with a precision of PrecisionHours ONLY stored the Hour.
	Precision PrecisionLevel
	// LeapSecond reports a seconds component of 60, which PS3.5 6.2 allows
	// for a leap second. A time.Time cannot hold second 60 without rolling
	// over into the next minute (and, at 23:59:60, the next day), so Time then
	// holds the last instant of the minute, hh:mm:59.999999999: it sorts after
	// every other value of that minute and before the next one. DCM and String
	// render the seconds as 60, with the parsed fraction.
	LeapSecond bool

	// leapFraction is the fraction of a leap second in nanoseconds, which
	// Time cannot carry.
	leapFraction int
}

// leapSecondNanos is the nanosecond a leap second is clamped to.
const leapSecondNanos = 999999999

// clockSeconds returns the seconds and nanoseconds to render, restoring a
// leap second's 60 and fraction.
func clockSeconds(value time.Time, leap bool, leapFraction int) (int, int) {
	if leap {
		return 60, leapFraction
	}
	return value.Second(), value.Nanosecond()
}

// DCM converts internal time.Time value to dicom TM string, truncating the output
// to the DA value's Precision.
//
// NOTE: Time zones are ignored in this operation, as TM does not support encoding them.
// Make sure values are converted to UTC before passing if that is the desired output.
func (tm Time) DCM() string {
	builder := strings.Builder{}
	builder.Grow(13)
	second, nanos := clockSeconds(tm.Time, tm.LeapSecond, tm.leapFraction)

	writeZeroPaddedInt(&builder, tm.Time.Hour(), 2)
	if !isIncluded(PrecisionMinutes, tm.Precision) {
		return builder.String()
	}

	writeZeroPaddedInt(&builder, tm.Time.Minute(), 2)
	if !isIncluded(PrecisionSeconds, tm.Precision) {
		return builder.String()
	}

	writeZeroPaddedInt(&builder, second, 2)
	if !isIncluded(PrecisionMS1, tm.Precision) {
		return builder.String()
	}

	builder.WriteRune('.')
	writeTruncatedMicroseconds(&builder, nanos, tm.Precision)

	return builder.String()
}

// String implements fmt.Stringer.
func (tm Time) String() string {
	builder := strings.Builder{}
	builder.Grow(15)
	second, nanos := clockSeconds(tm.Time, tm.LeapSecond, tm.leapFraction)

	writeZeroPaddedInt(&builder, tm.Time.Hour(), 2)
	if !isIncluded(PrecisionMinutes, tm.Precision) {
		return builder.String()
	}

	builder.WriteByte(':')
	writeZeroPaddedInt(&builder, tm.Time.Minute(), 2)
	if !isIncluded(PrecisionSeconds, tm.Precision) {
		return builder.String()
	}

	builder.WriteByte(':')
	writeZeroPaddedInt(&builder, second, 2)
	if !isIncluded(PrecisionMS1, tm.Precision) {
		return builder.String()
	}

	builder.WriteRune('.')
	writeTruncatedMicroseconds(&builder, nanos, tm.Precision)

	return builder.String()
}

// Holds group indexes for a given source types regex.
type timeRegexGroups struct {
	Hours   int
	Minutes int
	Seconds int
	Fractal int
}

// Cached info on TM time regex groups.
var timeRegexGroupsTM = timeRegexGroups{
	Hours:   tmRegexGroupHours,
	Minutes: tmRegexGroupMinutes,
	Seconds: tmRegexGroupSeconds,
	Fractal: tmRegexGroupFractal,
}

// Cached info on DT time regex groups.
var timeRegexGroupsDT = timeRegexGroups{
	Hours:   dtRegexGroupHours,
	Minutes: dtRegexGroupMinutes,
	Seconds: dtRegexGroupSeconds,
	Fractal: dtRegexGroupFractal,
}

func extractTime(
	matches []string,
	precisionIn PrecisionLevel,
	isTM bool,
) (
	hours durationInfo,
	minutes durationInfo,
	seconds durationInfo,
	nanos durationInfo,
	precisionOut PrecisionLevel,
	err error,
) {
	groupIndexes := timeRegexGroupsTM
	if !isTM {
		groupIndexes = timeRegexGroupsDT
	}

	hours, err = extractDurationInfo(matches, groupIndexes.Hours, false)
	if err != nil {
		return hours, minutes, seconds, nanos, precisionOut, err
	}
	precisionOut = updatePrecision(hours, precisionIn, PrecisionHours, false)

	minutes, err = extractDurationInfo(matches, groupIndexes.Minutes, false)
	if err != nil {
		return hours, minutes, seconds, nanos, precisionOut, err
	}
	precisionOut = updatePrecision(minutes, precisionOut, PrecisionMinutes, false)

	seconds, err = extractDurationInfo(matches, groupIndexes.Seconds, false)
	if err != nil {
		return hours, minutes, seconds, nanos, precisionOut, err
	}
	precisionOut = updatePrecision(seconds, precisionOut, PrecisionSeconds, false)

	nanos, err = extractDurationInfo(matches, groupIndexes.Fractal, true)
	if err != nil {
		return hours, minutes, seconds, nanos, precisionOut, err
	}

	if nanos.PresentInSource {
		precisionOut = nanos.FractalPrecision
	}

	return hours, minutes, seconds, nanos, precisionOut, err
}

// ParseTime converts DICOM TM (time) value to dcmtime.Time. A leap second
// (SS=60) is clamped to hh:mm:59.999999999 with LeapSecond set.
func ParseTime(tmString string) (Time, error) {
	matches := tmRegex.FindStringSubmatch(tmString)
	// If no full match is found, return an error
	if len(matches) == 0 {
		return Time{}, ErrParseTM
	}

	hours, minutes, seconds, nanos, precision, err := extractTime(
		matches, PrecisionFull, true,
	)
	if err != nil {
		return Time{}, err
	}
	if !validTimeComponents(hours, minutes, seconds, nanos) {
		return Time{}, ErrParseTM
	}
	leap, leapFraction := clampLeapSecond(&seconds, &nanos)

	// Return a new time.Time with the given values and UTC encoding.
	parsed := time.Date(
		// If these are set to 0, they try to roll back to the last
		// date that makes sense, so we are going to set all
		// of them to 1.
		1,
		1,
		1,
		hours.Value,
		minutes.Value,
		seconds.Value,
		nanos.Value,
		zeroTimezone,
	)

	return Time{
		Time:         parsed,
		Precision:    precision,
		LeapSecond:   leap,
		leapFraction: leapFraction,
	}, nil
}

// clampLeapSecond turns a seconds component of 60 into the last instant of
// the minute, returning whether it did and the fraction it replaced.
func clampLeapSecond(seconds, nanos *durationInfo) (bool, int) {
	if seconds.Value != 60 {
		return false, 0
	}
	fraction := nanos.Value
	seconds.Value = 59
	nanos.Value = leapSecondNanos
	return true, fraction
}
