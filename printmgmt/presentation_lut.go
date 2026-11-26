package printmgmt

import (
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// PresentationLUT is one hardcopy LUT: exactly one shape or table (PS3.3
// C.11.4). Tables map every 8/12-bit input to 10..16-bit unsigned P-values.
type PresentationLUT struct {
	Shape                 string // IDENTITY or LIN OD
	InputBits, OutputBits int
	Entries               []uint16
	Explanation           string // default repertoire ASCII, at most 64 characters
}

func (p PresentationLUT) clone() PresentationLUT {
	p.Entries = append([]uint16(nil), p.Entries...)
	return p
}
func (p PresentationLUT) Validate() error {
	if p.Shape != "" {
		if (p.Shape != "IDENTITY" && p.Shape != "LIN OD") || p.InputBits != 0 || p.OutputBits != 0 || len(p.Entries) != 0 || p.Explanation != "" {
			return invalid(TagPresentationLUTShape, TagPresentationLUTSequence)
		}
		return nil
	}
	if (p.InputBits != 8 && p.InputBits != 12) || p.OutputBits < 10 || p.OutputBits > 16 || len(p.Entries) != (1<<p.InputBits) {
		return invalid(TagLUTDescriptor, TagLUTData)
	}
	if !validNativeText(p.Explanation, "") {
		return invalid(TagLUTExplanation)
	}
	maximum := uint32(1) << p.OutputBits
	for _, v := range p.Entries {
		if uint32(v) >= maximum {
			return invalid(TagLUTData)
		}
	}
	return nil
}
func (p PresentationLUT) validateImage(image *Image) error {
	if image == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if image.Color || (image.Bits != 8 && image.Bits != 12) || (p.Shape == "" && p.InputBits != image.Bits) {
		return invalid(TagReferencedPresentationLUT, TagBitsStored)
	}
	return nil
}

// DisplayLUT resolves P-values using the existing display/GSDF implementation.
// A calibrated printer LUT may be supplied for LIN OD; nil uses the explicit
// film density range. This is a calculation, never a measured printer result.
func (p PresentationLUT) DisplayLUT(inputBits int, density display.PrintDensity, calibration *display.DisplayLUT) (*display.LUT, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if inputBits != 8 && inputBits != 12 {
		return nil, invalid(TagBitsStored)
	}
	switch p.Shape {
	case "LIN OD":
		if err := validateNativeDensity(density); err != nil {
			return nil, err
		}
		if calibration != nil {
			return density.LinODPresentationLUTFor(calibration, inputBits)
		}
		return density.LinODPresentationLUT(inputBits)
	case "IDENTITY":
		count := 1 << inputBits
		entries := make([]uint16, count)
		for i := range entries {
			entries[i] = uint16(uint32(i) * 65535 / uint32(count-1))
		}
		return display.NewLUT([]int{count, 0, 16}, entries)
	default:
		if inputBits != p.InputBits {
			return nil, invalid(TagLUTDescriptor, TagBitsStored)
		}
		return display.NewLUT([]int{len(p.Entries), 0, p.OutputBits}, append([]uint16(nil), p.Entries...))
	}
}

func BuildPresentationLUT(p PresentationLUT) (*object.Object, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Shape != "" {
		return object.FromElements([]core.Element{text(TagPresentationLUTShape, core.VRCS, p.Shape)}, nil), nil
	}
	raw := make([]byte, 2*len(p.Entries))
	for i, v := range p.Entries {
		binary.LittleEndian.PutUint16(raw[2*i:], v)
	}
	fields := []core.Element{{Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRUS}, Value: core.Uint16Value{uint16(len(p.Entries)), 0, uint16(p.OutputBits)}}, core.NewRawElement(TagLUTData, core.VROW, raw)}
	if p.Explanation != "" {
		fields = append(fields, text(TagLUTExplanation, core.VRLO, p.Explanation))
	}
	return object.FromElements([]core.Element{sequenceOf(TagPresentationLUTSequence, core.DataSet{Elements: fields})}, nil), nil
}

func parsePresentationLUT(ds *object.Object) (PresentationLUT, error) {
	return parsePresentationLUTBudget(ds, 8192)
}
func parsePresentationLUTBudget(ds *object.Object, budget int) (PresentationLUT, error) {
	var p PresentationLUT
	if ds == nil {
		return p, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagPresentationLUTShape, TagPresentationLUTSequence)
	}
	shape, hasShape, err := presentationText(ds, TagPresentationLUTShape, core.VRCS)
	if err != nil {
		return p, err
	}
	e, hasTable := ds.Get(TagPresentationLUTSequence)
	if !hasShape && !hasTable {
		return p, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagPresentationLUTShape, TagPresentationLUTSequence)
	}
	if hasShape == hasTable {
		return p, invalid(TagPresentationLUTShape, TagPresentationLUTSequence)
	}
	if hasShape {
		if shape == "" {
			return p, failure(StatusMissingAttributeValue, "", ErrInvalidAttributes, TagPresentationLUTShape)
		}
		p.Shape = shape
		return p, p.Validate()
	}
	seq, ok := e.Value.(core.SequenceValue)
	if e.Header.VR != core.VRSQ || !ok || len(seq.Items) != 1 {
		return p, invalid(TagPresentationLUTSequence)
	}
	items, _ := ds.GetSequence(TagPresentationLUTSequence)
	item := items[0]
	desc, err := nativeUSValues(item, TagLUTDescriptor, 3)
	if err != nil {
		return p, err
	}
	switch desc[0] {
	case 256:
		p.InputBits = 8
	case 4096:
		p.InputBits = 12
	default:
		return p, invalid(TagLUTDescriptor)
	}
	p.OutputBits = int(desc[2])
	if desc[1] != 0 || p.OutputBits < 10 || p.OutputBits > 16 {
		return p, invalid(TagLUTDescriptor)
	}
	data, present := item.Get(TagLUTData)
	if !present {
		return p, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagLUTData)
	}
	if data.Header.VR != core.VROW {
		return p, invalid(TagLUTData)
	}
	count := int(desc[0])
	if 2*count > budget {
		return p, failure(StatusResourceLimitation, "", ErrInvalidAttributes, TagLUTData)
	}
	switch v := data.Value.(type) {
	case core.RawValue:
		if len(v) != 2*count {
			return p, invalid(TagLUTData)
		}
		p.Entries = make([]uint16, count)
		for i := range p.Entries {
			p.Entries[i] = item.ValueByteOrder().Uint16(v[2*i:])
		}
	case core.Uint16Value:
		if len(v) != count {
			return p, invalid(TagLUTData)
		}
		p.Entries = append([]uint16(nil), v...)
	default:
		return p, invalid(TagLUTData)
	}
	if value, present, err := nativeText(item, TagLUTExplanation, ""); err != nil {
		return p, err
	} else if present {
		p.Explanation = value
	}
	return p, p.Validate()
}

func nativeUSValues(ds *object.Object, tag core.Tag, count int) ([]uint16, error) {
	e, present := ds.Get(tag)
	if !present {
		return nil, failure(StatusMissingAttribute, "", ErrInvalidAttributes, tag)
	}
	if e.Header.VR != core.VRUS {
		return nil, invalid(tag)
	}
	switch v := e.Value.(type) {
	case core.Uint16Value:
		if len(v) == count {
			return append([]uint16(nil), v...), nil
		}
	case core.RawValue:
		if len(v) == 2*count {
			out := make([]uint16, count)
			for i := range out {
				out[i] = ds.ValueByteOrder().Uint16(v[2*i:])
			}
			return out, nil
		}
	}
	return nil, invalid(tag)
}
