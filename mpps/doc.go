// Package mpps implements the Modality Performed Procedure Step SOP Class
// (PS3.4 F.7, 2026d) over the library's normalized DIMSE services: dataset
// builders, the normative lifecycle IN PROGRESS -> COMPLETED | DISCONTINUED,
// an opt-in SCP handler with an injectable repository, and an SCU client.
//
// The supported subset is the N-CREATE and N-SET usage of Table F.7.2-1 for
// the Performed Procedure Step Relationship, Information and Image
// Acquisition Results modules, with the Performed Series Sequence and its
// image and non-image references. The Billing and Material Management Code
// module, Gender Identity and similar optional sequences are accepted on
// N-CREATE as opaque attributes and refused on N-SET. The retired MPPS
// Retrieve and Notification SOP Classes are not implemented.
//
// Scheduling, RIS/worklist coupling, authorization and persistence belong to
// the application: the Service stores through a Repository it is given, and
// never derives clinical data that the modality did not send.
package mpps

import "github.com/ThalesMMS/dicom-go/core"

// SOPClassUID is the Modality Performed Procedure Step SOP Class.
const SOPClassUID = "1.2.840.10008.3.1.2.3.3"

// Status values of Performed Procedure Step Status (0040,0252).
const (
	StatusInProgress   = "IN PROGRESS"
	StatusCompleted    = "COMPLETED"
	StatusDiscontinued = "DISCONTINUED"
)

// DIMSE statuses the SCP returns (PS3.7 Annex C, 10.1.5 and 10.1.1).
const (
	DIMSEStatusSuccess               uint16 = 0x0000
	DIMSEStatusInvalidAttributeValue uint16 = 0x0106
	DIMSEStatusProcessingFailure     uint16 = 0x0110
	DIMSEStatusDuplicateSOPInstance  uint16 = 0x0111
	DIMSEStatusNoSuchSOPInstance     uint16 = 0x0112
	DIMSEStatusMissingAttribute      uint16 = 0x0120
	DIMSEStatusMissingAttributeValue uint16 = 0x0121
	DIMSEStatusResourceLimitation    uint16 = 0x0213
)

// ErrorIDNoLongerUpdatable is the Error ID (0000,0903) of Table F.7.2-2 for an
// N-SET on a step that is no longer IN PROGRESS.
const ErrorIDNoLongerUpdatable uint16 = 0xA710

// ErrorCommentNoLongerUpdatable is the matching Error Comment (0000,0902).
const ErrorCommentNoLongerUpdatable = "Performed Procedure Step Object may no longer be updated"

// Attribute tags used by the subset.
var (
	TagSpecificCharacterSet              = core.NewTag(0x0008, 0x0005)
	TagAccessionNumber                   = core.NewTag(0x0008, 0x0050)
	TagRetrieveAETitle                   = core.NewTag(0x0008, 0x0054)
	TagModality                          = core.NewTag(0x0008, 0x0060)
	TagCodeValue                         = core.NewTag(0x0008, 0x0100)
	TagCodingSchemeDesignator            = core.NewTag(0x0008, 0x0102)
	TagCodingSchemeVersion               = core.NewTag(0x0008, 0x0103)
	TagCodeMeaning                       = core.NewTag(0x0008, 0x0104)
	TagLongCodeValue                     = core.NewTag(0x0008, 0x0119)
	TagURNCodeValue                      = core.NewTag(0x0008, 0x0120)
	TagProcedureCodeSequence             = core.NewTag(0x0008, 0x1032)
	TagSeriesDescription                 = core.NewTag(0x0008, 0x103E)
	TagPerformingPhysicianName           = core.NewTag(0x0008, 0x1050)
	TagOperatorsName                     = core.NewTag(0x0008, 0x1070)
	TagReferencedStudySequence           = core.NewTag(0x0008, 0x1110)
	TagReferencedPatientSequence         = core.NewTag(0x0008, 0x1120)
	TagReferencedImageSequence           = core.NewTag(0x0008, 0x1140)
	TagReferencedSOPClassUID             = core.NewTag(0x0008, 0x1150)
	TagReferencedSOPInstanceUID          = core.NewTag(0x0008, 0x1155)
	TagPatientName                       = core.NewTag(0x0010, 0x0010)
	TagPatientID                         = core.NewTag(0x0010, 0x0020)
	TagPatientBirthDate                  = core.NewTag(0x0010, 0x0030)
	TagPatientSex                        = core.NewTag(0x0010, 0x0040)
	TagProtocolName                      = core.NewTag(0x0018, 0x1030)
	TagStudyInstanceUID                  = core.NewTag(0x0020, 0x000D)
	TagSeriesInstanceUID                 = core.NewTag(0x0020, 0x000E)
	TagStudyID                           = core.NewTag(0x0020, 0x0010)
	TagRequestedProcedureDescription     = core.NewTag(0x0032, 0x1060)
	TagScheduledProcedureStepDescription = core.NewTag(0x0040, 0x0007)
	TagScheduledProtocolCodeSequence     = core.NewTag(0x0040, 0x0008)
	TagScheduledProcedureStepID          = core.NewTag(0x0040, 0x0009)
	TagReferencedNonImageSequence        = core.NewTag(0x0040, 0x0220)
	TagPerformedStationAETitle           = core.NewTag(0x0040, 0x0241)
	TagPerformedStationName              = core.NewTag(0x0040, 0x0242)
	TagPerformedLocation                 = core.NewTag(0x0040, 0x0243)
	TagPerformedProcedureStepStartDate   = core.NewTag(0x0040, 0x0244)
	TagPerformedProcedureStepStartTime   = core.NewTag(0x0040, 0x0245)
	TagPerformedProcedureStepEndDate     = core.NewTag(0x0040, 0x0250)
	TagPerformedProcedureStepEndTime     = core.NewTag(0x0040, 0x0251)
	TagPerformedProcedureStepStatus      = core.NewTag(0x0040, 0x0252)
	TagPerformedProcedureStepID          = core.NewTag(0x0040, 0x0253)
	TagPerformedProcedureStepDescription = core.NewTag(0x0040, 0x0254)
	TagPerformedProcedureTypeDescription = core.NewTag(0x0040, 0x0255)
	TagPerformedProtocolCodeSequence     = core.NewTag(0x0040, 0x0260)
	TagScheduledStepAttributesSequence   = core.NewTag(0x0040, 0x0270)
	TagCommentsOnPerformedProcedureStep  = core.NewTag(0x0040, 0x0280)
	TagDiscontinuationReasonCodeSequence = core.NewTag(0x0040, 0x0281)
	TagPerformedSeriesSequence           = core.NewTag(0x0040, 0x0340)
	TagRequestedProcedureID              = core.NewTag(0x0040, 0x1001)
	TagReasonForPerformedProcedureCodes  = core.NewTag(0x0040, 0x1012)
)
