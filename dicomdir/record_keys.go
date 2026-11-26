package dicomdir

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

const (
	defaultMaxDirectoryRecords          = 400_000
	defaultMaxRecordKeyBytes      int64 = 4 << 20
	defaultMaxTotalRecordKeyBytes int64 = 64 << 20
	defaultMaxRecordKeyElements         = 10_000
	defaultMaxRecordKeyDepth            = 32
)

type directoryKeyRule struct {
	tag  core.Tag
	kind string
}

func keyTag(group, element uint16) core.Tag { return core.NewTag(group, element) }

// SelectionKeys returns an independent snapshot of the instance-specific
// directory keys. Text and numeric bytes use UTF-8 and Little Endian.
func (r FileRecord) SelectionKeys() core.DataSet { return cloneDirectoryKeys(r.keys) }

func cloneDirectoryKeys(ds core.DataSet) core.DataSet {
	out := core.DataSet{Elements: make([]core.Element, len(ds.Elements))}
	for i, e := range ds.Elements {
		out.Elements[i] = e
		switch v := e.Value.(type) {
		case core.RawValue:
			out.Elements[i].Value = core.RawValue(core.CloneBytes(v))
		case core.StringValue:
			out.Elements[i].Value = append(core.StringValue(nil), v...)
		case core.SequenceValue:
			items := make([]core.DataSet, len(v.Items))
			for j := range items {
				items[j] = cloneDirectoryKeys(v.Items[j])
			}
			out.Elements[i].Value = core.SequenceValue{Items: items}
		}
	}
	return out
}

type directoryKeyBudget struct {
	ctx      context.Context
	limits   Limits
	bytes    int64
	elements int
}

func (b *directoryKeyBudget) charge(n int64, depth int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if n < 0 || depth > b.limits.MaxRecordKeyDepth || b.elements >= b.limits.MaxRecordKeyElements ||
		n > b.limits.MaxRecordKeyBytes-b.bytes {
		return ErrResourceLimit
	}
	b.bytes += n
	b.elements++
	return nil
}

// ownDirectoryElement charges before allocating/copying a requested value.
// Source objects remain valid only for the duration of the held source handle.
func ownDirectoryElement(src *object.Object, tag core.Tag, b *directoryKeyBudget, depth int) (core.Element, error) {
	e, ok := src.Get(tag)
	if !ok {
		return core.Element{}, ErrMissingRequiredKey
	}
	if e.VR() == core.VRSQ {
		items, ok := src.GetSequence(tag)
		if !ok {
			return core.Element{}, ErrInvalidRecord
		}
		if err := b.charge(16+int64(len(items))*16, depth); err != nil {
			return core.Element{}, err
		}
		owned := make([]core.DataSet, len(items))
		for i, item := range items {
			if item == nil {
				return core.Element{}, ErrInvalidRecord
			}
			var err error
			owned[i], err = ownDirectoryDataSet(item, b, depth+1)
			if err != nil {
				return core.Element{}, err
			}
		}
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: owned}}, nil
	}
	length, known := e.CalculatedLength()
	if !known || length == core.UndefinedLength {
		return core.Element{}, ErrInvalidRecord
	}
	if err := b.charge(int64(length)+16, depth); err != nil {
		return core.Element{}, err
	}
	if e.VR().IsStringLike() {
		values, ok := src.GetStrings(tag)
		if !ok && length != 0 {
			return core.Element{}, ErrInvalidRecord
		}
		utfBytes := int64(0)
		for _, value := range values {
			utfBytes += int64(len(value)) + 1
		}
		if utfBytes > int64(length) {
			extra := utfBytes - int64(length)
			if extra > b.limits.MaxRecordKeyBytes-b.bytes {
				return core.Element{}, ErrResourceLimit
			}
			b.bytes += extra
		}
		return directoryValueElement(tag, e.VR(), append(core.StringValue(nil), values...)), nil
	}
	buffer := bytes.NewBuffer(make([]byte, 0, int(length)))
	if _, err := src.CopyValueTo(tag, buffer); err != nil {
		return core.Element{}, ErrInvalidRecord
	}
	if buffer.Len() != int(length) {
		return core.Element{}, ErrInvalidRecord
	}
	raw := buffer.Bytes()
	if src.ValueByteOrder() == binary.BigEndian {
		width := 1
		switch e.VR() {
		case core.VRUS, core.VRSS, core.VROW, core.VRAT:
			width = 2
		case core.VRUL, core.VRSL, core.VRFL, core.VROL, core.VROF:
			width = 4
		case core.VRFD, core.VRSV, core.VRUV, core.VROD, core.VROV:
			width = 8
		}
		if len(raw)%width != 0 {
			return core.Element{}, ErrInvalidRecord
		}
		for i := 0; i < len(raw); i += width {
			for j := 0; j < width/2; j++ {
				raw[i+j], raw[i+width-1-j] = raw[i+width-1-j], raw[i+j]
			}
		}
	}
	return core.NewRawElement(tag, e.VR(), raw), nil
}

