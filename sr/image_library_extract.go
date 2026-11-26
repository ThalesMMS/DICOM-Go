package sr

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ImageLibraryExtractionFinding is value-free; Template/Row locate a selected
// normative descriptor. Absence is not a zero, a default or clinical inference.
type ImageLibraryExtractionFinding struct {
	Template    int
	Row, Reason string
}
type ImageLibraryExtraction struct {
	Entry ImageLibraryEntry
	// Metadata paths are relative to the extracted IMAGE item, rooted at 1.
	Metadata TemplateValidationMetadata
	Numeric  NumericRepresentationMetadata
	Findings []ImageLibraryExtractionFinding
}
type librarySourceRule struct {
	tid                     int
	row                     string
	tag                     core.Tag
	component, multiplicity int
	unit                    string
	sequence                core.Tag
}

func libraryTag(g, e uint16) core.Tag { return core.NewTag(g, e) }

// Explicit source mappings; spacing DS order is row (vertical), column
// (horizontal). No pixel data is read. MR coil/signal/scan-plane NUM/CID seams
// and PET clinically derived glucose/timing/counts are not guessed from text.
var librarySourceRules = []librarySourceRule{
	{1602, "1", libraryTag(0x0008, 0x0060), 0, 1, "", core.Tag{}},
	{1602, "2", libraryTag(0x0008, 0x2218), 0, 1, "", core.Tag{}},
	{1602, "2b", libraryTag(0x0018, 0x0015), 0, 1, "", core.Tag{}},
	{1602, "3", libraryTag(0x0020, 0x0062), 0, 1, "", core.Tag{}},
	{1602, "4", libraryTag(0x0008, 0x0020), 0, 1, "", core.Tag{}},
	{1602, "5", libraryTag(0x0008, 0x0030), 0, 1, "", core.Tag{}},
	{1602, "5a", libraryTag(0x0020, 0x000e), 0, 1, "", core.Tag{}},
	{1602, "5b", libraryTag(0x0020, 0x0011), 0, 1, "", core.Tag{}},
	{1602, "5c", libraryTag(0x0008, 0x103e), 0, 1, "", core.Tag{}},
	{1602, "5e", libraryTag(0x0008, 0x0021), 0, 1, "", core.Tag{}},
	{1602, "5f", libraryTag(0x0008, 0x0031), 0, 1, "", core.Tag{}},
	{1602, "5g", libraryTag(0x0020, 0x1209), 0, 1, "{instances}", core.Tag{}},
	{1602, "6", libraryTag(0x0008, 0x0023), 0, 1, "", core.Tag{}},
	{1602, "7", libraryTag(0x0008, 0x0033), 0, 1, "", core.Tag{}},
	{1602, "8", libraryTag(0x0008, 0x0022), 0, 1, "", core.Tag{}},
	{1602, "9", libraryTag(0x0008, 0x0032), 0, 1, "", core.Tag{}},
	{1602, "10", libraryTag(0x0020, 0x0052), 0, 1, "", core.Tag{}},
	{1602, "11", libraryTag(0x0028, 0x0010), 0, 1, "{pixels}", core.Tag{}},
	{1602, "12", libraryTag(0x0028, 0x0011), 0, 1, "{pixels}", core.Tag{}},
	{1602, "12a", libraryTag(0x0020, 0x0013), 0, 1, "", core.Tag{}},
	{1602, "12b", libraryTag(0x0028, 0x0008), 0, 1, "{frames}", core.Tag{}},
	{1603, "1", libraryTag(0x0054, 0x0220), 0, 1, "", core.Tag{}},
	{1603, "3", libraryTag(0x0020, 0x0020), 0, 2, "", core.Tag{}},
	{1603, "4", libraryTag(0x0020, 0x0020), 1, 2, "", core.Tag{}},
	{1603, "5", libraryTag(0x0018, 0x1164), 1, 2, "mm", core.Tag{}},
	{1603, "6", libraryTag(0x0018, 0x1164), 0, 2, "mm", core.Tag{}},
	{1603, "7", libraryTag(0x0018, 0x1510), 0, 1, "deg", core.Tag{}},
	{1603, "8", libraryTag(0x0018, 0x1511), 0, 1, "deg", core.Tag{}},
	{1604, "1", libraryTag(0x0028, 0x0030), 1, 2, "mm", core.Tag{}},
	{1604, "2", libraryTag(0x0028, 0x0030), 0, 2, "mm", core.Tag{}},
	{1604, "3", libraryTag(0x0018, 0x0088), 0, 1, "mm", core.Tag{}},
	{1604, "4", libraryTag(0x0018, 0x0050), 0, 1, "mm", core.Tag{}},
	{1604, "5", libraryTag(0x0020, 0x0032), 0, 3, "mm", core.Tag{}},
	{1604, "6", libraryTag(0x0020, 0x0032), 1, 3, "mm", core.Tag{}},
	{1604, "7", libraryTag(0x0020, 0x0032), 2, 3, "mm", core.Tag{}},
	{1604, "8", libraryTag(0x0020, 0x0037), 0, 6, "{-1:1}", core.Tag{}},
	{1604, "9", libraryTag(0x0020, 0x0037), 1, 6, "{-1:1}", core.Tag{}},
	{1604, "10", libraryTag(0x0020, 0x0037), 2, 6, "{-1:1}", core.Tag{}},
	{1604, "11", libraryTag(0x0020, 0x0037), 3, 6, "{-1:1}", core.Tag{}},
	{1604, "12", libraryTag(0x0020, 0x0037), 4, 6, "{-1:1}", core.Tag{}},
	{1604, "13", libraryTag(0x0020, 0x0037), 5, 6, "{-1:1}", core.Tag{}},
	{1605, "1", libraryTag(0x0018, 0x9302), 0, 1, "", core.Tag{}},
	{1605, "2", libraryTag(0x0018, 0x9315), 0, 1, "", core.Tag{}},
	{1606, "1", libraryTag(0x0018, 0x9005), 0, 1, "", core.Tag{}},
	{1606, "2", libraryTag(0x0018, 0x0087), 0, 1, "T", core.Tag{}},
	{1606, "6", libraryTag(0x0018, 0x9087), 0, 1, "s/mm2", core.Tag{}},
	{1607, "1", libraryTag(0x0054, 0x0300), 0, 1, "", libraryTag(0x0054, 0x0016)},
	{1607, "2", libraryTag(0x0054, 0x0304), 0, 1, "", libraryTag(0x0054, 0x0016)},
	{1607, "3", libraryTag(0x0018, 0x1075), 0, 1, "s", libraryTag(0x0054, 0x0016)},
	{1607, "3b", libraryTag(0x0008, 0x0050), 0, 1, "", core.Tag{}},
	{1607, "4", libraryTag(0x0018, 0x1078), 0, 1, "", libraryTag(0x0054, 0x0016)},
	{1607, "5", libraryTag(0x0018, 0x1079), 0, 1, "", libraryTag(0x0054, 0x0016)},
	{1607, "6", libraryTag(0x0018, 0x1071), 0, 1, "cm3", libraryTag(0x0054, 0x0016)},
	{1607, "7", libraryTag(0x0018, 0x1074), 0, 1, "Bq", libraryTag(0x0054, 0x0016)},
}

