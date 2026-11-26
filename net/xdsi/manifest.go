// Package xdsi implements bounded XDS-I.b manifest publication (RAD-68) and
// instance retrieval (RAD-69) clients. Callers supply authenticated peers,
// patient identity mapping, and the lifetime policy for published instances.
package xdsi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/sr"
)

const (
	maxReferences    = 512
	maxManifestBytes = 4 << 20
	dicomMediaType   = "application/dicom"
)

var (
	ErrManifest      = errors.New("xdsi: invalid or unsupported KOS manifest")
	ErrIdentity      = errors.New("xdsi: document identity differs from the manifest")
	ErrLimit         = errors.New("xdsi: resource limit exceeded")
	ErrProtocol      = errors.New("xdsi: invalid transaction response")
	ErrMapping       = errors.New("xdsi: patient or imaging source mapping missing")
	ErrAuthorization = errors.New("xdsi: transaction not authorized")
	ErrAvailability  = errors.New("xdsi: imaging instance availability not established")
)

var (
	tagPatientIssuerQualifiers = core.NewTag(0x0010, 0x0024)
	tagUniversalEntityID       = core.NewTag(0x0040, 0x0032)
	tagUniversalEntityIDType   = core.NewTag(0x0040, 0x0033)
	tagRetrieveLocationUID     = core.NewTag(0x0040, 0xE011)
	tagRetrieveURL             = core.NewTag(0x0008, 0x1190)
	tagRequestedEvidence       = core.NewTag(0x0040, 0xA375)
	tagReferencedSeries        = core.NewTag(0x0008, 0x1115)
	tagReferencedSOP           = core.NewTag(0x0008, 0x1199)
	tagReferencedSOPClass      = core.NewTag(0x0008, 0x1150)
	tagReferencedSOPInstance   = core.NewTag(0x0008, 0x1155)
	tagContentTemplate         = core.NewTag(0x0040, 0xA504)
	tagMappingResource         = core.NewTag(0x0008, 0x0105)
	tagTemplateID              = core.NewTag(0x0040, 0xDB00)
)

// PatientID identifies one patient in an explicitly named ISO OID domain.
type PatientID struct {
	ID, IssuerOID string
}

func (p PatientID) valid() bool {
	return len(p.ID) <= 64 && p.ID != "" && strings.TrimSpace(p.ID) == p.ID &&
		!strings.ContainsAny(p.ID, "^~\\&|") && isPrintableASCII(p.ID) && core.IsValidUID(p.IssuerOID)
}

func (p PatientID) cx() string {
	return p.ID + "^^^&" + p.IssuerOID + "&ISO"
}

func isPrintableASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 32 || text[i] > 126 {
			return false
		}
	}
	return true
}

func patientIDFrom(ds *object.Object) (PatientID, bool) {
	id, ok := object.PatientIDWithIssuer(ds)
	ids, idOK := ds.GetStrings(tags.PatientID)
	qualifiers, qualifierOK := ds.GetSequence(tagPatientIssuerQualifiers)
	if !ok || !idOK || len(ids) != 1 || !qualifierOK || len(qualifiers) != 1 ||
		id.Issuer.UniversalEntityIDType != "ISO" {
		return PatientID{}, false
	}
	p := PatientID{ID: id.ID, IssuerOID: id.Issuer.UniversalEntityID}
	return p, p.valid() &&
		singleString(qualifiers[0], tagUniversalEntityID, p.IssuerOID) &&
		singleString(qualifiers[0], tagUniversalEntityIDType, "ISO")
}

func singleString(ds *object.Object, tag core.Tag, want string) bool {
	values, ok := ds.GetStrings(tag)
	return ok && len(values) == 1 && values[0] == want
}

func singleUID(ds *object.Object, tag core.Tag) (string, bool) {
	values, ok := ds.GetUIDs(tag)
	if !ok || len(values) != 1 || !core.IsValidUID(values[0]) {
		return "", false
	}
	return values[0], true
}

// InstanceRef is a whole-image SOP reference qualified by study and series.
type InstanceRef struct {
	StudyUID, SeriesUID, SOPClassUID, SOPInstanceUID string
}

func (r InstanceRef) valid() bool {
	sopClass, ok := uid.LookupStorageSOPClass(r.SOPClassUID)
	return ok && sopClass.Category == uid.StorageCategoryImage && !sopClass.Retired &&
		core.IsValidUID(r.StudyUID) && core.IsValidUID(r.SeriesUID) && core.IsValidUID(r.SOPInstanceUID)
}

