package sr

import (
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/object"
	"math"
	"strings"
	"unicode/utf8"
)

var (
	contextActivitySession       = measurementContextConcept("C67447", "NCIt", "Activity Session")
	contextMethod                = measurementContextConcept("370129005", "SCT", "Measurement Method")
	contextLaterality            = measurementContextConcept("272741003", "SCT", "Laterality")
	contextTopographical         = measurementContextConcept("106233006", "SCT", "Topographical modifier")
	contextRWVM                  = measurementContextConcept("126100", "DCM", "Real World Value Map used for measurement")
	contextSubjectTimePoint      = measurementContextConcept("126070", "DCM", "Subject Time Point Identifier")
	contextProtocolTimePoint     = measurementContextConcept("126071", "DCM", "Protocol Time Point Identifier")
	contextTimePoint             = measurementContextConcept("C2348792", "UMLS", "Time Point")
	contextTimePointType         = measurementContextConcept("126072", "DCM", "Time Point Type")
	contextTimePointOrder        = measurementContextConcept("126073", "DCM", "Time Point Order")
	contextTemporalOffset        = measurementContextConcept("128740", "DCM", "Longitudinal Temporal Offset from Event")
	contextEventType             = measurementContextConcept("128741", "DCM", "Longitudinal Temporal Event Type")
	contextAlgorithmName         = measurementContextConcept("111001", "DCM", "Algorithm Name")
	contextAlgorithmVersion      = measurementContextConcept("111003", "DCM", "Algorithm Version")
	contextAlgorithmManufacturer = measurementContextConcept("122405", "DCM", "Algorithm Manufacturer")
	contextAlgorithmParameters   = measurementContextConcept("111002", "DCM", "Algorithm Parameters")
	contextAlgorithmFamily       = measurementContextConcept("111000", "DCM", "Algorithm Family")
	contextFindingCategory       = measurementContextConcept("276214006", "SCT", "Finding category")
)

func measurementContextConcept(value, scheme, meaning string) CodedEntry {
	return CodedEntry{CodeValue: value, CodingSchemeDesignator: scheme, CodeMeaning: meaning}
}