func libraryCatalogRow(tid int, label string) (catalogRow, bool) {
	for _, t := range standardTemplateCatalog {
		if t.id == tid {
			for _, r := range t.rows {
				if r.label == label {
					return r, true
				}
			}
		}
	}
	return catalogRow{}, false
}

type librarySourceBudget struct {
	items   int
	bytes   int64
	options ImageLibraryOptions
	ctx     context.Context
}

func (b *librarySourceBudget) resident(e core.Element) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	b.items++
	if b.items > b.options.MaxItems {
		return imageLibraryError("resource-limit", "")
	}
	var n int64
	switch v := e.Value.(type) {
	case core.RawValue:
		n = int64(len(v))
	case core.StringValue:
		for _, s := range v {
			if int64(len(s)) > b.options.MaxValueBytes-b.bytes-n {
				return imageLibraryError("resource-limit", "")
			}
			n += int64(len(s))
		}
	case core.Uint16Value:
		n = int64(len(v)) * 2
	case core.Int16Value:
		n = int64(len(v)) * 2
	case core.Uint32Value:
		n = int64(len(v)) * 4
	case core.Int32Value:
		n = int64(len(v)) * 4
	case core.Uint64Value:
		n = int64(len(v)) * 8
	case core.Int64Value:
		n = int64(len(v)) * 8
	case core.TagValue:
		n = int64(len(v)) * 4
	case core.Float32Value:
		n = int64(len(v)) * 4
	case core.Float64Value:
		n = int64(len(v)) * 8
	case core.SequenceValue:
		if len(v.Items) > b.options.MaxItems-b.items {
			return imageLibraryError("resource-limit", "")
		}
	default:
		return imageLibraryError("nonresident-or-custom-value", "")
	}
	if n > b.options.MaxValueBytes-b.bytes {
		return imageLibraryError("resource-limit", "")
	}
	b.bytes += n
	return nil
}
func (b *librarySourceBudget) strings(obj *object.Object, tag core.Tag) ([]string, bool, error) {
	e, ok := obj.Get(tag)
	if !ok {
		return nil, false, nil
	}
	if err := b.resident(e); err != nil {
		return nil, true, err
	}
	v, err := obj.LookupStrings(tag)
	if err != nil {
		return nil, true, imageLibraryError("source-value", "")
	}
	return v, true, nil
}
func (b *librarySourceBudget) sequence(obj *object.Object, tag core.Tag) ([]*object.Object, bool, error) {
	e, ok := obj.Get(tag)
	if !ok {
		return nil, false, nil
	}
	if err := b.resident(e); err != nil {
		return nil, true, err
	}
	if e.VR() != core.VRSQ {
		return nil, true, imageLibraryError("source-sequence", "")
	}
	// Inspect concrete nested elements and bytes before GetSequence allocates
	// object facades or consults character sets for each item.
	seq, concrete := e.Value.(core.SequenceValue)
	if !concrete {
		return nil, true, imageLibraryError("source-sequence", "")
	}
	if err := b.sequenceElements(seq.Items, 1); err != nil {
		return nil, true, err
	}
	v, ok := obj.GetSequence(tag)
	if !ok {
		return nil, true, imageLibraryError("source-sequence", "")
	}
	return v, true, nil
}

