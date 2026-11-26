package object

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
)

// InheritModules selects the modules InheritCommonModules writes.
type InheritModules uint16

const (
	// InheritPatient copies the Patient and Patient Study modules (PS3.3
	// C.7.1.1, C.7.2.2).
	InheritPatient InheritModules = 1 << iota
	// InheritClinicalTrial copies the Clinical Trial Subject and Study
	// modules (C.7.1.3, C.7.2.3).
	InheritClinicalTrial
	// InheritStudy copies the General Study module (C.7.2.1).
	InheritStudy
	// InheritFrameOfReference copies the Frame of Reference module (C.7.4.1)
	// when the source has one.
	InheritFrameOfReference
	// InheritEquipment writes the General and Enhanced General Equipment
	// attributes of the software creating the object (C.7.5.1, C.7.5.2),
	// never the source's equipment.
	InheritEquipment
	// InheritInstanceCreation writes Instance Creation Date and Time (and the
	// source's Timezone Offset From UTC, in which they are expressed).
	InheritInstanceCreation

	// InheritAll selects every module; it is what the zero value means.
	InheritAll = InheritPatient | InheritClinicalTrial | InheritStudy | InheritFrameOfReference | InheritEquipment | InheritInstanceCreation
)

// Equipment identifies the software that creates a derived object.
type Equipment struct {
	Manufacturer, ModelName, DeviceSerialNumber, SoftwareVersions string
}

// DefaultEquipment identifies dicom-go, with its module version when the
// build records one.
func DefaultEquipment() Equipment {
	version := "dicom-go"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, module := range append([]*debug.Module{&info.Main}, info.Deps...) {
			if module != nil && module.Path == "github.com/ThalesMMS/dicom-go" && module.Version != "" && module.Version != "(devel)" {
				version = "dicom-go " + module.Version
			}
		}
	}
	return Equipment{Manufacturer: "dicom-go", ModelName: "dicom-go", DeviceSerialNumber: "dicom-go", SoftwareVersions: version}
}

// InheritOptions configures InheritCommonModules.
type InheritOptions struct {
	// Modules selects what is written; zero means InheritAll.
	Modules InheritModules
	// Equipment identifies the creating software; nil uses DefaultEquipment.
	Equipment *Equipment
	// Overwrite replaces attributes dst already has; by default dst keeps
	// them (a writer's own Study Instance UID, for example).
	Overwrite bool
	// Now is the instance creation time; zero uses time.Now.
	Now time.Time
}

func inheritTagList(values ...[2]uint16) []core.Tag {
	out := make([]core.Tag, len(values))
	for i, v := range values {
		out[i] = core.NewTag(v[0], v[1])
	}
	return out
}

