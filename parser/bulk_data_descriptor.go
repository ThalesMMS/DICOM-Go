package parser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

var ErrInvalidBulkDataDescriptor = errors.New("dicom: invalid bulk data descriptor")

// Descriptor bounds are checked before any rule or path is copied.
const (
	MaxBulkDataRules     = 1024
	MaxBulkDataPathSteps = 65 // 64 ancestor sequences and one leaf.
)

// BulkDataPathStep identifies an ancestor sequence item or the leaf attribute.
// Ancestor ItemIndex is zero-based; -1 matches any item in a descriptor rule.
// The leaf has ItemIndex -1.
type BulkDataPathStep struct {
	Tag       core.Tag
	ItemIndex int
}

// BulkDataRule selects defined-length primitive values when Length is strictly
// greater than Threshold. Empty VR and Path match any eligible VR and path.
// A nonempty Path matches the entire path, including the leaf attribute.
type BulkDataRule struct {
	VR        core.VR
	Threshold int64
	Path      []BulkDataPathStep
}

// BulkDataDescriptor combines its rules by OR. It adds to the historical
// deferral options; a nil descriptor leaves their behavior unchanged.
// Each Reader snapshots the rules before reading. Sources must be seekable and
// not deflated. SQ containers and encapsulated fragment items are not selected.
type BulkDataDescriptor struct{ Rules []BulkDataRule }

// BulkDataVR reports whether PS3.18 permits BulkDataURI for vr.
func BulkDataVR(vr core.VR) bool {
	switch vr {
	case core.VRDS, core.VRFL, core.VRFD, core.VRIS, core.VRLT,
		core.VROB, core.VROD, core.VROF, core.VROL, core.VROV, core.VROW,
		core.VRSL, core.VRSS, core.VRST, core.VRSV, core.VRUC, core.VRUL,
		core.VRUN, core.VRUS, core.VRUT, core.VRUV:
		return true
	}
	return false
}

func compileBulkDataDescriptor(d *BulkDataDescriptor) ([]BulkDataRule, error) {
	if d == nil {
		return nil, nil
	}
	if len(d.Rules) > MaxBulkDataRules {
		return nil, fmt.Errorf("%w: more than %d rules", ErrInvalidBulkDataDescriptor, MaxBulkDataRules)
	}
	// Validate the entire input before allocation, including every path length.
	for i, rule := range d.Rules {
		if rule.Threshold < 0 || rule.VR != "" && !BulkDataVR(rule.VR) || len(rule.Path) > MaxBulkDataPathSteps {
			return nil, fmt.Errorf("%w: rule %d", ErrInvalidBulkDataDescriptor, i)
		}
		for j, step := range rule.Path {
			if step.ItemIndex < -1 || j == len(rule.Path)-1 && step.ItemIndex != -1 || step.Tag.Group == 0xfffe {
				return nil, fmt.Errorf("%w: rule %d path step %d", ErrInvalidBulkDataDescriptor, i, j)
			}
		}
	}
	copyRules := make([]BulkDataRule, len(d.Rules))
	for i, rule := range d.Rules {
		copyRules[i] = rule
		copyRules[i].Path = append([]BulkDataPathStep(nil), rule.Path...)
	}
	return copyRules, nil
}

func (r *Reader) bulkPath(tag core.Tag) []BulkDataPathStep {
	if len(r.seqDelimiters)/2+1 > MaxBulkDataPathSteps {
		return nil
	}
	path := make([]BulkDataPathStep, 0, len(r.seqDelimiters)/2+1)
	for _, frame := range r.seqDelimiters {
		if frame.typ == seqTokenTypeSequence {
			path = append(path, BulkDataPathStep{Tag: frame.tag, ItemIndex: frame.items - 1})
		}
	}
	return append(path, BulkDataPathStep{Tag: tag, ItemIndex: -1})
}