func ownDirectoryDataSet(src *object.Object, b *directoryKeyBudget, depth int) (core.DataSet, error) {
	if depth > b.limits.MaxRecordKeyDepth {
		return core.DataSet{}, ErrResourceLimit
	}
	out := core.DataSet{}
	for _, e := range src.Elements() {
		if e.Tag() == tagSpecificCharacterSetFS {
			continue
		}
		owned, err := ownDirectoryElement(src, e.Tag(), b, depth)
		if err != nil {
			return core.DataSet{}, err
		}
		out.Elements = append(out.Elements, owned)
	}
	// ASCII needs no extra declaration; non-ASCII decoded strings get an
	// independent UTF-8 declaration so inherited source encodings cannot leak.
	needsUTF8 := src.Has(tagSpecificCharacterSetFS)
	for _, e := range out.Elements {
		if values, ok := e.Value.(core.StringValue); ok {
			for _, value := range values {
				for _, r := range value {
					if r > 127 {
						needsUTF8 = true
						break
					}
				}
			}
		}
	}
	if needsUTF8 {
		if err := b.charge(26, depth); err != nil {
			return core.DataSet{}, err
		}
		out.Elements = append(out.Elements, textElement(tagSpecificCharacterSetFS, core.VRCS, "ISO_IR 192"))
	}
	sort.Slice(out.Elements, func(i, j int) bool { return out.Elements[i].Tag().Less(out.Elements[j].Tag()) })
	return out, nil
}

func (f *FileSet) readDirectoryKeys(ctx context.Context, r *FileRecord) error {
	return f.readDirectoryKeysWithLimits(ctx, r, f.options.Limits)
}
func (f *FileSet) readDirectoryKeysWithLimits(ctx context.Context, r *FileRecord, keyLimits Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.RecordType == RecordTypeImage {
		r.keyBytes = int64(len(r.InstanceNumber) + 16)
		if r.keyBytes > keyLimits.MaxRecordKeyBytes {
			return ErrResourceLimit
		}
		r.keys = core.DataSet{Elements: []core.Element{textElement(tagInstanceNumberFS, core.VRIS, r.InstanceNumber)}}
		return nil
	}
	handle, _, err := OpenReferencedFile(f.root, Reference{FileID: []string(r.FileID)})
	if err != nil {
		return ErrInvalidRecord
	}
	defer handle.Close()
	indexLimits := f.options.IndexOptions.Limits
	maxTotal := indexLimits.MaxTotalBytes
	if maxTotal == 0 {
		maxTotal = 512 << 20
	}
	header, selected, err := readSelectedDirectoryMetadata(ctx, handle, r.RecordType, keyLimits, maxTotal, false)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if isParserResourceLimit(err) || errors.Is(err, ErrResourceLimit) {
			return ErrResourceLimit
		}
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	class, _ := header.Meta.GetUID(tagMediaStorageSOPClassUIDDirectory)
	instance, _ := header.Meta.GetUID(tagMediaStorageSOPInstanceUIDDirectory)
	if header.TransferSyntax.UID != r.TransferSyntaxUID || class != r.SOPClassUID || instance != r.SOPInstanceUID {
		return ErrSourceChanged
	}
	b := &directoryKeyBudget{ctx: ctx, limits: keyLimits}
	keys, err := extractDirectoryKeys(selected, *r, b)
	if err != nil {
		return err
	}
	r.keys, r.keyBytes = keys, b.bytes
	return nil
}

