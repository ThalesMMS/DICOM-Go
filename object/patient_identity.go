package object

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// Patient identity correction (#1206): an archive that merges studies or
// unifies the patient identity of studies rewrites the patient
// identification attributes of every object it moves under the identity of
// a chosen source object.

var (
	tagPatientsName      = core.NewTag(0x0010, 0x0010)
	tagPatientsBirthDate = core.NewTag(0x0010, 0x0030)
	tagPatientsSex       = core.NewTag(0x0010, 0x0040)
)

// PatientIdentityTags lists, in tag order, the attributes CopyPatientIdentity
// replaces: Patient's Name, Patient ID, Issuer of Patient ID, Type of Patient
// ID, Issuer of Patient ID Qualifiers Sequence, Patient's Birth Date and
// Patient's Sex.
var PatientIdentityTags = []core.Tag{
	tagPatientsName, tagPatientIDValue, tagIssuerOfPatientID, tagTypeOfPatientID,
	tagIssuerOfPatientIDQualifiersSeq, tagPatientsBirthDate, tagPatientsSex,
}

// patientIdentityType2 are the Type 2 attributes of the Patient Module
// (PS3.3 C.7.1.1): they stay present, empty when the source has no value.
var patientIdentityType2 = map[core.Tag]bool{
	tagPatientsName: true, tagPatientIDValue: true, tagPatientsBirthDate: true, tagPatientsSex: true,
}

// ErrPatientIdentityCharacterSet reports a patient identity whose text the
// target data set cannot carry: the source and target declare different
// Specific Character Sets and the copied values are not plain ASCII.
var ErrPatientIdentityCharacterSet = errors.New("dicom: patient identity uses a character set the target does not declare")

// CopyPatientIdentity replaces the patient identification attributes of a
// data set (PatientIdentityTags) with those of Source:
//
//   - an attribute Source has is copied;
//   - a Type 2 attribute Source lacks is written empty when the data set has
//     a value, so no stale value survives;
//   - Issuer of Patient ID and Type of Patient ID that Source lacks are
//     removed, since they describe the replaced Patient ID;
//   - Issuer of Patient ID Qualifiers Sequence that Source lacks is kept
//     only when Issuer of Patient ID does not change; otherwise it would
//     declare a second authority for the new identifier.
//
// The copy is refused with ErrPatientIdentityCharacterSet when Source and the
// data set declare different Specific Character Sets and a copied text value
// is not plain ASCII.
type CopyPatientIdentity struct {
	Source *Object
}

// Coerce implements AttributeCoercion.
func (c CopyPatientIdentity) Coerce(obj *Object) ([]core.Element, error) {
	if obj == nil || c.Source == nil {
		return nil, fmt.Errorf("dicom: nil object passed to CopyPatientIdentity")
	}
	if characterSetTerms(c.Source) != characterSetTerms(obj) {
		for _, tag := range PatientIdentityTags {
			if element, ok := c.Source.Get(tag); ok && elementHasNonASCIIText(element) {
				return nil, fmt.Errorf("%w: %s", ErrPatientIdentityCharacterSet, tag)
			}
		}
	}
	issuerBefore, _ := obj.GetString(tagIssuerOfPatientID)
	issuerAfter, _ := c.Source.GetString(tagIssuerOfPatientID)
	issuerChanges := strings.TrimSpace(issuerBefore) != strings.TrimSpace(issuerAfter)

	var modified []core.Element
	for _, tag := range PatientIdentityTags {
		existing, present := obj.Get(tag)
		if replacement, ok := c.Source.Get(tag); ok {
			if present && elementsEqual(existing, replacement) {
				continue
			}
			modified = append(modified, previousValue(existing, present, replacement))
			obj.Put(replacement)
			continue
		}
		if !present {
			continue
		}
		switch {
		case patientIdentityType2[tag]:
			if elementEmpty(existing) {
				continue
			}
			modified = append(modified, existing)
			obj.Put(previousValue(core.Element{}, false, existing))
		case tag == tagIssuerOfPatientIDQualifiersSeq && !issuerChanges:
			continue
		default:
			modified = append(modified, existing)
			obj.Remove(tag)
		}
	}
	return modified, nil
}

// characterSetTerms is the normalized Specific Character Set of obj; ""
// means the default repertoire.
func characterSetTerms(obj *Object) string {
	element, ok := obj.Get(tagSpecificCharacterSet)
	if !ok {
		return ""
	}
	values := element.StringValues()
	terms := make([]string, 0, len(values))
	for _, value := range values {
		terms = append(terms, strings.TrimSpace(value))
	}
	return strings.TrimRight(strings.Join(terms, `\`), `\`)
}

// elementHasNonASCIIText reports a text value, at any sequence depth, with a
// byte outside 7-bit ASCII or an escape sequence.
func elementHasNonASCIIText(element core.Element) bool {
	switch value := element.Value.(type) {
	case core.SequenceValue:
		for _, item := range value.Items {
			for _, child := range item.Elements {
				if elementHasNonASCIIText(child) {
					return true
				}
			}
		}
		return false
	case core.StringValue:
		for _, part := range value {
			if !isPlainASCII(part) {
				return true
			}
		}
		return false
	case core.RawValue:
		return !isPlainASCII(string(value))
	}
	return false
}

func isPlainASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 || value[i] == 0x1b {
			return false
		}
	}
	return true
}
