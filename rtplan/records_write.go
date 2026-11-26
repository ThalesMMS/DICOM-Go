package rtplan

import (
	"context"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// WriteTreatmentRecord rebuilds the admitted typed properties on an independent
// matching template. It preserves optional unmodelled attributes by item
// identity. An ambiguous identity is refused, never matched by array position.
// Changed semantic content requires an explicitly distinct SOP Instance UID.
func WriteTreatmentRecord(ctx context.Context, rec *TreatmentRecord, opts WriteOptions) (*object.File, error) {
	if rec == nil {
		return nil, ErrInvalidObject
	}
	if !oneOf(rec.SOPClassUID, RTBeamsTreatmentRecordStorage, RTIonBeamsTreatmentRecordStorage, RTBrachyTreatmentRecordStorage, RTTreatmentSummaryRecordStorage) {
		return nil, ErrUnsupportedSOPClass
	}
	f, w, err := beginWrite(ctx, opts, rec.SOPClassUID, false, rec)
	if err != nil {
		return nil, err
	}
	before, err := ReadTreatmentRecord(ctx, opts.Template, ReadOptions{Limits: opts.Limits})
	if err != nil {
		return nil, err
	}
	rsWriteCommon(w, f.Dataset, rec)
	switch rec.SOPClassUID {
	case RTBeamsTreatmentRecordStorage, RTIonBeamsTreatmentRecordStorage:
		rsWriteBeams(w, f.Dataset, rec)
	case RTBrachyTreatmentRecordStorage:
		rsWriteBrachy(w, f.Dataset, rec)
	case RTTreatmentSummaryRecordStorage:
		if rec.Summary == nil {
			return nil, pathError(ErrInvalidObject, ft(0x30080200).String(), "summary is missing")
		}
		rsWriteSummary(w, f.Dataset, rec.Summary)
	}
	if rec.SOPClassUID != RTTreatmentSummaryRecordStorage {
		if rec.Summary != nil {
			rsWriteSummary(w, f.Dataset, rec.Summary)
		} else {
			for _, t := range []core.Tag{ft(0x30080200), ft(0x30080202), ft(0x30080054), ft(0x30080056), ft(0x30080220), ft(0x300800e0), ft(0x30080050)} {
				f.Dataset.Remove(t)
			}
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	actual, err := ReadTreatmentRecord(ctx, f, ReadOptions{Limits: opts.Limits})
	if err != nil {
		return nil, err
	}
	if !semanticEqual(rec, actual) {
		return nil, pathError(ErrInvalidObject, "", "model cannot be represented by the selected record profile")
	}
	return finishWrite(f, w, opts, rec.SOPClassUID, !semanticEqual(before, rec))
}

func rsDeclaredText(w *writeBuilder, o *object.Object, t core.Tag, vr core.VR, a Attribute[string]) {
	o.Remove(t)
	if a.Present {
		value := ""
		if a.Value != nil {
			value = *a.Value
		}
		w.str(o, t, vr, value)
	}
}
func rsTick(w *writeBuilder) bool {
	if w.err != nil {
		return false
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return false
	}
	return true
}
func rsAttrKey(a Attribute[int]) string {
	if a.Value == nil {
		return "-"
	}
	return strconv.Itoa(*a.Value)
}
func rsObjectIntKey(o *object.Object, t core.Tag) string {
	r := &reader{}
	return rsAttrKey(intAttribute(r, o, t, ""))
}
func rsCPKey(index Attribute[int], date, time string) string {
	return rsAttrKey(index) + "/" + date + "/" + time
}
func rsCPObjectKey(o *object.Object) string {
	r := &reader{}
	return rsCPKey(intAttribute(r, o, ft(0x300c00f0), ""), r.str(o, ft(0x30080024)), r.str(o, ft(0x30080025)))
}
func rsOldItems(w *writeBuilder, o *object.Object, seq core.Tag, key func(*object.Object) string) map[string]*object.Object {
	out := map[string]*object.Object{}
	items, _ := o.GetSequence(seq)
	for _, it := range items {
		if !rsTick(w) {
			break
		}
		k := key(it)
		if out[k] != nil {
			w.err = pathError(ErrInvalidObject, seq.String(), "template item identity is ambiguous")
			return out
		}
		out[k] = it
	}
	return out
}
func rsNewItem(w *writeBuilder, old map[string]*object.Object, seen map[string]bool, key string, seq core.Tag) *object.Object {
	if seen[key] {
		w.err = pathError(ErrInvalidObject, seq.String(), "model item identity is ambiguous")
	}
	seen[key] = true
	if o := old[key]; o != nil {
		return o
	}
	return object.New(std.Dictionary)
}
func rsSequence(o *object.Object, t core.Tag, items []*object.Object, required bool) {
	o.Remove(t)
	if required || len(items) > 0 {
		putSequence(o, t, items)
	}
}

func rsWriteCommon(w *writeBuilder, o *object.Object, rec *TreatmentRecord) {
	w.str(o, tagStudyInstanceUID, core.VRUI, rec.StudyInstanceUID)
	w.str(o, tagSeriesInstanceUID, core.VRUI, rec.SeriesInstanceUID)
	w.integer(o, ft(0x00200013), rec.InstanceNumber)
	w.str(o, tagModality, core.VRCS, "RTRECORD")
	rsDeclaredText(w, o, ft(0x30080250), core.VRDA, rec.Date)
	rsDeclaredText(w, o, ft(0x30080251), core.VRTM, rec.Time)
	rsDeclaredText(w, o, ft(0x00080201), core.VRSH, rec.TimezoneOffset)
	rsDeclaredText(w, o, ft(0x300a0709), core.VRCS, rec.ContentOrigin)
	refs := []Reference(nil)
	if rec.ReferencedPlan != nil {
		refs = append(refs, *rec.ReferencedPlan)
	}
	w.reference(o, tagReferencedRTPlanSequence, refs)
	if len(refs) == 0 {
		putSequence(o, tagReferencedRTPlanSequence, nil)
	}
	w.reference(o, ft(0x30080030), rec.RelatedRecords)
	w.int(o, ft(0x300c0022), rec.FractionGroupNumber)
	w.int(o, tagNumberOfFractionsPlanned, rec.FractionsPlanned)
	w.optionalText(o, tagPrimaryDosimeterUnit, core.VRCS, rec.PrimaryDosimeterUnit)
	if rec.Machine == nil {
		o.Remove(ft(0x300a0206))
	} else {
		items, _ := o.GetSequence(ft(0x300a0206))
		m := object.New(std.Dictionary)
		if len(items) == 1 {
			m = items[0]
		}
		for _, x := range []struct {
			t  core.Tag
			vr core.VR
			a  Attribute[string]
		}{{tagTreatmentMachineName, core.VRSH, rec.Machine.Name}, {ft(0x00080070), core.VRLO, rec.Machine.Manufacturer}, {ft(0x00080080), core.VRLO, rec.Machine.Institution}, {ft(0x00081090), core.VRLO, rec.Machine.Model}, {ft(0x00181000), core.VRLO, rec.Machine.SerialNumber}} {
			rsDeclaredText(w, m, x.t, x.vr, x.a)
		}
		w.optionalText(m, ft(0x300a0054), core.VRUI, rec.Machine.TableTopPositionAlignmentUID)
		putSequence(o, ft(0x300a0206), []*object.Object{m})
	}
	old := w.keyed(o, tagPatientSetupSequence, tagPatientSetupNumber)
	items := []*object.Object{}
	for _, v := range rec.PatientSetups {
		if !rsTick(w) {
			return
		}
		s := itemForKey(old, v.Number)
		w.integer(s, tagPatientSetupNumber, v.Number)
		w.optionalText(s, tagPatientSetupLabel, core.VRLO, v.Label)
		w.optionalText(s, tagPatientPosition, core.VRCS, v.PatientPosition)
		w.optionalText(s, tagPatientAdditionalPosition, core.VRLO, v.PatientAdditionalPosition)
		w.optionalText(s, tagSetupTechnique, core.VRCS, v.SetupTechnique)
		items = append(items, s)
	}
	rsSequence(o, tagPatientSetupSequence, items, false)
	rsWriteDoseReferences(w, o, rec.MeasuredDoseReferences, false)
	rsWriteDoseReferences(w, o, rec.CalculatedDoseReferences, true)
}

func rsWriteDoseReferences(w *writeBuilder, o *object.Object, values []RecordDoseReference, calc bool) {
	seq, num, val, desc := ft(0x30080010), ft(0x30080064), ft(0x30080016), ft(0x30080012)
	if calc {
		seq, num, val, desc = ft(0x30080070), ft(0x30080072), ft(0x30080076), ft(0x30080074)
	}
	old := rsOldItems(w, o, seq, func(o *object.Object) string {
		r := &reader{}
		return rsDoseKey(intAttribute(r, o, num, ""), intAttribute(r, o, tagReferencedDoseReferenceNumber, ""))
	})
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, v := range values {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsDoseKey(v.Number, v.PlanNumber), seq)
		w.int(it, num, v.Number)
		w.int(it, tagReferencedDoseReferenceNumber, v.PlanNumber)
		w.float(it, val, v.Value)
		w.optionalText(it, desc, core.VRST, v.Description)
		if !calc {
			w.str(it, ft(0x30040002), core.VRCS, v.Units)
			rsDeclaredText(w, it, ft(0x30080014), core.VRCS, v.Type)
		}
		items = append(items, it)
	}
	rsSequence(o, seq, items, false)
}
func rsWriteDoseValues(w *writeBuilder, o *object.Object, values []RecordDoseValue, calc bool) {
	seq, num, val := ft(0x30080080), ft(0x30080082), ft(0x30080016)
	if calc {
		seq, num, val = ft(0x30080090), ft(0x30080092), ft(0x30080076)
	}
	old := rsOldItems(w, o, seq, func(o *object.Object) string {
		r := &reader{}
		return rsDoseKey(intAttribute(r, o, num, ""), intAttribute(r, o, tagReferencedDoseReferenceNumber, ""))
	})
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, v := range values {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsDoseKey(v.LocalNumber, v.PlanNumber), seq)
		w.int(it, num, v.LocalNumber)
		w.int(it, tagReferencedDoseReferenceNumber, v.PlanNumber)
		w.numbers(it, val, []float64{v.Value})
		items = append(items, it)
	}
	rsSequence(o, seq, items, false)
}

func rsBeamKey(b RecordBeamSession) string {
	key := rsAttrKey(b.ReferencedBeamNumber) + "/" + rsAttrKey(b.FractionNumber)
	if len(b.ControlPoints) > 0 {
		key += "/" + b.ControlPoints[0].Date + "/" + b.ControlPoints[0].Time
	}
	if b.ReferencedBeamNumber.Value == nil {
		key += "/" + b.Name + "/" + b.Type + "/" + b.RadiationType
	}
	return key
}
func rsBeamObjectKey(o *object.Object, cpseq core.Tag) string {
	r := &reader{}
	b := RecordBeamSession{ReferencedBeamNumber: intAttribute(r, o, tagReferencedBeamNumber, ""), FractionNumber: intAttribute(r, o, ft(0x30080022), ""), Name: r.str(o, tagBeamName), Type: r.str(o, tagBeamType), RadiationType: r.str(o, tagRadiationType)}
	if points := r.sequence(o, cpseq); len(points) > 0 {
		b.ControlPoints = []RecordControlPoint{{Date: r.str(points[0], ft(0x30080024)), Time: r.str(points[0], ft(0x30080025))}}
	}
	return rsBeamKey(b)
}
func rsWriteBeams(w *writeBuilder, o *object.Object, rec *TreatmentRecord) {
	ion := rec.SOPClassUID == RTIonBeamsTreatmentRecordStorage
	seq, cpseq := ft(0x30080020), ft(0x30080040)
	if ion {
		seq, cpseq = ft(0x30080021), ft(0x30080041)
	}
	old := rsOldItems(w, o, seq, func(o *object.Object) string { return rsBeamObjectKey(o, cpseq) })
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, b := range rec.BeamSessions {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsBeamKey(b), seq)
		w.int(it, tagReferencedBeamNumber, b.ReferencedBeamNumber)
		w.int(it, tagReferencedPatientSetupNumber, b.ReferencedPatientSetupNumber)
		w.int(it, tagReferencedToleranceTableNumber, b.ReferencedToleranceTableNumber)
		w.optionalText(it, tagBeamName, core.VRLO, b.Name)
		w.optionalText(it, tagBeamDescription, core.VRST, b.Description)
		w.str(it, tagBeamType, core.VRCS, b.Type)
		w.str(it, tagRadiationType, core.VRCS, b.RadiationType)
		rsWriteSession(w, it, b.FractionNumber, b.DeliveryType, b.VerificationStatus, b.TerminationStatus)
		w.int(it, ft(0x300a0705), b.ClinicalFractionNumber)
		for _, x := range []struct {
			t core.Tag
			a Attribute[float64]
		}{{ft(0x30080032), b.SpecifiedPrimaryMeterset}, {ft(0x30080033), b.SpecifiedSecondaryMeterset}, {ft(0x30080036), b.DeliveredPrimaryMeterset}, {ft(0x30080037), b.DeliveredSecondaryMeterset}, {ft(0x3008003a), b.SpecifiedTreatmentTime}, {ft(0x3008003b), b.DeliveredTreatmentTime}, {tagSourceAxisDistance, b.SourceAxisDistance}} {
			w.float(it, x.t, x.a)
		}
		for _, t := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks} {
			if ion || t == tagNumberOfWedges || it.Has(t) {
				w.integer(it, t, 0)
			}
		}
		deviceOld := rsOldItems(w, it, ft(0x300800a0), func(o *object.Object) string { v, _ := o.GetString(tagRTBeamLimitingDeviceType); return clean(v) })
		devSeen := map[string]bool{}
		devices := []*object.Object{}
		for _, d := range b.Devices {
			v := rsNewItem(w, deviceOld, devSeen, d.Type, ft(0x300800a0))
			w.str(v, tagRTBeamLimitingDeviceType, core.VRCS, d.Type)
			w.integer(v, tagNumberOfLeafJawPairs, d.Pairs)
			devices = append(devices, v)
		}
		rsSequence(it, ft(0x300800a0), devices, !ion)
		if ion {
			w.int(it, ft(0x300a0304), b.AtomicNumber)
			w.int(it, ft(0x300a0302), b.MassNumber)
			w.int(it, ft(0x300a0306), b.ChargeState)
			w.str(it, ft(0x300a0308), core.VRCS, b.ScanMode)
			w.str(it, ft(0x300a0350), core.VRCS, b.PatientSupportType)
			for _, t := range []core.Tag{ft(0x300a0330), ft(0x300a0340)} {
				w.integer(it, t, 0)
			}
			snouts, _ := it.GetSequence(ft(0x300800f0))
			snout := object.New(std.Dictionary)
			if len(snouts) == 1 {
				snout = snouts[0]
			}
			if b.SnoutID != "" {
				w.str(snout, ft(0x300a030f), core.VRSH, b.SnoutID)
				putSequence(it, ft(0x300800f0), []*object.Object{snout})
			} else {
				it.Remove(ft(0x300800f0))
			}
			shOld := w.keyed(it, ft(0x300800f2), ft(0x300c0100))
			shifters := []*object.Object{}
			for _, s := range b.RangeShifters {
				sh := itemForKey(shOld, s.Number)
				w.integer(sh, ft(0x300c0100), s.Number)
				w.str(sh, ft(0x300a0318), core.VRSH, s.ID)
				shifters = append(shifters, sh)
			}
			w.integer(it, ft(0x300a0312), len(shifters))
			rsSequence(it, ft(0x300800f2), shifters, false)
		}
		w.reference(it, ft(0x300c0040), b.VerificationImages)
		rsWriteDoseValues(w, it, b.MeasuredDoses, false)
		rsWriteDoseValues(w, it, b.CalculatedDoses, true)
		cpOld := rsOldItems(w, it, cpseq, rsCPObjectKey)
		cpSeen := map[string]bool{}
		points := []*object.Object{}
		for _, cp := range b.ControlPoints {
			if !rsTick(w) {
				return
			}
			p := rsNewItem(w, cpOld, cpSeen, rsCPKey(cp.ReferencedIndex, cp.Date, cp.Time), cpseq)
			w.int(p, ft(0x300c00f0), cp.ReferencedIndex)
			w.str(p, ft(0x30080024), core.VRDA, cp.Date)
			w.str(p, ft(0x30080025), core.VRTM, cp.Time)
			w.float(p, ft(0x30080042), cp.SpecifiedMeterset)
			w.numbers(p, ft(0x30080044), []float64{cp.DeliveredMeterset})
			rateSet, rateDel := tagDoseRateSet, ft(0x30080048)
			if ion {
				rateSet, rateDel = ft(0x30080045), ft(0x30080046)
			}
			w.float(p, rateSet, cp.RateSet)
			w.float(p, rateDel, cp.RateDelivered)
			w.optionalText(p, ft(0x300a0015), core.VRCS, cp.EnergyUnit)
			rsWriteState(w, p, cp.State)
			for _, t := range []core.Tag{ft(0x300a0390), ft(0x300a0392), ft(0x300a0394), ft(0x30080047), ft(0x300a039a), ft(0x300a0393), ft(0x300a0391)} {
				p.Remove(t)
			}
			if ion && b.ScanMode == "MODULATED" {
				w.str(p, ft(0x300a0390), core.VRSH, cp.ScanSpotTuneID)
				w.integer(p, ft(0x300a0392), len(cp.SpotMetersets))
				w.fl(p, ft(0x300a0394), cp.SpotPositions)
				w.fl(p, ft(0x30080047), cp.SpotMetersets)
				w.int(p, ft(0x300a039a), cp.NumberOfPaintings)
				rsDeclaredText(w, p, ft(0x300a0393), core.VRCS, cp.SpotReordered)
				if len(cp.SpotPrescribedIndices) > 0 {
					out := make(core.StringValue, len(cp.SpotPrescribedIndices))
					for i, n := range cp.SpotPrescribedIndices {
						s, e := object.FormatIS(int64(n))
						if e != nil {
							w.err = pathError(ErrInvalidObject, ft(0x300a0391).String(), "spot index cannot be encoded")
						}
						out[i] = s
					}
					p.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x300a0391), VR: core.VRIS}, Value: out})
				}
			}
			points = append(points, p)
		}
		w.integer(it, tagNumberOfControlPoints, len(points))
		putSequence(it, cpseq, points)
		items = append(items, it)
	}
	putSequence(o, seq, items)
}
func rsWriteSession(w *writeBuilder, o *object.Object, fraction Attribute[int], delivery, verification Attribute[string], termination string) {
	w.int(o, ft(0x30080022), fraction)
	rsDeclaredText(w, o, tagTreatmentDeliveryType, core.VRCS, delivery)
	rsDeclaredText(w, o, ft(0x3008002c), core.VRCS, verification)
	w.str(o, ft(0x3008002a), core.VRCS, termination)
}
func rsWriteState(w *writeBuilder, o *object.Object, state RecordMachineState) {
	for _, x := range rsStateNumbers(&state) {
		w.float(o, x.t, *x.a)
	}
	for _, x := range rsStateTexts(&state) {
		rsDeclaredText(w, o, x.t, core.VRCS, *x.a)
	}
	old := rsOldItems(w, o, tagBLDPositionSequence, func(o *object.Object) string { v, _ := o.GetString(tagRTBeamLimitingDeviceType); return clean(v) })
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, d := range state.DevicePositions {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, d.DeviceType, tagBLDPositionSequence)
		w.str(it, tagRTBeamLimitingDeviceType, core.VRCS, d.DeviceType)
		w.numbers(it, tagLeafJawPositions, d.Positions)
		items = append(items, it)
	}
	rsSequence(o, tagBLDPositionSequence, items, false)
	shOld := w.keyed(o, ft(0x300a0360), ft(0x300c0100))
	items = nil
	for _, s := range state.RangeShifterSettings {
		if !rsTick(w) {
			return
		}
		it := itemForKey(shOld, s.Number)
		w.integer(it, ft(0x300c0100), s.Number)
		w.str(it, ft(0x300a0362), core.VRLO, s.Setting)
		w.float(it, ft(0x300a0364), s.Distance)
		w.float(it, ft(0x300a0366), s.WaterEquivalentThickness)
		items = append(items, it)
	}
	rsSequence(o, ft(0x300a0360), items, false)
}

