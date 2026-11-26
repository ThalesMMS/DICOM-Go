package sr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/object"
)

// KOSRejectionLimits bounds input/copy structure and variable metadata. Zero
// fields select defaults; positive overrides may only lower these finite caps.
type KOSRejectionLimits struct {
	MaxReferences, MaxItems, MaxElements, MaxDepth           int
	MaxQualityReasons, MaxDescriptionBytes, MaxMetadataBytes int
}

func DefaultKOSRejectionLimits() KOSRejectionLimits {
	return KOSRejectionLimits{10000, 50000, 200000, 32, 16, 65536, 1 << 20}
}

func normalizeKOSLimits(l KOSRejectionLimits) (KOSRejectionLimits, error) {
	d := DefaultKOSRejectionLimits()
	fields := []*int{&l.MaxReferences, &l.MaxItems, &l.MaxElements, &l.MaxDepth, &l.MaxQualityReasons, &l.MaxDescriptionBytes, &l.MaxMetadataBytes}
	caps := []int{d.MaxReferences, d.MaxItems, d.MaxElements, d.MaxDepth, d.MaxQualityReasons, d.MaxDescriptionBytes, d.MaxMetadataBytes}
	for i, f := range fields {
		if *f < 0 || *f > caps[i] {
			return l, kosLimit("invalid limits")
		}
		if *f == 0 {
			*f = caps[i]
		}
	}
	return l, nil
}

func kosInvalid(reason string) error {
	return fmt.Errorf("%w: KOS rejection %s", ErrInvalidDocument, reason)
}
func kosLimit(reason string) error {
	return fmt.Errorf("%w: KOS rejection %s", ErrResourceLimitExceeded, reason)
}
func kosContext(ctx context.Context) error {
	if ctx == nil {
		return kosInvalid("nil context")
	}
	return ctx.Err()
}

type kosBudget struct {
	ctx                    context.Context
	limits                 KOSRejectionLimits
	elements, items, bytes int
}

func (b *kosBudget) addBytes(n int) error {
	if n < 0 || n > b.limits.MaxMetadataBytes-b.bytes {
		return kosLimit("metadata bytes")
	}
	b.bytes += n
	return nil
}

// Inspect raw DataSets before GetSequence constructs facades/maps. Repeated DAG
// branches are charged repeatedly. Unused root bulk payloads are skipped.
func (b *kosBudget) walk(elements []core.Element, depth int, source bool) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if depth > b.limits.MaxDepth {
		return kosLimit("depth")
	}
	if len(elements) > b.limits.MaxElements-b.elements {
		return kosLimit("elements")
	}
	b.elements += len(elements)
	seen := make(map[core.Tag]struct{}, len(elements))
	for _, e := range elements {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if _, duplicate := seen[e.Tag()]; duplicate {
			return kosInvalid("duplicate item attribute")
		}
		seen[e.Tag()] = struct{}{}
		if source && depth == 0 && kosBulkTag(e.Tag()) {
			continue
		}
		if seq, ok := e.Value.(core.SequenceValue); ok {
			if e.VR() != core.VRSQ {
				return kosInvalid("sequence VR")
			}
			if len(seq.Items) > b.limits.MaxItems-b.items {
				return kosLimit("items")
			}
			b.items += len(seq.Items)
			for _, item := range seq.Items {
				if err := b.walk(item.Elements, depth+1, source); err != nil {
					return err
				}
			}
			continue
		}
		if e.VR() == core.VRSQ {
			return kosInvalid("unavailable sequence")
		}
		if e.Value == nil {
			if !e.Header.HasLength() || e.Length() != 0 {
				return kosInvalid("deferred metadata; materialize before use")
			}
			continue
		}
		var n int
		switch v := e.Value.(type) {
		case core.RawValue:
			n = len(v)
			// Charset decoding can expand encoded characters to UTF-8. Reserve a
			// conservative four bytes per input byte before calling text helpers.
			if e.VR().UsesSpecificCharacterSet() {
				if n > (b.limits.MaxMetadataBytes-b.bytes)/4 {
					return kosLimit("decoded metadata bytes")
				}
				n *= 4
			}
		case core.StringValue:
			if len(v) > b.limits.MaxItems {
				return kosLimit("value multiplicity")
			}
			if len(v) > 0 {
				n = len(v) - 1
			}
			for _, s := range v {
				if len(s) > b.limits.MaxMetadataBytes-n {
					return kosLimit("metadata bytes")
				}
				n += len(s)
			}
		case core.Uint16Value:
			n = kosTypedBytes(len(v), 2, b.limits.MaxMetadataBytes)
		case core.Int16Value:
			n = kosTypedBytes(len(v), 2, b.limits.MaxMetadataBytes)
		case core.Uint32Value:
			n = kosTypedBytes(len(v), 4, b.limits.MaxMetadataBytes)
		case core.Int32Value:
			n = kosTypedBytes(len(v), 4, b.limits.MaxMetadataBytes)
		case core.Uint64Value:
			n = kosTypedBytes(len(v), 8, b.limits.MaxMetadataBytes)
		case core.Int64Value:
			n = kosTypedBytes(len(v), 8, b.limits.MaxMetadataBytes)
		case core.Float32Value:
			n = kosTypedBytes(len(v), 4, b.limits.MaxMetadataBytes)
		case core.Float64Value:
			n = kosTypedBytes(len(v), 8, b.limits.MaxMetadataBytes)
		case core.TagValue:
			n = kosTypedBytes(len(v), 4, b.limits.MaxMetadataBytes)
		default:
			return kosInvalid("unavailable metadata payload")
		}
		if err := b.addBytes(n); err != nil {
			return err
		}
	}
	return nil
}

