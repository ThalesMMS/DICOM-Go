package dcmtime_test

// Cross-checks against DCMTK's date/time vectors (#1053): tvrdatim.cc
// (DcmDate/DcmTime/DcmDateTime) and tofdatim.cc (OFDate/OFTime/OFDateTime),
// imported under testdata/upstream/dcmtk. OFDate and OFTime also parse ISO
// 8601 extended forms ("2000-12-31", "12:15:30 -02:30"), which are not DICOM
// values and are skipped here; the DICOM forms are compared with the DA, TM
// and DT parsers. Divergences name their normative reason; docs/CONFORMANCE.md
// lists the same cases.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dcmtime"
)

func readDCMTKVectors(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "dcmtk", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// divergenceChecker compares dicom-go with DCMTK, allowing the listed
// divergences and reporting any that no longer apply.
type divergenceChecker struct {
	t           *testing.T
	file        string
	divergences map[int]string
	seen        map[int]bool
}

func (c *divergenceChecker) compare(line int, got, want any) {
	c.t.Helper()
	if c.seen == nil {
		c.seen = map[int]bool{}
	}
	reason, diverges := c.divergences[line]
	c.seen[line] = true
	switch {
	case got != want && !diverges:
		c.t.Errorf("%s:%d: dicom-go %v, DCMTK %v", c.file, line, got, want)
	case got == want && diverges:
		c.t.Errorf("%s:%d: now agrees with DCMTK; remove the divergence %q", c.file, line, reason)
	}
}

func (c *divergenceChecker) done() {
	c.t.Helper()
	for line := range c.divergences {
		if !c.seen[line] {
			c.t.Errorf("%s: divergence %d names no check", c.file, line)
		}
	}
}

const (
	acrNemaTime = "ACR-NEMA time HH:MM[:SS] read by DCMTK; PS3.5 6.2 TM notes the format is not " +
		"DICOM conformant, so ParseTime rejects it"
	acrNemaDate = "ACR-NEMA date YYYY.MM.DD read by DCMTK's OFDate; PS3.5 6.2 DA notes the format " +
		"is not DICOM conformant, so ParseDA rejects it (ParseDate reads it and sets IsNEMA)"
	timeWithOffset = "OFTime accepts a UTC offset after the time; PS3.5 6.2 TM has no offset " +
		"component (only DT does), so ParseTime rejects it"
)

var vrdatimDivergences = map[int]string{
	117: acrNemaTime,
	122: acrNemaTime,
}

// clock matches the times DCMTK prints (12:03, 12:03:15, 12:03:01.990000,
// 2002-04-10 12:03:15 +05:00).
var clock = regexp.MustCompile(`^(?:(\d{4})-(\d{2})-(\d{2}) )?(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(?: ([-+]\d{2}):(\d{2}))?$`)

// clockFields reduces a printed DCMTK time to comparable fields.
func clockFields(t *testing.T, s string) string {
	t.Helper()
	m := clock.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("unexpected DCMTK output %q", s)
	}
	return fieldsOf(m[1], m[2], m[3], m[4], m[5], m[6], m[7], m[8], m[9])
}

func fieldsOf(year, month, day, hour, minute, second, fraction, offsetHours, offsetMinutes string) string {
	num := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	nanos := 0
	if fraction != "" {
		nanos = num((fraction + "000000000")[:9])
	}
	offset := num(offsetHours)*60 + num(offsetMinutes)*sign(offsetHours)
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%09d %+d", num(year), num(month), num(day),
		num(hour), num(minute), num(second), nanos, offset)
}

func sign(s string) int {
	if strings.HasPrefix(s, "-") {
		return -1
	}
	return 1
}

func timeFields(value time.Time, withDate bool) string {
	year, month, day := 0, 0, 0
	if withDate {
		year, month, day = value.Year(), int(value.Month()), value.Day()
	}
	_, offset := value.Zone()
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%09d %+d", year, month, day,
		value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), offset/60)
}