var (
	inheritPatientTags = inheritTagList(
		[2]uint16{0x0008, 0x1120}, // Referenced Patient Sequence
		[2]uint16{0x0010, 0x0010}, [2]uint16{0x0010, 0x0020}, [2]uint16{0x0010, 0x0021}, [2]uint16{0x0010, 0x0022},
		[2]uint16{0x0010, 0x0024}, [2]uint16{0x0010, 0x0026}, [2]uint16{0x0010, 0x0027}, [2]uint16{0x0010, 0x0028},
		[2]uint16{0x0010, 0x0030}, [2]uint16{0x0010, 0x0032}, [2]uint16{0x0010, 0x0033}, [2]uint16{0x0010, 0x0034},
		[2]uint16{0x0010, 0x0035}, [2]uint16{0x0010, 0x0040}, [2]uint16{0x0010, 0x0200}, [2]uint16{0x0010, 0x0212},
		[2]uint16{0x0010, 0x0213}, [2]uint16{0x0010, 0x0214}, [2]uint16{0x0010, 0x0215}, [2]uint16{0x0010, 0x0216},
		[2]uint16{0x0010, 0x0217}, [2]uint16{0x0010, 0x0218}, [2]uint16{0x0010, 0x0219}, [2]uint16{0x0010, 0x0221},
		[2]uint16{0x0010, 0x0222}, [2]uint16{0x0010, 0x0223}, [2]uint16{0x0010, 0x0229},
		[2]uint16{0x0010, 0x1000}, [2]uint16{0x0010, 0x1001}, [2]uint16{0x0010, 0x1002}, [2]uint16{0x0010, 0x1100},
		[2]uint16{0x0010, 0x2160}, [2]uint16{0x0010, 0x2201}, [2]uint16{0x0010, 0x2202}, [2]uint16{0x0010, 0x2203},
		[2]uint16{0x0010, 0x2292}, [2]uint16{0x0010, 0x2293}, [2]uint16{0x0010, 0x2294}, [2]uint16{0x0010, 0x2295},
		[2]uint16{0x0010, 0x2296}, [2]uint16{0x0010, 0x2297}, [2]uint16{0x0010, 0x2298}, [2]uint16{0x0010, 0x2299},
		[2]uint16{0x0010, 0x4000}, [2]uint16{0x0012, 0x0062}, [2]uint16{0x0012, 0x0063}, [2]uint16{0x0012, 0x0064},
		// Patient Study
		[2]uint16{0x0008, 0x1080}, [2]uint16{0x0008, 0x1084}, [2]uint16{0x0010, 0x1010}, [2]uint16{0x0010, 0x1020},
		[2]uint16{0x0010, 0x1021}, [2]uint16{0x0010, 0x1022}, [2]uint16{0x0010, 0x1023}, [2]uint16{0x0010, 0x1024},
		[2]uint16{0x0010, 0x1030}, [2]uint16{0x0010, 0x2000}, [2]uint16{0x0010, 0x2110}, [2]uint16{0x0010, 0x2180},
		[2]uint16{0x0010, 0x21a0}, [2]uint16{0x0010, 0x21b0}, [2]uint16{0x0010, 0x21c0}, [2]uint16{0x0010, 0x21d0},
		[2]uint16{0x0038, 0x0010}, [2]uint16{0x0038, 0x0014}, [2]uint16{0x0038, 0x0060}, [2]uint16{0x0038, 0x0062},
		[2]uint16{0x0038, 0x0064}, [2]uint16{0x0038, 0x0500}, [2]uint16{0x0040, 0x3001},
	)
	inheritClinicalTrialTags = inheritTagList(
		[2]uint16{0x0012, 0x0010}, [2]uint16{0x0012, 0x0020}, [2]uint16{0x0012, 0x0021}, [2]uint16{0x0012, 0x0022},
		[2]uint16{0x0012, 0x0023}, [2]uint16{0x0012, 0x0030}, [2]uint16{0x0012, 0x0031}, [2]uint16{0x0012, 0x0040},
		[2]uint16{0x0012, 0x0042}, [2]uint16{0x0012, 0x0081}, [2]uint16{0x0012, 0x0082},
		[2]uint16{0x0012, 0x0050}, [2]uint16{0x0012, 0x0051}, [2]uint16{0x0012, 0x0052}, [2]uint16{0x0012, 0x0053},
		[2]uint16{0x0012, 0x0083},
	)
	inheritStudyTags = inheritTagList(
		[2]uint16{0x0008, 0x0020}, [2]uint16{0x0008, 0x0030}, [2]uint16{0x0008, 0x0050}, [2]uint16{0x0008, 0x0051},
		[2]uint16{0x0008, 0x0090}, [2]uint16{0x0008, 0x0096}, [2]uint16{0x0008, 0x009c}, [2]uint16{0x0008, 0x009d},
		[2]uint16{0x0008, 0x1030}, [2]uint16{0x0008, 0x1032}, [2]uint16{0x0008, 0x1048}, [2]uint16{0x0008, 0x1049},
		[2]uint16{0x0008, 0x1060}, [2]uint16{0x0008, 0x1062}, [2]uint16{0x0008, 0x1110}, [2]uint16{0x0020, 0x000d},
		[2]uint16{0x0020, 0x0010}, [2]uint16{0x0032, 0x1033}, [2]uint16{0x0032, 0x1034}, [2]uint16{0x0032, 0x1066},
		[2]uint16{0x0032, 0x1067}, [2]uint16{0x0040, 0x1012},
	)
	inheritFrameOfReferenceTags = inheritTagList([2]uint16{0x0020, 0x0052}, [2]uint16{0x0020, 0x1040})

	tagInheritCharacterSet    = core.NewTag(0x0008, 0x0005)
	tagInstanceCreationDate   = core.NewTag(0x0008, 0x0012)
	tagInstanceCreationTime   = core.NewTag(0x0008, 0x0013)
	tagTimezoneOffsetFromUTC  = core.NewTag(0x0008, 0x0201)
	tagInheritStudyUID        = core.NewTag(0x0020, 0x000d)
	tagInheritFrameOfRefUID   = core.NewTag(0x0020, 0x0052)
	tagInheritManufacturer    = core.NewTag(0x0008, 0x0070)
	tagInheritModelName       = core.NewTag(0x0008, 0x1090)
	tagInheritDeviceSerial    = core.NewTag(0x0018, 0x1000)
	tagInheritSoftwareVersion = core.NewTag(0x0018, 0x1020)

	// Type 2 attributes written empty when neither object has them.
	inheritType2 = map[InheritModules][]struct {
		tag core.Tag
		vr  core.VR
	}{
		InheritPatient: {{core.NewTag(0x0010, 0x0010), core.VRPN}, {core.NewTag(0x0010, 0x0020), core.VRLO}, {core.NewTag(0x0010, 0x0030), core.VRDA}, {core.NewTag(0x0010, 0x0040), core.VRCS}},
		InheritStudy: {{core.NewTag(0x0008, 0x0020), core.VRDA}, {core.NewTag(0x0008, 0x0030), core.VRTM}, {core.NewTag(0x0008, 0x0050), core.VRSH},
			{core.NewTag(0x0008, 0x0090), core.VRPN}, {core.NewTag(0x0020, 0x0010), core.VRSH}},
	}
)