// ExtractImageLibraryEntry is explicit, detached extraction of resident source
// attributes. It never resolves BulkData/deferred values, pixels, files or peers.
// Frame-local extraction requires one selected frame; conflicting root/shared/
// per-frame facts are rejected. Missing optional rows are reported by TID/row.
func ExtractImageLibraryEntry(ctx context.Context, source *object.Object, frames []int, options ImageLibraryOptions) (ImageLibraryExtraction, error) {
	var out ImageLibraryExtraction
	if ctx == nil || source == nil {
		return out, imageLibraryError("source-required", "")
	}
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return out, err
	}
	b := &librarySourceBudget{options: o, ctx: ctx}
	if e, ok := source.Get(libraryTag(0x0008, 0x0005)); ok {
		if err := b.resident(e); err != nil {
			return out, err
		}
	}
	if len(frames) > o.MaxItems {
		return out, imageLibraryError("resource-limit", "")
	}
	ref := ImageReference{Frames: append([]int(nil), frames...)}
	ids := []struct {
		tag core.Tag
		dst *string
	}{{libraryTag(0x0008, 0x0016), &ref.SOPClassUID}, {libraryTag(0x0008, 0x0018), &ref.SOPInstanceUID}, {libraryTag(0x0020, 0x000d), &ref.StudyInstanceUID}, {libraryTag(0x0020, 0x000e), &ref.SeriesInstanceUID}}
	for _, f := range ids {
		v, present, err := b.strings(source, f.tag)
		if err != nil {
			return out, err
		}
		if present {
			if len(v) != 1 || !validUID(v[0]) {
				return out, imageLibraryError("source-uid", "")
			}
			*f.dst = v[0]
		}
	}
	if !validUID(ref.SOPClassUID) || !validUID(ref.SOPInstanceUID) {
		return out, imageLibraryError("image-reference", "")
	}
	frameLimit := 0
	if values, present, err := b.strings(source, libraryTag(0x0028, 0x0008)); err != nil {
		return out, err
	} else if present {
		if len(values) != 1 {
			return out, imageLibraryError("source-multiplicity", "")
		}
		frameLimit, err = strconv.Atoi(values[0])
		if err != nil || frameLimit < 1 {
			return out, imageLibraryError("frame-number", "")
		}
	}
	for _, f := range frames {
		if f < 1 || (frameLimit > 0 && f > frameLimit) {
			return out, imageLibraryError("frame-number", "")
		}
	}
	v, present, err := b.strings(source, libraryTag(0x0008, 0x0060))
	if err != nil {
		return out, err
	}
	modality := ""
	if present {
		if len(v) != 1 || v[0] == "" {
			return out, imageLibraryError("source-modality", "")
		}
		modality = v[0]
	}
	// Only explicit standard acquisition macros are searched. No generic scan of
	// arbitrary nested content or pixel sequences is performed.
	scopes := []*object.Object{source}
	shared, present, err := b.sequence(source, libraryTag(0x5200, 0x9229))
	if err != nil {
		return out, err
	}
	if present {
		if len(shared) != 1 {
			return out, imageLibraryError("shared-cardinality", "")
		}
		scopes = append(scopes, shared[0])
	}
	per, present, err := b.sequence(source, libraryTag(0x5200, 0x9230))
	if err != nil {
		return out, err
	}
	if present {
		if len(frames) != 1 {
			return out, imageLibraryError("explicit-single-frame-required", "")
		}
		if frames[0] > len(per) {
			return out, imageLibraryError("frame-number", "")
		}
		scopes = append(scopes, per[frames[0]-1])
	}
	macros := []core.Tag{libraryTag(0x0028, 0x9110), libraryTag(0x0020, 0x9113), libraryTag(0x0020, 0x9116), libraryTag(0x0018, 0x9301), libraryTag(0x0018, 0x9314), libraryTag(0x0018, 0x9226), libraryTag(0x0018, 0x9117)}
	for _, scope := range append([]*object.Object(nil), scopes...) {
		for _, tag := range macros {
			items, p, err := b.sequence(scope, tag)
			if err != nil {
				return out, err
			}
			if p {
				if len(items) != 1 {
					return out, imageLibraryError("macro-cardinality", "")
				}
				scopes = append(scopes, items[0])
			}
		}
	}
	seen := map[string]bool{}
	for _, rule := range librarySourceRules {
		if !libraryApplies(rule.tid, modality) {
			continue
		}
		row, _ := libraryCatalogRow(rule.tid, rule.row)
		key := strconv.Itoa(rule.tid) + ":" + rule.row
		var found ContentItem
		var foundContext ContentItemCodeContexts
		foundAny := false
		for _, scope := range scopes {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			target := scope
			if rule.sequence != (core.Tag{}) {
				items, p, err := b.sequence(scope, rule.sequence)
				if err != nil {
					return out, err
				}
				if !p {
					continue
				}
				if len(items) != 1 {
					return out, imageLibraryError("radiopharmaceutical-cardinality", "")
				}
				target = items[0]
			}
			e, p := target.Get(rule.tag)
			if !p {
				continue
			}
			if err := b.resident(e); err != nil {
				return out, err
			}
			codeContext := ContentItemCodeContexts{}
			item := ContentItem{ValueType: row.valueType, RelationshipType: RelationshipHasAcqContext, ConceptName: FromCode(row.code)}
			if row.valueType == ValueNum {
				var nums []float64
				if e.VR() == core.VRUS || e.VR() == core.VRIS {
					ints, er := target.GetInts(rule.tag)
					if er != nil {
						return out, imageLibraryError("source-number", "")
					}
					for _, n := range ints {
						nums = append(nums, float64(n))
					}
				} else {
					nums, err = target.GetFloats(rule.tag)
					if err != nil {
						return out, imageLibraryError("source-number", "")
					}
				}
				if len(nums) != rule.multiplicity {
					return out, imageLibraryError("source-multiplicity", "")
				}
				n := nums[rule.component]
				if math.IsNaN(n) || math.IsInf(n, 0) {
					return out, imageLibraryError("source-number", "")
				}
				item.Measurement = &Measurement{n, CodedEntry{rule.unit, "UCUM", rule.unit}}
			} else if row.valueType == ValueCode && e.VR() == core.VRSQ {
				items, ok, err := b.sequence(target, rule.tag)
				if err != nil {
					return out, err
				}
				if !ok || len(items) != 1 {
					return out, imageLibraryError("code-cardinality", "")
				}
				// Code metadata, equivalent codes and long/URN identity use the
				// existing sidecar contract instead of silently flattening codes.
				dec := &decoder{maxItems: o.MaxItems}
				item.Code, codeContext.Value, codeContext.ValueEquivalentCodes, _ = decodeCodeWithMetadata(dec, target, rule.tag)
				if dec.err != nil || item.Code.Empty() {
					return out, imageLibraryError("code-value", "")
				}

			} else {
				strs, err := target.LookupStrings(rule.tag)
				if err != nil || len(strs) != rule.multiplicity {
					return out, imageLibraryError("source-multiplicity", "")
				}
				s := strs[rule.component]
				if s == "" {
					continue
				}
				switch row.valueType {
				case ValueText:
					item.Text = s
				case ValueDate:
					item.Date = s
				case ValueTime:
					item.Time = s
				case ValueDateTime:
					item.DateTime = s
				case ValueUIDRef:
					item.UID = s
				case ValueCode:
					code, ok := libraryDefinedTerm(rule.tid, rule.row, s)
					if !ok {
						out.Findings = append(out.Findings, ImageLibraryExtractionFinding{rule.tid, rule.row, "unmapped-defined-term"})
						continue
					}
					item.Code = code
				}
			}
			if foundAny && (!reflect.DeepEqual(found, item) || !reflect.DeepEqual(foundContext, codeContext)) {
				return out, imageLibraryError("source-conflict", "")
			}
			found, foundContext, foundAny = item, codeContext, true
		}
		if foundAny {
			out.Entry.Descriptors = append(out.Entry.Descriptors, found)
			path := "1." + strconv.Itoa(len(out.Entry.Descriptors))
			if !codeContextsEmpty(foundContext) {
				foundContext.Path = path
				out.Metadata.CodeContexts = append(out.Metadata.CodeContexts, foundContext)
			}
			if found.Measurement != nil {
				v := found.Measurement.Value
				decimal, err := object.FormatDS(v)
				if err != nil {
					return out, imageLibraryError("source-number", "")
				}
				out.Numeric.Entries = append(out.Numeric.Entries, NumericRepresentationEntry{Path: path, Representation: NumericRepresentation{DecimalString: decimal, HasDecimalString: true, FloatingPoint: v, HasFloatingPoint: true}})
			}
			seen[key] = true
		}
	}
	out.Entry.Image = ref
	for _, t := range standardTemplateCatalog {
		if t.id < 1602 || t.id > 1607 || !libraryApplies(t.id, modality) {
			continue
		}
		for _, r := range t.rows {
			if r.include != 0 || r.level > 0 {
				continue
			}
			key := strconv.Itoa(t.id) + ":" + r.label
			if !seen[key] {
				out.Findings = append(out.Findings, ImageLibraryExtractionFinding{t.id, r.label, "absent-or-not-directly-extractable"})
			}
		}
	}
	if err := ValidateImageLibrary(ctx, nil, []ImageLibraryGroup{{Entries: []ImageLibraryEntry{out.Entry}}}, o); err != nil {
		return ImageLibraryExtraction{}, err
	}
	return out, nil
}

