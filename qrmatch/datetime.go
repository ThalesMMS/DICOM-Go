package qrmatch

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

// dateTimePair is a DA attribute and the TM attribute that completes it.
type dateTimePair struct {
	date core.Tag
	time core.Tag
}

// combinedDateTimePairs are the DA/TM key pairs that combined date-time
// matching treats as one date-time (PS3.4 C.2.2.2.5).
var combinedDateTimePairs = []dateTimePair{
	{core.NewTag(0x0008, 0x0012), core.NewTag(0x0008, 0x0013)}, // Instance Creation Date/Time
	{core.NewTag(0x0008, 0x0020), core.NewTag(0x0008, 0x0030)}, // Study Date/Time
	{core.NewTag(0x0008, 0x0021), core.NewTag(0x0008, 0x0031)}, // Series Date/Time
	{core.NewTag(0x0008, 0x0022), core.NewTag(0x0008, 0x0032)}, // Acquisition Date/Time
	{core.NewTag(0x0008, 0x0023), core.NewTag(0x0008, 0x0033)}, // Content Date/Time
	{core.NewTag(0x0010, 0x0030), core.NewTag(0x0010, 0x0032)}, // Patient's Birth Date/Time
	{core.NewTag(0x0040, 0x0002), core.NewTag(0x0040, 0x0003)}, // Scheduled Procedure Step Start Date/Time
	{core.NewTag(0x0040, 0x0244), core.NewTag(0x0040, 0x0245)}, // Performed Procedure Step Start Date/Time
}

// matchCombinedDateTime matches the DA/TM pairs of query that combined
// date-time matching applies to. It returns the tags it handled, which the
// per-attribute loop then skips, and whether every handled pair matched.
func (state *matchState) matchCombinedDateTime(query, candidate *object.Object) (map[core.Tag]struct{}, bool, error) {
	if !state.combinedDateTime {
		return nil, true, nil
	}
	var handled map[core.Tag]struct{}
	matched := true
	for _, pair := range combinedDateTimePairs {
		dateElement, ok := query.Get(pair.date)
		if !ok {
			continue
		}
		timeElement, ok := query.Get(pair.time)
		if !ok {
			continue
		}
		dateValues, timeValues := stringValues(dateElement), stringValues(timeElement)
		if len(dateValues) != 1 || len(timeValues) != 1 {
			continue
		}
		if state.emptyValueMatching && (isEmptyMatchingKey(core.VRDA, dateValues) || isEmptyMatchingKey(core.VRTM, timeValues)) {
			continue
		}
		dateQuery, timeQuery := strings.TrimSpace(dateValues[0]), strings.TrimSpace(timeValues[0])
		if dateQuery == "" || timeQuery == "" || strings.ContainsAny(dateQuery+timeQuery, "*?") {
			continue
		}
		// PS3.4 C.2.2.2.5 combines a pair given in the same range matching
		// form; a range with a single value keeps independent matching.
		if strings.Contains(dateQuery, "-") != strings.Contains(timeQuery, "-") {
			continue
		}
		if !state.isSupported(pair.date) || !state.isSupported(pair.time) {
			continue
		}
		if handled == nil {
			handled = make(map[core.Tag]struct{}, 2)
		}
		handled[pair.date] = struct{}{}
		handled[pair.time] = struct{}{}
		ok, err := state.matchDateTimePair(pair, dateQuery, timeQuery, candidate)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			matched = false
		}
	}
	return handled, matched, nil
}

func (state *matchState) isSupported(tag core.Tag) bool {
	if state.supported == nil {
		return true
	}
	_, ok := state.supported[tag]
	return ok
}

