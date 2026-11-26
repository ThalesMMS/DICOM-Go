package rtplan

import (
	"context"
	"math"
	"slices"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func preparePhotonWrite(ctx context.Context, before, plan *Plan, dataset *object.Object, opts WriteOptions, changed bool) error {
	if plan.SOPClassUID != RTPlanStorage || len(plan.IonBeams) != 0 ||
		len(plan.Sources) != 0 || len(plan.BrachySetups) != 0 ||
		plan.BrachyTreatmentTechnique != "" || plan.BrachyTreatmentType != "" {
		return pathError(ErrUnsupportedFeature, tagBeamSequence.String(), "mixed beam families are outside the photon profile")
	}
	if err := checkPhotonTemplate(ctx, dataset); err != nil {
		return err
	}
	uid := opts.SOPInstanceUID
	if uid == "" {
		uid = before.SOPInstanceUID
	}
	if plan.ApprovalStatus != "" && !oneOf(plan.ApprovalStatus, "UNAPPROVED", "APPROVED", "REJECTED") {
		return pathError(ErrInvalidObject, tagApprovalStatus.String(), "invalid approval status")
	}
	if changed || uid != before.SOPInstanceUID {
		if plan.ApprovalStatus != "" && plan.ApprovalStatus != "UNAPPROVED" {
			return pathError(ErrInvalidObject, tagApprovalStatus.String(), "a new photon instance cannot carry a prior review")
		}
		for _, tag := range []core.Tag{ft(0x300e0004), ft(0x300e0005), ft(0x300e0008)} {
			dataset.Remove(tag)
		}
	}

	if err := checkPhotonReferences(plan, uid); err != nil {
		return err
	}

	if len(plan.FractionGroups) == 0 {
		return pathError(ErrInvalidObject, tagFractionGroupSequence.String(), "photon plan requires a fraction group")
	}
	for i, group := range plan.FractionGroups {
		path := item("", tagFractionGroupSequence, i)
		if group.NumberOfBeams <= 0 || group.NumberOfBrachySetups != 0 {
			return pathError(ErrInvalidObject, path, "expected beam references in a photon fraction group")
		}
		if group.BeamDoseMeaning != "" && !oneOf(group.BeamDoseMeaning, "BEAM_LEVEL", "FRACTION_LEVEL") {
			return pathError(ErrInvalidObject, join(path, tagBeamDoseMeaning), "invalid Beam Dose Meaning")
		}
	}

	setups := make(map[int]bool, len(plan.PatientSetups))
	for _, setup := range plan.PatientSetups {
		setups[setup.Number] = true
	}
	tolerances := make(map[int]bool, len(plan.ToleranceTables))
	for _, table := range plan.ToleranceTables {
		tolerances[table.Number] = true
	}
	for i := range plan.Beams {
		if err := ctx.Err(); err != nil {
			return err
		}
		beam := &plan.Beams[i]
		path := item("", tagBeamSequence, i)
		if n := beam.ReferencedPatientSetupNumber; n != nil && !setups[*n] {
			return pathError(ErrInvalidObject, join(path, tagReferencedPatientSetupNumber), "referenced patient setup is absent")
		}
		if n := beam.ReferencedToleranceTableNumber; n != nil && !tolerances[*n] {
			return pathError(ErrInvalidObject, join(path, tagReferencedToleranceTableNumber), "referenced tolerance table is absent")
		}
		if err := checkPhotonBeam(ctx, beam, path); err != nil {
			return err
		}
	}
	return nil
}

func checkPhotonReferences(plan *Plan, uid string) error {
	const doseStorage = "1.2.840.10008.5.1.4.1.1.481.2"
	const structureSetStorage = "1.2.840.10008.5.1.4.1.1.481.3"
	valid := func(ref Reference) bool {
		id := ref.SOPInstanceUID
		return core.IsValidUID(id) && id != uid &&
			id != plan.StudyInstanceUID && id != plan.SeriesInstanceUID && id != plan.FrameOfReferenceUID
	}
	if ref := plan.ReferencedStructureSet; ref != nil {
		if plan.Geometry != GeometryPatient || ref.SOPClassUID != structureSetStorage || !valid(*ref) {
			return pathError(ErrInvalidObject, tagReferencedStructureSetSequence.String(), "invalid structure set reference")
		}
	}
	seen := make(map[string]bool)
	for _, ref := range plan.ReferencedDoses {
		if ref.SOPClassUID != doseStorage || !valid(ref) || seen[ref.SOPInstanceUID] {
			return pathError(ErrInvalidObject, tagReferencedDoseSequence.String(), "invalid or duplicate dose reference")
		}
		seen[ref.SOPInstanceUID] = true
	}
	for _, ref := range plan.ReferencedPlans {
		if !oneOf(ref.SOPClassUID, RTPlanStorage, RTIonPlanStorage) || !valid(ref.Reference) || seen[ref.SOPInstanceUID] ||
			!oneOf(ref.Relationship, "PRIOR", "ALTERNATIVE", "PREDECESSOR", "VERIFIED_PLAN", "CONCURRENT") ||
			ref.Relationship == "VERIFIED_PLAN" && plan.Intent != "VERIFICATION" {
			return pathError(ErrInvalidObject, tagReferencedRTPlanSequence.String(), "invalid or duplicate plan reference")
		}
		seen[ref.SOPInstanceUID] = true
	}
	return nil
}

func checkPhotonTemplate(ctx context.Context, dataset *object.Object) error {
	beams, _ := dataset.GetSequence(tagBeamSequence)
	for i, beam := range beams {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := item("", tagBeamSequence, i)
		enhanced := ft(0x300800a3)
		if flag, ok := beam.GetString(enhanced); beam.Has(enhanced) && (!ok || flag != "NO") {
			return pathError(ErrUnsupportedFeature, join(path, enhanced), "enhanced beam limiting device definitions are outside the photon profile")
		}
		for _, tag := range []core.Tag{
			ft(0x300800a1), // Enhanced RT Beam Limiting Device Sequence
			ft(0x30020050), // Primary Fluence Mode Sequence
			ft(0x300a0420), // General Accessory Sequence
			tagWedgeSequence, tagCompensatorSequence, tagReferencedBolusSequence, tagBlockSequence, tagApplicatorSequence,
		} {
			if beam.Has(tag) {
				return pathError(ErrUnsupportedFeature, join(path, tag), "beam modifier is outside the photon profile")
			}
		}
		points, _ := beam.GetSequence(tagControlPointSequence)
		for j, point := range points {
			if err := ctx.Err(); err != nil {
				return err
			}
			for _, tag := range []core.Tag{
				ft(0x300800a2),                 // Enhanced RT Beam Limiting Opening Sequence
				ft(0x300a014a), ft(0x300a014c), // Gantry pitch
				ft(0x300a0142), ft(0x300a0146), // Table top pitch and roll rotation
				tagWedgePositionSequence,
			} {
				if point.Has(tag) {
					return pathError(ErrUnsupportedFeature, join(item(path, tagControlPointSequence, j), tag), "control-point modifier is outside the photon profile")
				}
			}
		}
	}
	return nil
}

func samePhotonNumber(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func checkPhotonBeam(ctx context.Context, beam *Beam, path string) error {
	if beam.RadiationType != "PHOTON" || beam.HighDoseTechniqueType != "" ||
		!oneOf(beam.TreatmentDeliveryType, "", "TREATMENT") ||
		!oneOf(beam.PrimaryDosimeterUnit, "", "MU", "MINUTE") ||
		beam.NumberOfWedges != 0 || beam.NumberOfCompensators != 0 || beam.NumberOfBoli != 0 || beam.NumberOfBlocks != 0 ||
		len(beam.Wedges) != 0 || len(beam.Blocks) != 0 || len(beam.Applicators) != 0 {
		return pathError(ErrUnsupportedFeature, path, "expected a conventional photon treatment beam without accessory modifiers")
	}
	if len(beam.ControlPoints) < 2 || beam.FinalCumulativeMetersetWeight == nil {
		return pathError(ErrInvalidObject, join(path, tagControlPointSequence), "expected at least two weighted control points and final weight")
	}
	first := beam.ControlPoints[0]
	rotationDuringIrradiation := false
	for i, point := range beam.ControlPoints {
		if err := ctx.Err(); err != nil {
			return err
		}
		cpPath := item(path, tagControlPointSequence, i)
		if point.Index != i || point.CumulativeMetersetWeight == nil || i == 0 && *point.CumulativeMetersetWeight != 0 {
			return pathError(ErrInvalidObject, cpPath, "invalid control point index or cumulative weight")
		}
		if !oneOf(point.GantryRotationDirection, RotationNone, RotationClockwise, RotationCounterClockwise) {
			return pathError(ErrInvalidObject, join(cpPath, tagGantryRotationDirection), "invalid rotation direction")
		}
		if len(point.WedgePositions) != 0 || point.TableTopPitchAngle != nil || point.TableTopRollAngle != nil ||
			point.BeamLimitingDeviceRotationDirection != RotationNone || point.PatientSupportRotationDirection != RotationNone ||
			point.TableTopEccentricRotationDirection != RotationNone ||
			point.BeamLimitingDeviceAngle != first.BeamLimitingDeviceAngle || point.PatientSupportAngle != first.PatientSupportAngle ||
			point.TableTopEccentricAngle != first.TableTopEccentricAngle ||
			!samePhotonNumber(point.NominalBeamEnergy, first.NominalBeamEnergy) ||
			!samePhotonNumber(point.TableTopLateralPosition, first.TableTopLateralPosition) ||
			!samePhotonNumber(point.TableTopLongitudinalPosition, first.TableTopLongitudinalPosition) ||
			!samePhotonNumber(point.TableTopVerticalPosition, first.TableTopVerticalPosition) ||
			!slices.Equal(point.IsocenterPosition, first.IsocenterPosition) {
			return pathError(ErrUnsupportedFeature, cpPath, "moving or tilted couch, rotating collimator or variable energy is outside the photon profile")
		}
		if beam.Type == BeamTypeStatic && (point.GantryRotationDirection != RotationNone ||
			point.GantryAngle != first.GantryAngle || !samePhotonNumber(point.DoseRateSet, first.DoseRateSet) ||
			!slices.EqualFunc(point.DevicePositions, first.DevicePositions, samePhotonPosition)) {
			return pathError(ErrInvalidObject, cpPath, "STATIC beam changes its treatment parameters")
		}
		if i > 0 {
			previous := beam.ControlPoints[i-1]
			if previous.GantryRotationDirection == RotationNone && previous.GantryAngle != point.GantryAngle {
				return pathError(ErrInvalidObject, join(cpPath, tagGantryAngle), "gantry angle changed without a preceding rotation")
			}
			if previous.GantryRotationDirection != RotationNone && *previous.CumulativeMetersetWeight < *point.CumulativeMetersetWeight {
				rotationDuringIrradiation = true
			}
		}
	}
	if beam.Type == BeamTypeDynamic && !rotationDuringIrradiation {
		return pathError(ErrUnsupportedFeature, path, "DYNAMIC photon beam requires gantry rotation during irradiation")
	}
	return nil
}

func samePhotonPosition(a, b DevicePosition) bool {
	return a.DeviceType == b.DeviceType && slices.Equal(a.Positions, b.Positions)
}

func writePhoton(w *writeBuilder, dataset *object.Object, plan *Plan) {
	if plan.ApprovalStatus == "" {
		dataset.Remove(tagApprovalStatus)
	}
	old := w.keyed(dataset, tagBeamSequence, tagBeamNumber)
	beams := make([]*object.Object, 0, len(plan.Beams))
	for _, beam := range plan.Beams {
		if !rsTick(w) {
			return
		}
		it := itemForKey(old, beam.Number)
		w.integer(it, tagBeamNumber, beam.Number)
		w.str(it, tagBeamType, core.VRCS, beam.Type)
		w.str(it, tagRadiationType, core.VRCS, beam.RadiationType)
		w.str(it, tagTreatmentMachineName, core.VRSH, beam.TreatmentMachineName)
		w.optionalText(it, tagBeamName, core.VRLO, beam.Name)
		w.optionalText(it, tagBeamDescription, core.VRST, beam.Description)
		w.optionalText(it, tagManufacturer, core.VRLO, beam.Manufacturer)
		w.optionalText(it, tagPrimaryDosimeterUnit, core.VRCS, beam.PrimaryDosimeterUnit)
		w.optionalText(it, tagTreatmentDeliveryType, core.VRCS, beam.TreatmentDeliveryType)
		optFloat(w, it, tagSourceAxisDistance, beam.SourceAxisDistance)
		optInt(w, it, tagReferencedPatientSetupNumber, beam.ReferencedPatientSetupNumber)
		optInt(w, it, tagReferencedToleranceTableNumber, beam.ReferencedToleranceTableNumber)
		optFloat(w, it, tagFinalCumulativeMetersetWeight, beam.FinalCumulativeMetersetWeight)
		for _, tag := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks} {
			w.integer(it, tag, 0)
		}
		writePhotonDevices(w, it, beam.Devices)
		writePhotonControlPoints(w, it, beam.ControlPoints)
		beams = append(beams, it)
	}
	putSequence(dataset, tagBeamSequence, beams)
}

func writePhotonDevices(w *writeBuilder, beam *object.Object, devices []BeamLimitingDevice) {
	key := func(o *object.Object) string {
		deviceType, _ := o.GetString(tagRTBeamLimitingDeviceType)
		return deviceType
	}
	old := rsOldItems(w, beam, tagBLDSequence, key)
	items := make([]*object.Object, 0, len(devices))
	for _, device := range devices {
		it := old[device.Type]
		if it == nil {
			it = object.New(nil)
		}
		w.str(it, tagRTBeamLimitingDeviceType, core.VRCS, device.Type)
		w.integer(it, tagNumberOfLeafJawPairs, device.Pairs)
		optFloat(w, it, tagSourceToBLDDistance, device.SourceDistance)
		if device.LeafBoundaries == nil && !it.Has(tagLeafPositionBoundaries) {
			it.Remove(tagLeafPositionBoundaries)
		} else {
			w.numbers(it, tagLeafPositionBoundaries, device.LeafBoundaries)
		}
		items = append(items, it)
	}
	putSequence(beam, tagBLDSequence, items)
}

func writePhotonControlPoints(w *writeBuilder, beam *object.Object, points []ControlPoint) {
	// PS3.3 C.8.8.14.5 requires a device that changes anywhere in the beam
	// to be encoded in every point. Stationary devices appear only at CP 0.
	first := points[0]
	positions := make(map[string][]float64, len(first.DevicePositions))
	for _, d := range first.DevicePositions {
		positions[d.DeviceType] = d.Positions
	}
	var angleChanges, directionChanges, rateChanges bool
	moving := make(map[string]bool)
	for _, point := range points[1:] {
		angleChanges = angleChanges || point.GantryAngle != first.GantryAngle
		directionChanges = directionChanges || point.GantryRotationDirection != first.GantryRotationDirection
		rateChanges = rateChanges || !samePhotonNumber(point.DoseRateSet, first.DoseRateSet)
		for _, d := range point.DevicePositions {
			moving[d.DeviceType] = moving[d.DeviceType] || !slices.Equal(positions[d.DeviceType], d.Positions)
		}
	}
	old := w.keyed(beam, tagControlPointSequence, tagControlPointIndex)
	items := make([]*object.Object, 0, len(points))
	for i, point := range points {
		if !rsTick(w) {
			return
		}
		it := itemForKey(old, point.Index)
		for _, d := range point.DevicePositions {
			if oneOf(d.DeviceType, DeviceX, DeviceY) && len(d.Positions) == 2 &&
				math.Abs(d.Positions[0]+d.Positions[1]) > positionTolerance {
				w.err = pathError(ErrInvalidObject, tagLeafJawPositions.String(), "symmetric jaw positions must oppose each other")
				return
			}
		}
		if i != 0 {
			point.DevicePositions = make([]DevicePosition, 0, len(moving))
			for _, d := range points[i].DevicePositions {
				if moving[d.DeviceType] {
					point.DevicePositions = append(point.DevicePositions, d)
				}
			}
		} else {
			point.Encoded |= FieldTableTopEccentricAngle
		}
		writeControlState(w, it, point)
		references, _ := it.GetSequence(tagReferencedDoseReferenceSeq)
		for _, reference := range references {
			if !reference.Has(tagCumulativeDoseRefCoefficient) {
				w.numbers(reference, tagCumulativeDoseRefCoefficient, nil)
			}
		}
		if len(references) > 0 {
			putSequence(it, tagReferencedDoseReferenceSeq, references)
		}
		if i == 0 {
			for _, tag := range []core.Tag{tagTableTopVerticalPosition, tagTableTopLongitudinalPosition, tagTableTopLateralPosition, tagIsocenterPosition} {
				if !it.Has(tag) {
					w.numbers(it, tag, nil)
				}
			}
		} else {
			for _, tag := range []core.Tag{
				tagNominalBeamEnergy, tagBLDAngle, tagBLDRotationDirection, tagPatientSupportAngle, tagPatientSupportRotationDir,
				tagTableTopEccentricAngle, tagTableTopEccentricRotationDir, tagTableTopVerticalPosition, tagTableTopLongitudinalPosition,
				tagTableTopLateralPosition, tagIsocenterPosition,
			} {
				it.Remove(tag)
			}
			if !angleChanges {
				it.Remove(tagGantryAngle)
			}
			if !directionChanges {
				it.Remove(tagGantryRotationDirection)
			}
			if !rateChanges {
				it.Remove(tagDoseRateSet)
			}
		}
		items = append(items, it)
	}
	w.integer(beam, tagNumberOfControlPoints, len(items))
	putSequence(beam, tagControlPointSequence, items)
}