func libraryDefinedTerm(tid int, row, term string) (CodedEntry, bool) {
	if tid == 1602 && row == "1" {
		return CodedEntry{term, "DCM", term}, true
	}
	if tid == 1602 && row == "3" {
		m := map[string]CodedEntry{"R": {"24028007", "SCT", "Right"}, "L": {"7771000", "SCT", "Left"}, "B": {"51440002", "SCT", "Bilateral"}, "U": {"66459002", "SCT", "Unilateral"}}
		v, ok := m[term]
		return v, ok
	}
	if tid == 1605 && row == "1" {
		m := map[string]CodedEntry{"SEQUENCED": {"113804", "DCM", "Sequenced Acquisition"}, "CONSTANT_ANGLE": {"113805", "DCM", "Constant Angle Acquisition"}, "STATIONARY": {"113806", "DCM", "Stationary Acquisition"}, "FREE": {"113807", "DCM", "Free Acquisition"}, "SPIRAL": {"116152004", "SCT", "Spiral Acquisition"}}
		v, ok := m[term]
		return v, ok
	}
	if tid == 1605 && row == "2" {
		m := map[string]CodedEntry{"FILTER_BACK_PROJ": {"113962", "DCM", "Filtered Back Projection"}, "ITERATIVE": {"113963", "DCM", "Iterative Reconstruction"}}
		v, ok := m[term]
		return v, ok
	}
	return CodedEntry{}, false
}