func extractDirectoryKeys(src *object.Object, r FileRecord, b *directoryKeyBudget) (core.DataSet, error) {
	out := core.DataSet{}
	if r.RecordType == RecordTypeWaveformPresentation && src.Has(keyTag(0x0040, 0xB030)) {
		series, err := directorySequence(src, keyTag(0x0008, 0x1115), 1, -1)
		if err != nil {
			return core.DataSet{}, err
		}
		for _, item := range series {
			if !item.Has(keyTag(0x0008, 0x114A)) {
				return core.DataSet{}, ErrMissingRequiredKey
			}
		}
	}
	for _, rule := range directoryKeyRules(r.RecordType) {
		required, include := directoryKeyCondition(src, r, rule)
		if !include {
			continue
		}
		entry, ok := std.Dictionary.ByTag(rule.tag)
		if !ok {
			return core.DataSet{}, ErrInvalidRecord
		}
		var e core.Element
		var err error
		switch {
		case rule.tag == tagInstanceNumberFS:
			if required && r.InstanceNumber == "" {
				return core.DataSet{}, ErrMissingRequiredKey
			}
			if err = b.charge(int64(len(r.InstanceNumber))+16, 0); err != nil {
				return core.DataSet{}, err
			}
			e = textElement(rule.tag, core.VRIS, r.InstanceNumber)
		case r.RecordType == RecordTypeSRDocument && rule.tag == keyTag(0x0040, 0xA030):
			e, err = verificationDirectoryKey(src, b)
		case (r.RecordType == RecordTypeSRDocument || r.RecordType == RecordTypeKeyObjectDocument) && rule.tag == keyTag(0x0040, 0xA730):
			e, err = titleModifierDirectoryKey(src, b)
		case !src.Has(rule.tag):
			if required {
				return core.DataSet{}, ErrMissingRequiredKey
			}
			if err = b.charge(16, 0); err != nil {
				return core.DataSet{}, err
			}
			if entry.VR == core.VRSQ {
				e = directoryValueElement(rule.tag, entry.VR, core.SequenceValue{})
			} else {
				e = core.NewRawElement(rule.tag, entry.VR, nil)
			}
		default:
			e, err = ownDirectoryElement(src, rule.tag, b, 0)
		}
		if err != nil {
			return core.DataSet{}, err
		}
		if e.VR() != entry.VR {
			return core.DataSet{}, ErrInvalidRecord
		}
		if required && !nonEmptyDirectoryKey(e) {
			return core.DataSet{}, ErrMissingRequiredKey
		}
		out.Elements = append(out.Elements, e)
	}
	var normalizeErr error
	out, normalizeErr = normalizeDirectoryKeySequences(r.RecordType, out, b)
	if normalizeErr != nil {
		return core.DataSet{}, normalizeErr
	}
	if err := validateDirectoryKeys(r.RecordType, out, r.SOPClassUID, b.limits); err != nil {
		return core.DataSet{}, err
	}
	return out, nil
}

func nonEmptyDirectoryKey(e core.Element) bool {
	if seq, ok := e.Value.(core.SequenceValue); ok {
		return len(seq.Items) != 0
	}
	if values, ok := e.Value.(core.StringValue); ok {
		for _, value := range values {
			if value != "" {
				return true
			}
		}
		return false
	}
	n, ok := e.CalculatedLength()
	return ok && n != 0 && n != core.UndefinedLength
}