func kosTypedBytes(count, width, cap int) int {
	if count > cap/width {
		return -1
	}
	return count * width
}

func kosBulkTag(t core.Tag) bool {
	return t == core.TagPixelData || t == core.NewTag(0x7fe0, 0x0008) || t == core.NewTag(0x7fe0, 0x0009) || t == core.NewTag(0x5600, 0x0020)
}

// Object.Elements may compact its order cache. Serialize this short extraction
// across our calls; all charset/sequence caches are then private to the facade.
// Callers still must not mutate/use the source through other Object APIs while
// this API snapshots it. Values remain borrowed and immutable during the call.
var kosSnapshotGate = make(chan struct{}, 1)

func kosFacade(ctx context.Context, src *object.Object, b *kosBudget, common bool) (*object.Object, error) {
	if src == nil {
		return nil, ErrNoDataset
	}
	select {
	case kosSnapshotGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-kosSnapshotGate }()
	if src.Len() > b.limits.MaxElements-b.elements {
		return nil, kosLimit("elements")
	}
	elems := src.Elements()
	if err := b.walk(elems, 0, common); err != nil {
		return nil, err
	}
	view := object.FromElements(elems, nil)
	view.SetValueByteOrder(src.ValueByteOrder())
	return view, nil
}

func kosString(o *object.Object, t core.Tag, vr core.VR, required bool) (string, error) {
	e, ok := o.Get(t)
	if !ok {
		if required {
			return "", kosInvalid("missing attribute")
		}
		return "", nil
	}
	if e.VR() != vr {
		return "", kosInvalid("attribute VR")
	}
	values, err := o.LookupStrings(t)
	if err != nil || len(values) > 1 || required && len(values) != 1 {
		return "", kosInvalid("attribute VM or text")
	}
	if len(values) == 0 {
		return "", nil
	}
	s := values[0]
	if !kosTextClean(s) || required && s == "" {
		return "", kosInvalid("attribute text")
	}
	return s, nil
}

func kosBoundSequence(o *object.Object, t core.Tag, min, max int) ([]*object.Object, error) {
	if e, ok := o.Get(t); ok {
		if v, ok := e.Value.(core.SequenceValue); ok && len(v.Items) > max {
			return nil, kosLimit("reference sequence")
		}
	}
	return kosSequence(o, t, min, max)
}

func kosSequence(o *object.Object, t core.Tag, min, max int) ([]*object.Object, error) {
	e, ok := o.Get(t)
	if !ok || e.VR() != core.VRSQ {
		return nil, kosInvalid("missing sequence or VR")
	}
	v, ok := e.Value.(core.SequenceValue)
	if !ok || len(v.Items) < min || len(v.Items) > max {
		return nil, kosInvalid("sequence cardinality")
	}
	items, ok := o.GetSequence(t)
	if !ok {
		return nil, kosInvalid("unavailable sequence")
	}
	return items, nil
}

func kosCode(o *object.Object, tag core.Tag) (CodedEntry, error) {
	items, err := kosSequence(o, tag, 1, 1)
	if err != nil {
		return CodedEntry{}, err
	}
	it := items[0]
	expected := 3
	if it.Has(tagCodingSchemeVersion) {
		expected++
	}
	if it.Len() != expected || it.Has(tagLongCodeValue) || it.Has(tagURNCodeValue) || it.Has(tagEquivalentCodeSequence) {
		return CodedEntry{}, kosInvalid("unsupported code form or context")
	}
	value, e1 := kosString(it, tagCodeValue, core.VRSH, true)
	scheme, e2 := kosString(it, tagCodingScheme, core.VRSH, true)
	meaning, e3 := kosString(it, tagCodeMeaning, core.VRLO, true)
	c := CodedEntry{CodeValue: value, CodingSchemeDesignator: scheme, CodeMeaning: meaning}
	if e1 != nil || e2 != nil || e3 != nil || !kosValidCode(c) {
		return c, kosInvalid("invalid code")
	}
	if _, err := kosString(it, tagCodingSchemeVersion, core.VRSH, false); err != nil {
		return c, err
	}
	return c, nil
}