// Metadata used after parsing to resolve implicit contextual VRs stays inline.
func bulkDescriptorContextTag(tag core.Tag) bool {
	return tag == core.NewTag(0x0028, 0x0103) || tag == core.NewTag(0x0028, 0x0100) || tag == core.NewTag(0x0028, 0x3002)
}

func (r *Reader) deferBulkValue(header core.ElementHeader) bool {
	if bulkDescriptorContextTag(header.Tag) {
		return false
	}
	if len(r.bulkDataRules) == 0 || !BulkDataVR(header.VR) || header.Length.IsUndefined() || dictionary.IsPrivateCreatorTag(header.Tag) {
		return false
	}
	for _, frame := range r.seqDelimiters {
		if frame.typ == seqTokenTypePixelSequence {
			return false
		}
	}
	var path []BulkDataPathStep
	for _, rule := range r.bulkDataRules {
		if int64(header.Length) <= rule.Threshold || rule.VR != "" && rule.VR != header.VR {
			continue
		}
		if len(rule.Path) == 0 {
			return true
		}
		if path == nil {
			path = r.bulkPath(header.Tag)
		}
		if len(rule.Path) != len(path) {
			continue
		}
		match := true
		for i, step := range rule.Path {
			if step.Tag != path[i].Tag || step.ItemIndex != -1 && step.ItemIndex != path[i].ItemIndex {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// ValueReference describes original encoded Value Field bytes, not a decoded
// or normalized value. Offset is physical, relative to SeekStart(0) of the
// original source. Length includes item headers/delimiter for deferred
// encapsulated Pixel Data. Path is an owned copy; ByteOrder is source encoding.
type ValueReference struct {
	// UndefinedLength preserves the original element header's length form.
	// Length remains the actual encoded span, including encapsulated items.
	UndefinedLength   bool
	Tag               core.Tag
	VR                core.VR
	Offset            int64
	Length            int64
	Path              []BulkDataPathStep
	ByteOrder         binary.ByteOrder
	TransferSyntaxUID string
	ExplicitVR        bool
}

// ValueReferenceAt returns metadata only for a recorded location.
// It does not read the payload or change the source cursor, and does not
// transfer source ownership.
func (r *Reader) ValueReferenceAt(location ValueLocation) (ValueReference, error) {
	if r == nil {
		return ValueReference{}, fmt.Errorf("dicom: nil reader")
	}
	ref, ok := r.valueReferences[location]
	if !ok {
		return ValueReference{}, fmt.Errorf("dicom: value location has no source reference")
	}
	seeker, ok := r.counter.r.(io.ReadSeeker)
	if !ok {
		return ValueReference{}, fmt.Errorf("dicom: source is not seekable")
	}
	if _, err := seeker.Seek(0, io.SeekCurrent); err != nil {
		return ValueReference{}, fmt.Errorf("dicom: source reference unavailable: %w", err)
	}
	if location.ValueOffset < r.baseOffset || r.baseOffset < 0 && location.ValueOffset > math.MaxInt64+r.baseOffset {
		return ValueReference{}, fmt.Errorf("dicom: invalid logical value offset")
	}
	delta := location.ValueOffset - r.baseOffset
	if r.rootOffset < 0 || delta > math.MaxInt64-r.rootOffset || location.Length < 0 {
		return ValueReference{}, fmt.Errorf("dicom: invalid physical value offset")
	}
	ref.Offset = r.rootOffset + delta
	if ref.Length > math.MaxInt64-ref.Offset || r.sourceEnd > 0 && location.Length > r.sourceEnd-location.ValueOffset {
		return ValueReference{}, fmt.Errorf("dicom: invalid physical value range")
	}
	if len(ref.Path) == 0 || len(ref.Path) > MaxBulkDataPathSteps {
		return ValueReference{}, fmt.Errorf("dicom: source reference path exceeds %d steps", MaxBulkDataPathSteps)
	}
	ref.Path = append([]BulkDataPathStep(nil), ref.Path...)
	return ref, nil
}
