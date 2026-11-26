package mpps

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrInvalidDataSet reports an Attribute or Modification List outside the
	// supported subset or its value rules.
	ErrInvalidDataSet = errors.New("mpps: invalid attribute list")
	// ErrNoSuchInstance reports an unknown Performed Procedure Step.
	ErrNoSuchInstance = errors.New("mpps: no such performed procedure step")
	// ErrDuplicate reports an N-CREATE for an existing SOP Instance UID.
	ErrDuplicate = errors.New("mpps: performed procedure step already exists")
	// ErrNotUpdatable reports an N-SET on a COMPLETED or DISCONTINUED step.
	ErrNotUpdatable = errors.New("mpps: performed procedure step may no longer be updated")
	// ErrFinalState reports a transition to COMPLETED or DISCONTINUED without
	// the Final State attributes of Table F.7.2-1.
	ErrFinalState = errors.New("mpps: final state attributes missing")
	// ErrConcurrentUpdate reports a lost compare-and-swap in a Repository.
	ErrConcurrentUpdate = errors.New("mpps: concurrent update")
	// ErrResourceLimit reports a configured bound.
	ErrResourceLimit = errors.New("mpps: resource limit")
)

// StatusError carries the DIMSE status an SCP returns for an error, with the
// attributes involved (tags only, never values).
type StatusError struct {
	Status       uint16
	Tags         []core.Tag
	ErrorComment string
	ErrorID      uint16 // 0 when absent
	Err          error
}

func (e *StatusError) Error() string {
	var tags []string
	for _, tag := range e.Tags {
		tags = append(tags, tag.String())
	}
	return fmt.Sprintf("%v (status %04X %s)", e.Err, e.Status, strings.Join(tags, " "))
}

func (e *StatusError) Unwrap() error { return e.Err }

// StatusOf maps an error of this package to its DIMSE status.
func StatusOf(err error) uint16 {
	var statusErr *StatusError
	switch {
	case err == nil:
		return DIMSEStatusSuccess
	case errors.As(err, &statusErr):
		return statusErr.Status
	case errors.Is(err, ErrNoSuchInstance):
		return DIMSEStatusNoSuchSOPInstance
	case errors.Is(err, ErrDuplicate):
		return DIMSEStatusDuplicateSOPInstance
	case errors.Is(err, ErrResourceLimit):
		return DIMSEStatusResourceLimitation
	case errors.Is(err, ErrInvalidDataSet):
		return DIMSEStatusInvalidAttributeValue
	default:
		return DIMSEStatusProcessingFailure
	}
}

// checker accumulates attribute problems by class; the first non-empty class
// in the order missing, missing value, invalid value becomes the status.
type checker struct {
	missing, empty, invalid []core.Tag
}

func (c *checker) err() error {
	switch {
	case len(c.missing) > 0:
		return &StatusError{Status: DIMSEStatusMissingAttribute, Tags: c.missing, Err: ErrInvalidDataSet}
	case len(c.empty) > 0:
		return &StatusError{Status: DIMSEStatusMissingAttributeValue, Tags: c.empty, Err: ErrInvalidDataSet}
	case len(c.invalid) > 0:
		return &StatusError{Status: DIMSEStatusInvalidAttributeValue, Tags: c.invalid, Err: ErrInvalidDataSet}
	}
	return nil
}

var (
	datePattern = regexp.MustCompile(`^[0-9]{8}$`)
	timePattern = regexp.MustCompile(`^[0-9]{2}([0-9]{2}([0-9]{2}(\.[0-9]{1,6})?)?)?$`)
	codePattern = regexp.MustCompile(`^[A-Z0-9_ ]{1,16}$`)
)