func TestTemporalValuesMatchDCMTK(t *testing.T) {
	var fixture struct {
		Values []struct {
			Line       int    `json:"line"`
			VR         string `json:"vr"`
			Components []int  `json:"components"`
			DICOM      string `json:"dicom"`
			Value      string `json:"value"`
			ParseOK    bool   `json:"parse_ok"`
			Printed    string `json:"printed"`
			ISO        []struct {
				Flags  map[string]bool `json:"flags"`
				Output string          `json:"output"`
			} `json:"iso"`
		} `json:"values"`
		TimeZones []struct {
			Line  int     `json:"line"`
			Value string  `json:"value"`
			OK    bool    `json:"ok"`
			Hours float64 `json:"hours"`
		} `json:"time_zones"`
	}
	readDCMTKVectors(t, "vrdatim_vectors.json", &fixture)
	if len(fixture.Values) < 10 || len(fixture.TimeZones) < 7 {
		t.Fatalf("%d values, %d time zones", len(fixture.Values), len(fixture.TimeZones))
	}
	check := divergenceChecker{t: t, file: "tvrdatim.cc", divergences: vrdatimDivergences}
	for _, v := range fixture.Values {
		if v.Components != nil {
			c := append(v.Components, 0, 0, 0)
			var got string
			switch v.VR {
			case "DA":
				got = dcmtime.Date{Time: time.Date(c[0], time.Month(c[1]), c[2], 0, 0, 0, 0, time.UTC), Precision: dcmtime.PrecisionFull}.DCM()
			case "TM":
				got = dcmtime.Time{Time: time.Date(1, 1, 1, c[0], c[1], c[2], 0, time.UTC), Precision: dcmtime.PrecisionSeconds}.DCM()
			case "DT":
				got = dcmtime.Datetime{Time: time.Date(c[0], time.Month(c[1]), c[2], c[3], c[4], c[5], 0, time.UTC), Precision: dcmtime.PrecisionSeconds, NoOffset: true}.DCM()
			}
			check.compare(v.Line, got, v.DICOM)
			continue
		}
		var parsed time.Time
		var err error
		withDate := v.VR == "DT"
		if withDate {
			var dt dcmtime.Datetime
			dt, err = dcmtime.ParseDatetime(v.Value)
			parsed = dt.Time
		} else {
			var tm dcmtime.Time
			tm, err = dcmtime.ParseTime(v.Value)
			parsed = tm.Time
		}
		want := "parse error"
		if v.ParseOK {
			printed := v.Printed
			if printed == "" && len(v.ISO) > 0 {
				printed = v.ISO[0].Output
			}
			want = clockFields(t, printed)
			if !withDate {
				want = "0000-00-00" + want[10:]
			}
		}
		got := "parse error"
		if err == nil {
			got = timeFields(parsed, withDate)
		}
		check.compare(v.Line, got, want)
	}
	for _, z := range fixture.TimeZones {
		// A DT offset is the only DICOM time zone notation (PS3.5 6.2).
		dt, err := dcmtime.ParseDatetime("20000101000000" + z.Value)
		got := "invalid"
		if err == nil {
			_, offset := dt.Time.Zone()
			got = strconv.FormatFloat(float64(offset)/3600, 'f', -1, 64)
		}
		want := "invalid"
		if z.OK {
			want = strconv.FormatFloat(z.Hours, 'f', -1, 64)
		}
		check.compare(z.Line, got, want)
	}
	check.done()
}

var ofdatimDivergences = map[int]string{
	54:  acrNemaDate,
	122: timeWithOffset,
	124: timeWithOffset,
	125: timeWithOffset,
	128: timeWithOffset,
	155: timeWithOffset,
}

// isoExtended reports the ISO 8601 extended forms (delimiters, 'T', spaces
// between the date and time) that are not DICOM DA, TM or DT values.
func isoExtended(method, value string) bool {
	if strings.ContainsAny(value, ":T") {
		return true
	}
	switch method {
	case "setISOFormattedDate":
		return strings.Contains(value, "-")
	case "setISOFormattedDateTime":
		return len(value) >= 10 && strings.ContainsAny(value[:10], "-.")
	}
	return false
}

func TestOFDateTimeSettersMatchDCMTK(t *testing.T) {
	var fixture struct {
		Entries []struct {
			Line          int       `json:"line"`
			Method        string    `json:"method"`
			OK            bool      `json:"ok"`
			Value         string    `json:"value"`
			Args          []float64 `json:"args"`
			TimeZoneHours *float64  `json:"time_zone_hours"`
		} `json:"entries"`
	}
	readDCMTKVectors(t, "ofdatim_vectors.json", &fixture)
	if len(fixture.Entries) < 80 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	offset := func(hours float64) string {
		minutes := int(math.Round(math.Abs(hours) * 60))
		s := "+"
		if hours < 0 {
			s = "-"
		}
		return fmt.Sprintf("%s%02d%02d", s, minutes/60, minutes%60)
	}
	check := divergenceChecker{t: t, file: "tofdatim.cc", divergences: ofdatimDivergences}
	compared := 0
	for _, e := range fixture.Entries {
		var err error
		zone := ""
		switch e.Method {
		case "setDate":
			_, err = dcmtime.ParseDA(fmt.Sprintf("%04d%02d%02d", int(e.Args[0]), int(e.Args[1]), int(e.Args[2])))
		case "setTime":
			_, err = dcmtime.ParseTime(fmt.Sprintf("%02d%02d%02d", int(e.Args[0]), int(e.Args[1]), int(e.Args[2])))
		case "setDateTime":
			_, err = dcmtime.ParseDatetime(fmt.Sprintf("%04d%02d%02d%02d%02d%02d", int(e.Args[0]), int(e.Args[1]),
				int(e.Args[2]), int(e.Args[3]), int(e.Args[4]), int(e.Args[5])))
		case "setTimeZone":
			_, err = dcmtime.ParseDatetime("20000101000000" + offset(e.Args[0]))
		case "setISOFormattedDate":
			if isoExtended(e.Method, e.Value) {
				continue
			}
			_, err = dcmtime.ParseDA(e.Value)
		case "setISOFormattedTime":
			if isoExtended(e.Method, e.Value) {
				continue
			}
			_, err = dcmtime.ParseTime(e.Value)
		case "setISOFormattedTimeZone":
			if isoExtended(e.Method, e.Value) {
				continue
			}
			var dt dcmtime.Datetime
			if dt, err = dcmtime.ParseDatetime("20000101000000" + e.Value); err == nil {
				_, seconds := dt.Time.Zone()
				zone = offset(float64(seconds) / 3600)
			}
		case "setISOFormattedDateTime":
			if isoExtended(e.Method, e.Value) {
				continue
			}
			_, err = dcmtime.ParseDatetime(e.Value)
		default:
			continue // setSecond: a fractional second, not a string form
		}
		compared++
		got, want := fmt.Sprint(err == nil), fmt.Sprint(e.OK)
		if e.TimeZoneHours != nil && err == nil && zone != "" {
			got, want = got+" "+zone, want+" "+offset(*e.TimeZoneHours)
		}
		check.compare(e.Line, got, want)
	}
	if compared < 40 {
		t.Fatalf("compared %d vectors", compared)
	}
	check.done()
}
