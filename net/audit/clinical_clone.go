package audit

// cloneClinicalMessage detaches mutable fields before a caller-owned sink sees
// an envelope, so a failed sink cannot mutate the retained retry payload.
func cloneClinicalMessage(m Message) Message {
	m.Event.TypeCodes = append([]Code(nil), m.Event.TypeCodes...)
	m.Sources = append([]AuditSource(nil), m.Sources...)
	for i := range m.Sources {
		m.Sources[i].TypeCodes = append([]Code(nil), m.Sources[i].TypeCodes...)
	}
	m.Participants = append([]ActiveParticipant(nil), m.Participants...)
	for i := range m.Participants {
		a := &m.Participants[i]
		a.RoleIDCodes = append([]Code(nil), a.RoleIDCodes...)
		if a.MediaIdentifier != nil {
			v := *a.MediaIdentifier
			a.MediaIdentifier = &v
		}
	}
	m.Objects = append([]ParticipantObject(nil), m.Objects...)
	for i := range m.Objects {
		o := &m.Objects[i]
		o.Details = append([]ParticipantObjectDetail(nil), o.Details...)
		d := &o.Description
		d.Accessions = append([]Accession(nil), d.Accessions...)
		d.SOPClasses = append([]SOPClass(nil), d.SOPClasses...)
		for j := range d.SOPClasses {
			d.SOPClasses[j].Instances = append([]StudyID(nil), d.SOPClasses[j].Instances...)
		}
		d.ContainsStudy.Studies = append([]StudyID(nil), d.ContainsStudy.Studies...)
		d.Encrypted = cloneFlag(d.Encrypted)
		d.Anonymized = cloneFlag(d.Anonymized)
	}
	return m
}