// value returns the trimmed text of tag and whether the element is present.
// Undecodable text is reported as invalid.
func (c *checker) value(ds *object.Object, tag core.Tag) (string, bool) {
	element, ok := ds.Get(tag)
	if !ok {
		return "", false
	}
	if _, isSequence := element.Value.(core.SequenceValue); isSequence {
		c.invalid = append(c.invalid, tag)
		return "", true
	}
	values, err := ds.LookupStrings(tag)
	if err != nil {
		c.invalid = append(c.invalid, tag)
		return "", true
	}
	return trimmed(strings.Join(values, `\`)), true
}

func (c *checker) type1(ds *object.Object, tag core.Tag) string {
	value, ok := c.value(ds, tag)
	switch {
	case !ok:
		c.missing = append(c.missing, tag)
	case value == "":
		c.empty = append(c.empty, tag)
	}
	return value
}

func (c *checker) type2(ds *object.Object, tag core.Tag) string {
	value, ok := c.value(ds, tag)
	if !ok {
		c.missing = append(c.missing, tag)
	}
	return value
}

func (c *checker) uid(ds *object.Object, tag core.Tag, required bool) {
	var value string
	if required {
		value = c.type1(ds, tag)
	} else {
		value = c.type2(ds, tag)
	}
	if value != "" && !core.IsValidUID(value) {
		c.invalid = append(c.invalid, tag)
	}
}

func (c *checker) date(ds *object.Object, tag core.Tag, required bool) {
	var value string
	if required {
		value = c.type1(ds, tag)
	} else {
		value = c.type2(ds, tag)
	}
	if value == "" {
		return
	}
	if _, err := time.Parse("20060102", value); err != nil || !datePattern.MatchString(value) {
		c.invalid = append(c.invalid, tag)
	}
}

func (c *checker) clock(ds *object.Object, tag core.Tag, required bool) {
	var value string
	if required {
		value = c.type1(ds, tag)
	} else {
		value = c.type2(ds, tag)
	}
	if value != "" && !timePattern.MatchString(value) {
		c.invalid = append(c.invalid, tag)
	}
}

// items returns the items of a sequence; ok is false when absent. A present
// non-sequence value is invalid.
func (c *checker) items(ds *object.Object, tag core.Tag, required bool) []*object.Object {
	element, ok := ds.Get(tag)
	if !ok {
		c.missing = append(c.missing, tag)
		return nil
	}
	if _, isSequence := element.Value.(core.SequenceValue); !isSequence {
		c.invalid = append(c.invalid, tag)
		return nil
	}
	items, _ := ds.GetSequence(tag)
	if required && len(items) == 0 {
		c.empty = append(c.empty, tag)
	}
	return items
}

func (c *checker) references(ds *object.Object, tag core.Tag) {
	for _, item := range c.items(ds, tag, false) {
		c.uid(item, TagReferencedSOPClassUID, true)
		c.uid(item, TagReferencedSOPInstanceUID, true)
	}
}

// codes checks the coded entry of each item (PS3.3 Table 8.8-1a): exactly
// one of Code Value, Long Code Value and URN Code Value, each with a value
// when present, and Coding Scheme Designator (Type 1C) whenever Code Value or
// Long Code Value is present. An item identified by URN Code Value alone
// needs no Coding Scheme Designator.
func (c *checker) codes(ds *object.Object, tag core.Tag) {
	element, ok := ds.Get(tag)
	if !ok {
		return
	}
	if _, isSequence := element.Value.(core.SequenceValue); !isSequence {
		c.invalid = append(c.invalid, tag)
		return
	}
	items, _ := ds.GetSequence(tag)
	for _, item := range items {
		var identifiers []core.Tag
		for _, identifier := range []core.Tag{TagCodeValue, TagLongCodeValue, TagURNCodeValue} {
			if _, present := item.Get(identifier); present {
				identifiers = append(identifiers, identifier)
				c.type1(item, identifier)
			}
		}
		switch {
		case len(identifiers) == 0:
			c.missing = append(c.missing, TagCodeValue)
		case len(identifiers) > 1:
			c.invalid = append(c.invalid, identifiers...)
		}
		if identifiers != nil && identifiers[0] != TagURNCodeValue {
			c.type1(item, TagCodingSchemeDesignator)
		}
		// Coding Scheme Version shall not be present without a Coding Scheme
		// Designator (PS3.4 Tables F.7.2-1a and F.7.2-1e).
		if _, version := item.Get(TagCodingSchemeVersion); version {
			if _, designator := item.Get(TagCodingSchemeDesignator); !designator {
				c.invalid = append(c.invalid, TagCodingSchemeVersion)
			}
		}
		// Code Meaning is Type 1 only in Scheduled Protocol Code Sequence
		// items (Table F.7.2-1a); the other MPPS code sequences use the
		// optional-meaning variant (Table F.7.2-1e), where it is Type 3.
		if tag == TagScheduledProtocolCodeSequence {
			c.type1(item, TagCodeMeaning)
		}
	}
}

func (c *checker) series(ds *object.Object, required bool) {
	for _, item := range c.items(ds, TagPerformedSeriesSequence, required) {
		c.type2(item, TagPerformingPhysicianName)
		c.type1(item, TagProtocolName)
		c.type2(item, TagOperatorsName)
		c.uid(item, TagSeriesInstanceUID, true)
		c.type2(item, TagSeriesDescription)
		if ae := c.type2(item, TagRetrieveAETitle); len(ae) > 16 {
			c.invalid = append(c.invalid, TagRetrieveAETitle)
		}
		c.references(item, TagReferencedImageSequence)
		c.references(item, TagReferencedNonImageSequence)
	}
}

// ValidateCreate checks an N-CREATE Attribute List against Table F.7.2-1:
// Type 1 attributes present with a value, Type 2 attributes present,
// Performed Procedure Step Status IN PROGRESS (otherwise 0106H, PS3.4
// F.7.2.1.3), and UID, date, time and code value syntax.
func ValidateCreate(ds *object.Object) error {
	if ds == nil {
		return &StatusError{Status: DIMSEStatusMissingAttribute, Err: ErrInvalidDataSet}
	}
	c := &checker{}
	for _, step := range c.items(ds, TagScheduledStepAttributesSequence, true) {
		c.uid(step, TagStudyInstanceUID, true)
		c.references(step, TagReferencedStudySequence)
		c.type2(step, TagAccessionNumber)
		c.type2(step, TagRequestedProcedureID)
		c.type2(step, TagRequestedProcedureDescription)
		c.type2(step, TagScheduledProcedureStepID)
		c.type2(step, TagScheduledProcedureStepDescription)
		if _, ok := step.Get(TagScheduledProtocolCodeSequence); !ok {
			c.missing = append(c.missing, TagScheduledProtocolCodeSequence)
		}
		c.codes(step, TagScheduledProtocolCodeSequence)
	}
	c.type2(ds, TagPatientName)
	c.type2(ds, TagPatientID)
	c.date(ds, TagPatientBirthDate, false)
	c.type2(ds, TagPatientSex)
	c.references(ds, TagReferencedPatientSequence)
	c.type1(ds, TagPerformedProcedureStepID)
	if ae := c.type1(ds, TagPerformedStationAETitle); len(ae) > 16 {
		c.invalid = append(c.invalid, TagPerformedStationAETitle)
	}
	c.type2(ds, TagPerformedStationName)
	c.type2(ds, TagPerformedLocation)
	c.date(ds, TagPerformedProcedureStepStartDate, true)
	c.clock(ds, TagPerformedProcedureStepStartTime, true)
	if status := c.type1(ds, TagPerformedProcedureStepStatus); status != "" && status != StatusInProgress {
		c.invalid = append(c.invalid, TagPerformedProcedureStepStatus)
	}
	c.type2(ds, TagPerformedProcedureStepDescription)
	c.type2(ds, TagPerformedProcedureTypeDescription)
	if _, ok := ds.Get(TagProcedureCodeSequence); !ok {
		c.missing = append(c.missing, TagProcedureCodeSequence)
	}
	c.codes(ds, TagProcedureCodeSequence)
	c.date(ds, TagPerformedProcedureStepEndDate, false)
	c.clock(ds, TagPerformedProcedureStepEndTime, false)
	if modality := c.type1(ds, TagModality); modality != "" && !codePattern.MatchString(modality) {
		c.invalid = append(c.invalid, TagModality)
	}
	c.type2(ds, TagStudyID)
	if _, ok := ds.Get(TagPerformedProtocolCodeSequence); !ok {
		c.missing = append(c.missing, TagPerformedProtocolCodeSequence)
	}
	c.codes(ds, TagPerformedProtocolCodeSequence)
	c.series(ds, false)
	return c.err()
}

// settableTags are the attributes Table F.7.2-1 allows in N-SET.
var settableTags = map[core.Tag]bool{
	TagSpecificCharacterSet:              true,
	TagPerformedProcedureStepStatus:      true,
	TagPerformedProcedureStepDescription: true,
	TagPerformedProcedureTypeDescription: true,
	TagProcedureCodeSequence:             true,
	TagReasonForPerformedProcedureCodes:  true,
	TagPerformedProcedureStepEndDate:     true,
	TagPerformedProcedureStepEndTime:     true,
	TagCommentsOnPerformedProcedureStep:  true,
	TagDiscontinuationReasonCodeSequence: true,
	TagPerformedProtocolCodeSequence:     true,
	TagPerformedSeriesSequence:           true,
}

// validateModificationShape checks what an N-SET Modification List can be
// checked for alone: only settable attributes, a known status value, and
// the syntax of dates, times, codes and series.
func validateModificationShape(ds *object.Object) error {
	if ds == nil || ds.Len() == 0 {
		return &StatusError{Status: DIMSEStatusInvalidAttributeValue, Err: ErrInvalidDataSet}
	}
	c := &checker{}
	for _, element := range ds.Elements() {
		if !settableTags[element.Tag()] {
			c.invalid = append(c.invalid, element.Tag())
		}
	}
	if len(c.invalid) > 0 {
		return c.err()
	}
	if _, ok := ds.Get(TagPerformedProcedureStepStatus); ok {
		switch status := c.type1(ds, TagPerformedProcedureStepStatus); status {
		case StatusInProgress, StatusCompleted, StatusDiscontinued, "":
		default:
			c.invalid = append(c.invalid, TagPerformedProcedureStepStatus)
		}
	}
	if _, ok := ds.Get(TagPerformedProcedureStepEndDate); ok {
		c.date(ds, TagPerformedProcedureStepEndDate, false)
	}
	if _, ok := ds.Get(TagPerformedProcedureStepEndTime); ok {
		c.clock(ds, TagPerformedProcedureStepEndTime, false)
	}
	for _, tag := range []core.Tag{TagProcedureCodeSequence, TagReasonForPerformedProcedureCodes, TagDiscontinuationReasonCodeSequence, TagPerformedProtocolCodeSequence} {
		c.codes(ds, tag)
	}
	if _, ok := ds.Get(TagPerformedSeriesSequence); ok {
		c.series(ds, false)
	}
	// N-SET has no Missing Attribute statuses: every problem is an invalid
	// value (PS3.7 10.1.3.1.9).
	c.invalid = append(append(c.invalid, c.missing...), c.empty...)
	c.missing, c.empty = nil, nil
	return c.err()
}

// ValidateFinal checks the Final State column of Table F.7.2-1 on the step as
// it would be after a transition to COMPLETED or DISCONTINUED: End Date and
// Time with values and at least one Performed Series item.
func ValidateFinal(ds *object.Object) error {
	c := &checker{}
	c.date(ds, TagPerformedProcedureStepEndDate, true)
	c.clock(ds, TagPerformedProcedureStepEndTime, true)
	c.series(ds, true)
	tags := append(append(append([]core.Tag(nil), c.missing...), c.empty...), c.invalid...)
	if len(tags) == 0 {
		return nil
	}
	return &StatusError{Status: DIMSEStatusProcessingFailure, Tags: tags, ErrorComment: "Final State attributes missing or invalid", Err: ErrFinalState}
}
