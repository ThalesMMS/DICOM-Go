package rtplan

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func writeIon(w *writeBuilder, o *object.Object, p *Plan) {
	o.Remove(tagBeamSequence)
	o.Remove(tagApplicationSetupSequence)
	o.Remove(ft(0x300a0210))
	o.Remove(tagBrachyTreatmentTechnique)
	o.Remove(tagBrachyTreatmentType)
	previous := w.keyed(o, ft(0x300a03a2), tagBeamNumber)
	beams := []*object.Object{}
	for _, b := range p.IonBeams {
		it := itemForKey(previous, b.Number)
		w.integer(it, tagBeamNumber, b.Number)
		for _, s := range []struct {
			t  core.Tag
			vr core.VR
			v  string
		}{{tagBeamName, core.VRLO, b.Name}, {tagBeamDescription, core.VRST, b.Description}, {tagBeamType, core.VRCS, b.Type}, {tagRadiationType, core.VRCS, b.RadiationType}, {ft(0x300a0308), core.VRCS, b.ScanMode}, {tagTreatmentMachineName, core.VRSH, b.TreatmentMachineName}, {tagPrimaryDosimeterUnit, core.VRCS, b.PrimaryDosimeterUnit}, {tagTreatmentDeliveryType, core.VRCS, b.TreatmentDeliveryType}, {ft(0x300a0350), core.VRCS, b.PatientSupportType}} {
			if s.v != "" || it.Has(s.t) {
				w.str(it, s.t, s.vr, s.v)
			}
		}
		w.numbers(it, ft(0x300a030a), b.VirtualSourceAxisDistances)
		w.int(it, ft(0x300a0302), b.MassNumber)
		w.int(it, ft(0x300a0304), b.AtomicNumber)
		w.int(it, ft(0x300a0306), b.ChargeState)
		optInt(w, it, tagReferencedPatientSetupNumber, b.ReferencedPatientSetupNumber)
		optInt(w, it, tagReferencedToleranceTableNumber, b.ReferencedToleranceTableNumber)
		w.float(it, tagFinalCumulativeMetersetWeight, b.FinalCumulativeMetersetWeight)
		for _, t := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks, ft(0x300a0330), ft(0x300a0340)} {
			w.integer(it, t, 0)
		}
		oldSnouts, _ := it.GetSequence(ft(0x300a030c))
		it.Remove(ft(0x300a030c))
		if b.SnoutID != "" {
			snout := object.New(nil)
			if len(oldSnouts) == 1 {
				id, _ := oldSnouts[0].GetString(ft(0x300a030f))
				if id == b.SnoutID {
					snout = oldSnouts[0]
				}
			}
			w.str(snout, ft(0x300a030f), core.VRSH, b.SnoutID)
			putSequence(it, ft(0x300a030c), []*object.Object{snout})
		}
		oldDevices, _ := it.GetSequence(ft(0x300a03a4))
		byType := map[string]*object.Object{}
		for _, d := range oldDevices {
			key, _ := d.GetString(tagRTBeamLimitingDeviceType)
			if byType[key] != nil {
				w.err = ErrInvalidObject
			}
			byType[key] = d
		}
		devices := []*object.Object{}
		for _, d := range b.Devices {
			v := byType[d.Type]
			if v == nil {
				v = object.New(nil)
			}
			w.str(v, tagRTBeamLimitingDeviceType, core.VRCS, d.Type)
			w.integer(v, tagNumberOfLeafJawPairs, d.Pairs)
			optFloat(w, v, ft(0x300a00bb), d.SourceDistance)
			v.Remove(tagLeafPositionBoundaries)
			if len(d.LeafBoundaries) > 0 {
				w.numbers(v, tagLeafPositionBoundaries, d.LeafBoundaries)
			}
			devices = append(devices, v)
		}
		it.Remove(ft(0x300a03a4))
		if len(devices) > 0 {
			putSequence(it, ft(0x300a03a4), devices)
		}
		rsOld := w.keyed(it, ft(0x300a0314), ft(0x300a0316))
		shifters := []*object.Object{}
		for _, s := range b.RangeShifters {
			v := itemForKey(rsOld, s.Number)
			w.integer(v, ft(0x300a0316), s.Number)
			w.str(v, ft(0x300a0318), core.VRSH, s.ID)
			w.str(v, ft(0x300a0320), core.VRCS, s.Type)
			w.optionalText(v, ft(0x300a0322), core.VRLO, s.Description)
			shifters = append(shifters, v)
		}
		w.integer(it, ft(0x300a0312), len(shifters))
		it.Remove(ft(0x300a0314))
		if len(shifters) > 0 {
			putSequence(it, ft(0x300a0314), shifters)
		}
		cpOld := w.keyed(it, ft(0x300a03a8), tagControlPointIndex)
		points := []*object.Object{}
		for j, cp := range b.ControlPoints {
			v := itemForKey(cpOld, cp.Index)
			writeControlState(w, v, cp.ControlPoint)
			if j == 0 {
				// Ion first-CP Type 2 fields remain present when their value is
				// unknown, including a newly keyed control-point item.
				for _, t := range []core.Tag{tagIsocenterPosition, tagTableTopVerticalPosition, tagTableTopLongitudinalPosition, tagTableTopLateralPosition, tagTableTopPitchAngle, tagTableTopRollAngle} {
					if !v.Has(t) {
						w.numbers(v, t, nil)
					}
				}
			}
			w.float(v, ft(0x300a035a), cp.MetersetRate)
			w.float(v, ft(0x300a030d), cp.SnoutPosition)
			w.float(v, ft(0x300a014a), cp.GantryPitchAngle)
			for _, x := range []struct {
				t core.Tag
				s string
			}{{ft(0x300a014c), cp.GantryPitchRotationDirection}, {ft(0x300a0142), cp.TableTopPitchRotationDirection}, {ft(0x300a0146), cp.TableTopRollRotationDirection}} {
				if j == 0 || x.s != "" || v.Has(x.t) {
					w.str(v, x.t, core.VRCS, x.s)
				}
			}
			settingsOld := w.keyed(v, ft(0x300a0360), ft(0x300c0100))
			settings := []*object.Object{}
			for _, s := range cp.RangeShifterSettings {
				rs := itemForKey(settingsOld, s.Number)
				w.integer(rs, ft(0x300c0100), s.Number)
				w.str(rs, ft(0x300a0362), core.VRLO, s.Setting)
				w.float(rs, ft(0x300a0364), s.Distance)
				w.float(rs, ft(0x300a0366), s.WaterEquivalentThickness)
				settings = append(settings, rs)
			}
			v.Remove(ft(0x300a0360))
			if len(settings) > 0 {
				putSequence(v, ft(0x300a0360), settings)
			}
			for _, t := range []core.Tag{ft(0x300a0390), ft(0x300a0392), ft(0x300a0394), ft(0x300a0396), ft(0x300a039a)} {
				v.Remove(t)
			}
			if b.ScanMode == "MODULATED" {
				w.str(v, ft(0x300a0390), core.VRSH, cp.ScanSpotTuneID)
				w.integer(v, ft(0x300a0392), len(cp.SpotWeights))
				w.fl(v, ft(0x300a0394), cp.SpotPositions)
				w.fl(v, ft(0x300a0396), cp.SpotWeights)
				w.int(v, ft(0x300a039a), cp.NumberOfPaintings)
			}
			points = append(points, v)
		}
		w.integer(it, tagNumberOfControlPoints, len(points))
		putSequence(it, ft(0x300a03a8), points)
		beams = append(beams, it)
	}
	putSequence(o, ft(0x300a03a2), beams)
}
func writeControlState(w *writeBuilder, o *object.Object, cp ControlPoint) {
	w.integer(o, tagControlPointIndex, cp.Index)
	optFloat(w, o, tagCumulativeMetersetWeight, cp.CumulativeMetersetWeight)
	for _, x := range []struct {
		t core.Tag
		v *float64
	}{{tagNominalBeamEnergy, cp.NominalBeamEnergy}, {tagDoseRateSet, cp.DoseRateSet}, {tagTableTopVerticalPosition, cp.TableTopVerticalPosition}, {tagTableTopLongitudinalPosition, cp.TableTopLongitudinalPosition}, {tagTableTopLateralPosition, cp.TableTopLateralPosition}, {tagTableTopPitchAngle, cp.TableTopPitchAngle}, {tagTableTopRollAngle, cp.TableTopRollAngle}, {tagSourceToSurfaceDistance, cp.SourceToSurfaceDistance}} {
		optFloat(w, o, x.t, x.v)
	}
	for _, x := range []struct {
		t     core.Tag
		v     float64
		field Field
	}{{tagGantryAngle, cp.GantryAngle, FieldGantryAngle}, {tagBLDAngle, cp.BeamLimitingDeviceAngle, FieldBeamLimitingDeviceAngle}, {tagPatientSupportAngle, cp.PatientSupportAngle, FieldPatientSupportAngle}, {tagTableTopEccentricAngle, cp.TableTopEccentricAngle, FieldTableTopEccentricAngle}} {
		if cp.Encodes(x.field) || o.Has(x.t) || x.field != FieldTableTopEccentricAngle {
			w.numbers(o, x.t, []float64{x.v})
		} else {
			o.Remove(x.t)
		}
	}
	for _, x := range []struct {
		t core.Tag
		v string
	}{{tagGantryRotationDirection, cp.GantryRotationDirection}, {tagBLDRotationDirection, cp.BeamLimitingDeviceRotationDirection}, {tagPatientSupportRotationDir, cp.PatientSupportRotationDirection}, {tagTableTopEccentricRotationDir, cp.TableTopEccentricRotationDirection}} {
		w.optionalText(o, x.t, core.VRCS, x.v)
	}
	isocenterPresent := o.Has(tagIsocenterPosition) || cp.Encodes(FieldIsocenterPosition)
	o.Remove(tagIsocenterPosition)
	if len(cp.IsocenterPosition) > 0 || isocenterPresent {
		w.numbers(o, tagIsocenterPosition, cp.IsocenterPosition)
	}
	old, _ := o.GetSequence(tagBLDPositionSequence)
	byType := map[string]*object.Object{}
	for _, d := range old {
		key, _ := d.GetString(tagRTBeamLimitingDeviceType)
		byType[key] = d
	}
	devices := []*object.Object{}
	for _, d := range cp.DevicePositions {
		v := byType[d.DeviceType]
		if v == nil {
			v = object.New(nil)
		}
		w.str(v, tagRTBeamLimitingDeviceType, core.VRCS, d.DeviceType)
		w.numbers(v, tagLeafJawPositions, d.Positions)
		devices = append(devices, v)
	}
	o.Remove(tagBLDPositionSequence)
	if len(devices) > 0 {
		putSequence(o, tagBLDPositionSequence, devices)
	}
	refsOld := w.keyed(o, tagReferencedDoseReferenceSeq, tagReferencedDoseReferenceNumber)
	refs := []*object.Object{}
	for _, r := range cp.DoseReferences {
		v := itemForKey(refsOld, r.Number)
		w.integer(v, tagReferencedDoseReferenceNumber, r.Number)
		optFloat(w, v, tagCumulativeDoseRefCoefficient, r.Coefficient)
		refs = append(refs, v)
	}
	o.Remove(tagReferencedDoseReferenceSeq)
	if len(refs) > 0 {
		putSequence(o, tagReferencedDoseReferenceSeq, refs)
	}
}
