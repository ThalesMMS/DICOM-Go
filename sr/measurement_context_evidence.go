package sr

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func admitMeasurementEvidence(input []MeasurementReferenceEvidence, o MeasurementContextOptions) ([]MeasurementReferenceEvidence, error) {
	if len(input) > o.MaxEvidence {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	out := make([]MeasurementReferenceEvidence, 0, len(input))
	seen := make(map[string]MeasurementReferenceEvidence, len(input))
	for _, e := range input {
		for _, u := range []string{e.StudyInstanceUID, e.SeriesInstanceUID, e.SOPClassUID, e.SOPInstanceUID} {
			if !core.IsValidUID(u) {
				return nil, measurementContextError(MeasurementContextEvidence, "")
			}
		}
		if previous, ok := seen[e.SOPInstanceUID]; ok {
			if previous != e {
				return nil, measurementContextError(MeasurementContextConflict, "")
			}
			continue
		}
		seen[e.SOPInstanceUID] = e
		out = append(out, e)
	}
	return out, nil
}

// Admission precedes GetSequence to bound facade growth. The resident sequence
// and UI values must be core values; Source/custom values are never invoked.
func readMeasurementEvidence(obj *object.Object, o MeasurementContextOptions) ([]MeasurementReferenceEvidence, error) {
	used := 0
	sequence := func(parent *object.Object, tag core.Tag, optional bool) ([]*object.Object, error) {
		element, present := parent.Get(tag)
		if !present && optional {
			return nil, nil
		}
		v, ok := element.Value.(core.SequenceValue)
		if !present || element.VR() != core.VRSQ || !ok {
			return nil, measurementContextError(MeasurementContextEvidence, "")
		}
		if len(v.Items) > o.MaxEvidenceItems-used {
			return nil, measurementContextError(MeasurementContextLimit, "")
		}
		used += len(v.Items)
		items, ok := parent.GetSequence(tag)
		if !ok {
			return nil, measurementContextError(MeasurementContextEvidence, "")
		}
		return items, nil
	}
	uid := func(parent *object.Object, tag core.Tag) (string, error) {
		element, present := parent.Get(tag)
		if !present || element.VR() != core.VRUI {
			return "", measurementContextError(MeasurementContextEvidence, "")
		}
		raw, err := numericResidentString(element, core.VRUI)
		if err != nil || len(raw) > 65 {
			return "", measurementContextError(MeasurementContextEvidence, "")
		}
		u := core.NormalizeUID(raw)
		if !core.IsValidUID(u) {
			return "", measurementContextError(MeasurementContextEvidence, "")
		}
		return u, nil
	}
	studies, err := sequence(obj, tagCurrentRequestedProcedureEvidence, true)
	if err != nil {
		return nil, err
	}
	refs := make([]MeasurementReferenceEvidence, 0)
	for _, study := range studies {
		su, err := uid(study, tagStudyInstanceUID)
		if err != nil {
			return nil, err
		}
		series, err := sequence(study, tagReferencedSeriesSequence, false)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			se, err := uid(s, tagSeriesInstanceUID)
			if err != nil {
				return nil, err
			}
			instances, err := sequence(s, tagRefSOPSequence, false)
			if err != nil {
				return nil, err
			}
			if len(instances) > o.MaxEvidence-len(refs) {
				return nil, measurementContextError(MeasurementContextLimit, "")
			}
			for _, instance := range instances {
				class, err := uid(instance, tagRefSOPClassUID)
				if err != nil {
					return nil, err
				}
				instanceUID, err := uid(instance, tagRefSOPInstanceUID)
				if err != nil {
					return nil, err
				}
				refs = append(refs, MeasurementReferenceEvidence{StudyInstanceUID: su, SeriesInstanceUID: se, SOPClassUID: class, SOPInstanceUID: instanceUID})
			}
		}
	}
	return admitMeasurementEvidence(refs, o)
}
func (r *DetailedMeasurementReport) evidenceFor(class, instance, path string) (MeasurementReferenceEvidence, error) {
	if !core.IsValidUID(class) || !core.IsValidUID(instance) {
		return MeasurementReferenceEvidence{}, measurementContextError(MeasurementContextEvidence, path)
	}
	if e, ok := r.evidenceIndex[instance]; ok {
		if e.SOPClassUID != class {
			return MeasurementReferenceEvidence{}, measurementContextError(MeasurementContextConflict, path)
		}
		return e, nil
	}
	return MeasurementReferenceEvidence{}, measurementContextError(MeasurementContextEvidence, path)
}
func (r *DetailedMeasurementReport) checkEvidence() error {
	w := newReferenceWalker(r.document.Content, ContentItemIdentifier{1}, 1)
	for {
		entry, ok := w.next()
		if !ok {
			break
		}
		i := entry.item
		if len(i.ReferencedContentItemIdentifier) == 0 {
			switch i.ValueType {
			case ValueImage:
				e, err := r.evidenceFor(i.Image.SOPClassUID, i.Image.SOPInstanceUID, entry.path.String())
				if err != nil {
					return err
				}
				if e.StudyInstanceUID != i.Image.StudyInstanceUID || e.SeriesInstanceUID != i.Image.SeriesInstanceUID {
					return measurementContextError(MeasurementContextConflict, entry.path.String())
				}
			case ValueComposite:
				if _, err := r.evidenceFor(i.Composite.SOPClassUID, i.Composite.SOPInstanceUID, entry.path.String()); err != nil {
					return err
				}
			case ValueWaveform:
				if _, err := r.evidenceFor(i.Waveform.SOPClassUID, i.Waveform.SOPInstanceUID, entry.path.String()); err != nil {
					return err
				}
			}
		}
		w.push(i.Children, entry.path, entry.depth+1)
	}
	return nil
}