// PromoteImageLibraryCommonDescriptors hoists exact common acquisition context
// into each group. It never merges groups or synthesizes image metadata. Bound
// encoding contexts must be explicitly remapped/rebound by the caller first;
// refusing them prevents promotion of semantically different code versions.
func PromoteImageLibraryCommonDescriptors(ctx context.Context, report *MeasurementReport, options ImageLibraryOptions) ([]ImageLibraryGroup, error) {
	if report == nil {
		return nil, imageLibraryError("report-required", "")
	}
	if report.ImageLibraryMetadata != nil {
		return nil, imageLibraryError("explicit-metadata-remap-required", "")
	}
	if err := ValidateImageLibrary(ctx, report.ImageLibraryDescriptors, report.ImageLibraryGroups, options); err != nil {
		return nil, err
	}
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return nil, err
	}
	// Include caller-owned headers in clone admission, even fields not emitted
	// by their fixed CONTAINER/IMAGE representation.
	if err := preflightLibraryClone(ctx, report.ImageLibraryGroups, o); err != nil {
		return nil, err
	}
	if _, err := snapshotValueDigest(reflect.ValueOf(report.ImageLibraryGroups), &snapshotHashBudget{remaining: o.MaxValueBytes}); err != nil {
		return nil, imageLibraryError("resource-limit", "")
	}
	out, err := cloneImageLibraryGroups(ctx, report.ImageLibraryGroups)
	if err != nil {
		return nil, err
	}
	for gi, g := range out {
		if len(g.Entries) < 2 {
			continue
		}
		entries := append([]ImageLibraryEntry(nil), g.Entries...)
		common := []ContentItem{}
		for _, candidate := range g.Entries[0].Descriptors {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			all := true
			for _, entry := range g.Entries[1:] {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				found := false
				for _, d := range entry.Descriptors {
					if reflect.DeepEqual(candidate, d) {
						found = true
						break
					}
				}
				if !found {
					all = false
					break
				}
			}
			if all {
				already := false
				for _, d := range g.Descriptors {
					already = already || reflect.DeepEqual(candidate, d)
				}
				if !already {
					common = append(common, candidate)
				}
			}
		}
		for ei, e := range entries {
			descriptors := []ContentItem{}
			for _, d := range e.Descriptors {
				promoted := false
				for _, c := range common {
					promoted = promoted || reflect.DeepEqual(c, d)
				}
				if !promoted {
					descriptors = append(descriptors, d)
				}
			}
			entries[ei].Descriptors = descriptors
		}
		out[gi].Descriptors = append(append([]ContentItem(nil), g.Descriptors...), common...)
		out[gi].Entries = entries
		out[gi].Order = nil
	}
	return out, nil
}