func validateDirectoryKeys(typ RecordType, keys core.DataSet, sop string, admission ...Limits) error {
	limits := Limits{}
	if len(admission) > 0 {
		limits = admission[0]
	}
	if err := normalizeFileSetLimits(&limits); err != nil {
		return err
	}
	if !standardFileRecordType(typ) {
		return ErrUnsupportedSOPClass
	}
	obj := object.FromDataSet(keys, std.Dictionary)
	for _, rule := range directoryKeyRules(typ) {
		if rule.kind == "1C" || rule.kind == "2C" {
			continue
		}
		e, ok := obj.Get(rule.tag)
		if !ok || rule.kind == "1" && !nonEmptyDirectoryKey(e) {
			return ErrMissingRequiredKey
		}
	}
	if _, err := validation.ValidateDataSet(context.Background(), keys, validation.Options{
		Mode: validation.ModeStrict, Dictionary: std.Dictionary, StopFirst: true,
		MaxFindings: 1, MaxDepth: limits.MaxRecordKeyDepth*2 + 2, MaxElements: limits.MaxRecordKeyElements,
	}); err != nil {
		return errors.Join(ErrInvalidRecord, ErrMissingRequiredKey)
	}
	return validateDirectoryKeySequences(obj, typ, sop)
}

func directoryKeysEqual(a, b core.DataSet) bool {
	// Keys are canonical owned values, so semantic comparison is independent of
	// source byte order or character-set declaration.
	return equalDirectoryKeyObjects(object.FromDataSet(a, std.Dictionary), object.FromDataSet(b, std.Dictionary))
}

func equalDirectoryKeyObjects(a, b *object.Object) bool {
	aa, bb := a.SortedElements(), b.SortedElements()
	if len(aa) != len(bb) {
		return false
	}
	for i, e := range aa {
		other := bb[i]
		if e.Tag() != other.Tag() || e.VR() != other.VR() {
			return false
		}
		if e.VR() == core.VRSQ {
			as, _ := a.GetSequence(e.Tag())
			bs, _ := b.GetSequence(e.Tag())
			if len(as) != len(bs) {
				return false
			}
			for j := range as {
				if !equalDirectoryKeyObjects(as[j], bs[j]) {
					return false
				}
			}
		} else if e.VR().IsStringLike() {
			as, _ := a.GetStrings(e.Tag())
			bs, _ := b.GetStrings(e.Tag())
			if !equalStrings(as, bs) {
				return false
			}
		} else {
			ar, _ := e.RawBytes()
			br, _ := other.RawBytes()
			if !bytes.Equal(ar, br) {
				return false
			}
		}
	}
	return true
}

