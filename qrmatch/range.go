package qrmatch

import (
	"fmt"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
)

func matchTemporal(vr core.VR, query, candidate string, lenientDates bool) (bool, error) {
	return matchTemporalIn(vr, query, candidate, lenientDates, nil, nil)
}

// matchTemporalIn matches like matchTemporal, reading DT values without a UTC
// offset in queryZone and candidateZone when they are non-nil (timezone query
// adjustment).
func matchTemporalIn(vr core.VR, query, candidate string, lenientDates bool, queryZone, candidateZone *time.Location) (bool, error) {
	query = strings.TrimSpace(query)
	candidate = strings.TrimSpace(candidate)
	if query == "" {
		return true, nil
	}
	if strings.ContainsAny(query, "*?") {
		return false, fmt.Errorf("%w: wildcard is not allowed for %s matching", ErrInvalidQuery, vr)
	}
	candidateTime, candidatePrecision, err := parseTemporalIn(vr, candidate, true, candidateZone)
	if err != nil {
		return false, nil
	}
	candidateStart, candidateEnd := temporalWindow(candidateTime, candidatePrecision, vr)
	bounds, err := queryBounds(vr, query, lenientDates, queryZone)
	if err != nil {
		return false, err
	}
	if bounds.lowerSet && candidateEnd.Before(bounds.lower) {
		return false, nil
	}
	if bounds.upperSet && candidateStart.After(bounds.upper) {
		return false, nil
	}
	return true, nil
}

// temporalBounds is the period a DA, TM or DT matching key selects; an unset
// bound is open.
type temporalBounds struct {
	lower, upper       time.Time
	lowerSet, upperSet bool
}

// maxDTRangeHyphens is the most '-' characters a DT range can hold: a
// negative UTC offset on each bound and the range separator.
const maxDTRangeHyphens = 3

// queryBounds reads a single-value or range matching key (PS3.4 C.2.2.2.5).
//
// A DT value may end in a negative UTC offset (&ZZXX, PS3.5 6.2), so a '-' in
// a DT key is the range separator only where it splits the key into valid
// bounds with the first not after the second. When no '-' does, the key is
// read as a single value with an offset. A key that splits validly at more
// than one '-' is ambiguous and invalid. A range reading wins over a single
// value, so every key that was a valid range before offsets were recognized
// keeps its meaning.
func queryBounds(vr core.VR, query string, lenientDates bool, zone *time.Location) (temporalBounds, error) {
	if !strings.Contains(query, "-") {
		value, precision, err := parseTemporalIn(vr, query, lenientDates, zone)
		if err != nil {
			return temporalBounds{}, fmt.Errorf("%w: invalid %s matching value", ErrInvalidQuery, vr)
		}
		start, end := temporalWindow(value, precision, vr)
		return temporalBounds{lower: start, upper: end, lowerSet: true, upperSet: true}, nil
	}
	invalidRange := fmt.Errorf("%w: invalid %s range", ErrInvalidQuery, vr)
	if vr != core.VRDT {
		if strings.Count(query, "-") != 1 {
			return temporalBounds{}, invalidRange
		}
		from, to, _ := strings.Cut(query, "-")
		bounds, ok := rangeBounds(vr, from, to, lenientDates, zone)
		if !ok {
			return temporalBounds{}, invalidRange
		}
		return bounds, nil
	}
	if strings.Count(query, "-") > maxDTRangeHyphens {
		return temporalBounds{}, invalidRange
	}
	var found temporalBounds
	readings := 0
	for index := 0; index < len(query); index++ {
		if query[index] != '-' {
			continue
		}
		if bounds, ok := rangeBounds(vr, query[:index], query[index+1:], lenientDates, zone); ok {
			found = bounds
			readings++
		}
	}
	switch readings {
	case 0:
		// Every '-' is part of a negative UTC offset.
		value, precision, err := parseTemporalIn(vr, query, lenientDates, zone)
		if err != nil {
			return temporalBounds{}, invalidRange
		}
		start, end := temporalWindow(value, precision, vr)
		return temporalBounds{lower: start, upper: end, lowerSet: true, upperSet: true}, nil
	case 1:
		return found, nil
	default:
		return temporalBounds{}, fmt.Errorf("%w: ambiguous %s range", ErrInvalidQuery, vr)
	}
}

// rangeBounds reads the bounds either side of a range separator. It reports
// false unless at least one bound is given, every given bound is valid, and
// the lower bound is not after the upper one.
func rangeBounds(vr core.VR, from, to string, lenientDates bool, zone *time.Location) (temporalBounds, bool) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" && to == "" {
		return temporalBounds{}, false
	}
	var bounds temporalBounds
	if from != "" {
		value, precision, err := parseTemporalIn(vr, from, lenientDates, zone)
		if err != nil {
			return temporalBounds{}, false
		}
		bounds.lower, _ = temporalWindow(value, precision, vr)
		bounds.lowerSet = true
	}
	if to != "" {
		value, precision, err := parseTemporalIn(vr, to, lenientDates, zone)
		if err != nil {
			return temporalBounds{}, false
		}
		_, bounds.upper = temporalWindow(value, precision, vr)
		bounds.upperSet = true
	}
	if bounds.lowerSet && bounds.upperSet && bounds.lower.After(bounds.upper) {
		return temporalBounds{}, false
	}
	return bounds, true
}

