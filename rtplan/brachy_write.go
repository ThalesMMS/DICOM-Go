package rtplan

import (
	"context"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// WritePlan writes an admitted conventional photon, Ion or detailed brachy
// profile over an explicit common-module template.
func WritePlan(ctx context.Context, p *Plan, opts WriteOptions) (*object.File, error) {
	if p == nil {
		return nil, ErrInvalidObject
	}
	if p.SOPClassUID != RTPlanStorage && p.SOPClassUID != RTIonPlanStorage {
		return nil, ErrUnsupportedSOPClass
	}
	f, w, err := beginWrite(ctx, opts, p.SOPClassUID, false, p)
	if err != nil {
		return nil, err
	}
	before, err := ReadWithOptions(ctx, opts.Template, ReadOptions{Limits: opts.Limits})
	if err != nil {
		return nil, err
	}
	photon := len(p.Beams) != 0
	changed := !semanticEqual(before, p)
	if photon != (len(before.Beams) != 0) {
		return nil, pathError(ErrUnsupportedFeature, tagBeamSequence.String(), "photon authoring requires a conventional RT Plan template")
	}
	if photon {
		if err := preparePhotonWrite(ctx, before, p, f.Dataset, opts, changed); err != nil {
			return nil, err
		}
	}
	writePlanCommon(w, f.Dataset, p)
	switch {
	case photon:
		writePhoton(w, f.Dataset, p)
	case p.SOPClassUID == RTIonPlanStorage:
		writeIon(w, f.Dataset, p)
	default:
		writeBrachy(w, f.Dataset, p)
	}
	if w.err != nil {
		return nil, w.err
	}
	actual, err := ReadWithOptions(ctx, f, ReadOptions{Limits: opts.Limits})
	if err != nil {
		return nil, err
	}
	if !semanticEqual(p, actual) {
		return nil, pathError(ErrInvalidObject, "", "model cannot be represented by the selected writer profile")
	}
	return finishWrite(f, w, opts, p.SOPClassUID, changed)
}
func optFloat(w *writeBuilder, o *object.Object, t core.Tag, v *float64) {
	_, present := o.Get(t)
	w.float(o, t, Attribute[float64]{v != nil || present, v})
}
func optInt(w *writeBuilder, o *object.Object, t core.Tag, v *int) {
	_, present := o.Get(t)
	w.int(o, t, Attribute[int]{v != nil || present, v})
}
func (w *writeBuilder) reference(o *object.Object, seq core.Tag, refs []Reference) {
	old, _ := o.GetSequence(seq)
	byUID := map[string]*object.Object{}
	for _, it := range old {
		uid, _ := it.GetUID(tagRefSOPInstanceUID)
		if byUID[uid] != nil {
			w.err = pathError(ErrInvalidObject, seq.String(), "template reference is duplicated")
		}
		byUID[uid] = it
	}
	items := make([]*object.Object, 0, len(refs))
	for _, ref := range refs {
		it := byUID[ref.SOPInstanceUID]
		if it == nil {
			it = object.New(nil)
		}
		w.str(it, tagRefSOPClassUID, core.VRUI, ref.SOPClassUID)
		w.str(it, tagRefSOPInstanceUID, core.VRUI, ref.SOPInstanceUID)
		items = append(items, it)
	}
	o.Remove(seq)
	if len(items) > 0 {
		putSequence(o, seq, items)
	}
}
func writePlanCommon(w *writeBuilder, o *object.Object, p *Plan) {
	for _, x := range []struct {
		t  core.Tag
		vr core.VR
		s  string
	}{
		{tagStudyInstanceUID, core.VRUI, p.StudyInstanceUID}, {tagSeriesInstanceUID, core.VRUI, p.SeriesInstanceUID}, {tagFrameOfReferenceUID, core.VRUI, p.FrameOfReferenceUID}, {tagRTPlanLabel, core.VRSH, p.Label}, {tagRTPlanName, core.VRLO, p.Name}, {tagRTPlanDescription, core.VRST, p.Description}, {tagRTPlanDate, core.VRDA, p.Date}, {tagRTPlanTime, core.VRTM, p.Time}, {tagPlanIntent, core.VRCS, p.Intent}, {tagRTPlanGeometry, core.VRCS, p.Geometry}, {tagApprovalStatus, core.VRCS, p.ApprovalStatus}, {tagPrescriptionDescription, core.VRST, p.PrescriptionDescription}} {
		if x.s != "" || o.Has(x.t) {
			w.str(o, x.t, x.vr, x.s)
		}
	}
	refs := []Reference(nil)
	if p.ReferencedStructureSet != nil {
		refs = append(refs, *p.ReferencedStructureSet)
	}
	w.reference(o, tagReferencedStructureSetSequence, refs)
	w.reference(o, tagReferencedDoseSequence, p.ReferencedDoses)
	pr := make([]Reference, len(p.ReferencedPlans))
	for i, v := range p.ReferencedPlans {
		pr[i] = v.Reference
	}
	w.reference(o, tagReferencedRTPlanSequence, pr)
	pis, _ := o.GetSequence(tagReferencedRTPlanSequence)
	for i, it := range pis {
		w.optionalText(it, tagRTPlanRelationship, core.VRCS, p.ReferencedPlans[i].Relationship)
	}
	if len(pis) > 0 {
		putSequence(o, tagReferencedRTPlanSequence, pis)
	}
	old := w.keyed(o, tagDoseReferenceSequence, tagDoseReferenceNumber)
	items := []*object.Object{}
	for _, d := range p.DoseReferences {
		it := itemForKey(old, d.Number)
		w.integer(it, tagDoseReferenceNumber, d.Number)
		w.optionalText(it, tagDoseReferenceUID, core.VRUI, d.UID)
		w.str(it, tagDoseReferenceStructureType, core.VRCS, d.StructureType)
		w.str(it, tagDoseReferenceType, core.VRCS, d.Type)
		w.optionalText(it, tagDoseReferenceDescription, core.VRLO, d.Description)
		optInt(w, it, tagReferencedROINumber, d.ReferencedROINumber)
		it.Remove(tagDoseReferencePointCoords)
		if len(d.PointCoordinates) > 0 {
			w.numbers(it, tagDoseReferencePointCoords, d.PointCoordinates)
		}
		for _, x := range []struct {
			t core.Tag
			v *float64
		}{{tagDeliveryMaximumDose, d.DeliveryMaximumDose}, {tagTargetMinimumDose, d.TargetMinimumDose}, {tagTargetPrescriptionDose, d.TargetPrescriptionDose}, {tagTargetMaximumDose, d.TargetMaximumDose}, {tagOrganAtRiskMaximumDose, d.OrganAtRiskMaximumDose}} {
			optFloat(w, it, x.t, x.v)
		}
		items = append(items, it)
	}
	o.Remove(tagDoseReferenceSequence)
	if len(items) > 0 {
		putSequence(o, tagDoseReferenceSequence, items)
	}
	old = w.keyed(o, tagPatientSetupSequence, tagPatientSetupNumber)
	items = nil
	for _, s := range p.PatientSetups {
		it := itemForKey(old, s.Number)
		w.integer(it, tagPatientSetupNumber, s.Number)
		w.optionalText(it, tagPatientSetupLabel, core.VRLO, s.Label)
		w.optionalText(it, tagPatientPosition, core.VRCS, s.PatientPosition)
		w.optionalText(it, tagPatientAdditionalPosition, core.VRLO, s.PatientAdditionalPosition)
		w.optionalText(it, tagSetupTechnique, core.VRCS, s.SetupTechnique)
		items = append(items, it)
	}
	o.Remove(tagPatientSetupSequence)
	if len(items) > 0 {
		putSequence(o, tagPatientSetupSequence, items)
	}
	toleranceSequence := tagToleranceTableSequence
	if p.SOPClassUID == RTIonPlanStorage {
		toleranceSequence = ft(0x300a03a0)
		o.Remove(tagToleranceTableSequence)
	}
	old = w.keyed(o, toleranceSequence, tagToleranceTableNumber)
	items = nil
	for _, s := range p.ToleranceTables {
		it := itemForKey(old, s.Number)
		w.integer(it, tagToleranceTableNumber, s.Number)
		w.optionalText(it, tagToleranceTableLabel, core.VRSH, s.Label)
		for _, x := range []struct {
			t core.Tag
			v *float64
		}{{tagGantryAngleTolerance, s.GantryAngle}, {tagBLDAngleTolerance, s.BeamLimitingDeviceAngle}, {tagPatientSupportAngleTol, s.PatientSupportAngle}, {tagTableTopEccentricAngleTol, s.TableTopEccentricAngle}, {tagTableTopVerticalTol, s.TableTopVerticalPosition}, {tagTableTopLongitudinalTol, s.TableTopLongitudinalPosition}, {tagTableTopLateralTol, s.TableTopLateralPosition}} {
			optFloat(w, it, x.t, x.v)
		}
		oldDevices, _ := it.GetSequence(tagBLDToleranceSequence)
		byType := map[string]*object.Object{}
		for _, v := range oldDevices {
			key, _ := v.GetString(tagRTBeamLimitingDeviceType)
			byType[key] = v
		}
		devices := []*object.Object{}
		for _, v := range s.BeamLimitingDevicePositionLimit {
			d := byType[v.DeviceType]
			if d == nil {
				d = object.New(nil)
			}
			w.str(d, tagRTBeamLimitingDeviceType, core.VRCS, v.DeviceType)
			w.numbers(d, tagBLDPositionTolerance, []float64{v.Position})
			devices = append(devices, d)
		}
		it.Remove(tagBLDToleranceSequence)
		if len(devices) > 0 {
			putSequence(it, tagBLDToleranceSequence, devices)
		}
		items = append(items, it)
	}
	o.Remove(toleranceSequence)
	if len(items) > 0 {
		putSequence(o, toleranceSequence, items)
	}
	old = w.keyed(o, tagFractionGroupSequence, tagFractionGroupNumber)
	items = nil
	for _, s := range p.FractionGroups {
		it := itemForKey(old, s.Number)
		w.integer(it, tagFractionGroupNumber, s.Number)
		w.optionalText(it, tagFractionGroupDescription, core.VRLT, s.Description)
		optInt(w, it, tagNumberOfFractionsPlanned, s.FractionsPlanned)
		optInt(w, it, tagFractionPatternDigitsPerDay, s.FractionPatternDigitsDay)
		optInt(w, it, tagRepeatFractionCycleLength, s.RepeatFractionCycleLength)
		w.optionalText(it, tagFractionPattern, core.VRLT, s.FractionPattern)
		w.optionalText(it, tagBeamDoseMeaning, core.VRCS, s.BeamDoseMeaning)
		w.integer(it, tagNumberOfBeams, s.NumberOfBeams)
		w.integer(it, tagNumberOfBrachySetups, s.NumberOfBrachySetups)
		rb := w.keyed(it, tagReferencedBeamSequence, tagReferencedBeamNumber)
		bs := []*object.Object{}
		for _, v := range s.ReferencedBeams {
			b := itemForKey(rb, v.BeamNumber)
			w.integer(b, tagReferencedBeamNumber, v.BeamNumber)
			optFloat(w, b, tagBeamDose, v.Dose)
			optFloat(w, b, tagBeamMeterset, v.Meterset)
			bs = append(bs, b)
		}
		it.Remove(tagReferencedBeamSequence)
		if len(bs) > 0 {
			putSequence(it, tagReferencedBeamSequence, bs)
		}
		rs := w.keyed(it, tagReferencedBrachySetupSequence, tagReferencedBrachySetupNumber)
		bs = nil
		for _, v := range s.ReferencedBrachySetups {
			b := itemForKey(rs, v.SetupNumber)
			w.integer(b, tagReferencedBrachySetupNumber, v.SetupNumber)
			optFloat(w, b, tagBrachySetupDose, v.Dose)
			bs = append(bs, b)
		}
		it.Remove(tagReferencedBrachySetupSequence)
		if len(bs) > 0 {
			putSequence(it, tagReferencedBrachySetupSequence, bs)
		}
		rd := w.keyed(it, tagReferencedDoseReferenceSeq, tagReferencedDoseReferenceNumber)
		bs = nil
		for _, v := range s.ReferencedDoseReference {
			b := itemForKey(rd, v.Number)
			w.integer(b, tagReferencedDoseReferenceNumber, v.Number)
			for _, x := range []struct {
				t core.Tag
				v *float64
			}{{tagConstraintWeight, v.ConstraintWeight}, {tagDeliveryMaximumDose, v.DeliveryMaximumDose}, {tagTargetPrescriptionDose, v.TargetPrescriptionDose}} {
				optFloat(w, b, x.t, x.v)
			}
			bs = append(bs, b)
		}
		it.Remove(tagReferencedDoseReferenceSeq)
		if len(bs) > 0 {
			putSequence(it, tagReferencedDoseReferenceSeq, bs)
		}
		items = append(items, it)
	}
	o.Remove(tagFractionGroupSequence)
	if len(items) > 0 {
		putSequence(o, tagFractionGroupSequence, items)
	}
}
func writeBrachy(w *writeBuilder, o *object.Object, p *Plan) {
	o.Remove(tagBeamSequence)
	o.Remove(ft(0x300a03a2))
	w.str(o, tagBrachyTreatmentTechnique, core.VRCS, p.BrachyTreatmentTechnique)
	w.str(o, tagBrachyTreatmentType, core.VRCS, p.BrachyTreatmentType)
	old := w.keyed(o, ft(0x300a0210), ft(0x300a0212))
	sources := []*object.Object{}
	for _, s := range p.Sources {
		it := itemForKey(old, s.Number)
		w.integer(it, ft(0x300a0212), s.Number)
		w.str(it, ft(0x300a0214), core.VRCS, s.Type)
		w.optionalText(it, ft(0x30080105), core.VRLO, s.SerialNumber)
		w.str(it, ft(0x300a0226), core.VRLO, s.IsotopeName)
		w.optionalText(it, ft(0x300a0229), core.VRCS, s.StrengthUnits)
		w.float(it, ft(0x300a0228), s.IsotopeHalfLife)
		w.float(it, ft(0x300a022a), s.ReferenceAirKermaRate)
		w.float(it, ft(0x300a022b), s.Strength)
		w.str(it, ft(0x300a022c), core.VRDA, s.StrengthReferenceDate)
		w.str(it, ft(0x300a022e), core.VRTM, s.StrengthReferenceTime)
		sources = append(sources, it)
	}
	putSequence(o, ft(0x300a0210), sources)
	old = w.keyed(o, tagApplicationSetupSequence, tagApplicationSetupNumber)
	setups := []*object.Object{}
	for _, s := range p.BrachySetups {
		it := itemForKey(old, s.Number)
		w.integer(it, tagApplicationSetupNumber, s.Number)
		w.str(it, tagApplicationSetupType, core.VRCS, s.Type)
		w.optionalText(it, tagApplicationSetupName, core.VRLO, s.Name)
		optFloat(w, it, tagTotalReferenceAirKerma, s.TotalReferenceAirKerma)
		previous := w.keyed(it, tagChannelSequence, ft(0x300a0282))
		channels := []*object.Object{}
		for _, c := range s.Channels {
			ch := itemForKey(previous, c.Number)
			w.integer(ch, ft(0x300a0282), c.Number)
			w.integer(ch, tagReferencedSourceNumber, c.SourceNumber)
			w.str(ch, ft(0x300a0288), core.VRCS, c.Movement)
			for _, x := range []struct {
				t core.Tag
				v Attribute[float64]
			}{{ft(0x300a0284), c.Length}, {ft(0x300a0286), c.TotalTime}, {ft(0x300a02a0), c.StepSize}, {ft(0x300a028c), c.PulseInterval}, {ft(0x300a02a4), c.TransferTubeLength}, {ft(0x300a02c8), c.FinalCumulativeTimeWeight}} {
				w.float(ch, x.t, x.v)
			}
			w.int(ch, ft(0x300a028a), c.NumberOfPulses)
			w.int(ch, ft(0x300a02a2), c.TransferTubeNumber)
			w.integer(ch, tagNumberOfControlPoints, len(c.ControlPoints))
			cpOld := w.keyed(ch, ft(0x300a02d0), tagControlPointIndex)
			points := []*object.Object{}
			for _, cp := range c.ControlPoints {
				v := itemForKey(cpOld, cp.Index)
				w.integer(v, tagControlPointIndex, cp.Index)
				w.float(v, ft(0x300a02d6), cp.CumulativeTimeWeight)
				w.numbers(v, ft(0x300a02d2), []float64{cp.RelativePosition})
				v.Remove(ft(0x300a02d4))
				v.Remove(ft(0x300a0412))
				if cp.Position3DPresent || len(cp.Position3D) > 0 {
					w.numbers(v, ft(0x300a02d4), cp.Position3D)
				}
				if cp.OrientationPresent || len(cp.Orientation) > 0 {
					w.numbers(v, ft(0x300a0412), cp.Orientation)
				}
				points = append(points, v)
			}
			putSequence(ch, ft(0x300a02d0), points)
			channels = append(channels, ch)
		}
		putSequence(it, tagChannelSequence, channels)
		setups = append(setups, it)
	}
	putSequence(o, tagApplicationSetupSequence, setups)
}