// ReadMeasurementReportWithContext is bounded and non-destructive. Profile
// refusal returns no facade, while the caller's original Object remains intact.
func ReadMeasurementReportWithContext(obj *object.Object, options MeasurementContextOptions) (*DetailedMeasurementReport, error) {
	if obj == nil {
		return nil, ErrNoDataset
	}
	o, err := normalizeMeasurementContextOptions(options)
	if err != nil {
		return nil, err
	}
	if err = admitMeasurementResident(obj.Elements(), o); err != nil {
		return nil, err
	}
	charset := []string{}
	charsetTag := core.NewTag(0x0008, 0x0005)
	if obj.Has(charsetTag) {
		values, err := obj.LookupStrings(charsetTag)
		if err != nil {
			return nil, measurementContextError(MeasurementContextShape, "")
		}
		if len(values) > 64 {
			return nil, measurementContextError(MeasurementContextLimit, "")
		}
		charset = append(charset, values...)
	}
	if _, err := obj.CharacterSet(); err != nil {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	evidence, err := readMeasurementEvidence(obj, o)
	if err != nil {
		return nil, err
	}
	references, err := readMeasurementReferenceMetadata(obj, o)
	if err != nil {
		return nil, err
	}
	if err = admitMeasurementContent(obj, o); err != nil {
		return nil, err
	}
	result, numeric, err := readDocumentNumericRepresentations(obj, o.Read)
	if err != nil {
		return nil, measurementContextReadError(err)
	}
	snapshot, err := BindMeasurementContextMetadata(result.Document, result.TemplateMetadata, numeric, references, o.Snapshot)
	if err != nil {
		return nil, err
	}
	bindOptions := o
	bindOptions.CharacterSet = charset // Read ignores Bind-only option overrides.
	bindOptions.ValueElementsBigEndian = obj.ValueByteOrder() == binary.BigEndian
	r, err := BindMeasurementReportWithContext(result.Document, snapshot, evidence, bindOptions)
	if err != nil {
		return nil, err
	}
	r.charset = append([]string(nil), charset...)
	r.frameOfReferenceUID = cleanString(obj, core.NewTag(0x0020, 0x0052))
	return r, nil
}

// Admission rejects resident scalar VM/VR ambiguity before generic decoding
// could choose its first value. It executes no source/custom Value methods.
func admitMeasurementContent(obj *object.Object, o MeasurementContextOptions) error {
	if e, present := obj.Get(tagValueType); !present {
		return measurementContextError(MeasurementContextShape, "1")
	} else {
		v, err := numericResidentString(e, core.VRCS)
		if err != nil || strings.Trim(v, " ") != string(ValueContainer) {
			return measurementContextError(MeasurementContextShape, "1")
		}
	}
	if err := admitMeasurementCode(obj, tagConceptNameCodeSeq); err != nil {
		return err
	}
	count := 0
	var walk func(*object.Object, int) error
	walk = func(parent *object.Object, depth int) error {
		element, present := parent.Get(tagContentSequence)
		if !present {
			return nil
		}
		v, ok := element.Value.(core.SequenceValue)
		if element.VR() != core.VRSQ || !ok {
			return measurementContextError(MeasurementContextShape, "")
		}
		if len(v.Items) > o.Read.MaxItems-count || len(v.Items) > 0 && depth > o.Read.MaxDepth {
			return measurementContextError(MeasurementContextLimit, "")
		}
		count += len(v.Items)
		items, _ := parent.GetSequence(tagContentSequence)
		for _, it := range items {
			for _, field := range []struct {
				tag core.Tag
				vr  core.VR
			}{{tagValueType, core.VRCS}, {tagRelationshipType, core.VRCS}, {tagTextValue, core.VRUT}, {tagUID, core.VRUI}, {tagPersonNameValue, core.VRPN}} {
				if e, present := it.Get(field.tag); present {
					s, err := measurementResidentScalar(e, field.vr)
					if err != nil {
						return measurementContextError(MeasurementContextShape, "")
					}
					if len(s) > o.MaxStringBytes {
						return measurementContextError(MeasurementContextLimit, "")
					}
				}
			}
			for _, tag := range []core.Tag{tagConceptNameCodeSeq, tagConceptCodeSeq, tagNumericValueQualifierCodeSeq} {
				if it.Has(tag) {
					if err := admitMeasurementCode(it, tag); err != nil {
						return err
					}
				}
			}
			if e, present := it.Get(tagMeasuredValueSeq); present {
				sq, ok := e.Value.(core.SequenceValue)
				if e.VR() != core.VRSQ || !ok || len(sq.Items) > 1 {
					return measurementContextError(MeasurementContextShape, "")
				}
				values, _ := it.GetSequence(tagMeasuredValueSeq)
				for _, m := range values {
					if err := admitMeasurementCode(m, tagMeasurementUnitsCode); err != nil {
						return err
					}
				}
			}
			if err := walk(it, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(obj, 1)
}

type measurementContextProjection struct {
	r           *DetailedMeasurementReport
	unsupported []MeasurementContextFinding
}

func (p *measurementContextProjection) unsupportedAt(path string) error {
	return p.findingAt(MeasurementContextUnsupported, path)
}
func (p *measurementContextProjection) findingAt(code, path string) error {
	if len(p.unsupported) >= p.r.options.MaxContextFindings {
		return measurementContextError(MeasurementContextLimit, path)
	}
	p.unsupported = append(p.unsupported, MeasurementContextFinding{Code: code, Path: path})
	return nil
}
func (p *measurementContextProjection) matches(i *ContentItem, path string, c CodedEntry) bool {
	return codeKeyForEntry(i.ConceptName, p.r.snapshot.index.code[path].ConceptName) == codeKeyForEntry(c, CodeContext{})
}
func (p *measurementContextProjection) code(i *ContentItem, path string, value bool) MeasurementContextCode {
	meta := p.r.snapshot.index.code[path]
	if value {
		return MeasurementContextCode{Code: i.Code, Context: meta.Value, EquivalentCodes: append([]EquivalentCodeEntry(nil), meta.ValueEquivalentCodes...)}
	}
	return MeasurementContextCode{Code: i.ConceptName, Context: meta.ConceptName, EquivalentCodes: append([]EquivalentCodeEntry(nil), meta.ConceptNameEquivalentCodes...)}
}
func (p *measurementContextProjection) row(i *ContentItem, path string, vt ValueType, rel string) error {
	if i.ValueType != vt || i.RelationshipType != rel || len(i.ReferencedContentItemIdentifier) != 0 {
		return measurementContextError(MeasurementContextShape, path)
	}
	switch vt {
	case ValueCode:
		return p.validCode(p.code(i, path, true), path)
	case ValueText:
		if i.Text == "" {
			return measurementContextError(MeasurementContextShape, path)
		}
		return p.text(i.Text, path)
	case ValueUIDRef:
		if !core.IsValidUID(i.UID) {
			return measurementContextError(MeasurementContextShape, path)
		}
	case ValueNum:
		if i.Measurement == nil || math.IsNaN(i.Measurement.Value) || math.IsInf(i.Measurement.Value, 0) {
			return measurementContextError(MeasurementContextShape, path)
		}
	}
	return nil
}
func (p *measurementContextProjection) text(s, path string) error {
	if len(s) > p.r.options.MaxStringBytes {
		return measurementContextError(MeasurementContextLimit, path)
	}
	if !utf8.ValidString(s) {
		return measurementContextError(MeasurementContextShape, path)
	}
	for _, r := range s {
		if r == 0 || r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return measurementContextError(MeasurementContextShape, path)
		}
	}
	return nil
}
func (p *measurementContextProjection) validCode(c MeasurementContextCode, path string) error {
	if !validCodeKey(codeKeyForEntry(c.Code, c.Context)) || c.Code.CodeMeaning == "" || utf8.RuneCountInString(c.Code.CodeMeaning) > 64 || !validCodeContextShape(c.Context) || !validEquivalentCodeEntries(c.EquivalentCodes) {
		return measurementContextError(MeasurementContextShape, path)
	}
	return p.text(c.Code.CodeMeaning, path)
}
func (p *measurementContextProjection) singleton(seen map[string]bool, key, path string) error {
	if seen[key] {
		return measurementContextError(MeasurementContextDuplicate, path)
	}
	seen[key] = true
	return nil
}
func (p *measurementContextProjection) known(i *ContentItem, path string) bool {
	for _, c := range []CodedEntry{contextActivitySession, contextMethod, contextLaterality, contextTopographical, contextRWVM, contextSubjectTimePoint, contextProtocolTimePoint, contextTimePoint, contextTimePointType, contextTimePointOrder, contextTemporalOffset, contextEventType, contextAlgorithmName, contextAlgorithmVersion, contextAlgorithmManufacturer, contextAlgorithmParameters, contextAlgorithmFamily, trackingIdentifierConcept, trackingUniqueIdentifierConcept, findingConcept, findingSiteConcept, referencedSegmentConcept, sourceSeriesConcept, derivationConcept} {
		if p.matches(i, path, c) {
			return true
		}
	}
	return false
}
func (r *DetailedMeasurementReport) project() (*MeasurementReport, MeasurementReportContext, []MeasurementContextFinding, error) {
	d := r.document
	p := &measurementContextProjection{r: r}
	c := MeasurementReportContext{}
	report := &MeasurementReport{SOPClassUID: d.SOPClassUID, SOPInstanceUID: d.SOPInstanceUID, StudyInstanceUID: d.StudyInstanceUID, SeriesInstanceUID: d.SeriesInstanceUID, FrameOfReferenceUID: r.frameOfReferenceUID, ContentDate: d.ContentDate, ContentTime: d.ContentTime, SeriesNumber: d.SeriesNumber, InstanceNumber: d.InstanceNumber, CompletionFlag: d.CompletionFlag, VerificationFlag: d.VerificationFlag, CompletionFlagDescription: d.CompletionFlagDescription, VerifyingObservers: append([]VerifyingObserver(nil), d.VerifyingObservers...), PredecessorDocuments: append([]DocumentReference(nil), d.PredecessorDocuments...), ContinuityOfContent: d.ContinuityOfContent, Title: d.Title, Common: d.Common}
	root := r.snapshot.index.identification["1"]
	if !measurementSOPClass(d.SOPClassUID) || root.MappingResource != "DCMR" || root.Identifier != "1500" {
		return nil, c, nil, measurementContextError(MeasurementContextShape, "1")
	}
	w := newReferenceWalker(d.Content, ContentItemIdentifier{1}, 1)
	for {
		entry, ok := w.next()
		if !ok {
			break
		}
		if measurementOpaqueValues(entry.item.ValueElements) {
			if err := p.findingAt(MeasurementContextOpaqueValues, entry.path.String()); err != nil {
				return nil, c, nil, err
			}
		}
		w.push(entry.item.Children, entry.path, entry.depth+1)
	}
	seen := map[string]bool{}
	var observer *Observer
	for idx := range d.Content {
		item := &d.Content[idx]
		path := appendPath(ContentItemIdentifier{1}, uint32(idx+1))
		key := path.String()
		switch {
		case p.matches(item, key, languageConcept):
			if err := p.row(item, key, ValueCode, RelationshipHasConceptMod); err != nil {
				return nil, c, nil, err
			}
			if err := p.singleton(seen, "language", key); err != nil {
				return nil, c, nil, err
			}
			report.Language = item.Code
		case p.matches(item, key, procedureReportedConcept):
			if err := p.row(item, key, ValueCode, RelationshipHasConceptMod); err != nil {
				return nil, c, nil, err
			}
			report.ProceduresReported = append(report.ProceduresReported, item.Code)
		case p.matches(item, key, observerTypeConcept):
			if err := p.row(item, key, ValueCode, RelationshipHasObsContext); err != nil {
				return nil, c, nil, err
			}
			if observer != nil {
				report.Observers = append(report.Observers, *observer)
			}
			observer = &Observer{}
		case p.observerAttribute(item, key):
			if observer == nil {
				return nil, c, nil, measurementContextError(MeasurementContextShape, key)
			}
			if err := p.readObserver(item, key, observer); err != nil {
				return nil, c, nil, err
			}
		case p.matches(item, key, imageLibraryConcept):
			if err := p.row(item, key, ValueContainer, RelationshipContains); err != nil {
				return nil, c, nil, err
			}
			if err := p.singleton(seen, "library", key); err != nil {
				return nil, c, nil, err
			}
			w := newReferenceWalker(item.Children, path, 1)
			for {
				entry, ok := w.next()
				if !ok {
					break
				}
				if entry.item.ValueType == ValueImage {
					ref := entry.item.Image
					ref.Frames = append([]int(nil), ref.Frames...)
					report.ImageLibrary = append(report.ImageLibrary, ref)
				}
				w.push(entry.item.Children, entry.path, entry.depth+1)
			}
		case p.matches(item, key, imagingMeasurementsConcept):
			if err := p.row(item, key, ValueContainer, RelationshipContains); err != nil {
				return nil, c, nil, err
			}
			if err := p.singleton(seen, "imaging", key); err != nil {
				return nil, c, nil, err
			}
			algorithm, err := p.algorithm(item.Children, path)
			if err != nil {
				return nil, c, nil, err
			}
			c.ImagingAlgorithm = algorithm
			for j := range item.Children {
				g := &item.Children[j]
				gp := appendPath(path, uint32(j+1))
				gk := gp.String()
				if p.algorithmConcept(g, gk) > 0 {
					continue
				}
				if !p.matches(g, gk, measurementGroupConcept) {
					if p.known(g, gk) {
						return nil, c, nil, measurementContextError(MeasurementContextShape, gk)
					}
					if err := p.unsupportedAt(gk); err != nil {
						return nil, c, nil, err
					}
					continue
				}
				identification := r.snapshot.index.identification[gk]
				if identification.MappingResource != "DCMR" || identification.Identifier != "1501" && identification.Identifier != "1411" {
					if err := p.unsupportedAt(gk); err != nil {
						return nil, c, nil, err
					}
					continue
				}
				group, gc, err := p.group(g, gp, identification.Identifier)
				if err != nil {
					return nil, c, nil, err
				}
				report.Groups = append(report.Groups, group)
				c.Groups = append(c.Groups, gc)
			}
		case p.matches(item, key, qualitativeEvaluationsConcept):
			if err := p.row(item, key, ValueContainer, RelationshipContains); err != nil {
				return nil, c, nil, err
			}
			if err := p.singleton(seen, "qualitative", key); err != nil {
				return nil, c, nil, err
			}
			var err error
			c.QualitativeAlgorithm, err = p.algorithm(item.Children, path)
			if err != nil {
				return nil, c, nil, err
			}
			for j := range item.Children {
				q := &item.Children[j]
				qp := appendPath(path, uint32(j+1))
				qk := qp.String()
				if p.algorithmConcept(q, qk) > 0 {
					continue
				}
				if q.ValueType == ValueCode || q.ValueType == ValueText {
					v, err := p.qualitative(q, qp)
					if err != nil {
						return nil, c, nil, err
					}
					c.Qualitative = append(c.Qualitative, v)
				} else if err := p.unsupportedAt(qk); err != nil {
					return nil, c, nil, err
				}
			}
		default:
			if p.known(item, key) {
				return nil, c, nil, measurementContextError(MeasurementContextShape, key)
			}
			if err := p.unsupportedAt(key); err != nil {
				return nil, c, nil, err
			}
		}
	}
	if observer != nil {
		report.Observers = append(report.Observers, *observer)
	}
	return report, c, p.unsupported, nil
}
func (p *measurementContextProjection) observerAttribute(i *ContentItem, path string) bool {
	for _, c := range []CodedEntry{personObserverNameConcept, personObserverOrganizationConcept, deviceObserverUIDConcept, deviceObserverNameConcept, deviceObserverManufacturerConcept, deviceObserverModelConcept, deviceObserverSerialConcept} {
		if p.matches(i, path, c) {
			return true
		}
	}
	return false
}
func (p *measurementContextProjection) readObserver(i *ContentItem, path string, o *Observer) error {
	vt := ValueText
	if p.matches(i, path, personObserverNameConcept) {
		vt = ValuePName
	}
	if p.matches(i, path, deviceObserverUIDConcept) {
		vt = ValueUIDRef
	}
	if err := p.row(i, path, vt, RelationshipHasObsContext); err != nil {
		return err
	}
	switch {
	case p.matches(i, path, personObserverNameConcept):
		o.PersonName = i.PersonName
	case p.matches(i, path, personObserverOrganizationConcept):
		o.OrganizationName = i.Text
	case p.matches(i, path, deviceObserverUIDConcept):
		o.DeviceUID = i.UID
	case p.matches(i, path, deviceObserverNameConcept):
		o.DeviceName = i.Text
	case p.matches(i, path, deviceObserverManufacturerConcept):
		o.DeviceManufacturer = i.Text
	case p.matches(i, path, deviceObserverModelConcept):
		o.DeviceModelName = i.Text
	case p.matches(i, path, deviceObserverSerialConcept):
		o.DeviceSerialNumber = i.Text
	}
	return nil
}

func (p *measurementContextProjection) algorithmConcept(i *ContentItem, path string) int {
	for index, c := range []CodedEntry{contextAlgorithmName, contextAlgorithmVersion, contextAlgorithmManufacturer, contextAlgorithmParameters, contextAlgorithmFamily} {
		if p.matches(i, path, c) {
			if index == 0 && i.ValueType == ValueCode {
				return 2
			}
			return []int{1, 3, 4, 5, 6}[index]
		}
	}
	return 0
}
func (p *measurementContextProjection) algorithm(items []ContentItem, parent ContentItemIdentifier) (*MeasurementAlgorithm, error) {
	var a *MeasurementAlgorithm
	seen := map[string]bool{}
	previous := 0
	for idx := range items {
		i := &items[idx]
		path := appendPath(parent, uint32(idx+1)).String()
		rank := p.algorithmConcept(i, path)
		if rank == 0 {
			continue
		}
		if rank < previous {
			return nil, measurementContextError(MeasurementContextShape, path)
		}
		previous = rank
		if a == nil {
			a = &MeasurementAlgorithm{}
		}
		vt := ValueText
		if rank == 2 || rank == 6 {
			vt = ValueCode
		}
		if err := p.row(i, path, vt, RelationshipHasConceptMod); err != nil {
			return nil, err
		}
		if len(i.Children) != 0 {
			return nil, measurementContextError(MeasurementContextShape, path)
		}
		if rank != 5 {
			if err := p.singleton(seen, string(rune('0'+rank)), path); err != nil {
				return nil, err
			}
		}
		switch rank {
		case 1:
			a.Name = i.Text
		case 2:
			c := p.code(i, path, true)
			a.NameCode = &c
		case 3:
			a.Version = i.Text
		case 4:
			a.Manufacturer = i.Text
		case 5:
			a.Parameters = append(a.Parameters, i.Text)
		case 6:
			c := p.code(i, path, true)
			a.Family = &c
		}
	}
	if a != nil && (a.Name == "" || a.Version == "") {
		return nil, measurementContextError(MeasurementContextShape, parent.String())
	}
	return a, nil
}
func (p *measurementContextProjection) site(i *ContentItem, parent ContentItemIdentifier) (MeasurementFindingSite, error) {
	path := parent.String()
	s := MeasurementFindingSite{Site: p.code(i, path, true)}
	if err := p.row(i, path, ValueCode, RelationshipHasConceptMod); err != nil {
		return s, err
	}
	seen := map[string]bool{}
	for index := range i.Children {
		child := &i.Children[index]
		key := appendPath(parent, uint32(index+1)).String()
		var target **MeasurementContextCode
		name := ""
		switch {
		case p.matches(child, key, contextLaterality):
			target = &s.Laterality
			name = "laterality"
		case p.matches(child, key, contextTopographical):
			target = &s.TopographicalModifier
			name = "topographical"
		default:
			if p.known(child, key) {
				return s, measurementContextError(MeasurementContextShape, key)
			}
			if err := p.unsupportedAt(key); err != nil {
				return s, err
			}
			continue
		}
		if err := p.singleton(seen, name, key); err != nil {
			return s, err
		}
		if err := p.row(child, key, ValueCode, RelationshipHasConceptMod); err != nil {
			return s, err
		}
		if len(child.Children) > 0 {
			return s, measurementContextError(MeasurementContextShape, key)
		}
		c := p.code(child, key, true)
		*target = &c
	}
	return s, nil
}
func (p *measurementContextProjection) rwvm(i *ContentItem, path, rel string) (*MeasurementReferenceEvidence, error) {
	if err := p.row(i, path, ValueComposite, rel); err != nil {
		return nil, err
	}
	if len(i.Children) != 0 || i.Composite.SOPClassUID != MeasurementRWVMStorage {
		return nil, measurementContextError(MeasurementContextShape, path)
	}
	e, err := p.r.evidenceFor(i.Composite.SOPClassUID, i.Composite.SOPInstanceUID, path)
	if err != nil {
		return nil, err
	}
	return &e, nil
}
func (p *measurementContextProjection) numeric(i *ContentItem, path, rel string) (*NumericRepresentation, error) {
	if err := p.row(i, path, ValueNum, rel); err != nil {
		return nil, err
	}
	if err := p.validCode(MeasurementContextCode{Code: i.Measurement.Units, Context: p.r.snapshot.index.code[path].MeasurementUnits}, path); err != nil {
		return nil, err
	}
	if n, ok := p.r.snapshot.index.numeric[path]; ok {
		return &n, nil
	}
	return nil, measurementContextError(MeasurementContextShape, path)
}
func (p *measurementContextProjection) timePoint(items []ContentItem, parent ContentItemIdentifier) (*MeasurementTimePoint, error) {
	var t *MeasurementTimePoint
	seen := map[string]bool{}
	for idx := range items {
		i := &items[idx]
		ip := appendPath(parent, uint32(idx+1))
		path := ip.String()
		rank := -1
		for k, c := range []CodedEntry{contextSubjectTimePoint, contextProtocolTimePoint, contextTimePoint, contextTimePointType, contextTimePointOrder, contextTemporalOffset} {
			if p.matches(i, path, c) {
				rank = k
				break
			}
		}
		if rank < 0 {
			continue
		}
		if t == nil {
			t = &MeasurementTimePoint{}
		}
		if rank != 3 {
			if err := p.singleton(seen, string(rune('0'+rank)), path); err != nil {
				return nil, err
			}
		}
		switch rank {
		case 0, 1, 2:
			if err := p.row(i, path, ValueText, RelationshipHasObsContext); err != nil {
				return nil, err
			}
			if len(i.Children) > 0 {
				return nil, measurementContextError(MeasurementContextShape, path)
			}
			switch rank {
			case 0:
				t.SubjectIdentifier = i.Text
			case 1:
				t.ProtocolIdentifier = i.Text
			case 2:
				t.TimePoint = i.Text
			}
		case 3:
			if err := p.row(i, path, ValueCode, RelationshipHasObsContext); err != nil {
				return nil, err
			}
			if len(i.Children) > 0 {
				return nil, measurementContextError(MeasurementContextShape, path)
			}
			t.Types = append(t.Types, p.code(i, path, true))
		case 4, 5:
			n, err := p.numeric(i, path, RelationshipHasObsContext)
			if err != nil {
				return nil, err
			}
			unit := "1"
			if rank == 5 {
				unit = "d"
			}
			if codeKeyForEntry(i.Measurement.Units, p.r.snapshot.index.code[path].MeasurementUnits) != codeKeyForEntry(measurementContextConcept(unit, "UCUM", ""), CodeContext{}) {
				return nil, measurementContextError(MeasurementContextShape, path)
			}
			if rank == 4 {
				if len(i.Children) > 0 {
					return nil, measurementContextError(MeasurementContextShape, path)
				}
				t.Order = n
			} else {
				t.TemporalOffset = n
				for j := range i.Children {
					ch := &i.Children[j]
					key := appendPath(ip, uint32(j+1)).String()
					if !p.matches(ch, key, contextEventType) {
						if err := p.unsupportedAt(key); err != nil {
							return nil, err
						}
						continue
					}
					if t.EventType != nil {
						return nil, measurementContextError(MeasurementContextDuplicate, key)
					}
					if err := p.row(ch, key, ValueCode, RelationshipHasConceptMod); err != nil {
						return nil, err
					}
					if len(ch.Children) > 0 {
						return nil, measurementContextError(MeasurementContextShape, key)
					}
					v := p.code(ch, key, true)
					t.EventType = &v
				}
				if t.EventType == nil {
					return nil, measurementContextError(MeasurementContextShape, path)
				}
			}
		}
	}
	if t != nil && t.TimePoint == "" {
		return nil, measurementContextError(MeasurementContextShape, parent.String())
	}
	return t, nil
}
func (p *measurementContextProjection) timePointConcept(i *ContentItem, path string) bool {
	for _, c := range []CodedEntry{contextSubjectTimePoint, contextProtocolTimePoint, contextTimePoint, contextTimePointType, contextTimePointOrder, contextTemporalOffset} {
		if p.matches(i, path, c) {
			return true
		}
	}
	return false
}
func (p *measurementContextProjection) qualitative(i *ContentItem, parent ContentItemIdentifier) (MeasurementQualitativeEvaluation, error) {
	path := parent.String()
	q := MeasurementQualitativeEvaluation{ConceptName: p.code(i, path, false), ValueType: i.ValueType}
	if p.known(i, path) || i.ValueType != ValueCode && i.ValueType != ValueText {
		return q, measurementContextError(MeasurementContextShape, path)
	}
	if err := p.validCode(q.ConceptName, path); err != nil {
		return q, err
	}
	if err := p.row(i, path, i.ValueType, RelationshipContains); err != nil {
		return q, err
	}
	if i.ValueType == ValueText {
		q.Text = i.Text
	} else {
		q.Code = p.code(i, path, true)
	}
	for idx := range i.Children {
		ch := &i.Children[idx]
		key := appendPath(parent, uint32(idx+1)).String()
		if i.ValueType == ValueCode && ch.ValueType == ValueCode && ch.RelationshipType == RelationshipHasConceptMod && !p.known(ch, key) {
			if err := p.row(ch, key, ValueCode, RelationshipHasConceptMod); err != nil {
				return q, err
			}
			name := p.code(ch, key, false)
			if err := p.validCode(name, key); err != nil {
				return q, err
			}
			if len(ch.Children) > 0 {
				return q, measurementContextError(MeasurementContextShape, key)
			}
			q.Modifiers = append(q.Modifiers, MeasurementQualitativeModifier{ConceptName: name, Value: p.code(ch, key, true)})
		} else {
			if p.known(ch, key) {
				return q, measurementContextError(MeasurementContextShape, key)
			}
			if err := p.unsupportedAt(key); err != nil {
				return q, err
			}
		}
	}
	return q, nil
}
func (p *measurementContextProjection) group(i *ContentItem, parent ContentItemIdentifier, template string) (MeasurementGroup, MeasurementGroupContext, error) {
	path := parent.String()
	g := MeasurementGroup{Template: template}
	c := MeasurementGroupContext{Path: path}
	if err := p.row(i, path, ValueContainer, RelationshipContains); err != nil {
		return g, c, err
	}
	var err error
	c.Algorithm, err = p.algorithm(i.Children, parent)
	if err != nil {
		return g, c, err
	}
	c.TimePoint, err = p.timePoint(i.Children, parent)
	if err != nil {
		return g, c, err
	}
	seen := map[string]bool{}
	for idx := range i.Children {
		ch := &i.Children[idx]
		cp := appendPath(parent, uint32(idx+1))
		key := cp.String()
		if p.algorithmConcept(ch, key) > 0 || p.timePointConcept(ch, key) {
			continue
		}
		singleton := func(name string, vt ValueType, rel string) error {
			if err := p.singleton(seen, name, key); err != nil {
				return err
			}
			return p.row(ch, key, vt, rel)
		}
		switch {
		case p.matches(ch, key, contextActivitySession):
			if err = singleton("session", ValueText, RelationshipHasObsContext); err != nil {
				return g, c, err
			}
			c.ActivitySession = ch.Text
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, trackingIdentifierConcept):
			if err = singleton("tracking-id", ValueText, RelationshipHasObsContext); err != nil {
				return g, c, err
			}
			g.Tracking.Identifier = ch.Text
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, trackingUniqueIdentifierConcept):
			if err = singleton("tracking-uid", ValueUIDRef, RelationshipHasObsContext); err != nil {
				return g, c, err
			}
			g.Tracking.UID = ch.UID
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, findingConcept):
			if err = singleton("finding", ValueCode, RelationshipContains); err != nil {
				return g, c, err
			}
			g.Finding = ch.Code
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, contextFindingCategory):
			if err = p.row(ch, key, ValueCode, RelationshipContains); err != nil {
				return g, c, err
			}
			if err = p.unsupportedAt(key); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, contextMethod):
			if err = singleton("method", ValueCode, RelationshipHasConceptMod); err != nil {
				return g, c, err
			}
			v := p.code(ch, key, true)
			c.Method = &v
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case p.matches(ch, key, findingSiteConcept):
			s, e := p.site(ch, cp)
			if e != nil {
				return g, c, e
			}
			c.FindingSites = append(c.FindingSites, s)
			g.FindingSites = append(g.FindingSites, s.Site.Code)
		case p.matches(ch, key, contextRWVM):
			if err = p.singleton(seen, "rwvm", key); err != nil {
				return g, c, err
			}
			c.RealWorldValueMap, err = p.rwvm(ch, key, RelationshipContains)
			if err != nil {
				return g, c, err
			}
		case p.matches(ch, key, referencedSegmentConcept):
			if template != "1411" {
				return g, c, measurementContextError(MeasurementContextShape, key)
			}
			if err = singleton("segment", ValueImage, RelationshipContains); err != nil {
				return g, c, err
			}
			detail, ok := p.r.snapshot.index.measurementReferences[key]
			if !ok || len(detail.Segments) != 1 || !measurementSegmentSOPClass(ch.Image.SOPClassUID) {
				return g, c, measurementContextError(MeasurementContextShape, key)
			}
			g.ReferencedSegment = SegmentReference{StudyInstanceUID: ch.Image.StudyInstanceUID, SeriesInstanceUID: ch.Image.SeriesInstanceUID, SOPClassUID: ch.Image.SOPClassUID, SOPInstanceUID: ch.Image.SOPInstanceUID, SegmentNumber: int(detail.Segments[0])}
		case p.matches(ch, key, sourceSeriesConcept):
			if template != "1411" {
				return g, c, measurementContextError(MeasurementContextShape, key)
			}
			if err = singleton("source-series", ValueUIDRef, RelationshipContains); err != nil {
				return g, c, err
			}
			g.SourceSeriesUID = ch.UID
			if err = p.scalarChildren(ch, cp); err != nil {
				return g, c, err
			}
		case ch.ValueType == ValueNum && ch.RelationshipType == RelationshipContains:
			if p.known(ch, key) {
				return g, c, measurementContextError(MeasurementContextShape, key)
			}
			m, mc, e := p.measurement(ch, cp, template)
			if e != nil {
				return g, c, e
			}
			g.Measurements = append(g.Measurements, m)
			c.Measurements = append(c.Measurements, mc)
		case (ch.ValueType == ValueCode || ch.ValueType == ValueText) && ch.RelationshipType == RelationshipContains:
			q, e := p.qualitative(ch, cp)
			if e != nil {
				return g, c, e
			}
			c.Qualitative = append(c.Qualitative, q)
		default:
			if p.known(ch, key) {
				return g, c, measurementContextError(MeasurementContextShape, key)
			}
			if err = p.unsupportedAt(key); err != nil {
				return g, c, err
			}
		}
	}
	return g, c, nil
}
func (p *measurementContextProjection) measurement(i *ContentItem, parent ContentItemIdentifier, template string) (ReportMeasurement, MeasurementNumericContext, error) {
	path := parent.String()
	m := ReportMeasurement{ConceptName: i.ConceptName}
	meta := p.r.snapshot.index.code[path]
	c := MeasurementNumericContext{Path: path, ConceptName: p.code(i, path, false)}
	n, err := p.numeric(i, path, RelationshipContains)
	if err != nil {
		return m, c, err
	}
	c.Numeric = n
	m.Value = i.Measurement.Value
	m.Units = i.Measurement.Units
	c.Units = MeasurementContextCode{Code: m.Units, Context: meta.MeasurementUnits, EquivalentCodes: append([]EquivalentCodeEntry(nil), meta.MeasurementUnitsEquivalentCodes...)}
	if err = p.validCode(c.ConceptName, path); err != nil {
		return m, c, err
	}
	c.Algorithm, err = p.algorithm(i.Children, parent)
	if err != nil {
		return m, c, err
	}
	seen := map[string]bool{}
	for idx := range i.Children {
		ch := &i.Children[idx]
		cp := appendPath(parent, uint32(idx+1))
		key := cp.String()
		if p.algorithmConcept(ch, key) > 0 {
			continue
		}
		switch {
		case p.matches(ch, key, contextMethod):
			if err = p.singleton(seen, "method", key); err != nil {
				return m, c, err
			}
			if err = p.row(ch, key, ValueCode, RelationshipHasConceptMod); err != nil {
				return m, c, err
			}
			v := p.code(ch, key, true)
			c.Method = &v
			if err = p.scalarChildren(ch, cp); err != nil {
				return m, c, err
			}
		case p.matches(ch, key, derivationConcept):
			if err = p.singleton(seen, "derivation", key); err != nil {
				return m, c, err
			}
			if err = p.row(ch, key, ValueCode, RelationshipHasConceptMod); err != nil {
				return m, c, err
			}
			m.Derivation = ch.Code
			if err = p.scalarChildren(ch, cp); err != nil {
				return m, c, err
			}
		case p.matches(ch, key, findingSiteConcept):
			s, e := p.site(ch, cp)
			if e != nil {
				return m, c, e
			}
			c.FindingSites = append(c.FindingSites, s)
		case p.matches(ch, key, contextRWVM):
			if err = p.singleton(seen, "rwvm", key); err != nil {
				return m, c, err
			}
			c.RealWorldValueMap, err = p.rwvm(ch, key, RelationshipInferredFrom)
			if err != nil {
				return m, c, err
			}
			if template == "1411" {
				// TID 1419 row 19 says CONTAINS, while the selected SR IOD only
				// permits CONTAINER CONTAINS. This explicit extension uses the
				// legal TID 300 INFERRED FROM form and records that catalogue seam.
				if err = p.findingAt(MeasurementContextRWVMExtension, key); err != nil {
					return m, c, err
				}
			}
		case p.matches(ch, key, sourceOfMeasurementConcept) && ch.ValueType == ValueImage:
			if err = p.singleton(seen, "image", key); err != nil {
				return m, c, err
			}
			if err = p.row(ch, key, ValueImage, RelationshipInferredFrom); err != nil {
				return m, c, err
			}
			m.Image = ch.Image
			m.Image.Frames = append([]int(nil), ch.Image.Frames...)
		case p.matches(ch, key, sourceOfMeasurementConcept) && ch.ValueType == ValueSCoord3D:
			if err = p.singleton(seen, "spatial", key); err != nil {
				return m, c, err
			}
			if err = p.row(ch, key, ValueSCoord3D, RelationshipInferredFrom); err != nil {
				return m, c, err
			}
			m.Spatial = ch.Spatial3D
			m.Spatial.Coordinates = append([]Point3D(nil), ch.Spatial3D.Coordinates...)
		default:
			if p.known(ch, key) {
				return m, c, measurementContextError(MeasurementContextShape, key)
			}
			if err = p.unsupportedAt(key); err != nil {
				return m, c, err
			}
		}
	}
	return m, c, nil
}

