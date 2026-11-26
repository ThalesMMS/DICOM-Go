package dcmtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dcmtime"
)

func TestParseDateRejectsOutOfRangeComponents(t *testing.T) {
	for _, value := range []string{
		"20210001", "20211301", "20210100", "20210132", "20210431",
		"20230229", "2021.00.01", "2021.04.31", "20211385",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := dcmtime.ParseDate(value); !errors.Is(err, dcmtime.ErrParseDA) {
				t.Fatalf("ParseDate(%q) error = %v, want ErrParseDA", value, err)
			}
		})
	}
	for _, value := range []string{"2024", "202402", "20240229", "2024.02.29"} {
		if _, err := dcmtime.ParseDate(value); err != nil {
			t.Fatalf("ParseDate(%q) error = %v", value, err)
		}
	}
}

func TestParseTimeRejectsOutOfRangeComponents(t *testing.T) {
	for _, value := range []string{
		"24", "2400", "1260", "125961", "256990", "1200.1",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := dcmtime.ParseTime(value); !errors.Is(err, dcmtime.ErrParseTM) {
				t.Fatalf("ParseTime(%q) error = %v, want ErrParseTM", value, err)
			}
		})
	}
	for _, value := range []string{"00", "2359", "235959.999999", "235960"} {
		if _, err := dcmtime.ParseTime(value); err != nil {
			t.Fatalf("ParseTime(%q) error = %v", value, err)
		}
	}
}

func TestParseDatetimeRejectsOutOfRangeComponentsAndOffsets(t *testing.T) {
	for _, value := range []string{
		"20211301", "20210431", "2024010124", "202401011260", "20240101125961",
		"202401011200.1", "20240101120000+1401", "20240101120000+1460",
		"20240101120000-1201", "20240101120000-1300", "20240101120000-0000",
		"20211301-9959",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := dcmtime.ParseDatetime(value); !errors.Is(err, dcmtime.ErrParseDT) {
				t.Fatalf("ParseDatetime(%q) error = %v, want ErrParseDT", value, err)
			}
		})
	}
	for _, value := range []string{
		"20240229", "20240101235960", "20240101120000+1400",
		"20240101120000-1200", "20240101120000+1359", "20240101120000-1159", "2007-0500",
	} {
		if _, err := dcmtime.ParseDatetime(value); err != nil {
			t.Fatalf("ParseDatetime(%q) error = %v", value, err)
		}
	}
}

// TestParseLeapSecondStaysInItsMinute checks that SS=60 (PS3.5 6.2) neither
// rolls over into the next minute or day nor loses its text.
func TestParseLeapSecondStaysInItsMinute(t *testing.T) {
	tm, err := dcmtime.ParseTime("235960")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(1, 1, 1, 23, 59, 59, 999999999, time.UTC); !tm.Time.Equal(want) || !tm.LeapSecond {
		t.Fatalf("ParseTime(235960) = %v leap %v, want %v leap true", tm.Time, tm.LeapSecond, want)
	}
	if tm.Precision != dcmtime.PrecisionSeconds || tm.DCM() != "235960" || tm.String() != "23:59:60" {
		t.Fatalf("ParseTime(235960) precision %v, DCM %q, String %q", tm.Precision, tm.DCM(), tm.String())
	}
	fraction, err := dcmtime.ParseTime("235960.25")
	if err != nil {
		t.Fatal(err)
	}
	if fraction.DCM() != "235960.25" || fraction.String() != "23:59:60.25" {
		t.Fatalf("ParseTime(235960.25) DCM %q, String %q", fraction.DCM(), fraction.String())
	}
	last, err := dcmtime.ParseTime("235959.999999")
	if err != nil {
		t.Fatal(err)
	}
	if !last.Time.Before(tm.Time) || last.LeapSecond {
		t.Fatalf("235959.999999 (%v) does not sort before 235960 (%v)", last.Time, tm.Time)
	}
	if clock, err := dcmtime.ParseTM("235960"); err != nil || clock.Hour() != 23 || clock.Second() != 59 {
		t.Fatalf("ParseTM(235960) = %v, %v", clock, err)
	}

	dt, err := dcmtime.ParseDatetime("20161231235960")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2016, 12, 31, 23, 59, 59, 999999999, time.UTC); !dt.Time.Equal(want) || !dt.LeapSecond {
		t.Fatalf("ParseDatetime(20161231235960) = %v leap %v, want %v leap true", dt.Time, dt.LeapSecond, want)
	}
	if dt.DCM() != "20161231235960" || dt.String() != "2016-12-31 23:59:60" {
		t.Fatalf("ParseDatetime(20161231235960) DCM %q, String %q", dt.DCM(), dt.String())
	}
	next, err := dcmtime.ParseDatetime("20170101000000")
	if err != nil {
		t.Fatal(err)
	}
	if !dt.Time.Before(next.Time) {
		t.Fatalf("20161231235960 (%v) does not sort before 20170101000000 (%v)", dt.Time, next.Time)
	}
	offset, err := dcmtime.ParseDatetime("20170101085960.5+0900")
	if err != nil {
		t.Fatal(err)
	}
	if offset.DCM() != "20170101085960.5+0900" || !offset.Time.Equal(dt.Time) {
		t.Fatalf("ParseDatetime(20170101085960.5+0900) DCM %q, instant %v", offset.DCM(), offset.Time)
	}
	if plain, err := dcmtime.ParseTime("235959"); err != nil || plain.LeapSecond || plain.DCM() != "235959" {
		t.Fatalf("ParseTime(235959) = %+v, %v", plain, err)
	}
}
