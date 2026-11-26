package sr

import (
	"github.com/ThalesMMS/dicom-go/core"
	"math"
)

type measurementAuthoredNode struct {
	item           ContentItem
	code           ContentItemCodeContexts
	identification *TemplateIdentification
	numeric        *NumericRepresentation
	reference      *MeasurementReferenceMetadataEntry
	children       []measurementAuthoredNode
}
type measurementContextAuthor struct {
	options  MeasurementContextOptions
	evidence []MeasurementReferenceEvidence
	count    int
}

func (a *measurementContextAuthor) reserve(n int) error {
	if n < 0 || n > a.options.Read.MaxItems-a.count {
		return measurementContextError(MeasurementContextLimit, "")
	}
	a.count += n
	return nil
}
func (a *measurementContextAuthor) scalar(vt ValueType, rel string, concept CodedEntry, text string) (measurementAuthoredNode, error) {
	if err := a.reserve(1); err != nil {
		return measurementAuthoredNode{}, err
	}
	if len(text) > a.options.MaxStringBytes {
		return measurementAuthoredNode{}, measurementContextError(MeasurementContextLimit, "")
	}
	return measurementAuthoredNode{item: ContentItem{ValueType: vt, RelationshipType: rel, ConceptName: concept, Text: text}}, nil
}
func (a *measurementContextAuthor) code(rel string, concept CodedEntry, value MeasurementContextCode) (measurementAuthoredNode, error) {
	if err := a.reserve(1); err != nil {
		return measurementAuthoredNode{}, err
	}
	if !validCodeKey(codeKeyForEntry(value.Code, value.Context)) || !validCodeContextShape(value.Context) || !validEquivalentCodeEntries(value.EquivalentCodes) {
		return measurementAuthoredNode{}, measurementContextError(MeasurementContextShape, "")
	}
	return measurementAuthoredNode{item: ContentItem{ValueType: ValueCode, RelationshipType: rel, ConceptName: concept, Code: value.Code}, code: ContentItemCodeContexts{Value: value.Context, ValueEquivalentCodes: append([]EquivalentCodeEntry(nil), value.EquivalentCodes...)}}, nil
}
func (a *measurementContextAuthor) algorithm(v *MeasurementAlgorithm) ([]measurementAuthoredNode, error) {
	if v == nil {
		return nil, nil
	}
	if v.Name == "" || v.Version == "" {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	out := []measurementAuthoredNode{}
	addText := func(concept CodedEntry, text string) error {
		n, err := a.scalar(ValueText, RelationshipHasConceptMod, concept, text)
		if err == nil {
			out = append(out, n)
		}
		return err
	}
	if err := addText(contextAlgorithmName, v.Name); err != nil {
		return nil, err
	}
	if v.NameCode != nil {
		n, err := a.code(RelationshipHasConceptMod, contextAlgorithmName, *v.NameCode)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := addText(contextAlgorithmVersion, v.Version); err != nil {
		return nil, err
	}
	if v.Manufacturer != "" {
		if err := addText(contextAlgorithmManufacturer, v.Manufacturer); err != nil {
			return nil, err
		}
	}
	for _, parameter := range v.Parameters {
		if parameter == "" {
			return nil, measurementContextError(MeasurementContextShape, "")
		}
		if err := addText(contextAlgorithmParameters, parameter); err != nil {
			return nil, err
		}
	}
	if v.Family != nil {
		n, err := a.code(RelationshipHasConceptMod, contextAlgorithmFamily, *v.Family)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
func (a *measurementContextAuthor) sites(sites []MeasurementFindingSite) ([]measurementAuthoredNode, error) {
	if len(sites) > a.options.Read.MaxItems-a.count {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	out := make([]measurementAuthoredNode, 0, len(sites))
	for _, site := range sites {
		n, err := a.code(RelationshipHasConceptMod, findingSiteConcept, site.Site)
		if err != nil {
			return nil, err
		}
		for _, field := range []struct {
			concept CodedEntry
			value   *MeasurementContextCode
		}{{contextLaterality, site.Laterality}, {contextTopographical, site.TopographicalModifier}} {
			if field.value != nil {
				child, err := a.code(RelationshipHasConceptMod, field.concept, *field.value)
				if err != nil {
					return nil, err
				}
				n.children = append(n.children, child)
			}
		}
		out = append(out, n)
	}
	return out, nil
}
func (a *measurementContextAuthor) rwvm(e *MeasurementReferenceEvidence, rel string) ([]measurementAuthoredNode, error) {
	if e == nil {
		return nil, nil
	}
	if e.SOPClassUID != MeasurementRWVMStorage {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	admitted, err := admitMeasurementEvidence([]MeasurementReferenceEvidence{*e}, a.options)
	if err != nil {
		return nil, err
	}
	if len(a.evidence) >= a.options.MaxEvidence {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	a.evidence = append(a.evidence, admitted...)
	if err = a.reserve(1); err != nil {
		return nil, err
	}
	return []measurementAuthoredNode{{item: ContentItem{ValueType: ValueComposite, RelationshipType: rel, ConceptName: contextRWVM, Composite: CompositeReference{SOPClassUID: e.SOPClassUID, SOPInstanceUID: e.SOPInstanceUID}}}}, nil
}
func (a *measurementContextAuthor) timePoint(t *MeasurementTimePoint) ([]measurementAuthoredNode, error) {
	if t == nil {
		return nil, nil
	}
	if t.TimePoint == "" || t.TemporalOffset == nil && t.EventType != nil || t.TemporalOffset != nil && t.EventType == nil {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	out := []measurementAuthoredNode{}
	for _, field := range []struct {
		concept CodedEntry
		value   string
	}{{contextSubjectTimePoint, t.SubjectIdentifier}, {contextProtocolTimePoint, t.ProtocolIdentifier}, {contextTimePoint, t.TimePoint}} {
		if field.value != "" {
			n, err := a.scalar(ValueText, RelationshipHasObsContext, field.concept, field.value)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
	}
	for _, value := range t.Types {
		n, err := a.code(RelationshipHasObsContext, contextTimePointType, value)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	for _, field := range []struct {
		concept CodedEntry
		unit    string
		numeric *NumericRepresentation
	}{{contextTimePointOrder, "1", t.Order}, {contextTemporalOffset, "d", t.TemporalOffset}} {
		if field.numeric != nil {
			value, _, err := field.numeric.Resolve()
			if err != nil {
				return nil, err
			}
			if err = a.reserve(1); err != nil {
				return nil, err
			}
			numeric := *field.numeric
			n := measurementAuthoredNode{item: ContentItem{ValueType: ValueNum, RelationshipType: RelationshipHasObsContext, ConceptName: field.concept, Measurement: &Measurement{Value: value, Units: measurementContextConcept(field.unit, "UCUM", field.unit)}}, numeric: &numeric}
			if field.unit == "d" {
				event, err := a.code(RelationshipHasConceptMod, contextEventType, *t.EventType)
				if err != nil {
					return nil, err
				}
				n.children = append(n.children, event)
			}
			out = append(out, n)
		}
	}
	return out, nil
}
func (a *measurementContextAuthor) qualitative(input []MeasurementQualitativeEvaluation) ([]measurementAuthoredNode, error) {
	if len(input) > a.options.Read.MaxItems-a.count {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	out := make([]measurementAuthoredNode, 0, len(input))
	for _, q := range input {
		if !validCodeKey(codeKeyForEntry(q.ConceptName.Code, q.ConceptName.Context)) || !validCodeContextShape(q.ConceptName.Context) || !validEquivalentCodeEntries(q.ConceptName.EquivalentCodes) {
			return nil, measurementContextError(MeasurementContextShape, "")
		}
		var n measurementAuthoredNode
		var err error
		switch q.ValueType {
		case ValueCode:
			if q.Text != "" {
				return nil, measurementContextError(MeasurementContextConflict, "")
			}
			n, err = a.code(RelationshipContains, q.ConceptName.Code, q.Code)
		case ValueText:
			if !q.Code.Code.Empty() || !codeContextEmpty(q.Code.Context) || len(q.Code.EquivalentCodes) > 0 || len(q.Modifiers) > 0 || q.Text == "" {
				return nil, measurementContextError(MeasurementContextConflict, "")
			}
			n, err = a.scalar(ValueText, RelationshipContains, q.ConceptName.Code, q.Text)
		default:
			return nil, measurementContextError(MeasurementContextShape, "")
		}
		if err != nil {
			return nil, err
		}
		n.code.ConceptName = q.ConceptName.Context
		n.code.ConceptNameEquivalentCodes = append([]EquivalentCodeEntry(nil), q.ConceptName.EquivalentCodes...)
		for _, modifier := range q.Modifiers {
			child, err := a.code(RelationshipHasConceptMod, modifier.ConceptName.Code, modifier.Value)
			if err != nil {
				return nil, err
			}
			child.code.ConceptName = modifier.ConceptName.Context
			child.code.ConceptNameEquivalentCodes = append([]EquivalentCodeEntry(nil), modifier.ConceptName.EquivalentCodes...)
			n.children = append(n.children, child)
		}
		out = append(out, n)
	}
	return out, nil
}
func (a *measurementContextAuthor) numericContext(n *measurementAuthoredNode, c MeasurementNumericContext, template string) error {
	if c.Path != "" {
		return measurementContextError(MeasurementContextConflict, "")
	}
	if c.Numeric != nil {
		value, _, err := c.Numeric.Resolve()
		if err != nil {
			return err
		}
		if math.Float64bits(value) != math.Float64bits(n.item.Measurement.Value) {
			return measurementContextError(MeasurementContextConflict, "")
		}
		copy := *c.Numeric
		n.numeric = &copy
	}
	for _, field := range []struct {
		got   MeasurementContextCode
		want  CodedEntry
		units bool
	}{{c.ConceptName, n.item.ConceptName, false}, {c.Units, n.item.Measurement.Units, true}} {
		if !field.got.Code.Empty() || !codeContextEmpty(field.got.Context) || len(field.got.EquivalentCodes) > 0 {
			if codeKeyForEntry(field.got.Code, CodeContext{}) != codeKeyForEntry(field.want, CodeContext{}) {
				return measurementContextError(MeasurementContextConflict, "")
			}
			if field.units {
				n.code.MeasurementUnits = field.got.Context
				n.code.MeasurementUnitsEquivalentCodes = append([]EquivalentCodeEntry(nil), field.got.EquivalentCodes...)
			} else {
				n.code.ConceptName = field.got.Context
				n.code.ConceptNameEquivalentCodes = append([]EquivalentCodeEntry(nil), field.got.EquivalentCodes...)
			}
		}
	}
	var prefix []measurementAuthoredNode
	if c.Method != nil {
		v, err := a.code(RelationshipHasConceptMod, contextMethod, *c.Method)
		if err != nil {
			return err
		}
		prefix = append(prefix, v)
	}
	sites, err := a.sites(c.FindingSites)
	if err != nil {
		return err
	}
	prefix = append(prefix, sites...)
	// Preserve the legacy derivation before per-measurement site rows.
	var derivation, rest []measurementAuthoredNode
	for _, child := range n.children {
		if child.item.ConceptName == derivationConcept {
			derivation = append(derivation, child)
		} else {
			rest = append(rest, child)
		}
	}
	if len(derivation) > 0 {
		prefix = append(prefix[:len(prefix)-len(sites)], append(derivation, sites...)...)
	}
	n.children = append(prefix, rest...)
	// Per-NUM references use the legal SR IOD form. Projection records the
	// explicit TID 1419 row 19 seam for 1411 groups.
	rwvm, err := a.rwvm(c.RealWorldValueMap, RelationshipInferredFrom)
	if err != nil {
		return err
	}
	n.children = append(n.children, rwvm...)
	algorithm, err := a.algorithm(c.Algorithm)
	if err != nil {
		return err
	}
	n.children = append(n.children, algorithm...)
	return nil
}
func (a *measurementContextAuthor) group(n *measurementAuthoredNode, c MeasurementGroupContext, legacy MeasurementGroup) error {
	if c.Path != "" || len(c.Measurements) > 0 && len(c.Measurements) != len(legacy.Measurements) {
		return measurementContextError(MeasurementContextConflict, "")
	}
	template := n.identification.Identifier
	sites := c.FindingSites
	if len(sites) > 0 && len(legacy.FindingSites) > 0 {
		if len(sites) != len(legacy.FindingSites) {
			return measurementContextError(MeasurementContextConflict, "")
		}
		for i := range sites {
			if codeKeyForEntry(sites[i].Site.Code, sites[i].Site.Context) != codeKeyForEntry(legacy.FindingSites[i], CodeContext{}) {
				return measurementContextError(MeasurementContextConflict, "")
			}
		}
	}
	// Partition freshly authored legacy rows; only this new author's nodes are
	// reordered. Read content is never reconstructed or reassociated this way.
	var tracking, finding, references, oldSites, numeric []measurementAuthoredNode
	for _, ch := range n.children {
		switch {
		case ch.item.ConceptName == trackingIdentifierConcept || ch.item.ConceptName == trackingUniqueIdentifierConcept:
			tracking = append(tracking, ch)
		case ch.item.ConceptName == findingConcept:
			finding = append(finding, ch)
		case ch.item.ConceptName == findingSiteConcept:
			oldSites = append(oldSites, ch)
		case ch.item.ValueType == ValueNum:
			numeric = append(numeric, ch)
		default:
			references = append(references, ch)
		}
	}
	if len(numeric) != len(legacy.Measurements) {
		return measurementContextError(MeasurementContextConflict, "")
	}
	for i := range numeric {
		if len(c.Measurements) > 0 {
			if err := a.numericContext(&numeric[i], c.Measurements[i], template); err != nil {
				return err
			}
		}
	}
	children := []measurementAuthoredNode{}
	if c.ActivitySession != "" {
		v, err := a.scalar(ValueText, RelationshipHasObsContext, contextActivitySession, c.ActivitySession)
		if err != nil {
			return err
		}
		children = append(children, v)
	}
	children = append(children, tracking...)
	children = append(children, finding...)
	timePoint, err := a.timePoint(c.TimePoint)
	if err != nil {
		return err
	}
	children = append(children, timePoint...)
	if template == "1411" {
		children = append(children, references...)
	}
	if c.Method != nil {
		v, err := a.code(RelationshipHasConceptMod, contextMethod, *c.Method)
		if err != nil {
			return err
		}
		children = append(children, v)
	}
	if len(sites) > 0 {
		v, err := a.sites(sites)
		if err != nil {
			return err
		}
		children = append(children, v...)
	} else {
		children = append(children, oldSites...)
	}
	rwvm, err := a.rwvm(c.RealWorldValueMap, RelationshipContains)
	if err != nil {
		return err
	}
	children = append(children, rwvm...)
	algorithm, err := a.algorithm(c.Algorithm)
	if err != nil {
		return err
	}
	children = append(children, algorithm...)
	if template == "1501" {
		children = append(children, references...)
	}
	children = append(children, numeric...)
	qualitative, err := a.qualitative(c.Qualitative)
	if err != nil {
		return err
	}
	children = append(children, qualitative...)
	n.children = children
	return nil
}

// BuildMeasurementReportWithContext authors a fresh tree from the legacy
// primitives, adds explicit typed context and binds all metadata once at final
// ordinals. Existing source trees are handled by Read, never this constructor.
func BuildMeasurementReportWithContext(report *MeasurementReport, c MeasurementReportContext, options MeasurementContextOptions) (*DetailedMeasurementReport, error) {
	o, err := normalizeMeasurementContextOptions(options)
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	if len(c.Groups) > 0 && len(c.Groups) != len(report.Groups) {
		return nil, measurementContextError(MeasurementContextConflict, "")
	}
	count := len(report.Groups) + len(report.Observers) + len(report.ProceduresReported) + len(report.ImageLibrary)
	if count > o.Read.MaxItems || len(report.ImageLibrary) > o.MaxEvidence {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	for _, g := range report.Groups {
		if len(g.Measurements) > o.Read.MaxItems-count || len(g.FindingSites) > o.Read.MaxItems-count-len(g.Measurements) {
			return nil, measurementContextError(MeasurementContextLimit, "")
		}
		count += len(g.Measurements) + len(g.FindingSites)
	}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	file.Dataset.Put(strElem(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 192"))
	evidence, err := readMeasurementEvidence(file.Dataset, o)
	if err != nil {
		return nil, err
	}
	refs, err := readMeasurementReferenceMetadata(file.Dataset, o)
	if err != nil {
		return nil, err
	}
	result, numeric, err := readDocumentNumericRepresentations(file.Dataset, o.Read)
	if err != nil {
		return nil, err
	}
	index, err := newTemplateMetadataIndex(result.TemplateMetadata)
	if err != nil {
		return nil, err
	}
	numbers := map[string]NumericRepresentation{}
	for _, e := range numeric.Entries {
		numbers[e.Path] = e.Representation
	}
	details := map[string]MeasurementReferenceMetadataEntry{}
	for _, e := range refs.Entries {
		details[e.Path] = e
	}
	a := &measurementContextAuthor{options: o, evidence: evidence}
	var annotate func([]ContentItem, ContentItemIdentifier) ([]measurementAuthoredNode, error)
	annotate = func(items []ContentItem, parent ContentItemIdentifier) ([]measurementAuthoredNode, error) {
		if err := a.reserve(len(items)); err != nil {
			return nil, err
		}
		out := make([]measurementAuthoredNode, len(items))
		for i := range items {
			path := appendPath(parent, uint32(i+1))
			key := path.String()
			v := items[i]
			v.Children = nil
			n := measurementAuthoredNode{item: v, code: index.code[key]}
			if id, ok := index.identification[key]; ok {
				n.identification = &id
			}
			if numeric, ok := numbers[key]; ok {
				n.numeric = &numeric
			}
			if ref, ok := details[key]; ok {
				n.reference = &ref
			}
			children, err := annotate(items[i].Children, path)
			if err != nil {
				return nil, err
			}
			n.children = children
			out[i] = n
		}
		return out, nil
	}
	tree, err := annotate(result.Document.Content, ContentItemIdentifier{1})
	if err != nil {
		return nil, err
	}
	for i := range tree {
		if tree[i].item.ConceptName == imagingMeasurementsConcept {
			if len(tree[i].children) != len(report.Groups) {
				return nil, measurementContextError(MeasurementContextConflict, "")
			}
			for j := range tree[i].children {
				gc := MeasurementGroupContext{}
				if len(c.Groups) > 0 {
					gc = c.Groups[j]
				}
				if err = a.group(&tree[i].children[j], gc, report.Groups[j]); err != nil {
					return nil, err
				}
			}
			algorithm, err := a.algorithm(c.ImagingAlgorithm)
			if err != nil {
				return nil, err
			}
			tree[i].children = append(algorithm, tree[i].children...)
		}
	}
	if len(c.Qualitative) > 0 || c.QualitativeAlgorithm != nil {
		if err = a.reserve(1); err != nil {
			return nil, err
		}
		q := measurementAuthoredNode{item: ContentItem{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: qualitativeEvaluationsConcept, ContinuityOfContent: "SEPARATE"}}
		algorithm, err := a.algorithm(c.QualitativeAlgorithm)
		if err != nil {
			return nil, err
		}
		rows, err := a.qualitative(c.Qualitative)
		if err != nil {
			return nil, err
		}
		q.children = append(algorithm, rows...)
		tree = append(tree, q)
	}
	metadata := TemplateValidationMetadata{}
	if id, ok := index.identification["1"]; ok {
		metadata.TemplateIdentifications = append(metadata.TemplateIdentifications, id)
	}
	if cc, ok := index.code["1"]; ok {
		metadata.CodeContexts = append(metadata.CodeContexts, cc)
	}
	finalNumeric := NumericRepresentationMetadata{}
	finalRefs := MeasurementReferenceMetadata{}
	var flatten func([]measurementAuthoredNode, ContentItemIdentifier) []ContentItem
	flatten = func(nodes []measurementAuthoredNode, parent ContentItemIdentifier) []ContentItem {
		out := make([]ContentItem, len(nodes))
		for i, n := range nodes {
			path := appendPath(parent, uint32(i+1))
			key := path.String()
			out[i] = n.item
			out[i].Children = flatten(n.children, path)
			cc := n.code
			cc.Path = key
			if !codeContextsEmpty(cc) {
				metadata.CodeContexts = append(metadata.CodeContexts, cc)
			}
			if n.identification != nil {
				id := *n.identification
				id.Path = key
				metadata.TemplateIdentifications = append(metadata.TemplateIdentifications, id)
			}
			if n.numeric != nil {
				finalNumeric.Entries = append(finalNumeric.Entries, NumericRepresentationEntry{Path: key, Representation: *n.numeric})
			}
			if n.reference != nil {
				ref := *n.reference
				ref.Path = key
				finalRefs.Entries = append(finalRefs.Entries, ref)
			}
		}
		return out
	}
	result.Document.Content = flatten(tree, ContentItemIdentifier{1})
	result.Document.Common = report.Common
	snapshot, err := BindMeasurementContextMetadata(result.Document, metadata, finalNumeric, finalRefs, o.Snapshot)
	if err != nil {
		return nil, err
	}
	o.CharacterSet = []string{"ISO_IR 192"}
	o.ValueElementsBigEndian = false
	r, err := BindMeasurementReportWithContext(result.Document, snapshot, a.evidence, o)
	if err != nil {
		return nil, err
	}
	r.frameOfReferenceUID = report.FrameOfReferenceUID
	return r, nil
}
