package sr

import (
	"context"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

// KOSRejectionKind identifies the four IOCM titles covered by this strict,
// single-study profile. It does not apply rejection to an archive or dataset.
type KOSRejectionKind uint8

const (
	KOSRejectionQuality KOSRejectionKind = iota + 1
	KOSRejectionPatientSafety
	KOSRejectionIncorrectWorklist
	KOSRejectionRetentionExpired
)

// KOSRejectionReference declares the referenced object's content type. The
// caller must select IMAGE/WAVEFORM/COMPOSITE according to the referenced IOD;
// this metadata-only API does not inspect the target instance. References are
// to entire SOP instances, never selected frames, segments or waveform channels.
// RetrieveURL is the optional Retrieve URL (0008,1190) of the referenced
// series in the evidence (CP-2595); every reference of a series carries the
// same value, empty included.
type KOSRejectionReference struct {
	ValueType                           ValueType
	StudyInstanceUID, SeriesInstanceUID string
	SOPClassUID, SOPInstanceUID         string
	RetrieveURL                         string
}

type KOSRejectionOptions struct {
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID string
	// CreatedAt supplies both content and instance creation timestamps. If zero,
	// Common.Now is used, then time.Now. A source offset is honored when present.
	CreatedAt                    time.Time
	SeriesNumber, InstanceNumber string
	References                   []KOSRejectionReference
	QualityReasons               []CodedEntry
	Description                  string
	Common                       object.DerivedCommon
	Limits                       KOSRejectionLimits
}

func kosRejectionTitle(kind KOSRejectionKind) CodedEntry {
	switch kind {
	case KOSRejectionQuality:
		return FromCode(dcm.RejectedForQualityReasons)
	case KOSRejectionPatientSafety:
		return FromCode(dcm.RejectedForPatientSafetyReasons)
	case KOSRejectionIncorrectWorklist:
		return FromCode(dcm.IncorrectModalityWorklistEntry)
	case KOSRejectionRetentionExpired:
		return FromCode(dcm.DataRetentionPolicyExpired)
	}
	return CodedEntry{}
}

// NewKOSRejection constructs a bounded in-memory KOS. Errors return nil and
// preserve all inputs. Common.Source metadata must be materialized; deferred
// root pixels are ignored. No filesystem, network or archive action is taken.
func NewKOSRejection(ctx context.Context, kind KOSRejectionKind, opts KOSRejectionOptions) (*object.Object, error) {
	if err := kosContext(ctx); err != nil {
		return nil, err
	}
	l, err := normalizeKOSLimits(opts.Limits)
	if err != nil {
		return nil, err
	}
	if kosRejectionTitle(kind).Empty() {
		return nil, kosInvalid("unknown kind")
	}
	for _, s := range []string{opts.SOPInstanceUID, opts.StudyInstanceUID, opts.SeriesInstanceUID} {
		if !core.IsValidUID(s) {
			return nil, kosInvalid("note UID")
		}
	}
	if len(opts.References) == 0 {
		return nil, kosInvalid("no references")
	}
	if len(opts.References) > l.MaxReferences || len(opts.QualityReasons) > l.MaxQualityReasons || len(opts.Description) > l.MaxDescriptionBytes {
		return nil, kosLimit("options")
	}
	if kind == KOSRejectionQuality && len(opts.QualityReasons) == 0 || kind != KOSRejectionQuality && len(opts.QualityReasons) != 0 {
		return nil, kosInvalid("quality reasons")
	}
	if !kosTextClean(opts.Description) || opts.Description != "" && strings.TrimSpace(opts.Description) == "" {
		return nil, kosInvalid("description")
	}
	if opts.SeriesNumber == "" {
		opts.SeriesNumber = "1"
	}
	if opts.InstanceNumber == "" {
		opts.InstanceNumber = "1"
	}
	if !kosPositiveIS(opts.SeriesNumber) || !kosPositiveIS(opts.InstanceNumber) {
		return nil, kosInvalid("instance or series number")
	}
	b := &kosBudget{ctx: ctx, limits: l}
	for _, s := range []string{opts.SOPInstanceUID, opts.StudyInstanceUID, opts.SeriesInstanceUID, opts.Description, opts.SeriesNumber, opts.InstanceNumber} {
		if err = b.addBytes(len(s)); err != nil {
			return nil, err
		}
	}
	// Bound the constant generated headers, timestamps, concept names and
	// default equipment before their construction as well as variable inputs.
	if err = b.addBytes(1024 + 128*len(opts.QualityReasons)); err != nil {
		return nil, err
	}
	seriesURLs := make(map[string]string)
	for _, r := range opts.References {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !kosReferenceValid(r) || r.StudyInstanceUID != opts.StudyInstanceUID || r.SeriesInstanceUID == opts.SeriesInstanceUID || r.SOPInstanceUID == opts.SOPInstanceUID {
			return nil, kosInvalid("reference identity or profile")
		}
		if url, ok := seriesURLs[r.SeriesInstanceUID]; ok && url != r.RetrieveURL {
			return nil, kosInvalid("conflicting series Retrieve URL")
		}
		seriesURLs[r.SeriesInstanceUID] = r.RetrieveURL
		if err = b.addBytes(len(r.RetrieveURL)); err != nil {
			return nil, err
		}
		for _, s := range []string{r.StudyInstanceUID, r.SeriesInstanceUID, r.SOPClassUID, r.SOPInstanceUID} {
			if err = b.addBytes(len(s) * 3); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range opts.QualityReasons {
		if !kosQualityCode(c) || !kosTextClean(c.CodeMeaning) {
			return nil, kosInvalid("quality code outside vendored CID 7011")
		}
		for _, s := range []string{c.CodeValue, c.CodingSchemeDesignator, c.CodeMeaning} {
			if err = b.addBytes(len(s)); err != nil {
				return nil, err
			}
		}
	}
	if opts.Common.Equipment != nil {
		e := opts.Common.Equipment
		for _, s := range []string{e.Manufacturer, e.ModelName, e.DeviceSerialNumber, e.SoftwareVersions} {
			if !kosTextClean(s) || len(s) > 64 {
				return nil, kosInvalid("equipment text")
			}
			if err = b.addBytes(len(s)); err != nil {
				return nil, err
			}
		}
	}
	common := opts.Common
	if common.Source != nil {
		common.Source, err = kosFacade(ctx, common.Source, b, true)
		if err != nil {
			return nil, err
		}
		study, e := kosString(common.Source, tagStudyInstanceUID, core.VRUI, true)
		if e != nil || study != opts.StudyInstanceUID {
			return nil, kosInvalid("source study")
		}
		if _, e := common.Source.CharacterSet(); e != nil {
			return nil, kosInvalid("source character set")
		}
	}
	// Reserve a conservative bound for both content and exact evidence before
	// maps, content slices, generic validators or module inheritance allocate.
	if 64+12*len(opts.References)+12*len(opts.QualityReasons) > l.MaxElements-b.elements ||
		8+4*len(opts.References)+4*len(opts.QualityReasons) > l.MaxItems-b.items || l.MaxDepth < 3 {
		return nil, kosLimit("generated structure")
	}
	created := opts.CreatedAt
	if created.IsZero() {
		created = common.Now
	}
	if created.IsZero() {
		created = time.Now()
	}
	if common.Source != nil && common.Source.Has(core.NewTag(0x0008, 0x0201)) {
		offset, e := kosString(common.Source, core.NewTag(0x0008, 0x0201), core.VRSH, true)
		if e != nil || len(offset) != 5 {
			return nil, kosInvalid("source timezone offset")
		}
		dt, e := dcmtime.ParseDatetime("20000101000000" + offset)
		if e != nil || dt.NoOffset {
			return nil, kosInvalid("source timezone offset")
		}
		created = created.In(dt.Time.Location())
	}
	if created.Year() < 1 || created.Year() > 9999 {
		return nil, kosInvalid("creation year")
	}
	common.Now = created
	identities := make(map[string]uint8, 3+4*len(opts.References))
	if !kosIdentityRoles(identities, opts.StudyInstanceUID, opts.SeriesInstanceUID, KeyObjectSelectionDocumentStorage, opts.SOPInstanceUID) {
		return nil, kosInvalid("UID role collision")
	}
	refs := make([]KOSRejectionReference, 0, len(opts.References))
	seen := make(map[string]KOSRejectionReference, len(opts.References))
	for _, r := range opts.References {
		if !kosIdentityRoles(identities, r.StudyInstanceUID, r.SeriesInstanceUID, r.SOPClassUID, r.SOPInstanceUID) {
			return nil, kosInvalid("UID role collision")
		}
		if previous, ok := seen[r.SOPInstanceUID]; ok {
			if previous != r {
				return nil, kosInvalid("conflicting reference")
			}
			continue
		}
		seen[r.SOPInstanceUID] = r
		refs = append(refs, r)
	}
	doc := &Document{SOPClassUID: KeyObjectSelectionDocumentStorage, SOPInstanceUID: opts.SOPInstanceUID,
		StudyInstanceUID: opts.StudyInstanceUID, SeriesInstanceUID: opts.SeriesInstanceUID, Modality: "KO",
		Title: kosRejectionTitle(kind), ContentDate: created.Format("20060102"), ContentTime: created.Format("150405"),
		SeriesNumber: opts.SeriesNumber, InstanceNumber: opts.InstanceNumber, Common: common}
	for _, c := range opts.QualityReasons {
		doc.Content = append(doc.Content, ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasConceptMod, ConceptName: FromCode(dcm.DocumentTitleModifier), Code: c})
	}
	if opts.Description != "" {
		doc.Content = append(doc.Content, ContentItem{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: FromCode(dcm.KeyObjectDescription), Text: opts.Description})
	}
	evidence := make([]ImageReference, 0, len(refs))
	for _, r := range refs {
		item := ContentItem{ValueType: r.ValueType, RelationshipType: RelationshipContains}
		image := ImageReference{StudyInstanceUID: r.StudyInstanceUID, SeriesInstanceUID: r.SeriesInstanceUID, SOPClassUID: r.SOPClassUID, SOPInstanceUID: r.SOPInstanceUID, RetrieveURL: r.RetrieveURL}
		switch r.ValueType {
		case ValueImage:
			item.Image = image
		case ValueComposite:
			item.Composite = CompositeReference{r.SOPClassUID, r.SOPInstanceUID}
		case ValueWaveform:
			item.Waveform.CompositeReference = CompositeReference{r.SOPClassUID, r.SOPInstanceUID}
		}
		doc.Content = append(doc.Content, item)
		evidence = append(evidence, image)
	}
	if err = kosValidateTemplate(ctx, doc, l); err != nil {
		return nil, err
	}
	elems, err := doc.Elements()
	if err != nil {
		return nil, err
	}
	dataset := object.FromElements(elems, nil)
	dataset.Put(strElem(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 192"))
	dataset.Put(currentRequestedProcedureEvidenceSequence(evidence))
	if common.Source != nil {
		dataset.SetValueByteOrder(common.Source.ValueByteOrder())
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = common.Apply(dataset, object.InheritWithoutFrameOfReference); err != nil {
		return nil, err
	}
	// The inherited structures are owned. Detach the extended typed arrays which
	// Common's generic fallback leaves shared, after checking their output size.
	finalBudget := &kosBudget{ctx: ctx, limits: l}
	if dataset.Len() > l.MaxElements {
		return nil, kosLimit("output elements")
	}
	final := dataset.Elements()
	if err = finalBudget.walk(final, 0, false); err != nil {
		return nil, err
	}
	kosOwnExtendedValues(final)
	for _, e := range final {
		dataset.Put(e)
	}
	if _, err = ReadKOSRejection(ctx, dataset, l); err != nil {
		return nil, err
	}
	return dataset, nil
}

func NewKOSQualityRejection(ctx context.Context, o KOSRejectionOptions) (*object.Object, error) {
	return NewKOSRejection(ctx, KOSRejectionQuality, o)
}
func NewKOSPatientSafetyRejection(ctx context.Context, o KOSRejectionOptions) (*object.Object, error) {
	return NewKOSRejection(ctx, KOSRejectionPatientSafety, o)
}
func NewKOSIncorrectWorklistRejection(ctx context.Context, o KOSRejectionOptions) (*object.Object, error) {
	return NewKOSRejection(ctx, KOSRejectionIncorrectWorklist, o)
}
func NewKOSRetentionExpiredRejection(ctx context.Context, o KOSRejectionOptions) (*object.Object, error) {
	return NewKOSRejection(ctx, KOSRejectionRetentionExpired, o)
}
