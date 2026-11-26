package ian

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrInvalidDataSet reports an Attribute List outside Table R.3-1 or its
	// value rules.
	ErrInvalidDataSet = errors.New("ian: invalid attribute list")
	// ErrDuplicate reports an N-CREATE for a notification UID already
	// received.
	ErrDuplicate = errors.New("ian: notification already received")
	// ErrResourceLimit reports a configured bound.
	ErrResourceLimit = errors.New("ian: resource limit")
)

// StatusError carries the DIMSE status an SCP returns for an error, with the
// attributes involved (tags only, never values).
type StatusError struct {
	Status       uint16
	Tags         []core.Tag
	ErrorComment string
	Err          error
}

func (e *StatusError) Error() string {
	tags := make([]string, 0, len(e.Tags))
	for _, tag := range e.Tags {
		tags = append(tags, tag.String())
	}
	return strings.TrimSpace(fmt.Sprintf("%v (status %04X %s)", e.Err, e.Status, strings.Join(tags, " ")))
}

func (e *StatusError) Unwrap() error { return e.Err }

// StatusOf maps an error to the N-CREATE status an SCP answers with. Errors
// that are not of this package map to 0110H Processing Failure.
func StatusOf(err error) uint16 {
	var statusErr *StatusError
	switch {
	case err == nil:
		return DIMSEStatusSuccess
	case errors.As(err, &statusErr):
		return statusErr.Status
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

func addTag(tags []core.Tag, tag core.Tag) []core.Tag {
	for _, existing := range tags {
		if existing == tag {
			return tags
		}
	}
	return append(tags, tag)
}

// values returns the trimmed values of tag and whether the element is
// present. A sequence or undecodable text is reported as invalid.
func (c *checker) values(ds *object.Object, tag core.Tag) ([]string, bool) {
	element, ok := ds.Get(tag)
	if !ok {
		return nil, false
	}
	if _, isSequence := element.Value.(core.SequenceValue); isSequence {
		c.invalid = addTag(c.invalid, tag)
		return nil, true
	}
	values, err := ds.LookupStrings(tag)
	if err != nil {
		c.invalid = addTag(c.invalid, tag)
		return nil, true
	}
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out, true
}

func (c *checker) value(ds *object.Object, tag core.Tag) (string, bool) {
	values, ok := c.values(ds, tag)
	return strings.Join(values, `\`), ok
}

func (c *checker) type1(ds *object.Object, tag core.Tag) string {
	value, ok := c.value(ds, tag)
	switch {
	case !ok:
		c.missing = addTag(c.missing, tag)
	case value == "":
		c.empty = addTag(c.empty, tag)
	}
	return value
}

func (c *checker) uid(ds *object.Object, tag core.Tag) {
	if value := c.type1(ds, tag); value != "" && !core.IsValidUID(value) {
		c.invalid = addTag(c.invalid, tag)
	}
}

func (c *checker) optionalUID(ds *object.Object, tag core.Tag) bool {
	value, ok := c.value(ds, tag)
	if ok && value != "" && !core.IsValidUID(value) {
		c.invalid = addTag(c.invalid, tag)
	}
	return value != ""
}

// items returns the items of a sequence that must be present; with required
// it must also hold at least one item.
func (c *checker) items(ds *object.Object, tag core.Tag, required bool) []*object.Object {
	element, ok := ds.Get(tag)
	if !ok {
		c.missing = addTag(c.missing, tag)
		return nil
	}
	if _, isSequence := element.Value.(core.SequenceValue); !isSequence {
		c.invalid = addTag(c.invalid, tag)
		return nil
	}
	items, _ := ds.GetSequence(tag)
	if required && len(items) == 0 {
		c.empty = addTag(c.empty, tag)
	}
	return items
}

func (c *checker) optionalSequence(ds *object.Object, tag core.Tag) {
	element, ok := ds.Get(tag)
	if !ok {
		return
	}
	if _, isSequence := element.Value.(core.SequenceValue); !isSequence {
		c.invalid = addTag(c.invalid, tag)
	}
}

func (c *checker) instance(ds *object.Object) {
	c.uid(ds, TagReferencedSOPClassUID)
	c.uid(ds, TagReferencedSOPInstanceUID)
	availability := c.type1(ds, TagInstanceAvailability)
	if availability != "" && !ValidAvailability(availability) {
		c.invalid = addTag(c.invalid, TagInstanceAvailability)
	}
	elsewhere := c.optionalUID(ds, TagRetrieveLocationUID)
	for _, tag := range []core.Tag{TagRetrieveURL, TagRetrieveURI} {
		if value, _ := c.value(ds, tag); value != "" {
			elsewhere = true
		}
	}
	c.optionalUID(ds, TagStorageMediaFileSetUID)
	titles, present := c.values(ds, TagRetrieveAETitle)
	for _, title := range titles {
		if len(title) > 16 || strings.ContainsAny(title, "\\\r\n") {
			c.invalid = addTag(c.invalid, TagRetrieveAETitle)
		}
	}
	if len(titles) == 0 && !elsewhere && availability != AvailabilityUnavailable {
		if present {
			c.empty = addTag(c.empty, TagRetrieveAETitle)
		} else {
			c.missing = addTag(c.missing, TagRetrieveAETitle)
		}
	}
}

// Validate checks an N-CREATE Attribute List against PS3.4 Table R.3-1 as
// described in the package documentation. The error is a *StatusError whose
// Status is the N-CREATE status an SCP answers with.
func Validate(ds *object.Object) error {
	if ds == nil {
		return &StatusError{Status: DIMSEStatusMissingAttribute, Err: ErrInvalidDataSet}
	}
	c := &checker{}
	for _, step := range c.items(ds, TagReferencedPerformedProcedureStepSequence, false) {
		c.uid(step, TagReferencedSOPClassUID)
		c.uid(step, TagReferencedSOPInstanceUID)
		c.optionalSequence(step, TagPerformedWorkitemCodeSequence)
	}
	c.uid(ds, TagStudyInstanceUID)
	for _, series := range c.items(ds, TagReferencedSeriesSequence, true) {
		c.uid(series, TagSeriesInstanceUID)
		for _, instance := range c.items(series, TagReferencedSOPSequence, true) {
			c.instance(instance)
		}
	}
	return c.err()
}
