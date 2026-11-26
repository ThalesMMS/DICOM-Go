package sr

import (
	"context"
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrNotKOSRejection identifies a different SOP Class or KOS document title.
var ErrNotKOSRejection = errors.New("dicom/sr: not a supported KOS rejection note")

// KOSRejectionNote is an owned, metadata-only interpretation. It authorizes no
// archive action. Any read error returns its zero value, never a partial note.
type KOSRejectionNote struct {
	Kind                                                   KOSRejectionKind
	Title                                                  CodedEntry
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID    string
	ContentDate, ContentTime, SeriesNumber, InstanceNumber string
	References                                             []KOSRejectionReference
	QualityReasons                                         []CodedEntry
	Description                                            string
}

// ReadKOSRejection accepts the single-study, whole-instance profile produced by
// NewKOSRejection. It requires exact evidence/content agreement and validates
// TID 2010. Deferred metadata must first be materialized by the caller; root
// bulk, including deferred Pixel Data, is ignored without accessing backing IO.
// Source/options and all their payloads must stay stable throughout the call.
func ReadKOSRejection(ctx context.Context, source *object.Object, limits KOSRejectionLimits) (KOSRejectionNote, error) {
	note, err := readKOSRejection(ctx, source, limits)
	if err != nil {
		return KOSRejectionNote{}, err
	}
	return note, nil
}

func readKOSRejection(ctx context.Context, source *object.Object, limits KOSRejectionLimits) (KOSRejectionNote, error) {
	var note KOSRejectionNote
	if err := kosContext(ctx); err != nil {
		return note, err
	}
	l, err := normalizeKOSLimits(limits)
	if err != nil {
		return note, err
	}
	b := &kosBudget{ctx: ctx, limits: l}
	o, err := kosFacade(ctx, source, b, true)
	if err != nil {
		return note, err
	}
	if _, err = o.CharacterSet(); err != nil {
		return note, kosInvalid("character set")
	}
	sop, err := kosString(o, tagSOPClassUID, core.VRUI, true)
	if err != nil {
		return note, err
	}
	if sop != KeyObjectSelectionDocumentStorage {
		return note, ErrNotKOSRejection
	}
	note.Title, err = kosCode(o, tagConceptNameCodeSeq)
	if err != nil {
		return note, err
	}
	for _, kind := range []KOSRejectionKind{KOSRejectionQuality, KOSRejectionPatientSafety, KOSRejectionIncorrectWorklist, KOSRejectionRetentionExpired} {
		t := kosRejectionTitle(kind)
		if note.Title.CodeValue == t.CodeValue && note.Title.CodingSchemeDesignator == t.CodingSchemeDesignator {
			note.Kind = kind
			break
		}
	}
	if note.Kind == 0 {
		return note, ErrNotKOSRejection
	}
	for _, f := range []struct {
		tag  core.Tag
		vr   core.VR
		want string
	}{
		{tagModality, core.VRCS, "KO"}, {tagValueType, core.VRCS, string(ValueContainer)}, {tagContinuityOfContent, core.VRCS, "SEPARATE"},
	} {
		got, e := kosString(o, f.tag, f.vr, true)
		if e != nil || got != f.want {
			return note, kosInvalid("root content or modality")
		}
	}
	for _, f := range []struct {
		tag core.Tag
		out *string
	}{
		{tagSOPInstanceUID, &note.SOPInstanceUID}, {tagStudyInstanceUID, &note.StudyInstanceUID}, {tagSeriesInstanceUID, &note.SeriesInstanceUID},
	} {
		*f.out, err = kosString(o, f.tag, core.VRUI, true)
		if err != nil || !core.IsValidUID(*f.out) {
			return note, kosInvalid("note UID")
		}
	}
	for _, f := range []struct {
		tag core.Tag
		out *string
	}{{tagSeriesNumber, &note.SeriesNumber}, {tagInstanceNumber, &note.InstanceNumber}} {
		*f.out, err = kosString(o, f.tag, core.VRIS, true)
		if err != nil || !kosPositiveIS(*f.out) {
			return note, kosInvalid("series or instance number")
		}
	}
	note.ContentDate, err = kosString(o, tagContentDate, core.VRDA, true)
	if err != nil {
		return note, err
	}
	date, err := dcmtime.ParseDate(note.ContentDate)
	if err != nil || date.IsNEMA || len(note.ContentDate) != 8 {
		return note, kosInvalid("content date")
	}
	note.ContentTime, err = kosString(o, tagContentTime, core.VRTM, true)
	if err != nil {
		return note, err
	}
	if _, err = dcmtime.ParseTime(note.ContentTime); err != nil || len(note.ContentTime) < 6 {
		return note, kosInvalid("content time precision")
	}
	if o.Has(tagReferencedContentItemIdentifier) || o.Has(core.NewTag(0x0040, 0xa525)) || o.Has(tagCompletionFlag) || o.Has(tagVerificationFlag) {
		return note, kosInvalid("unsupported root extension")
	}
	for _, tag := range []core.Tag{tagRelationshipType, tagTextValue, tagConceptCodeSeq, tagRefSOPSequence, tagMeasuredValueSeq,
		tagNumericValueQualifierCodeSeq, tagDateTimeValue, tagDateValue, tagTimeValue, tagUID, tagPersonNameValue, tagGraphicData,
		tagGraphicType, tagFiducialUID, tagPixelOriginInterpret, tagReferencedFrameOfRefUID, tagTemporalRangeType, tagReferencedSamples,
		tagReferencedTimeOffset, tagReferencedDateTime} {
		if o.Has(tag) {
			return note, kosInvalid("root value payload")
		}
	}
	if _, err = kosSequence(o, tagReferencedPerformedProcedureStep, 0, 0); err != nil {
		return note, err
	}
	// Explicit template identification is accepted only for DCMR TID 2010.
	if o.Has(tagContentTemplateSequence) {
		ids, e := kosSequence(o, tagContentTemplateSequence, 1, 1)
		if e != nil {
			return note, e
		}
		mapping, e1 := kosString(ids[0], core.NewTag(0x0008, 0x0105), core.VRCS, true)
		tid, e2 := kosString(ids[0], core.NewTag(0x0040, 0xdb00), core.VRCS, true)
		if e1 != nil || e2 != nil || mapping != "DCMR" || tid != "2010" || ids[0].Len() != 2 {
			return note, kosInvalid("template identification")
		}
	}
	evidence, err := kosReadEvidence(ctx, o, note, l)
	if err != nil {
		return note, err
	}
	items, err := kosSequence(o, tagContentSequence, 1, l.MaxItems)
	if err != nil {
		return note, err
	}
	seen := make(map[string]KOSRejectionReference)
	identities := make(map[string]uint8)
	if !kosIdentityRoles(identities, note.StudyInstanceUID, note.SeriesInstanceUID, KeyObjectSelectionDocumentStorage, note.SOPInstanceUID) {
		return note, kosInvalid("UID role collision")
	}
	referenceVisits := 0
	for _, it := range items {
		if err = ctx.Err(); err != nil {
			return note, err
		}
		vt, e1 := kosString(it, tagValueType, core.VRCS, true)
		rel, e2 := kosString(it, tagRelationshipType, core.VRCS, true)
		if e1 != nil || e2 != nil || it.Has(tagContentSequence) || it.Has(tagReferencedContentItemIdentifier) {
			return note, kosInvalid("content structure")
		}
		switch ValueType(vt) {
		case ValueImage, ValueWaveform, ValueComposite:
			referenceVisits++
			if referenceVisits > l.MaxReferences {
				return note, kosLimit("content references")
			}
			if rel != RelationshipContains || it.Len() != 3 {
				return note, kosInvalid("reference content")
			}
			uidPair, e := kosReadSOPPair(it)
			if e != nil {
				return note, e
			}
			r, ok := evidence[uidPair.SOPInstanceUID]
			if !ok || r.SOPClassUID != uidPair.SOPClassUID {
				return note, kosInvalid("missing or conflicting evidence")
			}
			r.ValueType = ValueType(vt)
			if !kosReferenceValid(r) || r.SOPInstanceUID == note.SOPInstanceUID || !kosIdentityRoles(identities, r.StudyInstanceUID, r.SeriesInstanceUID, r.SOPClassUID, r.SOPInstanceUID) {
				return note, kosInvalid("reference identity or role")
			}
			if old, exists := seen[r.SOPInstanceUID]; exists {
				if old != r {
					return note, kosInvalid("conflicting content reference")
				}
				continue
			}
			seen[r.SOPInstanceUID] = r
			note.References = append(note.References, r)
		case ValueCode:
			if len(note.QualityReasons) >= l.MaxQualityReasons {
				return note, kosLimit("quality reasons")
			}
			if note.Kind != KOSRejectionQuality || rel != RelationshipHasConceptMod || it.Len() != 4 {
				return note, kosInvalid("quality modifier")
			}
			name, e := kosCode(it, tagConceptNameCodeSeq)
			if e != nil || !kosSameCode(name, "113011") {
				return note, kosInvalid("modifier concept")
			}
			code, e := kosCode(it, tagConceptCodeSeq)
			if e != nil || !kosQualityCode(code) {
				return note, kosInvalid("quality code outside vendored CID 7011")
			}
			note.QualityReasons = append(note.QualityReasons, code)
		case ValueText:
			if rel != RelationshipContains || it.Len() != 4 || note.Description != "" {
				return note, kosInvalid("description content")
			}
			name, e := kosCode(it, tagConceptNameCodeSeq)
			if e != nil || !kosSameCode(name, "113012") {
				return note, kosInvalid("description concept")
			}
			note.Description, err = kosString(it, tagTextValue, core.VRUT, true)
			if err != nil || !kosTextClean(note.Description) || strings.TrimSpace(note.Description) == "" {
				return note, kosInvalid("description text")
			}
			if len(note.Description) > l.MaxDescriptionBytes {
				return note, kosLimit("description bytes")
			}
		default:
			return note, kosInvalid("unsupported content type")
		}
	}
	if len(seen) == 0 || len(seen) != len(evidence) {
		return note, kosInvalid("evidence/content mismatch")
	}
	if note.Kind == KOSRejectionQuality && len(note.QualityReasons) == 0 {
		return note, kosInvalid("missing quality reason")
	}
	decoded, err := ReadDocumentWithOptions(o, ReadOptions{MaxDepth: l.MaxDepth, MaxItems: l.MaxItems, MaxReferences: l.MaxReferences, MaxPathComponents: l.MaxDepth + 1, ResolveReferences: true, ReferenceMode: ValidationModeStrict, MaxFindings: 64, ValidateRelationships: true, RelationshipMode: ValidationModeStrict})
	if err != nil {
		return note, err
	}
	if err = kosValidateTemplate(ctx, decoded.Document, l); err != nil {
		return note, err
	}
	if err = ctx.Err(); err != nil {
		return note, err
	}
	return note, nil
}

func kosReadSOPPair(o *object.Object) (CompositeReference, error) {
	items, err := kosSequence(o, tagRefSOPSequence, 1, 1)
	if err != nil {
		return CompositeReference{}, err
	}
	return kosReadSOPItem(items[0])
}

func kosReadSOPItem(o *object.Object) (CompositeReference, error) {
	var r CompositeReference
	if o.Len() != 2 || !kosNoSelectors(o) {
		return r, kosInvalid("partial reference or SOP item attributes")
	}
	var e1, e2 error
	r.SOPClassUID, e1 = kosString(o, tagRefSOPClassUID, core.VRUI, true)
	r.SOPInstanceUID, e2 = kosString(o, tagRefSOPInstanceUID, core.VRUI, true)
	if e1 != nil || e2 != nil || !core.IsValidUID(r.SOPClassUID) || !core.IsValidUID(r.SOPInstanceUID) {
		return r, kosInvalid("referenced UID")
	}
	return r, nil
}

func kosReadEvidence(ctx context.Context, o *object.Object, note KOSRejectionNote, l KOSRejectionLimits) (map[string]KOSRejectionReference, error) {
	studies, err := kosSequence(o, tagCurrentRequestedProcedureEvidence, 1, 1)
	if err != nil {
		return nil, err
	}
	study, e := kosString(studies[0], tagStudyInstanceUID, core.VRUI, true)
	if e != nil || study != note.StudyInstanceUID || studies[0].Len() != 2 {
		return nil, kosInvalid("evidence study")
	}
	series, err := kosBoundSequence(studies[0], tagReferencedSeriesSequence, 1, l.MaxReferences)
	if err != nil {
		return nil, err
	}
	out := make(map[string]KOSRejectionReference)
	visits := 0
	for _, it := range series {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		seriesUID, e := kosString(it, tagSeriesInstanceUID, core.VRUI, true)
		// Retrieve URL is the one optional series attribute accepted
		// (Hierarchical Series Reference Macro, CP-2595).
		attributes := 2
		retrieveURL, e2 := kosString(it, tagRetrieveURL, core.VRUR, false)
		if it.Has(tagRetrieveURL) {
			attributes = 3
			if e2 == nil && !validRetrieveURL(retrieveURL) {
				e2 = kosInvalid("evidence Retrieve URL")
			}
		}
		if e != nil || e2 != nil || !core.IsValidUID(seriesUID) || seriesUID == note.SeriesInstanceUID || it.Len() != attributes {
			return nil, kosInvalid("evidence series")
		}
		sops, e := kosBoundSequence(it, tagRefSOPSequence, 1, l.MaxReferences-visits)
		if e != nil {
			return nil, e
		}
		visits += len(sops)
		for _, sop := range sops {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			p, e := kosReadSOPItem(sop)
			if e != nil {
				return nil, e
			}
			r := KOSRejectionReference{StudyInstanceUID: study, SeriesInstanceUID: seriesUID, SOPClassUID: p.SOPClassUID, SOPInstanceUID: p.SOPInstanceUID, RetrieveURL: retrieveURL}
			if old, ok := out[r.SOPInstanceUID]; ok && old != r {
				return nil, kosInvalid("conflicting evidence")
			}
			out[r.SOPInstanceUID] = r
		}
	}
	return out, nil
}