// directoryKeyRules transcribes the factual tag/type columns of PS3.3 F.5.
// Conditions and nested macros are implemented in record_keys_sequences.go.
func directoryKeyRules(typ RecordType) []directoryKeyRule {
	switch typ {
	case RecordType("IMAGE"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}}
	case RecordType("RT DOSE"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x3004, 0x000A), kind: "1"}}
	case RecordType("RT STRUCTURE SET"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x3006, 0x0002), kind: "1"}, {tag: keyTag(0x3006, 0x0008), kind: "2"}, {tag: keyTag(0x3006, 0x0009), kind: "2"}}
	case RecordType("RT PLAN"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x300A, 0x0002), kind: "1"}, {tag: keyTag(0x300A, 0x0006), kind: "2"}, {tag: keyTag(0x300A, 0x0007), kind: "2"}}
	case RecordType("RT TREAT RECORD"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x3008, 0x0250), kind: "2"}, {tag: keyTag(0x3008, 0x0251), kind: "2"}}
	case RecordType("PRESENTATION"):
		return []directoryKeyRule{{tag: keyTag(0x0070, 0x0082), kind: "1"}, {tag: keyTag(0x0070, 0x0083), kind: "1"}, {tag: keyTag(0x0008, 0x1115), kind: "1C"}, {tag: keyTag(0x0070, 0x0402), kind: "1C"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("WAVEFORM"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}}
	case RecordType("SR DOCUMENT"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0040, 0xA491), kind: "1"}, {tag: keyTag(0x0040, 0xA493), kind: "1"}, {tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0040, 0xA030), kind: "1C"}, {tag: keyTag(0x0040, 0xA043), kind: "1"}, {tag: keyTag(0x0040, 0xA730), kind: "1C"}}
	case RecordType("KEY OBJECT DOC"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0040, 0xA043), kind: "1"}, {tag: keyTag(0x0040, 0xA730), kind: "1C"}}
	case RecordType("SPECTROSCOPY"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0008), kind: "1"}, {tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0008, 0x9092), kind: "1C"}, {tag: keyTag(0x0028, 0x0008), kind: "1"}, {tag: keyTag(0x0028, 0x0010), kind: "1"}, {tag: keyTag(0x0028, 0x0011), kind: "1"}, {tag: keyTag(0x0028, 0x9001), kind: "1"}, {tag: keyTag(0x0028, 0x9002), kind: "1"}}
	case RecordType("RAW DATA"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "2"}}
	case RecordType("REGISTRATION"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("FIDUCIAL"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("HANGING PROTOCOL"):
		return []directoryKeyRule{{tag: keyTag(0x0072, 0x0002), kind: "1"}, {tag: keyTag(0x0072, 0x0004), kind: "1"}, {tag: keyTag(0x0072, 0x0006), kind: "1"}, {tag: keyTag(0x0072, 0x0008), kind: "1"}, {tag: keyTag(0x0072, 0x000A), kind: "1"}, {tag: keyTag(0x0072, 0x000C), kind: "1"}, {tag: keyTag(0x0072, 0x0014), kind: "1"}, {tag: keyTag(0x0072, 0x000E), kind: "2"}}
	case RecordType("ENCAP DOC"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "2"}, {tag: keyTag(0x0008, 0x0033), kind: "2"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0042, 0x0010), kind: "2"}, {tag: keyTag(0x0040, 0xE001), kind: "1C"}, {tag: keyTag(0x0040, 0xA043), kind: "2"}, {tag: keyTag(0x0042, 0x0012), kind: "1"}}
	case RecordType("VALUE MAP"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("STEREOMETRIC"):
		return []directoryKeyRule{}
	case RecordType("PALETTE"):
		return []directoryKeyRule{{tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("IMPLANT"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0070), kind: "1"}, {tag: keyTag(0x0022, 0x1095), kind: "1"}, {tag: keyTag(0x0068, 0x6210), kind: "1C"}, {tag: keyTag(0x0022, 0x1097), kind: "1"}}
	case RecordType("IMPLANT ASSY"):
		return []directoryKeyRule{{tag: keyTag(0x0076, 0x0001), kind: "1"}, {tag: keyTag(0x0008, 0x0070), kind: "1"}, {tag: keyTag(0x0076, 0x0020), kind: "1"}}
	case RecordType("IMPLANT GROUP"):
		return []directoryKeyRule{{tag: keyTag(0x0078, 0x0001), kind: "1"}, {tag: keyTag(0x0078, 0x0020), kind: "1"}}
	case RecordType("PLAN"):
		return []directoryKeyRule{}
	case RecordType("MEASUREMENT"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("SURFACE"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("SURFACE SCAN"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}}
	case RecordType("TRACT"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("ASSESSMENT"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0008, 0x0012), kind: "1"}, {tag: keyTag(0x0008, 0x0013), kind: "2"}}
	case RecordType("RADIOTHERAPY"):
		return []directoryKeyRule{{tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x3010, 0x0033), kind: "1C"}, {tag: keyTag(0x3010, 0x0034), kind: "1C"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}, {tag: keyTag(0x0070, 0x0084), kind: "2"}}
	case RecordType("ANNOTATION"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	case RecordType("INVENTORY"):
		return []directoryKeyRule{{tag: keyTag(0x0008, 0x0023), kind: "1"}, {tag: keyTag(0x0008, 0x0033), kind: "1"}, {tag: keyTag(0x0008, 0x0401), kind: "2"}, {tag: keyTag(0x0008, 0x0403), kind: "1"}, {tag: keyTag(0x0008, 0x0426), kind: "1"}, {tag: keyTag(0x0008, 0x0428), kind: "1"}, {tag: keyTag(0x0008, 0x0400), kind: "2"}}
	case RecordType("WF PRESENTATION"):
		return []directoryKeyRule{{tag: keyTag(0x0070, 0x0082), kind: "1"}, {tag: keyTag(0x0070, 0x0083), kind: "1"}, {tag: keyTag(0x0008, 0x1115), kind: "1C"}, {tag: keyTag(0x0020, 0x0013), kind: "1"}, {tag: keyTag(0x0070, 0x0080), kind: "1"}, {tag: keyTag(0x0070, 0x0081), kind: "2"}}
	}
	return nil
}

