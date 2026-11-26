// Package pet extracts classic DICOM PET metadata and converts modality values to SUVbw.
package pet

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	dicomtags "github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	petUnitsBQML            = "BQML"
	petUnitsGML             = "GML"
	petSUVTypeBW            = "BW"
	petDecayCorrectionAdmin = "ADMIN"
	petDecayCorrectionStart = "START"
	petCorrectedImageDecay  = "DECY"
)

// Metadata is the subset of PET Series and Isotope Module metadata needed
// for a body-weight-normalized standardized uptake value (SUVbw). The fields
// deliberately retain validity flags for temporal values: a zero time must not
// silently turn incomplete DICOM metadata into a quantitative clinical result.
type Metadata struct {
	Units                string
	SUVType              string
	DecayCorrection      string
	CorrectedImage       []string
	PatientWeightKg      float64
	TotalDoseBq          float64
	HalfLifeSeconds      float64
	AdministrationTime   time.Time
	AdministrationTimeOK bool
	AcquisitionTime      time.Time
	AcquisitionTimeOK    bool
}

// SUVbw converts a finite, nonnegative PET value after modality rescale.
// GML with SUV Type BW or an absent SUV Type is already SUVbw. BQML requires
// decay correction, body weight, and injected activity at the ADMIN or START
// reference time. The bool reports whether the conversion is available.
func (m *Metadata) SUVbw(rescaledValue float64) (float64, bool) {
	if m == nil || !finiteNonNegative(rescaledValue) {
		return 0, false
	}
	units := normalizePETTerm(m.Units)
	if units == petUnitsGML {
		suvType := normalizePETTerm(m.SUVType)
		if suvType != "" && suvType != petSUVTypeBW {
			return 0, false
		}
		return rescaledValue, true
	}
	if units != petUnitsBQML || !finitePositive(m.PatientWeightKg) || !finitePositive(m.TotalDoseBq) || !containsPETTerm(m.CorrectedImage, petCorrectedImageDecay) {
		return 0, false
	}

	doseAtReference := m.TotalDoseBq
	switch normalizePETTerm(m.DecayCorrection) {
	case petDecayCorrectionAdmin:
		// Pixel activity and injected dose are both referenced to the time of
		// radiopharmaceutical administration; no additional decay is applied.
	case petDecayCorrectionStart:
		if !finitePositive(m.HalfLifeSeconds) || !m.AdministrationTimeOK || !m.AcquisitionTimeOK {
			return 0, false
		}
		elapsed := m.AcquisitionTime.Sub(m.AdministrationTime).Seconds()
		if elapsed < 0 || math.IsNaN(elapsed) || math.IsInf(elapsed, 0) {
			return 0, false
		}
		doseAtReference *= math.Exp(-math.Ln2 * elapsed / m.HalfLifeSeconds)
	default:
		return 0, false
	}
	if !finitePositive(doseAtReference) {
		return 0, false
	}
	suv := rescaledValue * (m.PatientWeightKg * 1000) / doseAtReference
	if !finiteNonNegative(suv) {
		return 0, false
	}
	return suv, true
}

// ParseClassicMetadata reads top-level calibration from a classic PET dataset.
// It returns nil for a nil dataset or a modality other than PT or PET. Missing required
// attributes leave SUVbw unavailable. A single radiopharmaceutical item is
// required to obtain dose and half-life. The result describes the input instance;
// callers applying it to other instances must establish series-level applicability.
// Enhanced PET functional groups and per-frame calibration are outside this scope.
func ParseClassicMetadata(dataset *object.Object) *Metadata {
	if dataset == nil {
		return nil
	}
	modality := datasetTerm(dataset, dicomtags.Modality)
	if modality != "PT" && modality != "PET" {
		return nil
	}
	meta := &Metadata{
		Units:           datasetTerm(dataset, dicomtags.Units),
		SUVType:         datasetTerm(dataset, dicomtags.SUVType),
		DecayCorrection: datasetTerm(dataset, dicomtags.DecayCorrection),
	}
	if values, ok := dataset.GetStrings(dicomtags.CorrectedImage); ok {
		meta.CorrectedImage = make([]string, 0, len(values))
		for _, value := range values {
			if normalized := normalizePETTerm(value); normalized != "" {
				meta.CorrectedImage = append(meta.CorrectedImage, normalized)
			}
		}
	}
	meta.PatientWeightKg = singleFiniteFloat(dataset, dicomtags.PatientWeight)

	loc, timezoneKnown := dicomTimezone(dataset)
	var acquisitionOffsetKnown bool
	meta.AcquisitionTime, meta.AcquisitionTimeOK, acquisitionOffsetKnown = dicomAcquisitionDateTime(dataset, loc, timezoneKnown)
	items, ok := dataset.GetSequence(dicomtags.RadiopharmaceuticalInformationSequence)
	if !ok || len(items) != 1 || items[0] == nil {
		return meta
	}
	isotope := items[0]
	meta.TotalDoseBq = singleFiniteFloat(isotope, dicomtags.RadionuclideTotalDose)
	meta.HalfLifeSeconds = singleFiniteFloat(isotope, dicomtags.RadionuclideHalfLife)
	var administrationOffsetKnown bool
	if value, ok, offsetKnown := dicomDateTime(isotope, dicomtags.RadiopharmaceuticalStartDateTime, loc, timezoneKnown); ok {
		meta.AdministrationTime = value
		meta.AdministrationTimeOK = true
		administrationOffsetKnown = offsetKnown
	} else if meta.AcquisitionTimeOK {
		meta.AdministrationTime, meta.AdministrationTimeOK = dicomTimeOnDate(isotope, dicomtags.RadiopharmaceuticalStartTime, meta.AcquisitionTime, loc)
		administrationOffsetKnown = timezoneKnown
	}
	// A DT with an explicit offset cannot safely be compared to a time whose
	// offset is unknown. Two unknown offsets remain comparable because both
	// values share the same DICOM dataset time base.
	if meta.AdministrationTimeOK && meta.AcquisitionTimeOK && administrationOffsetKnown != acquisitionOffsetKnown {
		meta.AdministrationTimeOK = false
	}
	return meta
}

