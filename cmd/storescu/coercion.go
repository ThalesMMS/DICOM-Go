package main

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

var errAttributeAssignment = errors.New("invalid attribute assignment: use a known metadata keyword or tag with a valid VR/VM value")

type attributeAssignments []string

// Validation is deliberately deferred: flag must not print assignment values
// as part of its own invalid-value diagnostic.
func (a *attributeAssignments) Set(value string) error { *a = append(*a, value); return nil }
func (a *attributeAssignments) String() string         { return "" }

type attributeAssignment struct {
	tag  core.Tag
	vr   core.VR
	text string
}

func parseAttributeAssignments(values []string) ([]attributeAssignment, error) {
	assignments := make([]attributeAssignment, 0, len(values))
	for _, value := range values {
		name, text, ok := strings.Cut(value, "=")
		if !ok {
			return nil, errAttributeAssignment
		}
		tag, err := clifile.ParseTag(strings.TrimSpace(name))
		if err != nil || tag.Group == 0 || tag.Group == 2 || tag.Group == 0x0028 || tag.Group == 0x7FE0 || tag.Group&0xFF00 == 0x6000 {
			return nil, errAttributeAssignment
		}
		entry, ok := std.Dictionary.ByTag(tag)
		if !ok {
			return nil, errAttributeAssignment
		}
		vr, err := core.ParseVR(string(entry.VR))
		if err != nil || vr == core.VRSQ || !vr.IsStringLike() && vr != core.VRUS && vr != core.VRSS && vr != core.VRUL && vr != core.VRSL && vr != core.VRUV && vr != core.VRSV && vr != core.VRFL && vr != core.VRFD && vr != core.VRAT {
			return nil, errAttributeAssignment
		}
		element, err := attrpath.ElementFromString(tag, vr, text, binary.LittleEndian)
		if err != nil {
			return nil, errAttributeAssignment
		}
		if _, err := validation.ValidateElement(context.Background(), element, validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, ByteOrder: binary.LittleEndian}); err != nil {
			return nil, errAttributeAssignment
		}
		assignments = append(assignments, attributeAssignment{tag: tag, vr: vr, text: text})
	}
	return assignments, nil
}

func assignmentCoercion(assignments []attributeAssignment) object.AttributeCoercion {
	return object.CoercionFunc(func(dataset *object.Object) ([]core.Element, error) {
		attributes := object.New(nil)
		attributes.SetValueByteOrder(dataset.ValueByteOrder())
		for _, assignment := range assignments {
			element, err := attrpath.ElementFromString(assignment.tag, assignment.vr, assignment.text, dataset.ValueByteOrder())
			if err != nil {
				return nil, errAttributeAssignment
			}
			attributes.Put(element)
		}
		return (object.MergeAttributes{Attributes: attributes, Policy: object.UpdateOverwrite}).Coerce(dataset)
	})
}
