package tractography

import (
	"context"
	"reflect"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Read returns a detached stored-result model. It never loads deferred values
// or closes the Object. Any error, including cancellation, returns nil.
func Read(ctx context.Context, src *object.Object, opts ReadOptions) (*Results, error) {
	b, err := operation(ctx, opts.Limits)
	if err != nil {
		return nil, err
	}
	o, err := b.facade(src)
	if err != nil {
		return nil, err
	}
	r := &Results{Origin: UnknownOrigin}
	sop, err := stringValue(o, tSOPClass, core.VRUI, true)
	if err != nil || sop != Storage {
		return nil, invalid("SOP class")
	}
	mod, err := stringValue(o, tModality, core.VRCS, true)
	if err != nil || mod != "MR" {
		return nil, invalid("Modality")
	}
	for _, f := range []struct {
		t        core.Tag
		vr       core.VR
		v        *string
		required bool
	}{
		{tSOPInstance, core.VRUI, &r.SOPInstanceUID, true}, {tStudy, core.VRUI, &r.StudyInstanceUID, true}, {tSeries, core.VRUI, &r.SeriesInstanceUID, true}, {tFrame, core.VRUI, &r.FrameOfReferenceUID, true},
		{tDate, core.VRDA, &r.ContentDate, true}, {tTime, core.VRTM, &r.ContentTime, true}, {tLabel, core.VRCS, &r.Label, true}, {tDescription, core.VRLO, &r.Description, false},
	} {
		*f.v, err = stringValue(o, f.t, f.vr, f.required)
		if err != nil {
			return nil, err
		}
	}
	r.SeriesNumber, err = scalarIS(o, tSeriesNumber)
	if err != nil {
		return nil, err
	}
	r.InstanceNumber, err = scalarIS(o, tInstanceNumber)
	if err != nil {
		return nil, err
	}
	sets, err := sequence(o, tSets, true)
	if err != nil {
		return nil, err
	}
	r.TrackSets = make([]TrackSet, len(sets))
	for i, s := range sets {
		r.TrackSets[i], err = readSet(b, s)
		if err != nil {
			return nil, err
		}
	}
	r.References, err = readReferences(b, o, r.StudyInstanceUID)
	if err != nil {
		return nil, err
	}
	if len(r.References) > 0 {
		r.Origin = DICOMImages
	}
	if opts.RequireDICOMReferences && r.Origin == UnknownOrigin {
		return nil, ErrInvalidReference
	}
	if o.Has(tPPS) {
		v, e := sequence(o, tPPS, true)
		if e != nil || len(v) != 1 {
			return nil, invalid("PPS cardinality")
		}
		p, e := readPair(v[0])
		if e != nil {
			return nil, e
		}
		r.PerformedProcedureStep = &p
	}
	if err = modelValid(b, r, false); err != nil {
		return nil, err
	}
	// Validate complete mandatory/conditional IOD structure before returning.
	if err = checkIOD(o, b); err != nil {
		return nil, err
	}
	r.Metadata, err = cloneObject(b.ctx, o, true)
	if err != nil {
		return nil, err
	}
	if err = metadataValid(b, r.Metadata); err != nil {
		return nil, err
	}
	if err = b.ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}
func readSet(b *budget, o *object.Object) (TrackSet, error) {
	var s TrackSet
	var err error
	n, err := ints(o, tNumber, core.VRUL)
	if err != nil || len(n) != 1 || n[0] < 1 {
		return s, invalid("set number")
	}
	s.Number = uint32(n[0])
	s.Label, err = stringValue(o, tSetLabel, core.VRLO, true)
	if err != nil {
		return s, err
	}
	s.Description, err = stringValue(o, tSetDescription, core.VRUT, false)
	if err != nil {
		return s, err
	}
	s.Anatomy, err = readCode(o, tAnatomy)
	if err != nil {
		return s, err
	}
	anatomy, _ := sequence(o, tAnatomy, true)
	if anatomy[0].Has(tModifier) {
		ms, e := sequence(anatomy[0], tModifier, true)
		if e != nil {
			return s, e
		}
		s.AnatomyModifiers = make([]Code, len(ms))
		for i, m := range ms {
			s.AnatomyModifiers[i], err = readCodeItem(m)
			if err != nil {
				return s, err
			}
		}
	}
	s.Color, err = color(o)
	if err != nil {
		return s, err
	}
	if o.Has(tThickness) {
		f, e := scalarFL(o, tThickness)
		if e != nil {
			return s, e
		}
		v := f
		s.LineThickness = &v
	}
	s.Model, err = readCode(o, tModel)
	if err != nil {
		return s, err
	}
	if o.Has(tAcquisition) {
		v, e := readCode(o, tAcquisition)
		if e != nil {
			return s, e
		}
		s.Acquisition = &v
	}
	as, err := sequence(o, tAlgorithms, true)
	if err != nil {
		return s, err
	}
	s.Algorithms = make([]Algorithm, len(as))
	for i, o := range as {
		a := &s.Algorithms[i]
		a.Family, err = readCode(o, tFamily)
		if err != nil {
			return s, err
		}
		if o.Has(tNameCode) {
			v, e := readCode(o, tNameCode)
			if e != nil {
				return s, e
			}
			a.NameCode = &v
		}
		for _, f := range []struct {
			t   core.Tag
			vr  core.VR
			v   *string
			req bool
		}{{tName, core.VRLO, &a.Name, true}, {tVersion, core.VRLO, &a.Version, true}, {tParameters, core.VRLT, &a.Parameters, false}, {tAlgorithmSource, core.VRLO, &a.Source, false}} {
			*f.v, err = stringValue(o, f.t, f.vr, f.req)
			if err != nil {
				return s, err
			}
		}
	}
	ts, err := sequence(o, tTracks, true)
	if err != nil {
		return s, err
	}
	s.Tracks = make([]Track, len(ts))
	for i, o := range ts {
		if err = b.ctx.Err(); err != nil {
			return s, err
		}
		v, e := f32(b.ctx, o, tPoints)
		if e != nil {
			return s, e
		}
		if len(v) < 6 || len(v)%3 != 0 {
			return s, invalid("xyz count")
		}
		t := &s.Tracks[i]
		t.Points = make([][3]float32, len(v)/3)
		for i := range t.Points {
			if i%1024 == 0 {
				if err = b.ctx.Err(); err != nil {
					return s, err
				}
			}
			copy(t.Points[i][:], v[i*3:i*3+3])
		}
		t.Color, err = color(o)
		if err != nil {
			return s, err
		}
		if o.Has(tColors) {
			e, _ := o.Get(tColors)
			if uint64(e.Length()) != uint64(len(t.Points))*6 {
				return s, invalid("point color count")
			}
		}
		t.PointColors, err = pointColors(b.ctx, o)
		if err != nil {
			return s, err
		}
	}
	ms, err := sequence(o, tMeasurements, false)
	if err != nil {
		return s, err
	}
	if ms != nil {
		s.Measurements = make([]Measurement, len(ms))
	}
	for i, o := range ms {
		m := &s.Measurements[i]
		m.Quantity, err = readCode(o, tQuantity)
		if err != nil {
			return s, err
		}
		m.Units, err = readCode(o, tUnits)
		if err != nil {
			return s, err
		}
		vs, e := sequence(o, tValuesSeq, true)
		if e != nil {
			return s, e
		}
		if len(vs) != len(s.Tracks) {
			return s, invalid("measurement track count")
		}
		m.Tracks = make([]MeasurementValues, len(vs))
		for j, o := range vs {
			v := &m.Tracks[j]
			v.Values, err = f32(b.ctx, o, tFloats)
			if err != nil {
				return s, err
			}
			if o.Has(tIndices) {
				v.PointIndices, err = u32(b.ctx, o, tIndices)
				if err != nil {
					return s, err
				}
			}
		}
	}
	ss, err := sequence(o, tTrackStats, false)
	if err != nil {
		return s, err
	}
	if ss != nil {
		s.TrackStatistics = make([]TrackStatistic, len(ss))
	}
	for i, o := range ss {
		v := &s.TrackStatistics[i]
		v.Quantity, err = readCode(o, tQuantity)
		if err != nil {
			return s, err
		}
		v.Modifier, err = readCode(o, tModifier)
		if err != nil {
			return s, err
		}
		v.Units, err = readCode(o, tUnits)
		if err != nil {
			return s, err
		}
		v.Values, err = f32(b.ctx, o, tFloats)
		if err != nil {
			return s, err
		}
	}
	ss, err = sequence(o, tSetStats, false)
	if err != nil {
		return s, err
	}
	if ss != nil {
		s.Statistics = make([]TrackSetStatistic, len(ss))
	}
	for i, o := range ss {
		v := &s.Statistics[i]
		v.Quantity, err = readCode(o, tQuantity)
		if err != nil {
			return s, err
		}
		v.Modifier, err = readCode(o, tModifier)
		if err != nil {
			return s, err
		}
		v.Units, err = readCode(o, tUnits)
		if err != nil {
			return s, err
		}
		v.Value, err = scalarFloat(o, tScalar, core.VRFD)
		if err != nil {
			return s, err
		}
	}
	return s, nil
}
func readPair(o *object.Object) (SOPReference, error) {
	var r SOPReference
	var err error
	r.SOPClassUID, err = stringValue(o, tRefClass, core.VRUI, true)
	if err != nil {
		return r, err
	}
	r.SOPInstanceUID, err = stringValue(o, tRefInstance, core.VRUI, true)
	return r, err
}
func readReferences(b *budget, o *object.Object, study string) ([]ImageReference, error) {
	// Build the hierarchy independently; flat references must agree exactly.
	h := map[string]ImageReference{}
	addSeries := func(parent *object.Object, study string) error {
		ss, err := sequence(parent, tRefSeries, true)
		if err != nil {
			return err
		}
		for _, s := range ss {
			series, err := stringValue(s, tSeries, core.VRUI, true)
			if err != nil {
				return err
			}
			rs, err := sequence(s, tRefs, true)
			if err != nil {
				return err
			}
			for _, item := range rs {
				p, err := readPair(item)
				if err != nil {
					return err
				}
				r := ImageReference{SOPReference: p, StudyInstanceUID: study, SeriesInstanceUID: series}
				if _, exists := h[p.SOPInstanceUID]; exists {
					return ErrInvalidReference
				}
				h[p.SOPInstanceUID] = r
				if len(h) > b.l.MaxReferences {
					return limited("hierarchical references")
				}
				if item.Has(tFrames) || item.Has(tSegments) {
					return invalid("selectors in hierarchy")
				}
			}
		}
		return nil
	}
	if o.Has(tRefSeries) {
		if err := addSeries(o, study); err != nil {
			return nil, err
		}
	}
	if o.Has(tOtherStudies) {
		ss, err := sequence(o, tOtherStudies, true)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, s := range ss {
			id, err := stringValue(s, tStudy, core.VRUI, true)
			if err != nil {
				return nil, err
			}
			if id == study || seen[id] {
				return nil, ErrInvalidReference
			}
			seen[id] = true
			if err := addSeries(s, id); err != nil {
				return nil, err
			}
		}
	}
	rs, err := sequence(o, tRefs, false)
	if err != nil {
		return nil, err
	}
	if len(rs) > b.l.MaxReferences {
		return nil, limited("references")
	}
	out := make([]ImageReference, 0, len(rs))
	seen := map[string]ImageReference{}
	for _, o := range rs {
		p, err := readPair(o)
		if err != nil {
			return nil, err
		}
		r, ok := h[p.SOPInstanceUID]
		if !ok || r.SOPClassUID != p.SOPClassUID {
			return nil, ErrInvalidReference
		}
		if o.Has(tFrames) {
			v, e := ints(o, tFrames, core.VRIS)
			if e != nil {
				return nil, e
			}
			r.Frames = make([]int32, len(v))
			for i, f := range v {
				if f <= 0 || f > 2147483647 {
					return nil, ErrInvalidReference
				}
				r.Frames[i] = int32(f)
			}
		}
		if o.Has(tSegments) {
			v, e := ints(o, tSegments, core.VRUS)
			if e != nil {
				return nil, e
			}
			r.Segments = make([]uint16, len(v))
			for i, s := range v {
				if s == 0 {
					return nil, ErrInvalidReference
				}
				r.Segments[i] = uint16(s)
			}
		}
		if old, duplicate := seen[p.SOPInstanceUID]; duplicate {
			if !reflect.DeepEqual(old, r) {
				return nil, ErrInvalidReference
			}
			continue
		}
		seen[p.SOPInstanceUID] = r
		out = append(out, r)
	}
	if len(seen) != len(h) {
		return nil, ErrInvalidReference
	}
	return out, nil
}