// MeasurementReferenceMetadata preserves details omitted by legacy ImageReference
// without changing its public layout. It belongs to the existing guarded snapshot.
type MeasurementReferenceMetadata struct {
	Entries []MeasurementReferenceMetadataEntry
}
type MeasurementReferenceMetadataEntry struct {
	Path           string
	SOPClassUID    string
	SOPInstanceUID string
	Segments       []uint16
}

func cloneMeasurementReferenceMetadata(m MeasurementReferenceMetadata) MeasurementReferenceMetadata {
	out := MeasurementReferenceMetadata{Entries: append([]MeasurementReferenceMetadataEntry(nil), m.Entries...)}
	for i := range out.Entries {
		out.Entries[i].Segments = append([]uint16(nil), out.Entries[i].Segments...)
	}
	return out
}
func measurementReferenceMetadataCount(m MeasurementReferenceMetadata, max int) bool {
	if max < 0 || len(m.Entries) > max {
		return false
	}
	count := len(m.Entries)
	for _, e := range m.Entries {
		if len(e.Segments) > max-count {
			return false
		}
		count += len(e.Segments)
	}
	return true
}

// BindMeasurementContextMetadata explicitly binds all already-associated
// metadata once, using the same snapshot, limits, ambiguity proof and Check.
func BindMeasurementContextMetadata(d *Document, m TemplateValidationMetadata, n NumericRepresentationMetadata, references MeasurementReferenceMetadata, o MetadataSnapshotOptions) (*TemplateMetadataSnapshot, error) {
	return bindMeasurementSnapshot(d, m, n, references, o)
}
func (s *TemplateMetadataSnapshot) MeasurementReferences() MeasurementReferenceMetadata {
	if s == nil {
		return MeasurementReferenceMetadata{}
	}
	return cloneMeasurementReferenceMetadata(s.measurementReferences)
}
func indexMeasurementReferenceMetadata(d *Document, index *templateMetadataIndex, m MeasurementReferenceMetadata, o MetadataSnapshotOptions) error {
	index.measurementReferences = make(map[string]MeasurementReferenceMetadataEntry, len(m.Entries))
	for _, e := range m.Entries {
		if !validNumericPath(e.Path) || !core.IsValidUID(e.SOPClassUID) || !core.IsValidUID(e.SOPInstanceUID) || len(e.Segments) == 0 {
			return measurementContextError(MeasurementContextShape, e.Path)
		}
		if _, ok := index.measurementReferences[e.Path]; ok {
			return measurementContextError(MeasurementContextDuplicate, e.Path)
		}
		for _, segment := range e.Segments {
			if segment == 0 {
				return measurementContextError(MeasurementContextShape, e.Path)
			}
		}
		index.measurementReferences[e.Path] = e
	}
	if len(m.Entries) == 0 {
		return nil
	}
	w := newReferenceWalker(d.Content, ContentItemIdentifier{1}, 1)
	found, count := 0, 0
	for {
		e, ok := w.next()
		if !ok {
			break
		}
		count++
		if count > o.MaxItems || e.depth > o.MaxDepth {
			return measurementContextError(MeasurementContextLimit, e.path.String())
		}
		if ref, ok := index.measurementReferences[e.path.String()]; ok {
			if e.item.ValueType != ValueImage || len(e.item.ReferencedContentItemIdentifier) > 0 || e.item.Image.SOPClassUID != ref.SOPClassUID || e.item.Image.SOPInstanceUID != ref.SOPInstanceUID {
				return measurementContextError(MeasurementContextConflict, e.path.String())
			}
			found++
		}
		w.push(e.item.Children, e.path, e.depth+1)
	}
	if found != len(m.Entries) {
		return measurementContextError(MeasurementContextConflict, "")
	}
	return nil
}
func readMeasurementReferenceMetadata(obj *object.Object, o MeasurementContextOptions) (MeasurementReferenceMetadata, error) {
	m := MeasurementReferenceMetadata{}
	count, segments := 0, 0
	var visit func(*object.Object, ContentItemIdentifier, int) error
	visit = func(parent *object.Object, path ContentItemIdentifier, depth int) error {
		element, present := parent.Get(tagContentSequence)
		if !present {
			return nil
		}
		sq, ok := element.Value.(core.SequenceValue)
		if element.VR() != core.VRSQ || !ok {
			return measurementContextError(MeasurementContextShape, path.String())
		}
		if len(sq.Items) > o.Read.MaxItems-count || len(sq.Items) > 0 && depth > o.Read.MaxDepth {
			return measurementContextError(MeasurementContextLimit, path.String())
		}
		count += len(sq.Items)
		items, _ := parent.GetSequence(tagContentSequence)
		for idx, it := range items {
			ip := appendPath(path, uint32(idx+1))
			key := ip.String()
			vt, present := it.Get(tagValueType)
			if present {
				v, err := numericResidentString(vt, core.VRCS)
				if err != nil {
					return measurementContextError(MeasurementContextShape, key)
				}
				valueType := ValueType(strings.Trim(v, " "))
				if valueType == ValueImage || valueType == ValueComposite || valueType == ValueWaveform {
					element, present := it.Get(tagRefSOPSequence)
					sq, ok := element.Value.(core.SequenceValue)
					if !present || element.VR() != core.VRSQ || !ok || len(sq.Items) != 1 {
						return measurementContextError(MeasurementContextShape, key)
					}
					refs, _ := it.GetSequence(tagRefSOPSequence)
					ref := refs[0]
					// The legacy generic tree cannot retain optional attributes inside
					// this recognized macro. Refuse them before decoding rather than
					// quietly losing a presentation-state/RWVM/icon extension.
					for _, field := range ref.Elements() {
						switch field.Tag() {
						case tagRefSOPClassUID, tagRefSOPInstanceUID:
						case tagRefFrameNumber, tagReferencedSegmentNumber:
							if valueType != ValueImage {
								return measurementContextError(MeasurementContextShape, key)
							}
						case tagReferencedWaveformCh:
							if valueType != ValueWaveform {
								return measurementContextError(MeasurementContextShape, key)
							}
						default:
							return measurementContextError(MeasurementContextUnsupported, key)
						}
					}
					uid := func(tag core.Tag) (string, error) {
						e, present := ref.Get(tag)
						if !present {
							return "", measurementContextError(MeasurementContextShape, key)
						}
						v, err := measurementResidentScalar(e, core.VRUI)
						if err != nil || !core.IsValidUID(core.NormalizeUID(v)) {
							return "", measurementContextError(MeasurementContextShape, key)
						}
						return core.NormalizeUID(v), nil
					}
					c, err := uid(tagRefSOPClassUID)
					if err != nil {
						return err
					}
					u, err := uid(tagRefSOPInstanceUID)
					if err != nil {
						return err
					}
					if seg, present := ref.Get(tagReferencedSegmentNumber); present {
						if valueType != ValueImage {
							return measurementContextError(MeasurementContextShape, key)
						}
						if seg.VR() != core.VRUS {
							return measurementContextError(MeasurementContextShape, key)
						}
						var values []uint16
						switch v := seg.Value.(type) {
						case core.Uint16Value:
							if len(v) > o.Snapshot.MaxItems-segments {
								return measurementContextError(MeasurementContextLimit, key)
							}
							values = append([]uint16(nil), v...)
						case core.RawValue:
							if len(v)%2 != 0 {
								return measurementContextError(MeasurementContextShape, key)
							}
							if len(v)/2 > o.Snapshot.MaxItems-segments {
								return measurementContextError(MeasurementContextLimit, key)
							}
							values = make([]uint16, len(v)/2)
							for i := range values {
								values[i] = ref.ValueByteOrder().Uint16(v[2*i:])
							}
						default:
							return measurementContextError(MeasurementContextShape, key)
						}
						if len(values) == 0 {
							return measurementContextError(MeasurementContextShape, key)
						}
						for _, v := range values {
							if v == 0 {
								return measurementContextError(MeasurementContextShape, key)
							}
						}
						segments += len(values)
						if len(m.Entries) >= o.Snapshot.MaxItems-segments {
							return measurementContextError(MeasurementContextLimit, key)
						}
						m.Entries = append(m.Entries, MeasurementReferenceMetadataEntry{Path: key, SOPClassUID: c, SOPInstanceUID: u, Segments: values})
					}
				}
			}
			if err := visit(it, ip, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(obj, ContentItemIdentifier{1}, 1)
	return m, err
}
func restoreMeasurementReferenceMetadata(elements *[]core.Element, items []ContentItem, parent ContentItemIdentifier, refs map[string]MeasurementReferenceMetadataEntry) error {
	if len(refs) == 0 || len(items) == 0 {
		return nil
	}
	index := elementIndex(*elements, tagContentSequence)
	if index < 0 {
		return measurementContextError(MeasurementContextShape, parent.String())
	}
	sq, ok := (*elements)[index].Value.(core.SequenceValue)
	if !ok || len(sq.Items) != len(items) {
		return measurementContextError(MeasurementContextShape, parent.String())
	}
	for i := range items {
		path := appendPath(parent, uint32(i+1))
		data := &sq.Items[i]
		if ref, present := refs[path.String()]; present {
			index := elementIndex(data.Elements, tagRefSOPSequence)
			if index < 0 {
				return measurementContextError(MeasurementContextShape, path.String())
			}
			value, ok := data.Elements[index].Value.(core.SequenceValue)
			if !ok || len(value.Items) != 1 {
				return measurementContextError(MeasurementContextShape, path.String())
			}
			attr := core.Element{Header: core.ElementHeader{Tag: tagReferencedSegmentNumber, VR: core.VRUS}, Value: core.Uint16Value(append([]uint16(nil), ref.Segments...))}
			replaceOrAppendElement(&value.Items[0].Elements, attr)
			data.Elements[index] = seqElement(tagRefSOPSequence, value.Items...)
		}
		if err := restoreMeasurementReferenceMetadata(&data.Elements, items[i].Children, path, refs); err != nil {
			return err
		}
	}
	(*elements)[index].Value = sq
	return nil
}

// Admit the resident source before any generic text/sequence decoder can call a
// custom Value implementation. This is a finite shape/size walk, not another
// fingerprint, stored guard, or payload materializer.
func admitMeasurementResident(elements []core.Element, o MeasurementContextOptions) error {
	remaining := o.Snapshot.MaxValueBytes
	items := 0
	charge := func(n int64) error {
		if n < 0 || n > remaining {
			return measurementContextError(MeasurementContextLimit, "")
		}
		remaining -= n
		return nil
	}
	var walk func([]core.Element, int) error
	walk = func(elements []core.Element, depth int) error {
		if depth > 64 || len(elements) > o.MaxEvidenceItems-items {
			return measurementContextError(MeasurementContextLimit, "")
		}
		items += len(elements)
		for _, e := range elements {
			if err := charge(16); err != nil {
				return err
			}
			switch v := e.Value.(type) {
			case core.RawValue:
				// Check resident raw text before any decoder can copy it to a string.
				// This finite profile limits the whole raw textual attribute, including
				// delimiters and padding, rather than promising peak allocation bounds.
				if e.VR().IsStringLike() && len(v) > o.MaxStringBytes {
					return measurementContextError(MeasurementContextLimit, "")
				}
				if err := charge(int64(len(v))); err != nil {
					return err
				}
			case core.StringValue:
				if len(v) > o.MaxEvidenceItems-items {
					return measurementContextError(MeasurementContextLimit, "")
				}
				items += len(v)
				for _, s := range v {
					if len(s) > o.MaxStringBytes {
						return measurementContextError(MeasurementContextLimit, "")
					}
					if err := charge(int64(len(s))); err != nil {
						return err
					}
				}
			case core.SequenceValue:
				if len(v.Items) > o.MaxEvidenceItems-items {
					return measurementContextError(MeasurementContextLimit, "")
				}
				items += len(v.Items)
				for _, it := range v.Items {
					if err := walk(it.Elements, depth+1); err != nil {
						return err
					}
				}
			case core.Uint16Value:
				if err := charge(int64(len(v)) * 2); err != nil {
					return err
				}
			case core.Int16Value:
				if err := charge(int64(len(v)) * 2); err != nil {
					return err
				}
			case core.Uint32Value:
				if err := charge(int64(len(v)) * 4); err != nil {
					return err
				}
			case core.Int32Value:
				if err := charge(int64(len(v)) * 4); err != nil {
					return err
				}
			case core.Float32Value:
				if err := charge(int64(len(v)) * 4); err != nil {
					return err
				}
			case core.Uint64Value:
				if err := charge(int64(len(v)) * 8); err != nil {
					return err
				}
			case core.Int64Value:
				if err := charge(int64(len(v)) * 8); err != nil {
					return err
				}
			case core.Float64Value:
				if err := charge(int64(len(v)) * 8); err != nil {
					return err
				}
			case core.TagValue:
				if err := charge(int64(len(v)) * 4); err != nil {
					return err
				}
			default:
				return measurementContextError(MeasurementContextShape, "")
			}
		}
		return nil
	}
	return walk(elements, 0)
}
func admitMeasurementCode(parent *object.Object, tag core.Tag) error {
	e, present := parent.Get(tag)
	sq, ok := e.Value.(core.SequenceValue)
	if !present || e.VR() != core.VRSQ || !ok || len(sq.Items) != 1 {
		return measurementContextError(MeasurementContextShape, "")
	}
	items, _ := parent.GetSequence(tag)
	var visit func(*object.Object, int) error
	visit = func(it *object.Object, depth int) error {
		if depth > 64 {
			return measurementContextError(MeasurementContextLimit, "")
		}
		count := 0
		urn := false
		for _, field := range []struct {
			tag core.Tag
			vr  core.VR
		}{{tagCodeValue, core.VRSH}, {tagLongCodeValue, core.VRUC}, {tagURNCodeValue, core.VRUR}} {
			if e, present := it.Get(field.tag); present {
				v, err := measurementResidentScalar(e, field.vr)
				if err != nil || core.TrimTextValue(field.vr, v) == "" {
					return measurementContextError(MeasurementContextShape, "")
				}
				count++
				urn = field.tag == tagURNCodeValue
			}
		}
		if count != 1 {
			return measurementContextError(MeasurementContextShape, "")
		}
		for _, field := range []struct {
			tag      core.Tag
			vr       core.VR
			required bool
		}{{tagCodingScheme, core.VRSH, !urn}, {tagCodeMeaning, core.VRLO, true}, {tagCodingSchemeVersion, core.VRSH, false}} {
			if e, present := it.Get(field.tag); present {
				v, err := measurementResidentScalar(e, field.vr)
				if err != nil || core.TrimTextValue(field.vr, v) == "" {
					return measurementContextError(MeasurementContextShape, "")
				}
			} else if field.required {
				return measurementContextError(MeasurementContextShape, "")
			}
		}
		if it.Has(tagCodingSchemeVersion) && !it.Has(tagCodingScheme) {
			return measurementContextError(MeasurementContextShape, "")
		}
		if e, present := it.Get(tagEquivalentCodeSequence); present {
			sq, ok := e.Value.(core.SequenceValue)
			if e.VR() != core.VRSQ || !ok || len(sq.Items) == 0 {
				return measurementContextError(MeasurementContextShape, "")
			}
			values, _ := it.GetSequence(tagEquivalentCodeSequence)
			for _, child := range values {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(items[0], 0)
}
func measurementResidentScalar(e core.Element, vr core.VR) (string, error) {
	s, err := numericResidentString(e, vr)
	if err != nil {
		return "", err
	}
	if vr.UsesTextValueDelimiter() && strings.Contains(s, "\\") {
		return "", measurementContextError(MeasurementContextShape, "")
	}
	return s, nil
}
func (p *measurementContextProjection) scalarChildren(i *ContentItem, path ContentItemIdentifier) error {
	for idx := range i.Children {
		child := &i.Children[idx]
		key := appendPath(path, uint32(idx+1)).String()
		if p.known(child, key) {
			return measurementContextError(MeasurementContextShape, key)
		}
		if err := p.unsupportedAt(key); err != nil {
			return err
		}
	}
	return nil
}

func measurementContextReadError(err error) error {
	if errors.Is(err, ErrNumericRepresentation) || errors.Is(err, ErrMetadataSnapshot) || errors.Is(err, ErrTemplateValidation) {
		return err
	}
	if errors.Is(err, ErrResourceLimitExceeded) {
		return measurementContextError(MeasurementContextLimit, "")
	}
	return measurementContextError(MeasurementContextShape, "")
}

// TID 1411 row 7 references volumetric segmentation or surface segmentation.
// The finite profile admits the three supported PS3.4 B.5-1 classes by UID;
// the standard catalogue supplies their identities, never clinical Code Meaning.
func measurementSegmentSOPClass(value string) bool {
	e, ok := uid.LookupStorageSOPClass(value)
	if !ok {
		return false
	}
	switch e.Keyword {
	case "SegmentationStorage", "SurfaceSegmentationStorage", "LabelMapSegmentationStorage":
		return true
	}
	return false
}

// The existing snapshot has already bounded and fingerprinted these resident
// values. This inspection classifies opaque output; it stores no second proof.
func measurementOpaqueValues(elements []core.Element) bool {
	for _, e := range elements {
		switch v := e.Value.(type) {
		case core.RawValue:
			if !e.VR().IsStringLike() {
				return true
			}
		case core.SequenceValue:
			for _, it := range v.Items {
				if measurementOpaqueValues(it.Elements) {
					return true
				}
			}
		}
	}
	return false
}
func (r *DetailedMeasurementReport) opaqueOutputAdmission() error {
	if !r.options.ValueElementsBigEndian {
		return nil
	}
	w := newReferenceWalker(r.document.Content, ContentItemIdentifier{1}, 1)
	for {
		entry, ok := w.next()
		if !ok {
			break
		}
		if measurementOpaqueValues(entry.item.ValueElements) {
			return measurementContextError(MeasurementContextOpaqueValues, entry.path.String())
		}
		w.push(entry.item.Children, entry.path, entry.depth+1)
	}
	return nil
}