// Preflight bytes and concrete values before any decoder getter, including
// metadata sub-sequences. Attribute count is a separate bounded expansion of
// the content-item allowance; no custom Value implementation is invoked.
func preflightLibraryObject(ctx context.Context, obj *object.Object, o ImageLibraryOptions) error {
	if obj == nil {
		return ErrNoDataset
	}
	budgetOptions := o
	budgetOptions.MaxItems *= 64
	b := &librarySourceBudget{options: budgetOptions, ctx: ctx}
	var visit func([]core.Element, int) error
	visit = func(elements []core.Element, depth int) error {
		if depth > o.MaxDepth*4+4 {
			return imageLibraryError("resource-limit", "")
		}
		if len(elements) > budgetOptions.MaxItems-b.items {
			return imageLibraryError("resource-limit", "")
		}
		for _, e := range elements {
			if err := b.resident(e); err != nil {
				return err
			}
			if seq, ok := e.Value.(core.SequenceValue); ok {
				for _, item := range seq.Items {
					if err := visit(item.Elements, depth+1); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if obj.Len() > budgetOptions.MaxItems {
		return imageLibraryError("resource-limit", "")
	}
	return visit(obj.Elements(), 0)
}

// MetadataAt explicitly relocates detached extraction metadata to an IMAGE
// item's full library-relative path before BindImageLibraryMetadata. The caller
// selects the group/entry; this function does not infer structural associations.
func (e ImageLibraryExtraction) MetadataAt(imagePath string) (TemplateValidationMetadata, NumericRepresentationMetadata, error) {
	if !validNumericPath(imagePath) {
		return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("metadata-path", "")
	}
	if len(e.Metadata.CodeContexts)+len(e.Metadata.TemplateIdentifications)+len(e.Numeric.Entries) > DefaultImageLibraryOptions().MaxItems {
		return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("resource-limit", "")
	}
	for _, c := range e.Metadata.CodeContexts {
		if !validNumericPath(c.Path) || (c.Path != "1" && !strings.HasPrefix(c.Path, "1.")) {
			return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("metadata-path", "")
		}
	}
	for _, n := range e.Numeric.Entries {
		if !validNumericPath(n.Path) || (n.Path != "1" && !strings.HasPrefix(n.Path, "1.")) {
			return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("metadata-path", "")
		}
	}
	for _, id := range e.Metadata.TemplateIdentifications {
		if !validNumericPath(id.Path) || (id.Path != "1" && !strings.HasPrefix(id.Path, "1.")) {
			return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("metadata-path", "")
		}
	}
	budget := &snapshotHashBudget{remaining: DefaultImageLibraryOptions().MaxValueBytes}
	if _, err := snapshotValueDigest(reflect.ValueOf(e.Metadata), budget); err != nil {
		return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("resource-limit", "")
	}
	if _, err := snapshotValueDigest(reflect.ValueOf(e.Numeric), budget); err != nil {
		return TemplateValidationMetadata{}, NumericRepresentationMetadata{}, imageLibraryError("resource-limit", "")
	}
	m := cloneTemplateValidationMetadata(e.Metadata)
	n := cloneNumericMetadata(e.Numeric)
	for i := range m.CodeContexts {
		m.CodeContexts[i].Path = imagePath + m.CodeContexts[i].Path[1:]
	}
	for i := range m.TemplateIdentifications {
		m.TemplateIdentifications[i].Path = imagePath + m.TemplateIdentifications[i].Path[1:]
	}
	for i := range n.Entries {
		n.Entries[i].Path = imagePath + n.Entries[i].Path[1:]
	}
	return m, n, nil
}

func (b *librarySourceBudget) sequenceElements(items []core.DataSet, depth int) error {
	if depth > b.options.MaxDepth*4+4 {
		return imageLibraryError("resource-limit", "")
	}
	for _, item := range items {
		if len(item.Elements) > b.options.MaxItems-b.items {
			return imageLibraryError("resource-limit", "")
		}
		for _, e := range item.Elements {
			if err := b.resident(e); err != nil {
				return err
			}
			if seq, ok := e.Value.(core.SequenceValue); ok {
				if err := b.sequenceElements(seq.Items, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