// InheritCommonModules writes into dst the modules that tie a derived object
// to its patient and study, so that an archive files it with its source: the
// Patient, Clinical Trial and General Study attributes and the Frame of
// Reference copied from src, the equipment of the creating software, and the
// instance creation date and time. Type 2 attributes neither object has are
// written empty, and a Study Instance UID is generated when neither has one.
// src may be nil, which writes only those defaults. Attributes dst already
// has are kept unless opts.Overwrite; copied values are deep copies. Text is
// copied decoded from src's Specific Character Set, so dst encodes it in its
// own when written; a dst without a declaration takes src's.
func InheritCommonModules(dst, src *Object, opts InheritOptions) error {
	if dst == nil {
		return fmt.Errorf("dicom: nil destination")
	}
	modules := opts.Modules
	if modules == 0 {
		modules = InheritAll
	}
	if src != nil && modules&(InheritPatient|InheritClinicalTrial|InheritStudy) != 0 && !dst.Has(tagInheritCharacterSet) {
		if element, ok := src.Get(tagInheritCharacterSet); ok {
			copied, err := src.detachedElementForOrder(element, dst.ValueByteOrder())
			if err != nil {
				return err
			}
			dst.Put(copied)
		}
	}
	for _, group := range []struct {
		module InheritModules
		tags   []core.Tag
	}{
		{InheritPatient, inheritPatientTags},
		{InheritClinicalTrial, inheritClinicalTrialTags},
		{InheritStudy, inheritStudyTags},
		{InheritFrameOfReference, inheritFrameOfReferenceTags},
	} {
		if modules&group.module == 0 || src == nil {
			continue
		}
		if group.module == InheritFrameOfReference && !src.Has(tagInheritFrameOfRefUID) {
			continue
		}
		for _, tag := range group.tags {
			if dst.Has(tag) && !opts.Overwrite {
				continue
			}
			element, ok := src.Get(tag)
			if !ok {
				continue
			}
			copied, err := src.detachedElementForOrder(element, dst.ValueByteOrder())
			if err != nil {
				return err
			}
			dst.Put(copied)
		}
	}
	for module, attributes := range inheritType2 {
		if modules&module == 0 {
			continue
		}
		for _, a := range attributes {
			if !dst.Has(a.tag) {
				dst.Put(core.NewRawElement(a.tag, a.vr, nil))
			}
		}
	}
	if modules&InheritFrameOfReference != 0 && dst.Has(tagInheritFrameOfRefUID) && !dst.Has(core.NewTag(0x0020, 0x1040)) {
		dst.Put(core.NewRawElement(core.NewTag(0x0020, 0x1040), core.VRLO, nil))
	}
	if modules&InheritStudy != 0 && !dst.Has(tagInheritStudyUID) {
		uid, err := newInheritUID()
		if err != nil {
			return err
		}
		dst.Put(core.Element{Header: core.ElementHeader{Tag: tagInheritStudyUID, VR: core.VRUI}, Value: core.StringValue{uid}})
	}
	if modules&InheritEquipment != 0 {
		equipment := DefaultEquipment()
		if opts.Equipment != nil {
			equipment = *opts.Equipment
		}
		for _, e := range []struct {
			tag   core.Tag
			vr    core.VR
			value string
		}{
			{tagInheritManufacturer, core.VRLO, equipment.Manufacturer},
			{tagInheritModelName, core.VRLO, equipment.ModelName},
			{tagInheritDeviceSerial, core.VRLO, equipment.DeviceSerialNumber},
			{tagInheritSoftwareVersion, core.VRLO, equipment.SoftwareVersions},
		} {
			if value := strings.TrimSpace(e.value); value != "" && (opts.Overwrite || !dst.Has(e.tag)) {
				dst.Put(core.Element{Header: core.ElementHeader{Tag: e.tag, VR: e.vr}, Value: core.StringValue{value}})
			}
		}
	}
	if modules&InheritInstanceCreation != 0 && (opts.Overwrite || !dst.Has(tagInstanceCreationDate)) {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		if src != nil && !dst.Has(tagTimezoneOffsetFromUTC) {
			if element, ok := src.Get(tagTimezoneOffsetFromUTC); ok {
				if copied, err := src.detachedElementForOrder(element, dst.ValueByteOrder()); err == nil {
					dst.Put(copied)
				}
			}
		}
		if offset, ok := dst.GetString(tagTimezoneOffsetFromUTC); ok {
			if zone, err := time.Parse("-0700", strings.TrimSpace(offset)); err == nil {
				now = now.In(zone.Location())
			}
		}
		dst.Put(core.Element{Header: core.ElementHeader{Tag: tagInstanceCreationDate, VR: core.VRDA}, Value: core.StringValue{now.Format("20060102")}})
		dst.Put(core.Element{Header: core.ElementHeader{Tag: tagInstanceCreationTime, VR: core.VRTM}, Value: core.StringValue{now.Format("150405")}})
	}
	return nil
}

