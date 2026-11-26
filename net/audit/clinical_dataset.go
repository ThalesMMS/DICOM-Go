package audit

import (
	"bytes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
)

func validateQueryData(policy ClinicalPolicy, data []byte, uid string) error {
	p, err := policy.Normalize()
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return clinicalError("required", "Query.DataSet")
	}
	if len(data) > p.Limits.MaxQueryBytes {
		return clinicalError("limit", "Query.DataSet")
	}
	syntax, ok := transfer.DefaultRegistry.Get(uid)
	if !ok || syntax.Encapsulated {
		return clinicalError("syntax", "Query.TransferSyntax")
	}
	ds, err := object.ReadDataSetWithOptions(bytes.NewReader(data), syntax, object.ReadFileOptions{MaxTotalBytes: int64(p.Limits.MaxQueryBytes), MaxElementBytes: int64(p.Limits.MaxQueryBytes), MaxElements: 4096, MaxSequenceDepth: 16})
	if err != nil || ds == nil || ds.Len() == 0 {
		return clinicalError("dataset", "Query.DataSet")
	}
	return nil
}

type queryBuffer struct {
	bytes.Buffer
	limit int
}

func (w *queryBuffer) Write(b []byte) (int, error) {
	if len(b) > w.limit-w.Len() {
		return 0, clinicalError("limit", "Query.DataSet")
	}
	return w.Buffer.Write(b)
}

var _ io.Writer = (*queryBuffer)(nil)

// EncodeClinicalQuery encodes the logical request using its negotiated syntax.
// This is an explicit representation of the decoded query, not the original
// P-DATA byte stream. The original charset and actual transfer syntax are kept.
func EncodeClinicalQuery(p ClinicalPolicy, ds *object.Object, syntax transfer.Syntax) ([]byte, error) {
	if !p.IncludeQuery {
		return nil, clinicalError("opt_in", "Query")
	}
	p, err := p.Normalize()
	if err != nil {
		return nil, err
	}
	if ds == nil {
		return nil, clinicalError("required", "Query.DataSet")
	}
	w := &queryBuffer{limit: p.Limits.MaxQueryBytes}
	if err := object.WriteDataSet(w, ds, syntax); err != nil {
		return nil, clinicalError("dataset", "Query.DataSet")
	}
	data := w.Bytes()
	if err := validateQueryData(p, data, syntax.UID); err != nil {
		return nil, err
	}
	return bytes.Clone(data), nil
}

// ClinicalIdentityFromObject captures only named metadata after explicit opt-in.
// Study Name is Study Description, falling back to the explicit Study ID; it
// never substitutes a UID or a patient identifier for a missing name.
func ClinicalIdentityFromObject(p ClinicalPolicy, ds *object.Object) (PatientIdentity, StudyIdentity, error) {
	if !p.IncludeIdentities {
		return PatientIdentity{}, StudyIdentity{}, clinicalError("opt_in", "Identity")
	}
	p, err := p.Normalize()
	if err != nil {
		return PatientIdentity{}, StudyIdentity{}, err
	}
	if ds == nil {
		return PatientIdentity{}, StudyIdentity{}, clinicalError("required", "DataSet")
	}
	get := func(g, e uint16) string { v, _ := ds.GetString(core.NewTag(g, e)); return v }
	patient := PatientIdentity{ID: get(0x0010, 0x0020), Name: get(0x0010, 0x0010), Issuer: get(0x0010, 0x0021)}
	study := StudyIdentity{UID: get(0x0020, 0x000d), Name: get(0x0008, 0x1030)}
	if study.Name == "" {
		study.Name = get(0x0020, 0x0010)
	}
	if !clinicalText(patient.ID) || !clinicalText(patient.Name) {
		return PatientIdentity{}, StudyIdentity{}, clinicalError("required", "Patient")
	}
	if !clinicalUID(study.UID) || !clinicalText(study.Name) {
		return PatientIdentity{}, StudyIdentity{}, clinicalError("required", "Study")
	}
	if len(patient.ID)+len(patient.Name)+len(patient.Issuer)+len(study.UID)+len(study.Name) > p.Limits.MaxIdentityBytes {
		return PatientIdentity{}, StudyIdentity{}, clinicalError("limit", "Identity")
	}
	return patient, study, nil
}