// ManifestOptions configures a stable, single-study manifest of whole images.
// Source is a materialized instance from the patient/study whose common
// modules will be inherited. Its local Patient ID must carry an ISO issuer
// qualifier. RetrieveLocationUID and RetrieveAETitle identify the imaging
// source for every referenced series.
type ManifestOptions struct {
	Source                               *object.Object
	SOPInstanceUID, SeriesUID            string
	RetrieveLocationUID, RetrieveAETitle string
	CreatedAt                            time.Time
	References                           []InstanceRef
}

// Manifest is a validated immutable Part 10 KOS in this package's profile.
type Manifest struct {
	data                     []byte
	patient                  PatientID
	uid, studyUID, sourceUID string
	refs                     []InstanceRef
}

// Bytes returns a caller-owned copy of the Part 10 manifest.
func (m *Manifest) Bytes() []byte { return slices.Clone(m.data) }

// References returns the SOP references in evidence order.
func (m *Manifest) References() []InstanceRef { return slices.Clone(m.refs) }

func (m *Manifest) SOPInstanceUID() string { return m.uid }

func manifestReferences(ctx context.Context, studyUID, manifestUID, manifestSeries string, refs []InstanceRef) error {
	if len(refs) == 0 {
		return ErrManifest
	}
	if len(refs) > maxReferences {
		return ErrLimit
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !ref.valid() || ref.StudyUID != studyUID || ref.SeriesUID == manifestSeries || ref.SOPInstanceUID == manifestUID {
			return ErrManifest
		}
		if _, dup := seen[ref.SOPInstanceUID]; dup {
			return ErrManifest
		}
		seen[ref.SOPInstanceUID] = struct{}{}
	}
	return nil
}

// BuildManifest reuses the KOS writer for TID 2010 and adds the XDS-I.b
// retrieval identities to its evidence hierarchy.
func BuildManifest(ctx context.Context, opts ManifestOptions) (*Manifest, error) {
	if opts.Source == nil || !core.IsValidUID(opts.SOPInstanceUID) ||
		!core.IsValidUID(opts.SeriesUID) || opts.SeriesUID == opts.SOPInstanceUID ||
		!core.IsValidUID(opts.RetrieveLocationUID) || ul.ValidateAETitle(opts.RetrieveAETitle) != nil ||
		opts.RetrieveAETitle != strings.TrimSpace(opts.RetrieveAETitle) || opts.CreatedAt.IsZero() {
		return nil, ErrManifest
	}
	if _, ok := patientIDFrom(opts.Source); !ok {
		return nil, ErrMapping
	}
	if err := boundSourceMetadata(ctx, opts.Source); err != nil {
		return nil, err
	}
	studyUID, ok := singleUID(opts.Source, tags.StudyInstanceUID)
	if !ok || studyUID == opts.SOPInstanceUID || studyUID == opts.SeriesUID {
		return nil, ErrManifest
	}
	if err := manifestReferences(ctx, studyUID, opts.SOPInstanceUID, opts.SeriesUID, opts.References); err != nil {
		return nil, err
	}
	imageRefs := make([]sr.ImageReference, 0, len(opts.References))
	for _, ref := range opts.References {
		imageRefs = append(imageRefs, sr.ImageReference{
			StudyInstanceUID: ref.StudyUID, SeriesInstanceUID: ref.SeriesUID,
			SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID,
		})
	}
	doc := sr.NewKeyObjectSelection(opts.SOPInstanceUID, imageRefs, nil)
	doc.Title = sr.FromCode(dcm.Manifest)
	doc.SeriesInstanceUID = opts.SeriesUID
	created := opts.CreatedAt.UTC()
	doc.ContentDate, doc.ContentTime = created.Format("20060102"), created.Format("150405")
	doc.Common = object.DerivedCommon{Source: opts.Source, Now: created}
	ds, err := doc.Dataset()
	if err != nil {
		return nil, ErrManifest
	}
	studies, _ := ds.GetSequence(tagRequestedEvidence)
	if len(studies) != 1 {
		return nil, ErrManifest
	}
	series, _ := studies[0].GetSequence(tagReferencedSeries)
	for _, item := range series {
		item.Put(derivedio.Str(tags.RetrieveAETitle, core.VRAE, opts.RetrieveAETitle))
		item.Put(derivedio.UI(tagRetrieveLocationUID, opts.RetrieveLocationUID))
	}
	studies[0].SetSequence(tagReferencedSeries, series...)
	ds.SetSequence(tagRequestedEvidence, studies...)
	template := derivedio.Object(
		derivedio.Str(tagMappingResource, core.VRCS, "DCMR"),
		derivedio.Str(tagTemplateID, core.VRCS, "2010"),
	)
	ds.SetSequence(tagContentTemplate, template)
	file, err := derivedio.File(sr.KeyObjectSelectionDocumentStorage, opts.SOPInstanceUID, ds)
	if err != nil {
		return nil, ErrManifest
	}
	var out bytes.Buffer
	if err := object.WriteFile(&limitedWriter{ctx: ctx, dst: &out, left: maxManifestBytes}, file); err != nil {
		return nil, err
	}
	return parseManifest(ctx, out.Bytes())
}