// detachedElementForOrder returns a deep copy of element that no longer shares
// memory with o or reads through o's source. Text values are decoded from
// o's Specific Character Set (a value that does not decode is copied as
// bytes), and sequence items are copied the same way. Raw fixed-width numeric
// units are copied in the destination's value byte order; typed values keep
// their representation and are encoded by the destination's writer.
func (o *Object) detachedElementForOrder(element core.Element, target binary.ByteOrder) (core.Element, error) {
	return o.detachedElementWithSource(element, target, o)
}

func (o *Object) detachedElementWithSource(element core.Element, target binary.ByteOrder, root *Object) (core.Element, error) {
	switch {
	case element.VR() == core.VRSQ || element.VR() == core.VRUN && isSequenceValue(element.Value):
		items, ok := o.GetSequence(element.Tag())
		if !ok {
			break
		}
		out := make([]core.DataSet, len(items))
		for i, item := range items {
			for _, e := range item.Elements() {
				copied, err := item.detachedElementWithSource(e, target, root)
				if err != nil {
					return core.Element{}, err
				}
				out[i].Elements = append(out[i].Elements, copied)
			}
		}
		element.Value = core.SequenceValue{Items: out}
		return element, nil
	}
	if element.Value == nil && element.Header.HasLength() && element.EncodedLength() != 0 {
		var value bytes.Buffer
		var err error
		if o == root {
			_, err = root.CopyValueTo(element.Tag(), &value)
		} else {
			_, err = root.CopyNestedValueTo(o, element.Tag(), &value)
		}
		if err != nil {
			return core.Element{}, err
		}
		element.Value = core.RawValue(value.Bytes())
	}
	// Deferred text must be available before decoding it in the source scope's
	// charset; copying its raw bytes would reinterpret them in dst's charset.
	if element.VR().IsStringLike() && element.Tag() != tagInheritCharacterSet {
		if values, err := o.decodeTextValues(element); err == nil {
			values = append([]string(nil), values...)
			if element.VR() != core.VRUI {
				values = TrimNULPadding(values)
			}
			element.Value = core.StringValue(values)
			element.Header.Length, element.Header.LengthSet = 0, false
			return element, nil
		}
	}
	if raw, ok := element.Value.(core.RawValue); ok && o.ValueByteOrder() != target {
		if width := valueencode.ByteOrderWidth(element.VR()); width > 1 {
			if len(raw)%width != 0 {
				return core.Element{}, fmt.Errorf("dicom: inherited %s with VR %s has incomplete %d-byte numeric unit", element.Tag(), element.VR(), width)
			}
			element.Value = core.RawValue(valueencode.ReverseFixedWidth(raw, width))
			return element, nil
		}
	}
	element.Value = cloneInheritedValue(element.Value)
	return element, nil
}