func (state *matchState) matchDateTimePair(pair dateTimePair, dateQuery, timeQuery string, candidate *object.Object) (bool, error) {
	lower, upper, lowerSet, upperSet, err := combinedQueryWindow(dateQuery, timeQuery, state.lenientDates, zoneOrUTC(state.queryZone))
	if err != nil {
		return false, err
	}
	dateElement, ok := candidate.Get(pair.date)
	if !ok {
		return false, nil
	}
	candidateTime := ""
	if timeElement, ok := candidate.Get(pair.time); ok {
		for _, value := range stringValues(timeElement) {
			if value = strings.TrimSpace(value); value != "" {
				candidateTime = value
				break
			}
		}
	}
	candidateDates := stringValues(dateElement)
	if err := state.checkValues(candidateDates); err != nil {
		return false, err
	}
	zone := zoneOrUTC(state.candidateZone)
	for _, value := range candidateDates {
		start, end, ok := candidateDateTimeWindow(strings.TrimSpace(value), candidateTime, zone)
		if !ok {
			continue
		}
		if lowerSet && end.Before(lower) {
			continue
		}
		if upperSet && start.After(upper) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// combinedQueryWindow turns a DA and TM matching value in the same form into
// one date-time window: a range's lower bounds start it and its upper bounds
// end it; an omitted time bound is the start or end of the day. A time bound
// without its date bound cannot be placed and is ignored.
func combinedQueryWindow(dateQuery, timeQuery string, lenientDates bool, zone *time.Location) (lower, upper time.Time, lowerSet, upperSet bool, err error) {
	invalid := func() (time.Time, time.Time, bool, bool, error) {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("%w: invalid combined DA/TM matching value", ErrInvalidQuery)
	}
	dateLower, dateUpper, timeLower, timeUpper := dateQuery, dateQuery, timeQuery, timeQuery
	if strings.Contains(dateQuery, "-") {
		if strings.Count(dateQuery, "-") != 1 || strings.Count(timeQuery, "-") != 1 {
			return invalid()
		}
		dateBounds := strings.SplitN(dateQuery, "-", 2)
		timeBounds := strings.SplitN(timeQuery, "-", 2)
		dateLower, dateUpper = strings.TrimSpace(dateBounds[0]), strings.TrimSpace(dateBounds[1])
		timeLower, timeUpper = strings.TrimSpace(timeBounds[0]), strings.TrimSpace(timeBounds[1])
		if dateLower == "" && dateUpper == "" {
			return invalid()
		}
	}
	startOfDay, endOfDay := dayBounds()
	timeStart, timeEnd := startOfDay, endOfDay
	if timeLower != "" {
		value, precision, parseErr := parseTemporal(core.VRTM, timeLower, false)
		if parseErr != nil {
			return invalid()
		}
		timeStart, _ = timeWindow(value, precision, time.UTC)
	}
	if timeUpper != "" {
		value, precision, parseErr := parseTemporal(core.VRTM, timeUpper, false)
		if parseErr != nil {
			return invalid()
		}
		_, timeEnd = timeWindow(value, precision, time.UTC)
	}
	if dateLower != "" {
		value, precision, parseErr := parseTemporal(core.VRDA, dateLower, lenientDates)
		if parseErr != nil {
			return invalid()
		}
		day, _ := dateWindow(value, precision, time.UTC)
		lower, lowerSet = atTimeOfDay(day, timeStart, zone), true
	}
	if dateUpper != "" {
		value, precision, parseErr := parseTemporal(core.VRDA, dateUpper, lenientDates)
		if parseErr != nil {
			return invalid()
		}
		_, day := dateWindow(value, precision, time.UTC)
		upper, upperSet = atTimeOfDay(day, timeEnd, zone), true
	}
	if lowerSet && upperSet && lower.After(upper) {
		return invalid()
	}
	return lower, upper, lowerSet, upperSet, nil
}

// candidateDateTimeWindow is the period a candidate's date and time cover;
// without a readable time the whole day.
func candidateDateTimeWindow(dateValue, timeValue string, zone *time.Location) (time.Time, time.Time, bool) {
	value, precision, err := parseTemporal(core.VRDA, dateValue, true)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	firstDay, lastDay := dateWindow(value, precision, time.UTC)
	timeStart, timeEnd := dayBounds()
	if timeValue != "" {
		if parsed, timePrecision, err := parseTemporal(core.VRTM, timeValue, true); err == nil {
			timeStart, timeEnd = timeWindow(parsed, timePrecision, time.UTC)
		}
	}
	return atTimeOfDay(firstDay, timeStart, zone), atTimeOfDay(lastDay, timeEnd, zone), true
}

func dayBounds() (time.Time, time.Time) {
	return time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1, 1, 1, 23, 59, 59, 999999999, time.UTC)
}

func atTimeOfDay(day, clock time.Time, zone *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), zone)
}

func zoneOrUTC(zone *time.Location) *time.Location {
	if zone == nil {
		return time.UTC
	}
	return zone
}

// timezoneAdjustment returns the zones timezone query adjustment reads query
// and candidate values in: nil zones when the query carries no Timezone
// Offset From UTC. A malformed query offset is an invalid query; a malformed
// candidate offset falls back like an absent one.
func timezoneAdjustment(query, candidate *object.Object, fallback *time.Location) (*time.Location, *time.Location, error) {
	queryOffset, ok := query.GetString(tags.TimezoneOffsetFromUTC)
	if !ok || strings.TrimSpace(queryOffset) == "" {
		return nil, nil, nil
	}
	queryZone, err := ParseTimezoneOffset(queryOffset)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	candidateZone := fallback
	if candidateOffset, ok := candidate.GetString(tags.TimezoneOffsetFromUTC); ok {
		if zone, err := ParseTimezoneOffset(candidateOffset); err == nil {
			candidateZone = zone
		}
	}
	if candidateZone == nil {
		candidateZone = queryZone
	}
	return queryZone, candidateZone, nil
}

// ParseTimezoneOffset parses a Timezone Offset From UTC (0008,0201) value of
// the form &ZZXX (PS3.3 C.12.1.1.8), such as +0100 or -0500, into a fixed
// zone. As for a DT offset (PS3.5 6.2), the offset must lie in -1200 to
// +1400 with minutes 00 to 59, and UTC is +0000, never -0000.
func ParseTimezoneOffset(value string) (*time.Location, error) {
	value = strings.TrimSpace(value)
	if len(value) != 5 || (value[0] != '+' && value[0] != '-') {
		return nil, fmt.Errorf("dicom qrmatch: timezone offset %q is not &ZZXX", value)
	}
	hours, err := strconv.Atoi(value[1:3])
	if err != nil || value[1] < '0' || value[1] > '9' {
		return nil, fmt.Errorf("dicom qrmatch: timezone offset %q hours", value)
	}
	minutes, err := strconv.Atoi(value[3:5])
	if err != nil || value[3] < '0' || value[3] > '9' || minutes > 59 {
		return nil, fmt.Errorf("dicom qrmatch: timezone offset %q minutes", value)
	}
	maxHours := 14
	if value[0] == '-' {
		maxHours = 12
	}
	if hours > maxHours || hours == maxHours && minutes != 0 {
		return nil, fmt.Errorf("dicom qrmatch: timezone offset %q is outside -1200 to +1400", value)
	}
	if value == "-0000" {
		return nil, fmt.Errorf("dicom qrmatch: timezone offset %q: UTC is +0000", value)
	}
	offset := hours*3600 + minutes*60
	if value[0] == '-' {
		offset = -offset
	}
	return time.FixedZone(value, offset), nil
}
