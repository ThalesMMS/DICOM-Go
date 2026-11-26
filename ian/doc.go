// Package ian implements the Instance Availability Notification SOP Class
// (PS3.4 Annex R, 2026d) over the library's normalized DIMSE services: a
// builder and validator for the N-CREATE Attribute List (the Instance
// Availability Notification Module, PS3.3 C.4.23, with the requirements of
// PS3.4 Table R.3-1), an SCU client and an opt-in SCP handler.
//
// A notification announces, for one study, the SOP Instances of one or more
// series together with their availability (ONLINE, NEARLINE, OFFLINE or
// UNAVAILABLE) and where to retrieve them. N-CREATE is the only operation of
// the SOP Class; the SCP keeps no managed instance after answering.
//
// The validator checks the attributes the standard requires and leaves
// optional ones as opaque attributes: the Referenced Performed Procedure Step
// Sequence (Type 2) with SOP Class and Instance UIDs in each item; the Study
// Instance UID; at least one Referenced Series Sequence item with a Series
// Instance UID and at least one Referenced SOP Sequence item carrying the SOP
// Class and Instance UIDs and an enumerated Instance Availability. A Retrieve
// AE Title with a value is required unless the instance is UNAVAILABLE or the
// item names another retrieve location (Retrieve Location UID, Retrieve URL or
// Retrieve URI). A failure maps to the N-CREATE statuses of PS3.7 10.1.5:
// 0120H for a missing attribute, 0121H for a missing value and 0106H for an
// invalid value, with the offending tags (never values) in the Attribute
// Identifier List.
//
// What a received notification triggers (a retrieve, an index update) belongs
// to the application: the Service hands each validated notification to a
// Handler and answers with the status the Handler returns.
package ian

import "github.com/ThalesMMS/dicom-go/core"

// SOPClassUID is the Instance Availability Notification SOP Class.
const SOPClassUID = "1.2.840.10008.5.1.4.33"

// ModalityPerformedProcedureStepSOPClassUID is the usual SOP Class of a
// Referenced Performed Procedure Step Sequence item.
const ModalityPerformedProcedureStepSOPClassUID = "1.2.840.10008.3.1.2.3.3"

// Instance Availability (0008,0056) enumerated values (PS3.3 C.4.23.1.1).
const (
	AvailabilityOnline      = "ONLINE"
	AvailabilityNearline    = "NEARLINE"
	AvailabilityOffline     = "OFFLINE"
	AvailabilityUnavailable = "UNAVAILABLE"
)

// Availabilities lists the enumerated Instance Availability values.
func Availabilities() []string {
	return []string{AvailabilityOnline, AvailabilityNearline, AvailabilityOffline, AvailabilityUnavailable}
}

// ValidAvailability reports whether value is an enumerated Instance
// Availability.
func ValidAvailability(value string) bool {
	switch value {
	case AvailabilityOnline, AvailabilityNearline, AvailabilityOffline, AvailabilityUnavailable:
		return true
	}
	return false
}

// DIMSE statuses of N-CREATE (PS3.7 10.1.5.1.6 and Annex C).
const (
	DIMSEStatusSuccess               uint16 = 0x0000
	DIMSEStatusInvalidAttributeValue uint16 = 0x0106
	DIMSEStatusProcessingFailure     uint16 = 0x0110
	DIMSEStatusDuplicateSOPInstance  uint16 = 0x0111
	DIMSEStatusInvalidObjectInstance uint16 = 0x0117
	DIMSEStatusMissingAttribute      uint16 = 0x0120
	DIMSEStatusMissingAttributeValue uint16 = 0x0121
	DIMSEStatusResourceLimitation    uint16 = 0x0213
)

// Attribute tags of the Instance Availability Notification Module.
var (
	TagSpecificCharacterSet                     = core.NewTag(0x0008, 0x0005)
	TagRetrieveAETitle                          = core.NewTag(0x0008, 0x0054)
	TagInstanceAvailability                     = core.NewTag(0x0008, 0x0056)
	TagCodeValue                                = core.NewTag(0x0008, 0x0100)
	TagCodingSchemeDesignator                   = core.NewTag(0x0008, 0x0102)
	TagCodeMeaning                              = core.NewTag(0x0008, 0x0104)
	TagReferencedPerformedProcedureStepSequence = core.NewTag(0x0008, 0x1111)
	TagReferencedSeriesSequence                 = core.NewTag(0x0008, 0x1115)
	TagReferencedSOPClassUID                    = core.NewTag(0x0008, 0x1150)
	TagReferencedSOPInstanceUID                 = core.NewTag(0x0008, 0x1155)
	TagRetrieveURL                              = core.NewTag(0x0008, 0x1190)
	TagReferencedSOPSequence                    = core.NewTag(0x0008, 0x1199)
	TagStudyInstanceUID                         = core.NewTag(0x0020, 0x000D)
	TagSeriesInstanceUID                        = core.NewTag(0x0020, 0x000E)
	TagPerformedWorkitemCodeSequence            = core.NewTag(0x0040, 0x4019)
	TagRetrieveURI                              = core.NewTag(0x0040, 0xE010)
	TagRetrieveLocationUID                      = core.NewTag(0x0040, 0xE011)
	TagStorageMediaFileSetID                    = core.NewTag(0x0088, 0x0130)
	TagStorageMediaFileSetUID                   = core.NewTag(0x0088, 0x0140)
)
