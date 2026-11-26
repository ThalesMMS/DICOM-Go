package attrpath

import (
	"context"
	"encoding/binary"
	"errors"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// elementFromBytes accepts encoded value bytes, never decimal text or a DICOM
// element header. The session reserves per-target ownership before calling it.
func elementFromBytes(ctx context.Context, dataset *object.Object, tag core.Tag, seg Segment, existing *core.Element, value []byte, sourceOrder binary.ByteOrder, opts Options) (core.Element, error) {
	if ctx == nil || dataset == nil || (sourceOrder != binary.LittleEndian && sourceOrder != binary.BigEndian) {
		return core.Element{}, ErrValue
	}
	if err := ctx.Err(); err != nil {
		return core.Element{}, sessionError(err)
	}
	if len(value)%2 != 0 || uint64(len(value)) > math.MaxUint32 {
		return core.Element{}, ErrValue
	}
	vr := opts.VR
	if vr == "" && existing != nil && existing.VR() != core.VRUN {
		vr = existing.VR()
	}
	if vr == "" {
		if seg.Private() {
			entry, ok := dictionary.LookupScopedEntry(opts.Dictionary, tag, seg.Creator)
			if ok {
				vr = entry.VR
			}
		} else {
			spec, ok := dictionary.LookupVRSpec(opts.Dictionary, tag)
			if ok && len(spec.Values()) == 1 {
				vr = spec.Values()[0]
			} else if ok {
				// Resolve on a small temporary facade with the original sibling
				// context; the borrowed object itself is never changed.
				var siblings []core.Element
				for _, el := range dataset.Elements() {
					if el.Tag() == tag {
						continue
					}
					if _, seq := el.Value.(core.SequenceValue); !seq {
						siblings = append(siblings, el)
					}
				}
				siblings = append(siblings, core.Element{Header: core.ElementHeader{Tag: tag}, Value: core.RawValue{}})
				probe := object.FromElements(siblings, opts.Dictionary)
				probe.SetValueByteOrder(dataset.ValueByteOrder())
				if err := probe.ResolveContextualVRs(); err != nil {
					return core.Element{}, ErrUnknownVR
				}
				if el, ok := probe.Get(tag); ok {
					vr = el.VR()
				}
			}
		}
	}
	if _, err := core.ParseVR(string(vr)); err != nil || vr == "" || vr == core.VRUN {
		return core.Element{}, ErrUnknownVR
	}
	if vr == core.VRSQ {
		return core.Element{}, ErrValue
	}
	if existing != nil && existing.Value != nil {
		switch existing.Value.Kind() {
		case core.ValueSequence, core.ValueFragments, core.ValueBulkData, core.ValueDiscarded:
			return core.Element{}, ErrValue
		}
	}
	width := valueencode.ByteOrderWidth(vr)
	if len(value)%width != 0 || vr == core.VRAT && len(value)%4 != 0 {
		return core.Element{}, ErrValue
	}
	raw := value
	if width > 1 && sourceOrder != dataset.ValueByteOrder() {
		raw = valueencode.ReverseFixedWidth(value, width)
	}
	el := core.NewRawElement(tag, vr, raw)
	el.Header.Length = 0
	el.Header.LengthSet = false
	charset, err := dataset.CharacterSet()
	if err != nil {
		return core.Element{}, ErrValue
	}
	charsetElement := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue(charset.Names())}
	result, err := validation.ValidateDataSet(ctx, core.DataSet{Elements: []core.Element{charsetElement, el}}, validation.Options{Mode: validation.ModeStrict, Dictionary: opts.Dictionary, ByteOrder: dataset.ValueByteOrder(), MaxDepth: 1, MaxElements: 2, MaxFindings: 16})
	if result.Report.Truncated {
		return core.Element{}, ErrLimit
	}
	if _, bad := firstError(result.Report); bad {
		return core.Element{}, ErrValue
	}
	if err != nil {
		var invalid *validation.ValidationError
		if !errors.As(err, &invalid) {
			return core.Element{}, sessionError(err)
		}
	}
	// Validation owns its cloned values. Reuse that detached raw backing rather
	// than cloning the same payload once more after validation.
	for _, validated := range result.DataSet.Elements {
		if validated.Tag() == tag {
			validated.Header.Length = 0
			validated.Header.LengthSet = false
			return validated, nil
		}
	}
	return core.Element{}, ErrValue
}
