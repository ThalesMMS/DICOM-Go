package qrmatch

import (
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

var (
	studyDateTag = core.NewTag(0x0008, 0x0020)
	studyTimeTag = core.NewTag(0x0008, 0x0030)
	timezoneTag  = tags.TimezoneOffsetFromUTC
	acqDTTag     = core.NewTag(0x0008, 0x002A) // Acquisition DateTime
)

func TestMatchCombinedDateTime(t *testing.T) {
	tests := []struct {
		name                     string
		queryDate, queryTime     string
		candidateDate, candTime  string
		independent, combined    bool
		omitCandidateTimeElement bool
	}{
		// PS3.4 C.2.2.2.5 example: July 5 10:00 until July 7 18:00.
		{name: "middle day early morning", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060706", candTime: "0700", independent: false, combined: true},
		{name: "first day before start", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060705", candTime: "0930", independent: false, combined: false},
		{name: "first day after start", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060705", candTime: "2300", independent: false, combined: true},
		{name: "last day after end", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060707", candTime: "1801", independent: false, combined: false},
		{name: "inside both", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060706", candTime: "1200", independent: true, combined: true},
		{name: "open lower bounds", queryDate: "-20060707", queryTime: "-1800", candidateDate: "20060101", candTime: "2300", independent: false, combined: true},
		{name: "open upper bounds", queryDate: "20060705-", queryTime: "1000-", candidateDate: "20060801", candTime: "0100", independent: false, combined: true},
		{name: "single values", queryDate: "20060705", queryTime: "1015", candidateDate: "20060705", candTime: "101530", independent: true, combined: true},
		{name: "candidate without time covers the day", queryDate: "20060705-20060707", queryTime: "1000-1800", candidateDate: "20060705", omitCandidateTimeElement: true, independent: false, combined: true},
		// Different forms keep independent matching.
		{name: "range date single time", queryDate: "20060705-20060707", queryTime: "1000", candidateDate: "20060706", candTime: "0700", independent: false, combined: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query := dataset(str(studyDateTag, core.VRDA, tc.queryDate), str(studyTimeTag, core.VRTM, tc.queryTime))
			candidateElements := []core.Element{str(studyDateTag, core.VRDA, tc.candidateDate)}
			if !tc.omitCandidateTimeElement {
				candidateElements = append(candidateElements, str(studyTimeTag, core.VRTM, tc.candTime))
			}
			candidate := dataset(candidateElements...)
			for _, combined := range []bool{false, true} {
				want := tc.independent
				if combined {
					want = tc.combined
				}
				if tc.omitCandidateTimeElement && !combined {
					continue // independent matching needs the TM attribute
				}
				result, err := Match(query, candidate, Options{CombinedDateTime: combined})
				if err != nil {
					t.Fatal(err)
				}
				if result.Match != want {
					t.Fatalf("CombinedDateTime=%v: Match = %v, want %v", combined, result.Match, want)
				}
			}
		})
	}
}

func TestMatchCombinedDateTimeRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ date, time string }{
		{"20060707-20060705", "1000-1800"},
		{"20060705-20060707", "10x0-1800"},
		{"-", "-"},
	} {
		query := dataset(str(studyDateTag, core.VRDA, tc.date), str(studyTimeTag, core.VRTM, tc.time))
		candidate := dataset(str(studyDateTag, core.VRDA, "20060706"), str(studyTimeTag, core.VRTM, "1200"))
		if _, err := Match(query, candidate, Options{CombinedDateTime: true}); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("%s %s: err = %v, want ErrInvalidQuery", tc.date, tc.time, err)
		}
	}
}

func TestMatchCombinedDateTimeRespectsSupportedMatching(t *testing.T) {
	query := dataset(str(studyDateTag, core.VRDA, "20060705-20060707"), str(studyTimeTag, core.VRTM, "1000-1800"))
	candidate := dataset(str(studyDateTag, core.VRDA, "20060706"), str(studyTimeTag, core.VRTM, "0700"))
	// Study Time is not supported: the pair is not combined, the date alone
	// matches and Study Time is reported as an unsupported optional key.
	result, err := Match(query, candidate, Options{CombinedDateTime: true, SupportedMatching: []core.Tag{studyDateTag}})
	if err != nil || !result.Match || len(result.UnsupportedOptional) != 1 {
		t.Fatalf("Match = %+v, %v", result, err)
	}
}

func TestMatchTimezoneAdjustment(t *testing.T) {
	newYork := time.FixedZone("-0500", -5*3600)
	tests := []struct {
		name      string
		query     []core.Element
		candidate []core.Element
		fallback  *time.Location
		adjust    bool
		want      bool
	}{
		{
			name:      "DT shifted to candidate offset",
			query:     []core.Element{str(acqDTTag, core.VRDT, "20240101100000-20240101110000"), str(timezoneTag, core.VRSH, "+0100")},
			candidate: []core.Element{str(acqDTTag, core.VRDT, "20240101043000"), str(timezoneTag, core.VRSH, "-0500")},
			adjust:    true, want: true, // 04:30 -0500 is 10:30 +0100
		},
		{
			name:      "DT not adjusted without the option",
			query:     []core.Element{str(acqDTTag, core.VRDT, "20240101100000-20240101110000"), str(timezoneTag, core.VRSH, "+0100")},
			candidate: []core.Element{str(acqDTTag, core.VRDT, "20240101043000"), str(timezoneTag, core.VRSH, "-0500")},
			adjust:    false, want: false,
		},
		{
			name:      "candidate fallback zone",
			query:     []core.Element{str(acqDTTag, core.VRDT, "20240101100000-20240101110000"), str(timezoneTag, core.VRSH, "+0100")},
			candidate: []core.Element{str(acqDTTag, core.VRDT, "20240101043000")},
			fallback:  newYork, adjust: true, want: true,
		},
		{
			name:      "candidate with explicit DT offset",
			query:     []core.Element{str(acqDTTag, core.VRDT, "20240101100000-20240101110000"), str(timezoneTag, core.VRSH, "+0100")},
			candidate: []core.Element{str(acqDTTag, core.VRDT, "20240101093000+0000")},
			adjust:    true, want: true,
		},
		{
			name:      "no query offset leaves values unadjusted",
			query:     []core.Element{str(acqDTTag, core.VRDT, "20240101100000-20240101110000")},
			candidate: []core.Element{str(acqDTTag, core.VRDT, "20240101043000"), str(timezoneTag, core.VRSH, "-0500")},
			adjust:    true, want: false,
		},
		{
			name:      "combined study date and time cross midnight",
			query:     []core.Element{str(studyDateTag, core.VRDA, "20240102"), str(studyTimeTag, core.VRTM, "0300"), str(timezoneTag, core.VRSH, "+0900")},
			candidate: []core.Element{str(studyDateTag, core.VRDA, "20240101"), str(studyTimeTag, core.VRTM, "1300"), str(timezoneTag, core.VRSH, "-0500")},
			adjust:    true, want: true, // 2024-01-02 03:00 +0900 is 2024-01-01 13:00 -0500
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Match(dataset(tc.query...), dataset(tc.candidate...), Options{
				TimezoneAdjustment: tc.adjust,
				CombinedDateTime:   true,
				CandidateTimezone:  tc.fallback,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Match != tc.want {
				t.Fatalf("Match = %v, want %v", result.Match, tc.want)
			}
		})
	}
	query := dataset(str(acqDTTag, core.VRDT, "20240101"), str(timezoneTag, core.VRSH, "+2500"))
	if _, err := Match(query, dataset(str(acqDTTag, core.VRDT, "20240101")), Options{TimezoneAdjustment: true}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("malformed query offset err = %v", err)
	}
}

func TestParseTimezoneOffset(t *testing.T) {
	for value, want := range map[string]int{"+0000": 0, "-0500": -18000, "+0530": 19800, "+1400": 50400} {
		zone, err := ParseTimezoneOffset(value)
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if _, offset := time.Date(2024, 1, 1, 0, 0, 0, 0, zone).Zone(); offset != want {
			t.Fatalf("%s offset %d, want %d", value, offset, want)
		}
	}
	for _, value := range []string{"", "0500", "+05", "+1500", "+0560", "+-100", "+05:0"} {
		if _, err := ParseTimezoneOffset(value); err == nil {
			t.Fatalf("%q parsed", value)
		}
	}
}
