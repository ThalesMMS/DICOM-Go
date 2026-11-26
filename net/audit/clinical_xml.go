package audit

import "encoding/xml"

// Omit absent optional descriptions instead of serializing empty elements
// (PS3.15 A.5.2). Keep the public value structs for source compatibility.
func (o ParticipantObject) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	type wire struct {
		IDTypeCode  Code                          `xml:"ParticipantObjectIDTypeCode"`
		Name        string                        `xml:"ParticipantObjectName,omitempty"`
		Query       string                        `xml:"ParticipantObjectQuery,omitempty"`
		Details     []ParticipantObjectDetail     `xml:"ParticipantObjectDetail"`
		Description *ParticipantObjectDescription `xml:"ParticipantObjectDescription,omitempty"`
		ID          string                        `xml:"ParticipantObjectID,attr"`
		TypeCode    string                        `xml:"ParticipantObjectTypeCode,attr,omitempty"`
		Role        string                        `xml:"ParticipantObjectTypeCodeRole,attr,omitempty"`
		LifeCycle   string                        `xml:"ParticipantObjectDataLifeCycle,attr,omitempty"`
	}
	w := wire{IDTypeCode: o.IDTypeCode, Name: o.Name, Query: o.Query, Details: o.Details, ID: o.ID, TypeCode: o.TypeCode, Role: o.Role, LifeCycle: o.LifeCycle}
	d := o.Description
	if len(d.Accessions)+len(d.SOPClasses)+len(d.ContainsStudy.Studies) > 0 || d.Encrypted != nil || d.Anonymized != nil {
		w.Description = &d
	}
	return e.EncodeElement(w, start)
}

func (d ParticipantObjectDescription) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	type wire struct {
		Accessions    []Accession    `xml:"Accession"`
		SOPClasses    []SOPClass     `xml:"SOPClass"`
		ContainsStudy *ContainsStudy `xml:"ParticipantObjectContainsStudy,omitempty"`
		Encrypted     *bool          `xml:"Encrypted,omitempty"`
		Anonymized    *bool          `xml:"Anonymized,omitempty"`
	}
	w := wire{Accessions: d.Accessions, SOPClasses: d.SOPClasses, Encrypted: d.Encrypted, Anonymized: d.Anonymized}
	if len(d.ContainsStudy.Studies) > 0 {
		w.ContainsStudy = &d.ContainsStudy
	}
	return e.EncodeElement(w, start)
}