// The inheritance writer can copy sequence-valued common modules. Bound
// materialized metadata before asking it to detach those values. Root pixel
// data is excluded because it is never inherited by a KOS.
func boundSourceMetadata(ctx context.Context, source *object.Object) error {
	if source.Len() > 4096 {
		return ErrLimit
	}
	remaining := int64(maxManifestBytes)
	items := 4096
	var visit func([]core.Element, int) error
	visit = func(elements []core.Element, depth int) error {
		if depth > 12 {
			return ErrLimit
		}
		for _, item := range elements {
			if err := ctx.Err(); err != nil {
				return err
			}
			items--
			if items < 0 {
				return ErrLimit
			}
			if depth == 0 && (item.Tag() == core.NewTag(0x7fe0, 0x0010) || item.Tag() == core.NewTag(0x7fe0, 0x0008) || item.Tag() == core.NewTag(0x7fe0, 0x0009)) {
				continue
			}
			remaining -= 12
			switch value := item.Value.(type) {
			case core.SequenceValue:
				if len(value.Items) > items {
					return ErrLimit
				}
				for _, child := range value.Items {
					if err := visit(child.Elements, depth+1); err != nil {
						return err
					}
				}
			default:
				if item.Value == nil {
					return ErrManifest
				}
				size, known := item.Value.EncodedLength()
				if !known {
					return ErrManifest
				}
				remaining -= int64(size)
			}
			if remaining < 0 {
				return ErrLimit
			}
		}
		return nil
	}
	return visit(source.Elements(), 0)
}

// ParseManifest validates an independently obtained Part 10 KOS against this
// single-study, single-imaging-source profile. URLs in the document are never
// followed. This consumer accepts only explicit location UID routing.
func ParseManifest(ctx context.Context, data []byte) (*Manifest, error) {
	if len(data) > maxManifestBytes {
		return nil, ErrLimit
	}
	return parseManifest(ctx, slices.Clone(data))
}

func parseManifest(ctx context.Context, data []byte) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := readPart10(ctx, data, Limits{DocumentBytes: maxManifestBytes, TotalBytes: maxManifestBytes, MetadataBytes: maxManifestBytes})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrManifest
	}
	defer file.Close()
	ds := file.Dataset
	uid, ok := singleUID(ds, tags.SOPInstanceUID)
	metaUID, metaOK := singleUID(file.Meta, tags.MediaStorageSOPInstanceUID)
	class, classOK := singleUID(ds, tags.SOPClassUID)
	metaClass, metaClassOK := singleUID(file.Meta, tags.MediaStorageSOPClassUID)
	if !ok || !metaOK || !classOK || !metaClassOK || metaUID != uid ||
		class != sr.KeyObjectSelectionDocumentStorage || metaClass != class || !singleString(ds, tags.Modality, "KO") {
		return nil, ErrManifest
	}
	patient, ok := patientIDFrom(ds)
	if !ok {
		return nil, ErrMapping
	}
	studyUID, studyOK := singleUID(ds, tags.StudyInstanceUID)
	manifestSeries, seriesOK := singleUID(ds, tags.SeriesInstanceUID)
	if !studyOK || !seriesOK || uid == studyUID || uid == manifestSeries || studyUID == manifestSeries {
		return nil, ErrManifest
	}
	if template, ok := ds.GetSequence(tagContentTemplate); ok &&
		(len(template) != 1 || !singleString(template[0], tagMappingResource, "DCMR") ||
			!singleString(template[0], tagTemplateID, "2010")) {
		return nil, ErrManifest
	}
	refs, sourceUID, err := manifestEvidence(ds, studyUID)
	if err != nil {
		return nil, err
	}
	if err := manifestReferences(ctx, studyUID, uid, manifestSeries, refs); err != nil {
		return nil, err
	}
	result, err := sr.ReadDocumentWithOptions(ds, sr.ReadOptions{MaxDepth: 8, MaxItems: maxReferences + 1, MaxReferences: maxReferences})
	if err != nil || result.Document == nil || result.Document.Title.CodeValue != dcm.Manifest.Value ||
		result.Document.Title.CodingSchemeDesignator != dcm.Manifest.Scheme {
		return nil, ErrManifest
	}
	if err := matchManifestContent(result.Document, refs); err != nil {
		return nil, err
	}
	return &Manifest{data: data, patient: patient, uid: uid, studyUID: studyUID, sourceUID: sourceUID, refs: refs}, nil
}