func rsSetupKey(s RecordBrachySession) string {
	key := rsAttrKey(s.ReferencedSetupNumber) + "/" + rsAttrKey(s.FractionNumber)
	if s.ReferencedSetupNumber.Value == nil {
		key += "/" + s.Type + "/" + s.Name
	}

	return key
}
func rsSetupObjectKey(o *object.Object) string {
	r := &reader{}
	s := RecordBrachySession{ReferencedSetupNumber: intAttribute(r, o, tagReferencedBrachySetupNumber, ""), FractionNumber: intAttribute(r, o, ft(0x30080022), ""), Type: r.str(o, tagApplicationSetupType), Name: r.str(o, tagApplicationSetupName)}
	chs := r.sequence(o, ft(0x30080130))
	if len(chs) > 0 {
		points := r.sequence(chs[0], ft(0x30080160))
		if len(points) > 0 {
			s.Channels = []RecordBrachyChannel{{ControlPoints: []RecordBrachyControlPoint{{Date: r.str(points[0], ft(0x30080024)), Time: r.str(points[0], ft(0x30080025))}}}}
		}
	}
	return rsSetupKey(s)
}
func rsWriteBrachy(w *writeBuilder, o *object.Object, rec *TreatmentRecord) {
	w.str(o, tagBrachyTreatmentTechnique, core.VRCS, rec.BrachyTechnique)
	w.str(o, tagBrachyTreatmentType, core.VRCS, rec.BrachyType)
	sourceOld := w.keyed(o, ft(0x30080100), ft(0x300a0212))
	sources := []*object.Object{}
	for _, s := range rec.BrachySources {
		if !rsTick(w) {
			return
		}
		it := itemForKey(sourceOld, s.Number)
		w.integer(it, ft(0x300a0212), s.Number)
		w.str(it, ft(0x300a0214), core.VRCS, s.Type)
		w.str(it, ft(0x300a0226), core.VRLO, s.IsotopeName)
		w.optionalText(it, ft(0x300a0229), core.VRCS, s.StrengthUnits)
		for _, x := range []struct {
			t core.Tag
			a Attribute[float64]
		}{{ft(0x300a0228), s.IsotopeHalfLife}, {ft(0x300a022a), s.ReferenceAirKermaRate}, {ft(0x300a022b), s.Strength}} {
			w.float(it, x.t, x.a)
		}
		w.str(it, ft(0x300a022c), core.VRDA, s.StrengthReferenceDate)
		w.str(it, ft(0x300a022e), core.VRTM, s.StrengthReferenceTime)
		rsDeclaredText(w, it, ft(0x300a0216), core.VRLO, s.Manufacturer)
		rsDeclaredText(w, it, ft(0x30080105), core.VRLO, s.RecordedSerialNumber)
		w.optionalText(it, ft(0x300a021b), core.VRSH, s.ModelID)
		sources = append(sources, it)
	}
	putSequence(o, ft(0x30080100), sources)
	old := rsOldItems(w, o, ft(0x30080110), rsSetupObjectKey)
	seen := map[string]bool{}
	setups := []*object.Object{}
	for _, s := range rec.BrachySessions {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsSetupKey(s), ft(0x30080110))
		w.int(it, tagReferencedBrachySetupNumber, s.ReferencedSetupNumber)
		w.str(it, tagApplicationSetupType, core.VRCS, s.Type)
		w.optionalText(it, tagApplicationSetupName, core.VRLO, s.Name)
		w.optionalText(it, ft(0x30080116), core.VRCS, s.Check)
		w.numbers(it, tagTotalReferenceAirKerma, []float64{s.TotalReferenceAirKerma})
		rsWriteSession(w, it, s.FractionNumber, s.DeliveryType, s.VerificationStatus, s.TerminationStatus)
		w.reference(it, ft(0x300c0040), s.VerificationImages)
		rsWriteDoseValues(w, it, s.MeasuredDoses, false)
		rsWriteDoseValues(w, it, s.CalculatedDoses, true)
		chOld := w.keyed(it, ft(0x30080130), ft(0x300a0282))
		channels := []*object.Object{}
		for _, ch := range s.Channels {
			if !rsTick(w) {
				return
			}
			c := itemForKey(chOld, ch.Number)
			w.integer(c, ft(0x300a0282), ch.Number)
			w.integer(c, tagReferencedSourceNumber, ch.SourceNumber)
			w.int(c, ft(0x00741406), ch.ReferencedChannelNumber)
			w.str(c, ft(0x300a0288), core.VRCS, ch.Movement)
			w.numbers(c, ft(0x30080132), []float64{ch.SpecifiedTime})
			w.numbers(c, ft(0x30080134), []float64{ch.DeliveredTime})
			for _, x := range []struct {
				t core.Tag
				a Attribute[float64]
			}{{ft(0x300a0284), ch.Length}, {ft(0x300a0271), ch.EffectiveLength}, {ft(0x300a0272), ch.InnerLength}, {ft(0x300a02a4), ch.TransferTubeLength}, {ft(0x3008013a), ch.SpecifiedPulseInterval}, {ft(0x3008013c), ch.DeliveredPulseInterval}} {
				w.float(c, x.t, x.a)
			}
			w.optionalText(c, ft(0x300a0273), core.VRSH, ch.AfterloaderChannelID)
			w.int(c, ft(0x300a02a2), ch.TransferTubeNumber)
			w.int(c, ft(0x30080136), ch.SpecifiedPulses)
			w.int(c, ft(0x30080138), ch.DeliveredPulses)
			rsWriteSafe(w, c, ch.SafePosition)
			rsWriteDoseValues(w, c, ch.MeasuredDoses, false)
			rsWriteDoseValues(w, c, ch.CalculatedDoses, true)
			appOld := rsOldItems(w, c, ft(0x30080140), func(o *object.Object) string {
				r := &reader{}
				return rsAttrKey(intAttribute(r, o, ft(0x30080142), "")) + "/" + r.str(o, ft(0x300a0291))
			})
			appSeen := map[string]bool{}
			apps := []*object.Object{}
			for _, a := range ch.Applicators {
				key := rsAttrKey(a.Number) + "/"
				if a.ID.Value != nil {
					key += *a.ID.Value
				}
				app := rsNewItem(w, appOld, appSeen, key, ft(0x30080140))
				w.int(app, ft(0x30080142), a.Number)
				rsDeclaredText(w, app, ft(0x300a0291), core.VRSH, a.ID)
				w.str(app, ft(0x300a0292), core.VRCS, a.Type)
				w.optionalText(app, ft(0x300a0294), core.VRLO, a.Name)
				w.numbers(app, ft(0x300a0296), []float64{a.Length})
				w.float(app, ft(0x300a0274), a.TipLength)
				w.float(app, ft(0x300a02a0), a.StepSize)
				apps = append(apps, app)
			}
			rsSequence(c, ft(0x30080140), apps, false)
			rsWriteBrachyCP(w, c, ft(0x30080160), ch.ControlPoints)
			w.integer(c, tagNumberOfControlPoints, len(ch.ControlPoints))
			pulseOld := w.keyed(c, ft(0x30080171), ft(0x30080172))
			pulses := []*object.Object{}
			for _, p := range ch.Pulses {
				if !rsTick(w) {
					return
				}
				pulse := itemForKey(pulseOld, p.Number)
				w.integer(pulse, ft(0x30080172), p.Number)
				rsWriteSafe(w, pulse, p.SafePosition)
				rsWriteBrachyCP(w, pulse, ft(0x30080173), p.ControlPoints)
				pulses = append(pulses, pulse)
			}
			rsSequence(c, ft(0x30080171), pulses, false)
			channels = append(channels, c)
		}
		putSequence(it, ft(0x30080130), channels)
		setups = append(setups, it)
	}
	putSequence(o, ft(0x30080110), setups)
}
func rsWriteSafe(w *writeBuilder, o *object.Object, s RecordSafePosition) {
	for _, x := range []struct {
		t  core.Tag
		vr core.VR
		v  string
	}{{ft(0x30080162), core.VRDA, s.ExitDate}, {ft(0x30080164), core.VRTM, s.ExitTime}, {ft(0x30080166), core.VRDA, s.ReturnDate}, {ft(0x30080168), core.VRTM, s.ReturnTime}} {
		w.optionalText(o, x.t, x.vr, x.v)
	}
}
func rsWriteBrachyCP(w *writeBuilder, o *object.Object, seq core.Tag, points []RecordBrachyControlPoint) {
	old := rsOldItems(w, o, seq, rsCPObjectKey)
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, p := range points {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsCPKey(p.ReferencedIndex, p.Date, p.Time), seq)
		w.int(it, ft(0x300c00f0), p.ReferencedIndex)
		w.str(it, ft(0x30080024), core.VRDA, p.Date)
		w.str(it, ft(0x30080025), core.VRTM, p.Time)
		w.numbers(it, ft(0x300a02d2), []float64{p.RelativePosition})
		items = append(items, it)
	}
	putSequence(o, seq, items)
}