// TrimNULPadding removes trailing NUL and space padding from text values.
// PS3.5 6.2 pads every text VR but UI with a space; sources that pad with
// NUL write a value dciodvfy rejects, so a derived object copying the value
// drops that padding and the writer pads it correctly.
func TrimNULPadding(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		if strings.HasSuffix(v, "\x00") {
			v = strings.TrimRight(v, " \x00")
		}
		out[i] = v
	}
	return out
}

func isSequenceValue(value core.Value) bool {
	_, ok := value.(core.SequenceValue)
	return ok
}

func cloneInheritedValue(value core.Value) core.Value {
	switch v := value.(type) {
	case core.RawValue:
		return core.RawValue(core.CloneBytes(v))
	case core.StringValue:
		return append(core.StringValue(nil), v...)
	case core.SequenceValue:
		items := make([]core.DataSet, len(v.Items))
		for i, item := range v.Items {
			elements := make([]core.Element, len(item.Elements))
			for j, e := range item.Elements {
				e.Value = cloneInheritedValue(e.Value)
				elements[j] = e
			}
			items[i] = core.DataSet{Elements: elements}
		}
		return core.SequenceValue{Items: items}
	case core.Uint16Value:
		return append(core.Uint16Value(nil), v...)
	case core.Int16Value:
		return append(core.Int16Value(nil), v...)
	case core.Uint32Value:
		return append(core.Uint32Value(nil), v...)
	case core.Int32Value:
		return append(core.Int32Value(nil), v...)
	case core.Uint64Value:
		return append(core.Uint64Value(nil), v...)
	case core.Int64Value:
		return append(core.Int64Value(nil), v...)
	case core.TagValue:
		return append(core.TagValue(nil), v...)
	case core.FragmentSequence:
		out := core.FragmentSequence{OffsetTable: core.CloneBytes(v.OffsetTable)}
		if v.Fragments != nil {
			out.Fragments = make([][]byte, len(v.Fragments))
			for index, fragment := range v.Fragments {
				out.Fragments[index] = core.CloneBytes(fragment)
			}
		}
		return out
	case core.Float32Value:
		return append(core.Float32Value(nil), v...)
	case core.Float64Value:
		return append(core.Float64Value(nil), v...)
	default:
		return value
	}
}

func newInheritUID() (string, error) {
	return core.NewUID()
}

// DerivedCommon carries what a writer of a derived object needs for the
// common modules: an instance of the patient and study, and the creating
// software. Its zero value writes only the conformant defaults.
type DerivedCommon struct {
	// Source is an instance whose Patient, Clinical Trial, Study and Frame
	// of Reference modules the derived object inherits; nil copies nothing.
	Source *Object
	// Equipment identifies the creating software; nil uses DefaultEquipment.
	Equipment *Equipment
	// Now is the Instance Creation Date and Time; zero uses the current
	// time. Set it for byte-reproducible output.
	Now time.Time
}

// Apply writes the selected modules into dst (InheritCommonModules); what
// dst already has, such as the writer's own UIDs, is kept.
func (c DerivedCommon) Apply(dst *Object, modules InheritModules) error {
	return InheritCommonModules(dst, c.Source, InheritOptions{Modules: modules, Equipment: c.Equipment, Now: c.Now})
}

// InheritWithoutFrameOfReference is InheritAll for IODs without a Frame of
// Reference module (presentation states, structure sets, SR, encapsulated
// documents, Secondary Capture).
const InheritWithoutFrameOfReference = InheritAll &^ InheritFrameOfReference