func manifestEvidence(ds *object.Object, studyUID string) ([]InstanceRef, string, error) {
	studies, ok := ds.GetSequence(tagRequestedEvidence)
	if !ok || len(studies) != 1 || !singleString(studies[0], tags.StudyInstanceUID, studyUID) {
		return nil, "", ErrManifest
	}
	series, ok := studies[0].GetSequence(tagReferencedSeries)
	if !ok || len(series) == 0 {
		return nil, "", ErrManifest
	}
	if len(series) > maxReferences {
		return nil, "", ErrLimit
	}
	var refs []InstanceRef
	var sourceUID, aeTitle string
	seen := make(map[string]bool, len(series))
	for _, item := range series {
		seriesUID, seriesOK := singleUID(item, tags.SeriesInstanceUID)
		location, locationOK := singleUID(item, tagRetrieveLocationUID)
		ae, aeOK := item.GetString(tags.RetrieveAETitle)
		if !seriesOK || seen[seriesUID] || !locationOK || !aeOK ||
			!singleString(item, tags.RetrieveAETitle, ae) || ul.ValidateAETitle(ae) != nil || (sourceUID != "" && (location != sourceUID || ae != aeTitle)) {
			return nil, "", ErrManifest
		}
		if url, _ := item.GetString(tagRetrieveURL); url != "" {
			return nil, "", ErrManifest
		}
		seen[seriesUID] = true
		sourceUID, aeTitle = location, ae
		sops, ok := item.GetSequence(tagReferencedSOP)
		if !ok || len(sops) == 0 {
			return nil, "", ErrManifest
		}
		if len(sops) > maxReferences-len(refs) {
			return nil, "", ErrLimit
		}
		for _, sop := range sops {
			class, classOK := singleUID(sop, tagReferencedSOPClass)
			instance, instanceOK := singleUID(sop, tagReferencedSOPInstance)
			if !classOK || !instanceOK {
				return nil, "", ErrManifest
			}
			refs = append(refs, InstanceRef{StudyUID: studyUID, SeriesUID: seriesUID, SOPClassUID: class, SOPInstanceUID: instance})
		}
	}
	return refs, sourceUID, nil
}

func matchManifestContent(doc *sr.Document, refs []InstanceRef) error {
	if len(doc.Content) != len(refs) {
		return ErrManifest
	}
	expected := make(map[string]InstanceRef, len(refs))
	for _, ref := range refs {
		expected[ref.SOPInstanceUID] = ref
	}
	for _, item := range doc.Content {
		ref, ok := expected[item.Image.SOPInstanceUID]
		if !ok || item.ValueType != sr.ValueImage || item.RelationshipType != sr.RelationshipContains ||
			item.Image.SOPClassUID != ref.SOPClassUID || item.Image.StudyInstanceUID != ref.StudyUID ||
			item.Image.SeriesInstanceUID != ref.SeriesUID || len(item.Image.Frames) != 0 || len(item.Children) != 0 {
			return ErrManifest
		}
		delete(expected, item.Image.SOPInstanceUID)
	}
	return nil
}

type limitedWriter struct {
	ctx  context.Context
	dst  io.Writer
	left int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.left {
		return 0, ErrLimit
	}
	n, err := w.dst.Write(p)
	w.left -= int64(n)
	return n, err
}