func dicomAcquisitionDateTime(obj *object.Object, loc *time.Location, timezoneKnown bool) (time.Time, bool, bool) {
	if value, ok, offsetKnown := dicomDateTime(obj, dicomtags.AcquisitionDateTime, loc, timezoneKnown); ok {
		return value, true, offsetKnown
	}
	value, ok := dicomDateAndTime(obj, dicomtags.AcquisitionDate, dicomtags.AcquisitionTime, loc)
	return value, ok, timezoneKnown
}

func dicomDateTime(obj *object.Object, tag core.Tag, loc *time.Location, timezoneKnown bool) (time.Time, bool, bool) {
	if obj == nil {
		return time.Time{}, false, false
	}
	value, err := obj.LookupDateTime(tag)
	if err != nil || value.Precision > dcmtime.PrecisionMinutes {
		return time.Time{}, false, false
	}
	parsed := value.Time
	if value.NoOffset {
		parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), loc)
	}
	return parsed, true, !value.NoOffset || timezoneKnown
}

func dicomDateAndTime(obj *object.Object, dateTag, timeTag core.Tag, loc *time.Location) (time.Time, bool) {
	if obj == nil {
		return time.Time{}, false
	}
	date, err := obj.LookupDate(dateTag)
	if err != nil || date.Precision > dcmtime.PrecisionDay {
		return time.Time{}, false
	}
	clock, err := obj.LookupTime(timeTag)
	if err != nil || clock.Precision > dcmtime.PrecisionMinutes {
		return time.Time{}, false
	}
	return time.Date(date.Time.Year(), date.Time.Month(), date.Time.Day(), clock.Time.Hour(), clock.Time.Minute(), clock.Time.Second(), clock.Time.Nanosecond(), loc), true
}

func dicomTimeOnDate(obj *object.Object, timeTag core.Tag, date time.Time, loc *time.Location) (time.Time, bool) {
	if obj == nil || date.IsZero() {
		return time.Time{}, false
	}
	clock, err := obj.LookupTime(timeTag)
	if err != nil || clock.Precision > dcmtime.PrecisionMinutes {
		return time.Time{}, false
	}
	return time.Date(date.Year(), date.Month(), date.Day(), clock.Time.Hour(), clock.Time.Minute(), clock.Time.Second(), clock.Time.Nanosecond(), loc), true
}

func dicomTimezone(obj *object.Object) (*time.Location, bool) {
	if obj == nil {
		return time.FixedZone("", 0), false
	}
	raw, ok := obj.GetString(dicomtags.TimezoneOffsetFromUTC)
	if !ok {
		return time.FixedZone("", 0), false
	}
	raw = strings.TrimSpace(raw)
	if len(raw) != 5 || (raw[0] != '+' && raw[0] != '-') {
		return time.FixedZone("", 0), false
	}
	hours, errH := strconv.Atoi(raw[1:3])
	minutes, errM := strconv.Atoi(raw[3:5])
	if errH != nil || errM != nil || hours > 14 || minutes > 59 || (hours == 14 && minutes != 0) {
		return time.FixedZone("", 0), false
	}
	offset := hours*3600 + minutes*60
	if raw[0] == '-' {
		offset = -offset
	}
	return time.FixedZone(raw, offset), true
}

func singleFiniteFloat(obj *object.Object, tag core.Tag) float64 {
	values, err := obj.GetFloats(tag)
	if err != nil || len(values) != 1 || math.IsNaN(values[0]) || math.IsInf(values[0], 0) {
		return 0
	}
	return values[0]
}

func datasetTerm(obj *object.Object, tag core.Tag) string {
	value, _ := obj.GetString(tag)
	return normalizePETTerm(strings.TrimRight(value, "\x00"))
}

func normalizePETTerm(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func containsPETTerm(values []string, want string) bool {
	want = normalizePETTerm(want)
	for _, value := range values {
		if normalizePETTerm(value) == want {
			return true
		}
	}
	return false
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
