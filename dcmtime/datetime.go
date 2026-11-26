package dcmtime

import (
	"strings"
	"time"
)

// Datetime holds data for a parsed DICOM datetime (DT) value.
type Datetime struct {
	// Time is the underlying time.Time value.
	Time time.Time
	// Precision with which this value was stored. For instance, a DT value with a
	// precision of PrecisionYear ONLY stored the year.
	Precision PrecisionLevel
	// NoOffset: if false, offset information was not specifically included in the
	// original DT string, and will not be rendered with DCM()
	//
	// We use the negated version here for safer defaults - by default, without setting
	// this field explicitly, the timezone will be included.
	NoOffset bool
	// LeapSecond reports a seconds component of 60 (a leap second). As in
	// Time, the value is then held as hh:mm:59.999999999 so that it neither
	// rolls over into the next minute or day nor sorts before earlier values
	// of its minute; DCM and String render the seconds as 60.
	LeapSecond bool

	// leapFraction is the fraction of a leap second in nanoseconds.
	leapFraction int
}

// clock returns the Time rendering dt's time of day.
func (dt Datetime) clock() Time {
	return Time{Time: dt.Time, Precision: dt.Precision, LeapSecond: dt.LeapSecond, leapFraction: dt.leapFraction}
}

// DCM converts time.Time value to dicom DT string. Values are truncated to the
// DT.Precision value.
//
// If NoOffset is true, no offset will be encoded.
func (dt Datetime) DCM() string {
	builder := strings.Builder{}

	// We start by using the existing DA and TM formatters, since the bulk of datetime
	// is just those two formats slammed together.
	builder.WriteString(Date{Time: dt.Time, Precision: dt.Precision}.DCM())

	// Check that at lead
	if isIncluded(PrecisionHours, dt.Precision) {
		builder.WriteString(dt.clock().DCM())
	}

	// If we are not rendering the offset, return the current value
	if dt.NoOffset {
		return builder.String()
	}

	// Write the timezone info.
	builder.WriteString(dt.Time.Format("-0700"))

	return builder.String()
}

// String implements fmt.Stringer.
func (dt Datetime) String() string {
	builder := strings.Builder{}

	// We start by using the existing DA and TM formatters, since the bulk of datetime
	// is just those two formats slammed together.
	builder.WriteString(Date{Time: dt.Time, Precision: dt.Precision}.String())

	// Check that at lead
	if isIncluded(PrecisionHours, dt.Precision) {
		builder.WriteRune(' ')
		builder.WriteString(dt.clock().String())
	}

	// If we are not rendering the offset, return the current value
	if dt.NoOffset {
		return builder.String()
	}

	// Write the timezone info.
	builder.WriteString(dt.Time.Format(" -07:00"))

	return builder.String()
}

// ParseDatetime converts DICOM DT (datetime) value to time.Time, PrecisionLevel, and
// offset presence as UTC. A leap second (SS=60) is clamped to
// hh:mm:59.999999999 with LeapSecond set.
func ParseDatetime(dtString string) (Datetime, error) {
	matches := dtRegex.FindStringSubmatch(dtString)
	if len(matches) == 0 {
		return Datetime{}, ErrParseDT
	}

	year, month, day, precision, err := extractDate(matches, PrecisionFull, false)
	if err != nil {
		return Datetime{}, err
	}
	hours, minutes, seconds, nanos, precision, err := extractTime(
		matches, precision, false,
	)
	if err != nil {
		return Datetime{}, err
	}
	if !validDateComponents(year, month, day) || !validTimeComponents(hours, minutes, seconds, nanos) {
		return Datetime{}, ErrParseDT
	}
	leap, leapFraction := clampLeapSecond(&seconds, &nanos)

	var hasOffset bool

	offsetHours, err := extractDurationInfo(matches, dtRegexGroupOffsetHours, false)
	if err != nil {
		return Datetime{}, ErrParseDT
	}
	// If hours are not present, there is either no offset or the regex will fail,
	// so we only need to check this here.
	if offsetHours.PresentInSource {
		hasOffset = true
	}

	offsetMinutes, err := extractDurationInfo(matches, dtRegexGroupOffsetMinutes, false)
	if err != nil {
		return Datetime{}, ErrParseDT
	}

	offsetSign := 1
	// If the zone sign is '-', then we need to multiply the offset by -1
	if matches[dtRegexGroupOffsetSign] == "-" {
		offsetSign = -1
	}
	if hasOffset && !validUTCOffset(matches[dtRegexGroupOffsetSign], offsetHours, offsetMinutes) {
		return Datetime{}, ErrParseDT
	}

	// Get zone offset in seconds.
	offset := (offsetHours.Value*3600 + offsetMinutes.Value*60) * offsetSign

	// Return a new time.Time with the given values and UTC encoding.
	parsed := time.Date(
		year.Value,
		time.Month(month.Value),
		day.Value,
		hours.Value,
		minutes.Value,
		seconds.Value,
		nanos.Value,
		time.FixedZone("", offset),
	)

	return Datetime{
		Time:         parsed,
		Precision:    precision,
		NoOffset:     !hasOffset,
		LeapSecond:   leap,
		leapFraction: leapFraction,
	}, nil
}