func kosValidCode(c CodedEntry) bool {
	return ValidCode(c, CodeContext{}) && utf8.ValidString(c.CodeValue) && utf8.ValidString(c.CodingSchemeDesignator) && utf8.ValidString(c.CodeMeaning) &&
		utf8.RuneCountInString(c.CodeValue) <= 16 && utf8.RuneCountInString(c.CodingSchemeDesignator) <= 16 && utf8.RuneCountInString(c.CodeMeaning) <= 64
}

func kosQualityCode(c CodedEntry) bool {
	if !kosValidCode(c) {
		return false
	}
	g, ok := codes.ContextGroupByID(7011)
	if !ok {
		return false
	}
	_, ok = g.Member(codes.Code{Value: c.CodeValue, Scheme: c.CodingSchemeDesignator})
	return ok
}

func kosReferenceValid(r KOSRejectionReference) bool {
	if r.ValueType != ValueImage && r.ValueType != ValueWaveform && r.ValueType != ValueComposite {
		return false
	}
	for _, s := range []string{r.StudyInstanceUID, r.SeriesInstanceUID, r.SOPClassUID, r.SOPInstanceUID} {
		if !core.IsValidUID(s) {
			return false
		}
	}
	if r.RetrieveURL != "" && !validRetrieveURL(r.RetrieveURL) {
		return false
	}
	c, ok := uid.LookupStorageSOPClass(r.SOPClassUID)
	if !ok {
		return false
	}
	switch c.Category {
	case uid.StorageCategoryNonPatient, uid.StorageCategoryProtocol, uid.StorageCategorySecurity, uid.StorageCategoryNDT, uid.StorageCategoryLegacy:
		return false
	}
	// The caller declares the referenced IOD's role; a category/modality is not
	// an image-IOD classifier (e.g. MR Spectroscopy is catalogued as image).
	return true
}

func kosSameCode(a CodedEntry, value string) bool {
	return a.CodingSchemeDesignator == "DCM" && a.CodeValue == value
}

// UIDs are globally unique across semantic roles. Repeated identities within
// the same role are allowed; reference-tuple conflicts are checked separately.
func kosIdentityRoles(roles map[string]uint8, study, series, class, instance string) bool {
	for i, s := range []string{study, series, class, instance} {
		role := uint8(i + 1)
		if old, ok := roles[s]; ok && old != role {
			return false
		}
		roles[s] = role
	}
	return true
}

var kosRegistryOnce sync.Once
var kosRegistry *TemplateRegistry
var kosRegistryErr error

func kosValidateTemplate(ctx context.Context, doc *Document, l KOSRejectionLimits) error {
	kosRegistryOnce.Do(func() { kosRegistry, kosRegistryErr = NewStandardTemplateRegistry(2010) })
	if kosRegistryErr != nil {
		return kosRegistryErr
	}
	v, err := NewTemplateValidator(kosRegistry, TemplateValidationOptions{Mode: ValidationModeStrict, MaxDepth: l.MaxDepth, MaxItems: l.MaxItems, MaxSteps: 1000000, MaxFindings: 64})
	if err != nil {
		return err
	}
	key, _ := StandardTemplateKey(2010)
	_, err = v.Validate(ctx, doc, key)
	return err
}

func kosPositiveIS(s string) bool {
	if len(s) == 0 || len(s) > 12 {
		return false
	}
	positive := false
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		positive = positive || c != '0'
	}
	return positive
}

// InheritCommonModules owns its copied structure. Complete ownership for the
// additional typed slices its generic fallback can keep shared (UV/SV/AT).
func kosOwnExtendedValues(elements []core.Element) {
	for i := range elements {
		switch v := elements[i].Value.(type) {
		case core.Uint64Value:
			elements[i].Value = append(core.Uint64Value(nil), v...)
		case core.Int64Value:
			elements[i].Value = append(core.Int64Value(nil), v...)
		case core.TagValue:
			elements[i].Value = append(core.TagValue(nil), v...)
		case core.SequenceValue:
			for j := range v.Items {
				kosOwnExtendedValues(v.Items[j].Elements)
			}
		}
	}
}

func kosNoSelectors(o *object.Object) bool {
	return !o.Has(tagRefFrameNumber) && !o.Has(tagReferencedWaveformCh) && !o.Has(core.NewTag(0x0062, 0x000b)) && !o.Has(core.NewTag(0x0040, 0xa170))
}

func kosTextClean(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, '\x00') }
