package qrmatch

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

// TestMatchDTNegativeOffsetIsNotARange checks that a '-' opening a DT UTC
// offset (PS3.5 6.2) is not read as the range separator (PS3.4 C.2.2.2.5.3),
// alone or on either bound of a range.
func TestMatchDTNegativeOffsetIsNotARange(t *testing.T) {
	tests := []struct {
		name, query, candidate string
		want                   bool
	}{
		{"single value same instant", "20260101120000-0300", "20260101150000+0000", true},
		{"single value same text", "20260101120000-0300", "20260101120000-0300", true},
		{"single value other instant", "20260101120000-0300", "20260101120000+0000", false},
		{"partial value with offset", "2007-0500", "20070615", true},
		{"closed range with offsets inside", "20260101110000-0300-20260101130000-0300", "20260101150000+0000", true},
		{"closed range with offsets after", "20260101110000-0300-20260101130000-0300", "20260101170000+0000", false},
		{"closed range with offsets before", "20260101110000-0300-20260101130000-0300", "20260101135959+0000", false},
		{"closed range offset on lower only", "20260101120000-0300-20260101160000+0000", "20260101155959+0000", true},
		{"closed range offset on upper only", "20260101140000+0000-20260101120000-0300", "20260101150001+0000", false},
		{"upper bound with offset includes", "-20260101120000-0300", "20260101150000+0000", true},
		{"upper bound with offset excludes", "-20260101120000-0300", "20260101150001+0000", false},
		{"lower bound with offset includes", "20260101120000-0300-", "20260101150000+0000", true},
		{"lower bound with offset excludes", "20260101120000-0300-", "20260101145959+0000", false},
		{"range without offsets", "20260604100000-20260604120000", "20260604110000", true},
		{"range read before offset", "1000-1100", "10500101", true},
		{"padded range", "20260101110000-0300 - 20260101130000-0300", "20260101150000+0000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Match(dataset(str(tags.AcquisitionDateTime, core.VRDT, tt.query)),
				dataset(str(tags.AcquisitionDateTime, core.VRDT, tt.candidate)), Options{})
			if err != nil {
				t.Fatalf("Match(%q, %q) error = %v", tt.query, tt.candidate, err)
			}
			if result.Match != tt.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tt.query, tt.candidate, result.Match, tt.want)
			}
		})
	}
}

func TestMatchDTRangeKeysWithoutAValidReadingAreInvalid(t *testing.T) {
	candidate := dataset(str(tags.AcquisitionDateTime, core.VRDT, "20260101150000+0000"))
	for _, query := range []string{
		"-",
		"20260102-20260101",
		"20260101120000-0300-0300",
		"20260101120000-1300",
		"20260101-0300-20260102-0300-",
		"1000-1100-1100", // a valid range at either of two '-'
	} {
		t.Run(query, func(t *testing.T) {
			_, err := Match(dataset(str(tags.AcquisitionDateTime, core.VRDT, query)), candidate, Options{})
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("Match(%q) error = %v, want ErrInvalidQuery", query, err)
			}
		})
	}
}