func directoryValueElement(tag core.Tag, vr core.VR, value core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: value}
}

type directoryTokenFrame struct {
	tag      core.Tag
	item     bool
	wanted   bool
	elements []core.Element
	items    []core.DataSet
	offset   int64
	seen     map[core.Tag]bool
}

// readSelectedDirectoryMetadata bounds requested bytes and item counts at
// token boundaries, before constructing a dataset or allocating value bytes.
// Unrequested primitives/bulk values are skipped. Container parsing uses the
// existing SelectiveReader and remains bounded by extent, depth and tokens.
func readSelectedDirectoryMetadata(ctx context.Context, handle io.ReadSeeker, typ RecordType, limits Limits, maxTotal int64, directory bool) (*object.Part10Header, *object.Object, error) {
	if err := normalizeFileSetLimits(&limits); err != nil {
		return nil, nil, err
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return nil, nil, ErrInvalidRecord
	}
	header, err := object.ReadPart10Header(handle, object.ReadFileOptions{MaxElementBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxElements: 256, MaxSequenceDepth: 4})
	if err != nil {
		return nil, nil, err
	}
	if directory && header.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
		return nil, nil, ErrInvalidRecord
	}
	wanted := map[core.Tag]bool{tagSpecificCharacterSetFS: true, tagSOPClassUIDFS: true, tagSOPInstanceUIDFS: true}
	for _, rule := range directoryKeyRules(typ) {
		wanted[rule.tag] = true
	}
	if typ == RecordTypeSRDocument {
		wanted[keyTag(0x0040, 0xA073)] = true
		wanted[keyTag(0x0008, 0x0201)] = true
	}
	if typ == RecordTypeWaveformPresentation {
		wanted[keyTag(0x0040, 0xB030)] = true
	}
	var source io.Reader = handle
	if header.TransferSyntax.Deflated {
		inflated := flate.NewReader(handle)
		defer inflated.Close()
		source = inflated
	}
	var stack []directoryTokenFrame
	var elements []core.Element
	bytesUsed, recordBytes := int64(0), int64(0)
	selectedCount, recordCount, records, tokens := 0, 0, 0, 0
	charge := func(n int64) error {
		if n < 0 {
			return ErrResourceLimit
		}
		if directory {
			if n > limits.MaxTotalRecordKeyBytes-bytesUsed || n > limits.MaxRecordKeyBytes-recordBytes {
				return ErrResourceLimit
			}
			recordBytes += n
			recordCount++
			if recordCount > limits.MaxRecordKeyElements {
				return ErrResourceLimit
			}
		} else if n > limits.MaxRecordKeyBytes-bytesUsed || selectedCount >= limits.MaxRecordKeyElements {
			return ErrResourceLimit
		}
		bytesUsed += n
		selectedCount++
		return nil
	}
	rootSeen := map[core.Tag]bool{}
	var selectionErr error
	selector := func(_ context.Context, _ validation.Path, h core.ElementHeader, _ int64) (parser.SelectiveDisposition, error) {
		include := directory || wanted[h.Tag]
		if len(stack) > 0 {
			include = stack[len(stack)-1].wanted
		}
		if include {
			seen := rootSeen
			if len(stack) > 0 {
				frame := &stack[len(stack)-1]
				if frame.seen == nil {
					frame.seen = map[core.Tag]bool{}
				}
				seen = frame.seen
			}
			if seen[h.Tag] {
				selectionErr = ErrInvalidRecord
				return parser.SelectiveStop, selectionErr
			}
			seen[h.Tag] = true
		}
		if h.VR == core.VRSQ || h.VR == core.VRUN && h.Length.IsUndefined() {
			return parser.SelectiveMaterialize, nil
		}
		if !include && dictionary.IsPrivateCreatorTag(h.Tag) {
			if h.Length.IsUndefined() || h.VR != core.VRLO || h.Length > 64 {
				selectionErr = ErrInvalidRecord
				return parser.SelectiveStop, selectionErr
			}
			if err := charge(int64(h.Length) + 16); err != nil {
				selectionErr = err
				return parser.SelectiveStop, err
			}
			return parser.SelectiveMaterialize, nil
		}
		if !include {
			return parser.SelectiveSkip, nil
		}
		if h.Length.IsUndefined() {
			return parser.SelectiveSkip, nil
		}
		if err := charge(int64(h.Length) + 16); err != nil {
			selectionErr = err
			return parser.SelectiveStop, err
		}
		return parser.SelectiveMaterialize, nil
	}
	reader, err := parser.NewSelectiveReader(ctx, source, header.TransferSyntax, parser.ReaderOptions{
		Dictionary: std.Dictionary, BaseOffset: header.DataSetOffset, MaxTotalBytes: maxTotal,
		MaxElementBytes: limits.MaxRecordKeyBytes, MaxSequenceDepth: limits.MaxRecordKeyDepth + 2, MaxFragments: 200000,
	}, parser.SelectiveReaderOptions{Select: selector})
	if err != nil {
		return nil, nil, err
	}
	appendElement := func(e core.Element) {
		if len(stack) == 0 {
			elements = append(elements, e)
		} else {
			stack[len(stack)-1].elements = append(stack[len(stack)-1].elements, e)
		}
	}
	maxTokens := 1_000_000
	if directory {
		maxTokens = 16_000_000
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		tok, err := reader.Next()
		if selectionErr != nil {
			return nil, nil, selectionErr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		tokens++
		if tokens > maxTokens {
			return nil, nil, ErrResourceLimit
		}
		switch tok.Kind {
		case parser.TokenStartSequence:
			include := directory || wanted[tok.Header.Tag]
			if len(stack) > 0 {
				include = stack[len(stack)-1].wanted
			}
			if include {
				if err := charge(16); err != nil {
					return nil, nil, err
				}
			}
			stack = append(stack, directoryTokenFrame{tag: tok.Header.Tag, wanted: include})
		case parser.TokenStartItem:
			if len(stack) == 0 || stack[len(stack)-1].item {
				return nil, nil, ErrInvalidRecord
			}
			include := stack[len(stack)-1].wanted
			if directory && len(stack) == 1 && stack[0].tag == tagDirectoryRecordSequence {
				recordBytes, recordCount = 0, 0
				records++
				if records > limits.MaxDirectoryRecords {
					return nil, nil, ErrResourceLimit
				}
			}
			if include {
				if err := charge(32); err != nil {
					return nil, nil, err
				}
			}
			stack = append(stack, directoryTokenFrame{item: true, wanted: include, offset: tok.Offset})
		case parser.TokenEndItem:
			if len(stack) < 2 || !stack[len(stack)-1].item {
				return nil, nil, ErrInvalidRecord
			}
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if frame.wanted {
				stack[len(stack)-1].items = append(stack[len(stack)-1].items, core.DataSet{Elements: frame.elements, ItemOffset: frame.offset, ItemOffsetSet: true})
			}
		case parser.TokenEndSequence:
			if len(stack) == 0 || stack[len(stack)-1].item {
				return nil, nil, ErrInvalidRecord
			}
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if frame.wanted {
				appendElement(directoryValueElement(frame.tag, core.VRSQ, core.SequenceValue{Items: frame.items}))
			}
		case parser.TokenElement:
			include := directory || wanted[tok.Header.Tag]
			if len(stack) > 0 {
				include = stack[len(stack)-1].wanted
			}
			if include {
				appendElement(tok.Element)
			}
		default:
			return nil, nil, ErrInvalidRecord
		}
	}
	if len(stack) != 0 {
		return nil, nil, ErrInvalidRecord
	}
	obj := object.FromDataSet(core.DataSet{Elements: elements}, std.Dictionary)
	obj.SetValueByteOrder(header.TransferSyntax.ByteOrder)
	return &header, obj, nil
}