// parseTemporal parses a DA, TM or DT value. A DA value other than YYYYMMDD
// (a year, a year and month, or the ACR-NEMA YYYY.MM.DD form) is accepted
// only when lenientDates is set.
func parseTemporal(vr core.VR, value string, lenientDates bool) (time.Time, dcmtime.PrecisionLevel, error) {
	return parseTemporalIn(vr, value, lenientDates, nil)
}

// parseTemporalIn parses like parseTemporal and, when zone is non-nil, reads
// a DT value that carries no UTC offset in zone.
func parseTemporalIn(vr core.VR, value string, lenientDates bool, zone *time.Location) (time.Time, dcmtime.PrecisionLevel, error) {
	switch vr {
	case core.VRDA:
		parsed, err := dcmtime.ParseDate(value)
		if err != nil {
			return time.Time{}, 0, err
		}
		if !lenientDates && (parsed.IsNEMA || parsed.Precision != dcmtime.PrecisionFull) {
			return time.Time{}, 0, dcmtime.ErrParseDA
		}
		return parsed.Time, parsed.Precision, nil
	case core.VRTM:
		parsed, err := dcmtime.ParseTime(value)
		if err != nil {
			return time.Time{}, 0, err
		}
		return parsed.Time, leapPrecision(parsed.Precision, parsed.LeapSecond), nil
	case core.VRDT:
		parsed, err := dcmtime.ParseDatetime(value)
		if err != nil {
			return time.Time{}, 0, err
		}
		precision := leapPrecision(parsed.Precision, parsed.LeapSecond)
		if zone != nil && parsed.NoOffset {
			t := parsed.Time
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), zone), precision, nil
		}
		return parsed.Time, precision, nil
	default:
		return time.Time{}, 0, fmt.Errorf("unsupported temporal VR %s", vr)
	}
}

// leapPrecision returns the precision to match a value at. dcmtime holds a
// leap second (SS=60) as the last instant of its minute, hh:mm:59.999999999;
// it is matched as that instant, which windows of minute or coarser
// precision contain and the window of second 59 does not.
func leapPrecision(precision dcmtime.PrecisionLevel, leap bool) dcmtime.PrecisionLevel {
	if leap {
		return dcmtime.PrecisionFull
	}
	return precision
}

// temporalWindow is the period a value covers at its precision. Windows of
// minute or coarser precision end at the last nanosecond so that they contain
// a leap second; a window of seconds or a fraction ends at the last
// microsecond DICOM can express.
func temporalWindow(value time.Time, precision dcmtime.PrecisionLevel, vr core.VR) (time.Time, time.Time) {
	loc := value.Location()
	if loc == nil {
		loc = time.UTC
	}
	switch vr {
	case core.VRDA:
		return dateWindow(value, precision, loc)
	case core.VRTM:
		return timeWindow(value, precision, loc)
	default:
		dateStart, dateEnd := dateWindow(value, precision, loc)
		if precision == dcmtime.PrecisionYear || precision == dcmtime.PrecisionMonth || precision == dcmtime.PrecisionDay {
			start := time.Date(dateStart.Year(), dateStart.Month(), dateStart.Day(), 0, 0, 0, 0, loc)
			end := time.Date(dateEnd.Year(), dateEnd.Month(), dateEnd.Day(), 23, 59, 59, 999999999, loc)
			return start, end
		}
		timeStart, timeEnd := timeWindow(value, precision, loc)
		start := time.Date(dateStart.Year(), dateStart.Month(), dateStart.Day(), timeStart.Hour(), timeStart.Minute(), timeStart.Second(), timeStart.Nanosecond(), loc)
		end := time.Date(dateEnd.Year(), dateEnd.Month(), dateEnd.Day(), timeEnd.Hour(), timeEnd.Minute(), timeEnd.Second(), timeEnd.Nanosecond(), loc)
		return start, end
	}
}

func dateWindow(value time.Time, precision dcmtime.PrecisionLevel, loc *time.Location) (time.Time, time.Time) {
	year, month, day := value.Date()
	switch precision {
	case dcmtime.PrecisionYear:
		return time.Date(year, 1, 1, 0, 0, 0, 0, loc), time.Date(year, 12, 31, 0, 0, 0, 0, loc)
	case dcmtime.PrecisionMonth:
		start := time.Date(year, month, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 1, -1)
		return start, end
	default:
		dayValue := time.Date(year, month, day, 0, 0, 0, 0, loc)
		return dayValue, dayValue
	}
}

func timeWindow(value time.Time, precision dcmtime.PrecisionLevel, loc *time.Location) (time.Time, time.Time) {
	base := time.Date(1, 1, 1, value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), loc)
	switch precision {
	case dcmtime.PrecisionHours:
		return time.Date(1, 1, 1, value.Hour(), 0, 0, 0, loc), time.Date(1, 1, 1, value.Hour(), 59, 59, 999999999, loc)
	case dcmtime.PrecisionMinutes:
		return time.Date(1, 1, 1, value.Hour(), value.Minute(), 0, 0, loc), time.Date(1, 1, 1, value.Hour(), value.Minute(), 59, 999999999, loc)
	case dcmtime.PrecisionSeconds:
		return time.Date(1, 1, 1, value.Hour(), value.Minute(), value.Second(), 0, loc), time.Date(1, 1, 1, value.Hour(), value.Minute(), value.Second(), 999999000, loc)
	default:
		return base, base
	}
}
