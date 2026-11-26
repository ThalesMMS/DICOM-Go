package object

import (
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// Patient identification with the assigning authority (#1165). A Patient ID
// (0010,0020) is unique only within the authority that issued it, PS3.3
// C.7.1.1 (Patient Identification Module) and C.7.1.4 (Other Patient IDs
// Sequence). The issuer follows the HL7 v2 HD data type: a local namespace
// in Issuer of Patient ID (0010,0021) and, in the single item of Issuer of
// Patient ID Qualifiers Sequence (0010,0024), a Universal Entity ID
// (0040,0032) with its Universal Entity ID Type (0040,0033).

var (
	tagPatientIDValue                 = core.NewTag(0x0010, 0x0020)
	tagIssuerOfPatientID              = core.NewTag(0x0010, 0x0021)
	tagTypeOfPatientID                = core.NewTag(0x0010, 0x0022)
	tagIssuerOfPatientIDQualifiersSeq = core.NewTag(0x0010, 0x0024)
	tagOtherPatientIDsSequence        = core.NewTag(0x0010, 0x1002)
	tagIssuerQualifierUniversalID     = core.NewTag(0x0040, 0x0032)
	tagIssuerQualifierUniversalIDType = core.NewTag(0x0040, 0x0033)
)

// HL7 v2 HD encoding used by Issuer.String and ParseIssuer.
const (
	issuerHDSeparator        = "&"
	issuerHDEscapedSeparator = `\T\`
	issuerHDEscape           = `\`
	issuerHDEscapedEscape    = `\E\`
	issuerHDComponents       = 3
)

// Issuer is the assigning authority of an identifier, the HL7 v2 HD data
// type as DICOM encodes it. The zero Issuer means the issuer is unknown.
type Issuer struct {
	// LocalNamespaceEntityID is the local name of the authority, Issuer of
	// Patient ID (0010,0021) for a Patient ID.
	LocalNamespaceEntityID string
	// UniversalEntityID is the globally unique identifier of the authority,
	// Universal Entity ID (0040,0032), for example an ISO OID.
	UniversalEntityID string
	// UniversalEntityIDType is the scheme of UniversalEntityID, Universal
	// Entity ID Type (0040,0033), for example ISO, DNS or UUID.
	UniversalEntityIDType string
}

// IsZero reports whether the issuer carries no namespace and no universal
// identifier.
func (i Issuer) IsZero() bool {
	i = i.normalized()
	return i.LocalNamespaceEntityID == "" && i.UniversalEntityID == ""
}

func (i Issuer) normalized() Issuer {
	return Issuer{
		LocalNamespaceEntityID: strings.TrimSpace(i.LocalNamespaceEntityID),
		UniversalEntityID:      strings.TrimSpace(i.UniversalEntityID),
		UniversalEntityIDType:  strings.TrimSpace(i.UniversalEntityIDType),
	}
}

// Matches reports whether two issuers can name the same authority. An issuer
// present on both sides must agree: when both carry a Universal Entity ID the
// universal identifiers decide (with their types when both declare one);
// otherwise, when both carry a local namespace, the namespaces decide. A side
// without an issuer matches any issuer. Two issuers that share no comparable
// component do not match, so identifiers from authorities that cannot be told
// apart stay distinct.
func (i Issuer) Matches(other Issuer) bool {
	left, right := i.normalized(), other.normalized()
	if left.IsZero() || right.IsZero() {
		return true
	}
	if left.UniversalEntityID != "" && right.UniversalEntityID != "" {
		if left.UniversalEntityID != right.UniversalEntityID {
			return false
		}
		return left.UniversalEntityIDType == "" || right.UniversalEntityIDType == "" ||
			left.UniversalEntityIDType == right.UniversalEntityIDType
	}
	if left.LocalNamespaceEntityID != "" && right.LocalNamespaceEntityID != "" {
		return left.LocalNamespaceEntityID == right.LocalNamespaceEntityID
	}
	return false
}

// String encodes the issuer as an HL7 v2 HD value: the namespace alone, or
// namespace&universalID&universalIDType when a universal identifier is
// present. "&" and "\" inside a component are written as the HL7 escapes
// \T\ and \E\. The zero Issuer encodes as "". ParseIssuer reverses it.
func (i Issuer) String() string {
	i = i.normalized()
	if i.IsZero() {
		return ""
	}
	if i.UniversalEntityID == "" {
		return escapeIssuerComponent(i.LocalNamespaceEntityID)
	}
	return strings.Join([]string{
		escapeIssuerComponent(i.LocalNamespaceEntityID),
		escapeIssuerComponent(i.UniversalEntityID),
		escapeIssuerComponent(i.UniversalEntityIDType),
	}, issuerHDSeparator)
}

// ParseIssuer decodes an HL7 v2 HD value written by Issuer.String.
// Components beyond the third are ignored.
func ParseIssuer(value string) Issuer {
	value = strings.TrimSpace(value)
	if value == "" {
		return Issuer{}
	}
	parts := strings.SplitN(value, issuerHDSeparator, issuerHDComponents+1)
	component := func(index int) string {
		if index >= len(parts) || index >= issuerHDComponents {
			return ""
		}
		return unescapeIssuerComponent(parts[index])
	}
	return Issuer{
		LocalNamespaceEntityID: component(0),
		UniversalEntityID:      component(1),
		UniversalEntityIDType:  component(2),
	}.normalized()
}

func escapeIssuerComponent(value string) string {
	value = strings.ReplaceAll(value, issuerHDEscape, issuerHDEscapedEscape)
	return strings.ReplaceAll(value, issuerHDSeparator, issuerHDEscapedSeparator)
}

func unescapeIssuerComponent(value string) string {
	if !strings.Contains(value, issuerHDEscape) {
		return value
	}
	var out strings.Builder
	for len(value) > 0 {
		switch {
		case strings.HasPrefix(value, issuerHDEscapedSeparator):
			out.WriteString(issuerHDSeparator)
			value = value[len(issuerHDEscapedSeparator):]
		case strings.HasPrefix(value, issuerHDEscapedEscape):
			out.WriteString(issuerHDEscape)
			value = value[len(issuerHDEscapedEscape):]
		default:
			out.WriteByte(value[0])
			value = value[1:]
		}
	}
	return out.String()
}

// IDWithIssuer is a patient identifier together with its assigning
// authority.
type IDWithIssuer struct {
	// ID is the Patient ID (0010,0020).
	ID string
	// TypeOfPatientID is Type of Patient ID (0010,0022): TEXT, RFID or
	// BARCODE. It describes the medium of the identifier and does not take
	// part in matching.
	TypeOfPatientID string
	// Issuer is the authority that assigned ID.
	Issuer Issuer
}

// Matches reports whether two identifiers can name the same patient: equal
// non-empty IDs (leading and trailing spaces are not significant) whose
// issuers match (see Issuer.Matches).
func (p IDWithIssuer) Matches(other IDWithIssuer) bool {
	id := strings.TrimSpace(p.ID)
	return id != "" && id == strings.TrimSpace(other.ID) && p.Issuer.Matches(other.Issuer)
}

// String renders the identifier as "ID" or "ID (issuer)" for display.
func (p IDWithIssuer) String() string {
	id := strings.TrimSpace(p.ID)
	if issuer := p.Issuer.String(); issuer != "" {
		return id + " (" + issuer + ")"
	}
	return id
}

// PatientIDWithIssuer returns the Patient ID (0010,0020) of a data set with
// its Issuer of Patient ID (0010,0021), Issuer of Patient ID Qualifiers
// Sequence (0010,0024) and Type of Patient ID (0010,0022). It reports false
// when the data set has no non-empty Patient ID.
func PatientIDWithIssuer(o *Object) (IDWithIssuer, bool) {
	if o == nil {
		return IDWithIssuer{}, false
	}
	return idWithIssuerFrom(o)
}

// AllPatientIDsWithIssuer returns every patient identifier of a data set:
// the Patient ID first, then each item of Other Patient IDs Sequence
// (0010,1002) in order, each with its own issuer. Items without a Patient ID
// and exact duplicates are skipped. The retired Other Patient IDs (0010,1000)
// carries no issuer and is not read.
func AllPatientIDsWithIssuer(o *Object) []IDWithIssuer {
	if o == nil {
		return nil
	}
	var ids []IDWithIssuer
	add := func(candidate IDWithIssuer) {
		for _, existing := range ids {
			if existing == candidate {
				return
			}
		}
		ids = append(ids, candidate)
	}
	if primary, ok := idWithIssuerFrom(o); ok {
		add(primary)
	}
	items, _ := o.GetSequence(tagOtherPatientIDsSequence)
	for _, item := range items {
		if other, ok := idWithIssuerFrom(item); ok {
			add(other)
		}
	}
	return ids
}

// AnyPatientIDMatches reports whether some identifier of left matches some
// identifier of right (see IDWithIssuer.Matches), for example the
// AllPatientIDsWithIssuer lists of two data sets.
func AnyPatientIDMatches(left, right []IDWithIssuer) bool {
	for _, a := range left {
		for _, b := range right {
			if a.Matches(b) {
				return true
			}
		}
	}
	return false
}

func idWithIssuerFrom(o *Object) (IDWithIssuer, bool) {
	id := trimmedString(o, tagPatientIDValue)
	if id == "" {
		return IDWithIssuer{}, false
	}
	issuer := Issuer{LocalNamespaceEntityID: trimmedString(o, tagIssuerOfPatientID)}
	if qualifiers, ok := o.GetSequence(tagIssuerOfPatientIDQualifiersSeq); ok && len(qualifiers) > 0 && qualifiers[0] != nil {
		issuer.UniversalEntityID = trimmedString(qualifiers[0], tagIssuerQualifierUniversalID)
		issuer.UniversalEntityIDType = trimmedString(qualifiers[0], tagIssuerQualifierUniversalIDType)
	}
	return IDWithIssuer{
		ID:              id,
		TypeOfPatientID: trimmedString(o, tagTypeOfPatientID),
		Issuer:          issuer.normalized(),
	}, true
}
