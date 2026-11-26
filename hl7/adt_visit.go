package hl7

import (
	"errors"
	"strings"
)

// PatientIdentifierChange is A47: Prior identifiers are corrected to Current.
// It does not imply a merge or a demographic update. Database conflicts are
// resolved by the consuming application.
type PatientIdentifierChange struct {
	Current []PatientIdentifier // PID-3, all populated repetitions
	Prior   []PatientIdentifier // MRG-1, all populated repetitions
}

// PatientLocation is the first PL value in PV1-3. The facility is an identifier,
// not the name of the imaging institution.
type PatientLocation struct {
	PointOfCare string
	Room        string
	Bed         string
	Facility    HierarchicDesignator
}

// Visit is the encounter subset of PV1. Local table values are kept verbatim.
// Absent/empty fields mean no update; the null flags mean clear.
type Visit struct {
	PatientClass                       string            // PV1-2
	Location                           PatientLocation   // PV1-3, first PL
	AdmissionType                      string            // PV1-4
	HospitalService                    string            // PV1-10
	Number                             PatientIdentifier // PV1-19, first CX
	ReferringPhysician                 string            // PV1-8, first XCN, in DICOM PN order
	ReferringNameHasReservedCharacters bool
	AdmittedDateTime                   string // PV1-44, TS.1, precision and offset intact
	DischargedDateTime                 string // PV1-45, TS.1, not mapped to retired DICOM tags
	LocationIsNull                     bool
	NumberIsNull                       bool
	ReferringPhysicianIsNull           bool
	AdmissionTypeIsNull                bool
	AdmittedDateTimeIsNull             bool
	DischargedDateTimeIsNull           bool
}

func visitFrom(pv1 *Segment) *Visit {
	location := pv1.Field(3).First()
	name, reserved := dicomPersonName(pv1.Field(8).First(), true)
	return &Visit{
		PatientClass:  pv1.Field(2).String(),
		Location:      PatientLocation{PointOfCare: location.String(1), Room: location.String(2), Bed: location.String(3), Facility: hdFromComponent(location.Component(4))},
		AdmissionType: pv1.Field(4).String(), HospitalService: pv1.Field(10).String(), Number: patientIdentifierFrom(pv1.Field(19).First()),
		ReferringPhysician: strings.Join(name, "^"), ReferringNameHasReservedCharacters: reserved,
		AdmittedDateTime: pv1.Field(44).String(), DischargedDateTime: pv1.Field(45).String(),
		LocationIsNull: pv1.Field(3).IsNull(), NumberIsNull: pv1.Field(19).IsNull(),
		ReferringPhysicianIsNull: pv1.Field(8).IsNull(), AdmissionTypeIsNull: pv1.Field(4).IsNull(),
		AdmittedDateTimeIsNull: pv1.Field(44).IsNull(), DischargedDateTimeIsNull: pv1.Field(45).IsNull(),
	}
}

func dicomPersonName(name Repetition, xcn bool) ([]string, bool) {
	var parts []string
	if xcn { // XCN: ID, family, given, middle, suffix, prefix.
		parts = []string{name.Component(2).Sub(1), name.String(3), name.String(4), name.String(6), name.String(5)}
	} else { // XPN: family, given, middle, suffix, prefix.
		parts = []string{name.Component(1).Sub(1), name.String(2), name.String(3), name.String(5), name.String(4)}
	}
	reserved := false
	for _, part := range parts {
		reserved = reserved || strings.ContainsAny(part, "^=\\")
	}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, reserved
}

func adtError(segment string, field int, code, reason string) *Error {
	return &Error{Code: code, Location: ErrorLocation{Segment: segment, Sequence: 1, Field: field}, Err: errors.New(reason)}
}
