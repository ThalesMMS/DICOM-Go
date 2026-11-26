package tractography

import (
	"context"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"math"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Write constructs a new independent Object. Common.Source supplies metadata,
// not pixels. Missing image references cannot establish OtherSource: the caller
// must explicitly resolve Origin. Errors return nil without modifying inputs.
func Write(ctx context.Context, r *Results, opts WriteOptions) (*object.Object, error) {
	b, err := operation(ctx, opts.Limits)
	if err != nil {
		return nil, err
	}
	if r != nil && r.Metadata != nil && (r.Metadata.Has(tag(0x4ffe, 1)) || r.Metadata.Has(tag(0xfffa, 0xfffa))) {
		return nil, unsupported("signed metadata rewrite")
	}
	if err = b.model(r); err != nil {
		return nil, err
	}
	if err = modelValid(b, r, true); err != nil {
		return nil, err
	}
	var metadata, commonSource *object.Object
	if r.Metadata != nil {
		metadata, err = b.facade(r.Metadata)
		if err != nil {
			return nil, err
		}
		if b.signatures {
			return nil, unsupported("signed metadata rewrite")
		}
		if err = metadataValid(b, metadata); err != nil {
			return nil, err
		}
	}
	if opts.Common.Source != nil {
		commonSource, err = b.facade(opts.Common.Source)
		if err != nil {
			return nil, err
		}
		if b.nestedSignatures {
			return nil, unsupported("signed common metadata rewrite")
		}
		for _, f := range []struct {
			t    core.Tag
			want string
		}{{tStudy, r.StudyInstanceUID}, {tFrame, r.FrameOfReferenceUID}} {
			if commonSource.Has(f.t) {
				v, e := stringValue(commonSource, f.t, core.VRUI, true)
				if e != nil || v != f.want {
					return nil, invalid("common source identity")
				}
			}
		}
		commonSource, err = cloneObject(b.ctx, commonSource, true)
		if err != nil {
			return nil, err
		}
		commonSource.Remove(tag(0x4ffe, 1))
		commonSource.Remove(tag(0xfffa, 0xfffa))
		if err = metadataValid(b, commonSource); err != nil {
			return nil, err
		}
	} else {
		commonSource = metadata
	}
	if !opts.Common.Now.IsZero() && (opts.Common.Now.Year() < 1 || opts.Common.Now.Year() > 9999) {
		return nil, invalid("creation date")
	}
	if opts.Common.Equipment != nil {
		e := opts.Common.Equipment
		for _, s := range []string{e.Manufacturer, e.ModelName, e.DeviceSerialNumber, e.SoftwareVersions} {
			if !requiredText(s, 64, false) {
				return nil, invalid("enhanced equipment")
			}
			if err = b.textValues(s); err != nil {
				return nil, err
			}
		}
	}
	o := newObject()
	if metadata != nil {
		o, err = cloneObject(b.ctx, metadata, true)
		if err != nil {
			return nil, err
		}
	}
	o.Put(textElement(tCharset, core.VRCS, "ISO_IR 192"))
	for _, f := range []struct {
		t  core.Tag
		vr core.VR
		s  string
	}{
		{tSOPClass, core.VRUI, Storage}, {tSOPInstance, core.VRUI, r.SOPInstanceUID}, {tStudy, core.VRUI, r.StudyInstanceUID}, {tSeries, core.VRUI, r.SeriesInstanceUID}, {tFrame, core.VRUI, r.FrameOfReferenceUID},
		{tModality, core.VRCS, "MR"}, {tDate, core.VRDA, r.ContentDate}, {tTime, core.VRTM, r.ContentTime}, {tLabel, core.VRCS, r.Label}, {tDescription, core.VRLO, r.Description},
	} {
		o.Put(textElement(f.t, f.vr, f.s))
	}
	putIS(o, tSeriesNumber, r.SeriesNumber)
	putIS(o, tInstanceNumber, r.InstanceNumber)
	if !o.Has(tag(0x20, 0x1040)) {
		o.Put(textElement(tag(0x20, 0x1040), core.VRLO, ""))
	}
	if opts.Common.Equipment != nil {
		for _, t := range []core.Tag{tag(8, 0x70), tag(8, 0x1090), tag(0x18, 0x1000), tag(0x18, 0x1020)} {
			o.Remove(t)
		}
	}
	if opts.Common.Equipment == nil {
		found := 0
		for _, t := range []core.Tag{tag(8, 0x70), tag(8, 0x1090), tag(0x18, 0x1000), tag(0x18, 0x1020)} {
			if o.Has(t) {
				found++
			}
		}
		if found != 0 && found != 4 {
			return nil, invalid("partial retained equipment")
		}
	}
	common := opts.Common
	common.Source = commonSource
	if err = common.Apply(o, object.InheritAll); err != nil {
		return nil, invalid("common modules")
	}
	creation, e := stringValue(o, tag(8, 0x12), core.VRDA, true)
	if e != nil || len(creation) != 8 {
		return nil, invalid("creation date")
	}
	if _, e = dcmtime.ParseDA(creation); e != nil {
		return nil, invalid("creation date")
	}
	sets := make([]*object.Object, len(r.TrackSets))
	for i, s := range r.TrackSets {
		sets[i], err = writeSet(b, s)
		if err != nil {
			return nil, err
		}
	}
	o.Put(sequenceElement(tSets, sets...))
	if r.PerformedProcedureStep != nil {
		o.Put(sequenceElement(tPPS, pair(*r.PerformedProcedureStep)))
	}
	rs, err := canonicalReferences(b.ctx, r, nil)
	if err != nil {
		return nil, err
	}
	if len(rs) > 0 {
		if err = writeReferences(b.ctx, o, rs, r.StudyInstanceUID); err != nil {
			return nil, err
		}
	}
	if err = checkIOD(o, b); err != nil {
		return nil, err
	}
	if err = b.ctx.Err(); err != nil {
		return nil, err
	}
	return o, nil
}
func putColor(o *object.Object, c *[3]uint16) {
	if c != nil {
		o.Put(numberElement(tColor, core.VRUS, core.Uint16Value{c[0], c[1], c[2]}))
	}
}
func writeSet(b *budget, s TrackSet) (*object.Object, error) {
	o := newObject()
	o.Put(numberElement(tNumber, core.VRUL, core.Uint32Value{s.Number}))
	o.Put(textElement(tSetLabel, core.VRLO, s.Label))
	if s.Description != "" {
		o.Put(textElement(tSetDescription, core.VRUT, s.Description))
	}
	a := codeItem(s.Anatomy)
	if len(s.AnatomyModifiers) > 0 {
		ms := make([]*object.Object, len(s.AnatomyModifiers))
		for i, c := range s.AnatomyModifiers {
			ms[i] = codeItem(c)
		}
		a.Put(sequenceElement(tModifier, ms...))
	}
	o.Put(sequenceElement(tAnatomy, a))
	putColor(o, s.Color)
	if s.LineThickness != nil {
		o.Put(numberElement(tThickness, core.VRFL, core.Float32Value{*s.LineThickness}))
	}
	putCode(o, tModel, s.Model)
	if s.Acquisition != nil {
		putCode(o, tAcquisition, *s.Acquisition)
	}
	as := make([]*object.Object, len(s.Algorithms))
	for i, a := range s.Algorithms {
		v := newObject()
		putCode(v, tFamily, a.Family)
		if a.NameCode != nil {
			putCode(v, tNameCode, *a.NameCode)
		}
		v.Put(textElement(tName, core.VRLO, a.Name))
		v.Put(textElement(tVersion, core.VRLO, a.Version))
		if a.Parameters != "" {
			v.Put(textElement(tParameters, core.VRLT, a.Parameters))
		}
		if a.Source != "" {
			v.Put(textElement(tAlgorithmSource, core.VRLO, a.Source))
		}
		as[i] = v
	}
	o.Put(sequenceElement(tAlgorithms, as...))
	ts := make([]*object.Object, len(s.Tracks))
	for i, t := range s.Tracks {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		v := newObject()
		points := make(core.RawValue, len(t.Points)*12)
		for j, p := range t.Points {
			if j%1024 == 0 {
				if err := b.ctx.Err(); err != nil {
					return nil, err
				}
			}
			for k, f := range p {
				binary.LittleEndian.PutUint32(points[j*12+k*4:], math.Float32bits(f))
			}
		}
		v.Put(numberElement(tPoints, core.VROF, points))
		putColor(v, t.Color)
		if t.PointColors != nil {
			colors := make(core.RawValue, len(t.PointColors)*6)
			for j, c := range t.PointColors {
				if j%1024 == 0 {
					if err := b.ctx.Err(); err != nil {
						return nil, err
					}
				}
				for k, n := range c {
					binary.LittleEndian.PutUint16(colors[j*6+k*2:], n)
				}
			}
			v.Put(numberElement(tColors, core.VROW, colors))
		}
		ts[i] = v
	}
	o.Put(sequenceElement(tTracks, ts...))
	if len(s.Measurements) > 0 {
		ms := make([]*object.Object, len(s.Measurements))
		for i, m := range s.Measurements {
			v := newObject()
			putCode(v, tQuantity, m.Quantity)
			putCode(v, tUnits, m.Units)
			vs := make([]*object.Object, len(m.Tracks))
			for j, value := range m.Tracks {
				owned, err := encodeOF(b.ctx, value.Values)
				if err != nil {
					return nil, err
				}
				item := newObject()
				item.Put(numberElement(tFloats, core.VROF, owned))
				if value.PointIndices != nil {
					own, err := encodeOL(b.ctx, value.PointIndices)
					if err != nil {
						return nil, err
					}
					item.Put(numberElement(tIndices, core.VROL, own))
				}
				vs[j] = item
			}
			v.Put(sequenceElement(tValuesSeq, vs...))
			ms[i] = v
		}
		o.Put(sequenceElement(tMeasurements, ms...))
	}
	if len(s.TrackStatistics) > 0 {
		ss := make([]*object.Object, len(s.TrackStatistics))
		for i, s := range s.TrackStatistics {
			v := newObject()
			putCode(v, tQuantity, s.Quantity)
			putCode(v, tModifier, s.Modifier)
			putCode(v, tUnits, s.Units)
			own, err := encodeOF(b.ctx, s.Values)
			if err != nil {
				return nil, err
			}
			v.Put(numberElement(tFloats, core.VROF, own))
			ss[i] = v
		}
		o.Put(sequenceElement(tTrackStats, ss...))
	}
	if len(s.Statistics) > 0 {
		ss := make([]*object.Object, len(s.Statistics))
		for i, s := range s.Statistics {
			v := newObject()
			putCode(v, tQuantity, s.Quantity)
			putCode(v, tModifier, s.Modifier)
			putCode(v, tUnits, s.Units)
			v.Put(numberElement(tScalar, core.VRFD, core.Float64Value{s.Value}))
			ss[i] = v
		}
		o.Put(sequenceElement(tSetStats, ss...))
	}
	return o, b.ctx.Err()
}
func pair(r SOPReference) *object.Object {
	o := newObject()
	o.Put(textElement(tRefClass, core.VRUI, r.SOPClassUID))
	o.Put(textElement(tRefInstance, core.VRUI, r.SOPInstanceUID))
	return o
}
func writeReferences(ctx context.Context, o *object.Object, rs []ImageReference, study string) error {
	flat := make([]*object.Object, len(rs))
	byStudy := map[string]*object.Object{}
	bySeries := map[string]*object.Object{}
	studies := []*object.Object{}
	same := []*object.Object{}
	seriesByStudy := map[string][]*object.Object{}
	instances := map[string][]*object.Object{}
	for i, r := range rs {
		if err := ctx.Err(); err != nil {
			return err
		}
		v := pair(r.SOPReference)
		if r.Frames != nil {
			s := make(core.StringValue, len(r.Frames))
			for i, f := range r.Frames {
				if i%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				s[i] = strconv.FormatInt(int64(f), 10)
			}
			v.Put(numberElement(tFrames, core.VRIS, s))
		}
		if r.Segments != nil {
			ownSegments, err := copySlice(ctx, r.Segments)
			if err != nil {
				return err
			}
			v.Put(numberElement(tSegments, core.VRUS, core.Uint16Value(ownSegments)))
		}
		flat[i] = v
		series := bySeries[r.SeriesInstanceUID]
		if series == nil {
			series = newObject()
			series.Put(textElement(tSeries, core.VRUI, r.SeriesInstanceUID))
			bySeries[r.SeriesInstanceUID] = series
			if r.StudyInstanceUID == study {
				same = append(same, series)
			} else {
				if byStudy[r.StudyInstanceUID] == nil {
					s := newObject()
					s.Put(textElement(tStudy, core.VRUI, r.StudyInstanceUID))
					byStudy[r.StudyInstanceUID] = s
					studies = append(studies, s)
				}
				seriesByStudy[r.StudyInstanceUID] = append(seriesByStudy[r.StudyInstanceUID], series)
			}
		}
		instances[r.SeriesInstanceUID] = append(instances[r.SeriesInstanceUID], pair(r.SOPReference))
	}
	for id, s := range bySeries {
		s.Put(sequenceElement(tRefs, instances[id]...))
	}
	for id, s := range byStudy {
		s.Put(sequenceElement(tRefSeries, seriesByStudy[id]...))
	}
	o.Put(sequenceElement(tRefs, flat...))
	if len(same) > 0 {
		o.Put(sequenceElement(tRefSeries, same...))
	}
	if len(studies) > 0 {
		o.Put(sequenceElement(tOtherStudies, studies...))
	}
	return ctx.Err()
}

func encodeOF(ctx context.Context, v []float32) (core.RawValue, error) {
	out := make(core.RawValue, len(v)*4)
	for i, f := range v {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out, ctx.Err()
}
func encodeOL(ctx context.Context, v []uint32) (core.RawValue, error) {
	out := make(core.RawValue, len(v)*4)
	for i, n := range v {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		binary.LittleEndian.PutUint32(out[i*4:], n)
	}
	return out, ctx.Err()
}