// TestMatchLeapSecond checks that SS=60 (PS3.5 6.2) is the last second of its
// minute rather than midnight of the next day.
func TestMatchLeapSecond(t *testing.T) {
	tests := []struct {
		name      string
		tag       core.Tag
		vr        core.VR
		query     string
		candidate string
		want      bool
	}{
		{"TM range to leap second includes it", tags.StudyTime, core.VRTM, "235959-235960", "235960", true},
		{"TM range to leap second includes its fraction", tags.StudyTime, core.VRTM, "235959-235960", "235960.5", true},
		{"TM range to leap second includes second 59", tags.StudyTime, core.VRTM, "235959-235960", "235959.999999", true},
		{"TM range to leap second includes its minute", tags.StudyTime, core.VRTM, "235959-235960", "2359", true},
		{"TM range to leap second excludes midnight", tags.StudyTime, core.VRTM, "235959-235960", "000000", false},
		{"TM leap second excludes second 59", tags.StudyTime, core.VRTM, "235960", "235959", false},
		{"TM minute includes leap second", tags.StudyTime, core.VRTM, "2359", "235960", true},
		{"TM hour includes leap second", tags.StudyTime, core.VRTM, "23", "235960", true},
		{"TM midnight excludes leap second", tags.StudyTime, core.VRTM, "000000", "235960", false},
		{"TM open upper bound includes leap second", tags.StudyTime, core.VRTM, "-235960", "235959.999999", true},
		{"DT leap second is not next day", tags.AcquisitionDateTime, core.VRDT, "20170101000000", "20161231235960", false},
		{"DT day includes leap second", tags.AcquisitionDateTime, core.VRDT, "20161231", "20161231235960", true},
		{"DT range to leap second includes it", tags.AcquisitionDateTime, core.VRDT, "20161231235959-20161231235960", "20161231235960", true},
		{"DT range from next day excludes leap second", tags.AcquisitionDateTime, core.VRDT, "20170101-", "20161231235960", false},
		{"DT leap second with offset", tags.AcquisitionDateTime, core.VRDT, "20161231235960+0000", "20170101005960+0100", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Match(dataset(str(tt.tag, tt.vr, tt.query)), dataset(str(tt.tag, tt.vr, tt.candidate)), Options{})
			if err != nil {
				t.Fatalf("Match(%q, %q) error = %v", tt.query, tt.candidate, err)
			}
			if result.Match != tt.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tt.query, tt.candidate, result.Match, tt.want)
			}
		})
	}
	_, err := Match(dataset(str(tags.StudyTime, core.VRTM, "235960-235959")), dataset(str(tags.StudyTime, core.VRTM, "235959")), Options{})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("descending leap-second range error = %v, want ErrInvalidQuery", err)
	}
}

func TestParseTimezoneOffsetRejectsOutOfRangeOffsets(t *testing.T) {
	for _, value := range []string{"-1300", "-1201", "-1230", "-1400", "+1401", "+1459", "+1500", "-0000"} {
		if _, err := ParseTimezoneOffset(value); err == nil {
			t.Errorf("ParseTimezoneOffset(%q) accepted an offset outside -1200 to +1400", value)
		}
	}
	for _, value := range []string{"-1200", "+1400", "+1345", "-1159", "+0000", "-0930"} {
		if _, err := ParseTimezoneOffset(value); err != nil {
			t.Errorf("ParseTimezoneOffset(%q) error = %v", value, err)
		}
	}
	query := dataset(str(acqDTTag, core.VRDT, "20240101"), str(timezoneTag, core.VRSH, "-1300"))
	if _, err := Match(query, dataset(str(acqDTTag, core.VRDT, "20240101")), Options{TimezoneAdjustment: true}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("out-of-range query offset err = %v, want ErrInvalidQuery", err)
	}
}

// TestParseTimezoneOffsetAgreesWithDTOffsets checks every &ZZXX against the
// DT offset rule of dcmtime, so a Timezone Offset From UTC and a DT suffix are
// accepted alike and read as the same offset.
func TestParseTimezoneOffsetAgreesWithDTOffsets(t *testing.T) {
	for _, sign := range []string{"+", "-"} {
		for value := 0; value <= 9999; value++ {
			offset := fmt.Sprintf("%s%04d", sign, value)
			zone, zoneErr := ParseTimezoneOffset(offset)
			dt, dtErr := dcmtime.ParseDatetime("2024" + offset)
			if (zoneErr == nil) != (dtErr == nil) {
				t.Fatalf("%s: ParseTimezoneOffset error %v, ParseDatetime error %v", offset, zoneErr, dtErr)
			}
			if zoneErr != nil {
				continue
			}
			_, want := dt.Time.Zone()
			if _, got := time.Date(2024, 1, 1, 0, 0, 0, 0, zone).Zone(); got != want {
				t.Fatalf("%s: offset %d, DT offset %d", offset, got, want)
			}
		}
	}
}