func rsSummaryGroupKey(g RecordFractionGroupSummary) string {
	key := rsAttrKey(g.ReferencedNumber)
	if g.Type.Value != nil {
		key += "/" + *g.Type.Value
	}
	return key
}
func rsWriteSummary(w *writeBuilder, o *object.Object, s *RecordSummary) {
	w.str(o, ft(0x30080200), core.VRCS, s.Status)
	w.optionalText(o, ft(0x30080202), core.VRST, s.Comment)
	rsDeclaredText(w, o, ft(0x30080054), core.VRDA, s.FirstDate)
	rsDeclaredText(w, o, ft(0x30080056), core.VRDA, s.MostRecentDate)
	old := rsOldItems(w, o, ft(0x30080220), func(o *object.Object) string {
		r := &reader{}
		return rsSummaryGroupKey(RecordFractionGroupSummary{ReferencedNumber: intAttribute(r, o, ft(0x300c0022), ""), Type: rsText(r, o, ft(0x30080224))})
	})
	seen := map[string]bool{}
	groups := []*object.Object{}
	for _, g := range s.FractionGroups {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsSummaryGroupKey(g), ft(0x30080220))
		w.int(it, ft(0x300c0022), g.ReferencedNumber)
		rsDeclaredText(w, it, ft(0x30080224), core.VRCS, g.Type)
		w.int(it, tagNumberOfFractionsPlanned, g.Planned)
		w.int(it, ft(0x3008005a), g.Delivered)
		fractionOld := w.keyed(it, ft(0x30080240), ft(0x30080223))
		fractions := []*object.Object{}
		for _, f := range g.Fractions {
			if !rsTick(w) {
				return
			}
			v := itemForKey(fractionOld, f.Number)
			w.integer(v, ft(0x30080223), f.Number)
			w.int(v, ft(0x300a0705), f.ClinicalNumber)
			rsDeclaredText(w, v, ft(0x30080250), core.VRDA, f.Date)
			rsDeclaredText(w, v, ft(0x30080251), core.VRTM, f.Time)
			rsDeclaredText(w, v, ft(0x3008002a), core.VRCS, f.TerminationStatus)
			fractions = append(fractions, v)
		}
		rsSequence(it, ft(0x30080240), fractions, false)
		groups = append(groups, it)
	}
	rsSequence(o, ft(0x30080220), groups, false)
	rsWriteSummaryDoses(w, o, ft(0x300800e0), s.MeasuredDoses)
	rsWriteSummaryDoses(w, o, ft(0x30080050), s.CalculatedDoses)
}
func rsWriteSummaryDoses(w *writeBuilder, o *object.Object, seq core.Tag, doses []RecordSummaryDose) {
	old := rsOldItems(w, o, seq, func(o *object.Object) string {
		r := &reader{}
		a := intAttribute(r, o, tagReferencedDoseReferenceNumber, "")
		if a.Value != nil {
			return rsAttrKey(a)
		}
		return "-/" + r.str(o, tagDoseReferenceDescription)
	})
	seen := map[string]bool{}
	items := []*object.Object{}
	for _, d := range doses {
		if !rsTick(w) {
			return
		}
		it := rsNewItem(w, old, seen, rsSummaryDoseKey(d), seq)
		w.int(it, tagReferencedDoseReferenceNumber, d.PlanNumber)
		w.optionalText(it, tagDoseReferenceDescription, core.VRLO, d.Description)
		w.numbers(it, ft(0x30080052), []float64{d.CumulativeDose})
		items = append(items, it)
	}
	rsSequence(o, seq, items, false)
}

func rsSummaryDoseKey(d RecordSummaryDose) string {
	if d.PlanNumber.Value != nil {
		return rsAttrKey(d.PlanNumber)
	}
	return "-/" + d.Description
}
