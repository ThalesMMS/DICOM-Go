package hl7

import (
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	adtCharacterSet      = core.NewTag(0x0008, 0x0005)
	adtPatientID         = core.NewTag(0x0010, 0x0020)
	adtPatientIssuer     = core.NewTag(0x0010, 0x0021)
	adtPatientQualifiers = core.NewTag(0x0010, 0x0024)
	adtOtherPatientIDs   = core.NewTag(0x0010, 0x1002)
)

// DICOMAttributes returns an independent sparse patient attribute set in UTF-8
// (ISO_IR 192). primaryIndex explicitly selects the PID-3 identifier for Patient
// ID; all others become Other Patient IDs Sequence items. Name uses the first
// PID-5 repetition, consistent with the existing Patient model. Absent/empty
// demographics produce no element; HL7 null produces a zero-length element.
// Partial birth dates remain supported by the HL7 model but return an Error
// when emitted as a modern DICOM DA. The result is not a complete IOD and is
// not applied to a dataset; identity/issuer replacement is the caller's policy.
func (p Patient) DICOMAttributes(primaryIndex int) (*object.Object, error) {
	if primaryIndex < 0 || primaryIndex >= len(p.Identifiers) {
		return nil, adtError("PID", 3, ErrorCodeDataType, "patient identifier index out of range")
	}
	out, err := identifierAttributes(p.Identifiers[primaryIndex], "PID", 3)
	if err != nil {
		return nil, err
	}
	others := make([]*object.Object, 0, len(p.Identifiers)-1)
	for i, id := range p.Identifiers {
		if i == primaryIndex {
			continue
		}
		item, err := identifierAttributes(id, "PID", 3)
		if err != nil {
			return nil, err
		}
		item.Remove(adtCharacterSet) // Inherit the parent character set.
		others = append(others, item)
	}
	if len(others) > 0 {
		out.Put(object.SequenceElement(adtOtherPatientIDs, others...))
	}
	if p.NameHasReservedCharacters && !p.NameIsNull {
		return nil, adtError("PID", 5, ErrorCodeDataType, "name contains an unrepresentable DICOM PN separator")
	}
	if err := adtPutText(out, core.NewTag(0x0010, 0x0010), core.VRPN, p.Name, p.NameIsNull, "PID", 5); err != nil {
		return nil, err
	}
	if p.BirthDate != "" && !p.BirthDateIsNull {
		if len(p.BirthDate) != 8 {
			return nil, adtError("PID", 7, ErrorCodeDataType, "birth date cannot be represented as a full DICOM DA")
		}
		if _, err := dcmtime.ParseDate(p.BirthDate); err != nil {
			return nil, adtError("PID", 7, ErrorCodeDataType, "invalid birth date")
		}
	}
	if err := adtPutText(out, core.NewTag(0x0010, 0x0030), core.VRDA, p.BirthDate, p.BirthDateIsNull, "PID", 7); err != nil {
		return nil, err
	}
	// A supplied unknown sex is explicitly empty; absent or empty means no update.
	if p.Sex != "" || p.SexIsNull {
		value := p.DICOMSex()
		if err := adtPutText(out, core.NewTag(0x0010, 0x0040), core.VRCS, value, p.SexIsNull || value == "", "PID", 8); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func identifierAttributes(id PatientIdentifier, segment string, field int) (*object.Object, error) {
	if id.ID == "" || id.ID == Null {
		return nil, adtError(segment, field, ErrorCodeRequiredFieldMissing, "missing identifier")
	}
	out := adtUTF8Object()
	if err := adtPutText(out, adtPatientID, core.VRLO, id.ID, false, segment, field); err != nil {
		return nil, err
	}
	issuer := id.Issuer
	if (issuer.UniversalEntityID == "") != (issuer.UniversalEntityIDType == "") {
		return nil, adtError(segment, field, ErrorCodeDataType, "universal assigning authority requires both ID and type")
	}
	if err := adtPutText(out, adtPatientIssuer, core.VRLO, issuer.LocalNamespaceEntityID, false, segment, field); err != nil {
		return nil, err
	}
	if issuer.UniversalEntityID != "" || issuer.UniversalEntityIDType != "" || id.IdentifierTypeCode != "" {
		item := adtUTF8Object()
		for _, v := range []struct {
			tag   core.Tag
			vr    core.VR
			value string
		}{
			{core.NewTag(0x0040, 0x0032), core.VRUT, issuer.UniversalEntityID},
			{core.NewTag(0x0040, 0x0033), core.VRCS, issuer.UniversalEntityIDType},
			{core.NewTag(0x0040, 0x0035), core.VRCS, id.IdentifierTypeCode},
		} {
			if err := adtPutText(item, v.tag, v.vr, v.value, false, segment, field); err != nil {
				return nil, err
			}
		}
		item.Remove(adtCharacterSet)
		out.Put(object.SequenceElement(adtPatientQualifiers, item))
	}
	return out, nil
}

// DICOMAttributes returns independent sparse visit attributes in UTF-8. It maps
// PV1-19 to Admission ID and issuer, PV1-3's first three components joined by
// "/" to Current Patient Location, PV1-4 to Route of Admissions, PV1-8's first
// name to Referring Physician's Name, and PV1-44 to Admitting Date/Time.
// An explicit PV1-44 offset sets Timezone Offset From UTC; local wall-clock
// values and precision are preserved. That offset affects all DA/TM and implicit
// DT values when merged into another dataset: reconcile timezone semantics first.
// Class, hospital service, facility and discharge time stay in the typed Visit;
// no unsupported code translations or retired tags are invented.
func (v Visit) DICOMAttributes() (*object.Object, error) {
	out := adtUTF8Object()
	if v.NumberIsNull {
		if err := adtPutText(out, core.NewTag(0x0038, 0x0010), core.VRLO, "", true, "PV1", 19); err != nil {
			return nil, err
		}
		out.Put(object.SequenceElement(core.NewTag(0x0038, 0x0014)))
	} else if v.Number.ID != "" {
		if err := adtPutText(out, core.NewTag(0x0038, 0x0010), core.VRLO, v.Number.ID, false, "PV1", 19); err != nil {
			return nil, err
		}
		issuer := v.Number.Issuer
		if (issuer.UniversalEntityID == "") != (issuer.UniversalEntityIDType == "") {
			return nil, adtError("PV1", 19, ErrorCodeDataType, "universal assigning authority requires both ID and type")
		}
		if !issuer.IsZero() {
			item := adtUTF8Object()
			for _, entry := range []struct {
				tag   core.Tag
				vr    core.VR
				value string
			}{
				{core.NewTag(0x0040, 0x0031), core.VRUT, issuer.LocalNamespaceEntityID},
				{core.NewTag(0x0040, 0x0032), core.VRUT, issuer.UniversalEntityID},
				{core.NewTag(0x0040, 0x0033), core.VRCS, issuer.UniversalEntityIDType},
			} {
				if err := adtPutText(item, entry.tag, entry.vr, entry.value, false, "PV1", 19); err != nil {
					return nil, err
				}
			}
			item.Remove(adtCharacterSet)
			out.Put(object.SequenceElement(core.NewTag(0x0038, 0x0014), item))
		}
	}
	parts := []string{v.Location.PointOfCare, v.Location.Room, v.Location.Bed}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if err := adtPutText(out, core.NewTag(0x0038, 0x0300), core.VRLO, strings.Join(parts, "/"), v.LocationIsNull, "PV1", 3); err != nil {
		return nil, err
	}
	if err := adtPutText(out, core.NewTag(0x0038, 0x0016), core.VRLO, v.AdmissionType, v.AdmissionTypeIsNull, "PV1", 4); err != nil {
		return nil, err
	}
	if v.ReferringNameHasReservedCharacters && !v.ReferringPhysicianIsNull {
		return nil, adtError("PV1", 8, ErrorCodeDataType, "name contains an unrepresentable DICOM PN separator")
	}
	if err := adtPutText(out, core.NewTag(0x0008, 0x0090), core.VRPN, v.ReferringPhysician, v.ReferringPhysicianIsNull, "PV1", 8); err != nil {
		return nil, err
	}
	if v.AdmittedDateTimeIsNull {
		for _, entry := range []struct {
			tag core.Tag
			vr  core.VR
		}{{core.NewTag(0x0038, 0x0020), core.VRDA}, {core.NewTag(0x0038, 0x0021), core.VRTM}} {
			if err := adtPutText(out, entry.tag, entry.vr, "", true, "PV1", 44); err != nil {
				return nil, err
			}
		}
	} else if v.AdmittedDateTime != "" {
		value := v.AdmittedDateTime
		if _, err := dcmtime.ParseDatetime(value); err != nil {
			return nil, adtError("PV1", 44, ErrorCodeDataType, "invalid admission timestamp")
		}
		if len(value) >= 5 && (value[len(value)-5] == '+' || value[len(value)-5] == '-') {
			offset := value[len(value)-5:]
			value = value[:len(value)-5]
			if err := adtPutText(out, core.NewTag(0x0008, 0x0201), core.VRSH, offset, false, "PV1", 44); err != nil {
				return nil, err
			}
		}
		if len(value) < 8 {
			return nil, adtError("PV1", 44, ErrorCodeDataType, "admission date cannot be represented as a full DICOM DA")
		}
		if err := adtPutText(out, core.NewTag(0x0038, 0x0020), core.VRDA, value[:8], false, "PV1", 44); err != nil {
			return nil, err
		}
		if err := adtPutText(out, core.NewTag(0x0038, 0x0021), core.VRTM, value[8:], false, "PV1", 44); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func adtUTF8Object() *object.Object {
	out := object.New(nil)
	out.Put(core.Element{Header: core.ElementHeader{Tag: adtCharacterSet, VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}})
	return out
}

func adtPutText(out *object.Object, tag core.Tag, vr core.VR, value string, clear bool, segment string, field int) error {
	if clear {
		value = ""
	} else if value == "" {
		return nil
	}
	if strings.Contains(value, "\\") {
		return adtError(segment, field, ErrorCodeDataType, "value contains a DICOM value separator")
	}
	if err := out.SetStrings(tag, vr, value); err != nil {
		// Do not expose shared validator messages that might include patient data.
		return adtError(segment, field, ErrorCodeDataType, "value cannot be represented by the DICOM attribute VR")
	}
	return nil
}

// DICOMCurrentAttributes maps only the corrected identifiers in an A47 to a
// sparse DICOM patient identity set. Prior remains available for explicit
// lookup/reconciliation; it is never written as an additional current ID.
func (c PatientIdentifierChange) DICOMCurrentAttributes(primaryIndex int) (*object.Object, error) {
	return (Patient{Identifiers: c.Current}).DICOMAttributes(primaryIndex)
}
