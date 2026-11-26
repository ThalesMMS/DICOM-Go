package rtplan

import (
	"github.com/ThalesMMS/dicom-go/core"
	"math"
)

// validatePlan checks the relations PS3.3 places between the modules of an
// RT Plan. Problems that leave the model ambiguous are errors; the others
// are warnings.
func validatePlan(r *reader, plan *Plan) {
	if plan.SOPInstanceUID == "" || plan.StudyInstanceUID == "" || plan.SeriesInstanceUID == "" {
		r.fail("", "SOP Instance, Study Instance or Series Instance UID is missing")
	}
	for _, x := range []struct {
		t core.Tag
		s string
	}{{tagSOPInstanceUID, plan.SOPInstanceUID}, {tagStudyInstanceUID, plan.StudyInstanceUID}, {tagSeriesInstanceUID, plan.SeriesInstanceUID}} {
		if x.s != "" && !core.IsValidUID(x.s) {
			r.fail(x.t.String(), "identity UID syntax is invalid")
		}
	}
	if plan.FrameOfReferenceUID != "" && !core.IsValidUID(plan.FrameOfReferenceUID) {
		r.fail(tagFrameOfReferenceUID.String(), "frame UID syntax is invalid")
	}
	if plan.Label == "" {
		r.fail(tagRTPlanLabel.String(), "RT Plan Label is missing")
	}
	switch plan.Geometry {
	case GeometryPatient:
		if plan.ReferencedStructureSet == nil {
			r.fail(tagReferencedStructureSetSequence.String(), "is required when RT Plan Geometry is PATIENT")
		}
	case GeometryTreatmentDevice:
	case "":
		r.fail(tagRTPlanGeometry.String(), "RT Plan Geometry is missing")
	default:
		r.fail(tagRTPlanGeometry.String(), "RT Plan Geometry is neither PATIENT nor TREATMENT_DEVICE")
	}
	if ref := plan.ReferencedStructureSet; ref != nil && (ref.SOPClassUID == "" || ref.SOPInstanceUID == "") {
		r.fail(tagReferencedStructureSetSequence.String()+"[0]", "referenced SOP Class or Instance UID is missing")
	}
	if plan.ApprovalStatus != "" && !oneOf(plan.ApprovalStatus, "APPROVED", "UNAPPROVED", "REJECTED") {
		r.warn(tagApprovalStatus.String(), "Approval Status is not an enumerated value")
	}
	if plan.Intent != "" && !oneOf(plan.Intent, "CURATIVE", "PALLIATIVE", "PROPHYLACTIC", "VERIFICATION", "MACHINE_QA", "RESEARCH", "SERVICE") {
		r.warn(tagPlanIntent.String(), "Plan Intent is not a defined term")
	}

	doseRefs := map[int]bool{}
	for i, ref := range plan.DoseReferences {
		path := item("", tagDoseReferenceSequence, i)
		if doseRefs[ref.Number] {
			r.fail(join(path, tagDoseReferenceNumber), "Dose Reference Number %d is not unique", ref.Number)
		}
		doseRefs[ref.Number] = true
		switch ref.StructureType {
		case "POINT", "VOLUME":
			if ref.ReferencedROINumber == nil {
				r.fail(join(path, tagReferencedROINumber), "is required for a %s dose reference", ref.StructureType)
			}
		case "COORDINATES":
			if len(ref.PointCoordinates) != 3 {
				r.fail(join(path, tagDoseReferencePointCoords), "needs 3 values for a COORDINATES dose reference")
			}
		case "SITE":
		case "":
			r.fail(join(path, tagDoseReferenceStructureType), "Dose Reference Structure Type is missing")
		default:
			r.warn(join(path, tagDoseReferenceStructureType), "Dose Reference Structure Type is not a defined term")
		}
		switch ref.Type {
		case "TARGET", "ORGAN_AT_RISK":
		case "":
			r.fail(join(path, tagDoseReferenceType), "Dose Reference Type is missing")
		default:
			r.fail(join(path, tagDoseReferenceType), "Dose Reference Type is neither TARGET nor ORGAN_AT_RISK")
		}
	}
	tolerances := map[int]bool{}
	for i, table := range plan.ToleranceTables {
		if tolerances[table.Number] {
			r.fail(join(item("", tagToleranceTableSequence, i), tagToleranceTableNumber), "Tolerance Table Number %d is not unique", table.Number)
		}
		tolerances[table.Number] = true
	}
	setups := map[int]bool{}
	for i, setup := range plan.PatientSetups {
		path := item("", tagPatientSetupSequence, i)
		if setups[setup.Number] {
			r.fail(join(path, tagPatientSetupNumber), "Patient Setup Number %d is not unique", setup.Number)
		}
		setups[setup.Number] = true
		if setup.PatientPosition == "" && setup.PatientAdditionalPosition == "" {
			r.warn(join(path, tagPatientPosition), "Patient Position is required when Patient Additional Position is absent")
		}
	}

	beams := map[int]*Beam{}
	for i := range plan.Beams {
		beam := &plan.Beams[i]
		path := item("", tagBeamSequence, i)
		if beams[beam.Number] != nil {
			r.fail(join(path, tagBeamNumber), "Beam Number %d is not unique", beam.Number)
		}
		beams[beam.Number] = beam
		validateBeam(r, plan, beam, path, setups, tolerances, doseRefs)
	}
	for i := range plan.IonBeams {
		b := &plan.IonBeams[i]
		path := item("", ft(0x300a03a2), i)
		if beams[b.Number] != nil {
			r.fail(join(path, tagBeamNumber), "beam number is duplicated")
		}
		beams[b.Number] = &Beam{Number: b.Number}
		if b.ReferencedPatientSetupNumber != nil && !setups[*b.ReferencedPatientSetupNumber] {
			r.fail(join(path, tagReferencedPatientSetupNumber), "patient setup reference is missing")
		}
		if b.ReferencedToleranceTableNumber != nil && !tolerances[*b.ReferencedToleranceTableNumber] {
			r.fail(join(path, tagReferencedToleranceTableNumber), "tolerance reference is missing")
		}
		for j, cp := range b.ControlPoints {
			cpPath := item(path, ft(0x300a03a8), j)
			for _, angle := range []struct {
				tag   core.Tag
				value float64
			}{{tagGantryAngle, cp.GantryAngle}, {tagBLDAngle, cp.BeamLimitingDeviceAngle}, {tagPatientSupportAngle, cp.PatientSupportAngle}} {
				if angle.value < 0 || angle.value >= 360 {
					r.fail(join(cpPath, angle.tag), "angle is outside [0, 360)")
				}
			}
			seen := map[int]bool{}
			for k, ref := range cp.DoseReferences {
				if seen[ref.Number] || !doseRefs[ref.Number] {
					r.fail(join(item(cpPath, tagReferencedDoseReferenceSeq, k), tagReferencedDoseReferenceNumber), "dose reference is missing or duplicated")
				}
				seen[ref.Number] = true
			}
		}
	}
	brachy := map[int]bool{}
	for i, setup := range plan.BrachySetups {
		if brachy[setup.Number] {
			r.fail(join(item("", tagApplicationSetupSequence, i), tagApplicationSetupNumber), "Application Setup Number %d is not unique", setup.Number)
		}
		brachy[setup.Number] = true
	}
	if (len(plan.Beams) > 0 || len(plan.IonBeams) > 0) && len(plan.BrachySetups) > 0 {
		r.fail("", "RT Beams and RT Brachy Application Setups shall not both be present")
	}

	groups := map[int]bool{}
	for i, group := range plan.FractionGroups {
		path := item("", tagFractionGroupSequence, i)
		if groups[group.Number] {
			r.fail(join(path, tagFractionGroupNumber), "Fraction Group Number %d is not unique", group.Number)
		}
		groups[group.Number] = true
		if group.FractionsPlanned != nil && *group.FractionsPlanned < 0 {
			r.fail(join(path, tagNumberOfFractionsPlanned), "is negative")
		}
		if group.NumberOfBeams < 0 || group.NumberOfBrachySetups < 0 {
			r.fail(path, "Number of Beams or Number of Brachy Application Setups is negative")
		}
		if group.NumberOfBeams > 0 && group.NumberOfBrachySetups > 0 {
			r.fail(path, "fraction group has both beams and brachy application setups")
		}
		if len(group.ReferencedBeams) != max(group.NumberOfBeams, 0) {
			r.fail(join(path, tagReferencedBeamSequence), "has %d items, Number of Beams is %d", len(group.ReferencedBeams), group.NumberOfBeams)
		}
		seen := map[int]bool{}
		for j, ref := range group.ReferencedBeams {
			refPath := item(path, tagReferencedBeamSequence, j)
			if seen[ref.BeamNumber] {
				r.fail(join(refPath, tagReferencedBeamNumber), "beam %d is referenced twice", ref.BeamNumber)
			}
			seen[ref.BeamNumber] = true
			if beams[ref.BeamNumber] == nil {
				r.fail(join(refPath, tagReferencedBeamNumber), "beam %d is not in the Beam Sequence", ref.BeamNumber)
			}
			if ref.Meterset != nil && *ref.Meterset < 0 {
				r.fail(join(refPath, tagBeamMeterset), "is negative")
			}
			if ref.Dose != nil && *ref.Dose < 0 {
				r.fail(join(refPath, tagBeamDose), "is negative")
			}
		}
		if len(group.ReferencedBrachySetups) != max(group.NumberOfBrachySetups, 0) {
			r.fail(join(path, tagReferencedBrachySetupSequence), "has %d items, Number of Brachy Application Setups is %d", len(group.ReferencedBrachySetups), group.NumberOfBrachySetups)
		}
		for j, ref := range group.ReferencedBrachySetups {
			if !brachy[ref.SetupNumber] {
				r.fail(join(item(path, tagReferencedBrachySetupSequence, j), tagReferencedBrachySetupNumber), "application setup %d is not in the Application Setup Sequence", ref.SetupNumber)
			}
		}
		for j, ref := range group.ReferencedDoseReference {
			if !doseRefs[ref.Number] {
				r.fail(join(item(path, tagReferencedDoseReferenceSeq, j), tagReferencedDoseReferenceNumber), "dose reference %d is not in the Dose Reference Sequence", ref.Number)
			}
		}
	}
}

func validateBeam(r *reader, plan *Plan, beam *Beam, path string, setups, tolerances, doseRefs map[int]bool) {
	switch beam.Type {
	case BeamTypeStatic, BeamTypeDynamic:
	case "":
		r.fail(join(path, tagBeamType), "Beam Type is missing")
	default:
		r.fail(join(path, tagBeamType), "Beam Type is neither STATIC nor DYNAMIC")
	}
	if beam.RadiationType != "" && !oneOf(beam.RadiationType, "PHOTON", "ELECTRON", "NEUTRON", "PROTON") {
		r.warn(join(path, tagRadiationType), "Radiation Type is not a defined term")
	}
	if beam.SourceAxisDistance != nil && *beam.SourceAxisDistance <= 0 {
		r.fail(join(path, tagSourceAxisDistance), "is not positive")
	}
	if n := beam.ReferencedPatientSetupNumber; n != nil && !setups[*n] {
		if len(plan.PatientSetups) > 0 {
			r.fail(join(path, tagReferencedPatientSetupNumber), "patient setup %d is not in the Patient Setup Sequence", *n)
		} else {
			r.warn(join(path, tagReferencedPatientSetupNumber), "patient setup %d is referenced but the plan has no Patient Setup Sequence", *n)
		}
	}
	if n := beam.ReferencedToleranceTableNumber; n != nil && !tolerances[*n] {
		if len(plan.ToleranceTables) > 0 {
			r.fail(join(path, tagReferencedToleranceTableNumber), "tolerance table %d is not in the Tolerance Table Sequence", *n)
		} else {
			r.warn(join(path, tagReferencedToleranceTableNumber), "tolerance table %d is referenced but the plan has no Tolerance Table Sequence", *n)
		}
	}

	if len(beam.Devices) == 0 {
		r.fail(join(path, tagBLDSequence), "Beam Limiting Device Sequence is missing or empty")
	}
	validateFamilyDevices(r, beam.Devices, path, tagBLDSequence)
	validateBlocks(r, beam, path)
	wedges := map[int]bool{}
	for j, wedge := range beam.Wedges {
		if wedges[wedge.Number] {
			r.fail(join(item(path, tagWedgeSequence, j), tagWedgeNumber), "Wedge Number %d is not unique", wedge.Number)
		}
		wedges[wedge.Number] = true
	}

	cps := beam.ControlPoints
	if len(cps) < 2 {
		r.fail(join(path, tagControlPointSequence), "a beam needs at least 2 control points, has %d", len(cps))
	}
	weighted := 0
	for i, cp := range cps {
		cpPath := item(path, tagControlPointSequence, i)
		if cp.Index != i {
			r.fail(join(cpPath, tagControlPointIndex), "Control Point Index is %d, want %d", cp.Index, i)
		}
		if cp.CumulativeMetersetWeight != nil {
			weighted++
		}
		if i == 0 {
			required := []struct {
				field Field
				tag   string
				name  string
			}{
				{FieldGantryAngle, tagGantryAngle.String(), "Gantry Angle"},
				{FieldGantryRotationDirection, tagGantryRotationDirection.String(), "Gantry Rotation Direction"},
				{FieldBeamLimitingDeviceAngle, tagBLDAngle.String(), "Beam Limiting Device Angle"},
				{FieldBeamLimitingDeviceRotationDirection, tagBLDRotationDirection.String(), "Beam Limiting Device Rotation Direction"},
				{FieldPatientSupportAngle, tagPatientSupportAngle.String(), "Patient Support Angle"},
				{FieldPatientSupportRotationDirection, tagPatientSupportRotationDir.String(), "Patient Support Rotation Direction"},
				{FieldTableTopEccentricAngle, tagTableTopEccentricAngle.String(), "Table Top Eccentric Angle"},
				{FieldTableTopEccentricRotationDirection, tagTableTopEccentricRotationDir.String(), "Table Top Eccentric Rotation Direction"},
			}
			for _, req := range required {
				if !cp.Encodes(req.field) {
					r.fail(cpPath+"/"+req.tag, "%s is required in the first control point", req.name)
				}
			}
			for _, device := range beam.Devices {
				if _, ok := cp.Device(device.Type); !ok && device.Type != "" {
					r.fail(join(cpPath, tagBLDPositionSequence), "the first control point does not position device %s", device.Type)
				}
			}
		}
		for _, angle := range []struct {
			field Field
			value float64
			tag   string
		}{
			{FieldGantryAngle, cp.GantryAngle, tagGantryAngle.String()},
			{FieldBeamLimitingDeviceAngle, cp.BeamLimitingDeviceAngle, tagBLDAngle.String()},
			{FieldPatientSupportAngle, cp.PatientSupportAngle, tagPatientSupportAngle.String()},
			{FieldTableTopEccentricAngle, cp.TableTopEccentricAngle, tagTableTopEccentricAngle.String()},
		} {
			if cp.Encodes(angle.field) && (angle.value < 0 || angle.value >= 360) {
				r.fail(cpPath+"/"+angle.tag, "angle %g is outside [0, 360)", angle.value)
			}
		}
		for _, direction := range []struct {
			field Field
			value string
			tag   string
		}{
			{FieldGantryRotationDirection, cp.GantryRotationDirection, tagGantryRotationDirection.String()},
			{FieldBeamLimitingDeviceRotationDirection, cp.BeamLimitingDeviceRotationDirection, tagBLDRotationDirection.String()},
			{FieldPatientSupportRotationDirection, cp.PatientSupportRotationDirection, tagPatientSupportRotationDir.String()},
			{FieldTableTopEccentricRotationDirection, cp.TableTopEccentricRotationDirection, tagTableTopEccentricRotationDir.String()},
		} {
			if cp.Encodes(direction.field) && !oneOf(direction.value, RotationClockwise, RotationCounterClockwise, RotationNone) {
				r.fail(cpPath+"/"+direction.tag, "rotation direction is not CW, CC or NONE")
			}
		}
		if cp.NominalBeamEnergy != nil && cp.Encodes(FieldNominalBeamEnergy) && *cp.NominalBeamEnergy <= 0 {
			r.fail(join(cpPath, tagNominalBeamEnergy), "is not positive")
		}
		for j, wedge := range cp.WedgePositions {
			wedgePath := item(cpPath, tagWedgePositionSequence, j)
			if !wedges[wedge.WedgeNumber] {
				r.fail(join(wedgePath, tagReferencedWedgeNumber), "wedge %d is not in the Wedge Sequence", wedge.WedgeNumber)
			}
			if !oneOf(wedge.Position, "IN", "OUT") {
				r.warn(join(wedgePath, tagWedgePosition), "Wedge Position is neither IN nor OUT")
			}
		}
		for j, ref := range cp.DoseReferences {
			if !doseRefs[ref.Number] {
				r.fail(join(item(cpPath, tagReferencedDoseReferenceSeq, j), tagReferencedDoseReferenceNumber), "dose reference %d is not in the Dose Reference Sequence", ref.Number)
			}
		}
		if beam.Type == BeamTypeStatic && i > 0 && staticChange(cps[0], cp) {
			r.warn(cpPath, "a STATIC beam changes its machine parameters between control points")
		}
	}

	switch {
	case weighted == 0:
		if beam.FinalCumulativeMetersetWeight != nil {
			r.warn(join(path, tagFinalCumulativeMetersetWeight), "is present although no control point has a Cumulative Meterset Weight")
		}
	case weighted != len(cps):
		r.fail(join(path, tagControlPointSequence), "Cumulative Meterset Weight is empty in %d of %d control points", len(cps)-weighted, len(cps))
	default:
		if *cps[0].CumulativeMetersetWeight != 0 {
			r.warn(join(item(path, tagControlPointSequence, 0), tagCumulativeMetersetWeight), "the first control point does not start at weight 0")
		}
		for i := 1; i < len(cps); i++ {
			if *cps[i].CumulativeMetersetWeight < *cps[i-1].CumulativeMetersetWeight {
				r.fail(join(item(path, tagControlPointSequence, i), tagCumulativeMetersetWeight), "decreases from %g to %g", *cps[i-1].CumulativeMetersetWeight, *cps[i].CumulativeMetersetWeight)
				break
			}
		}
		final := beam.FinalCumulativeMetersetWeight
		last := *cps[len(cps)-1].CumulativeMetersetWeight
		switch {
		case final == nil:
			r.fail(join(path, tagFinalCumulativeMetersetWeight), "is required when control points carry Cumulative Meterset Weights")
		case *final <= 0:
			r.fail(join(path, tagFinalCumulativeMetersetWeight), "is not positive")
		case math.Abs(last-*final) > 1e-6*math.Max(1, math.Abs(*final)):
			r.fail(join(path, tagFinalCumulativeMetersetWeight), "is %g but the last control point is at %g", *final, last)
		}
	}
}

func validateBlocks(r *reader, beam *Beam, path string) {
	seen := map[int]bool{}
	var aperture *Block
	for j, block := range beam.Blocks {
		blockPath := item(path, tagBlockSequence, j)
		if seen[block.Number] {
			r.fail(join(blockPath, tagBlockNumber), "Block Number must be unique within the beam")
		}
		seen[block.Number] = true
		if !oneOf(block.Type, "APERTURE", "SHIELDING") {
			r.fail(join(blockPath, tagBlockType), "Block Type is not APERTURE or SHIELDING")
		}
		if block.Divergence != "" && !oneOf(block.Divergence, "PRESENT", "ABSENT") {
			r.fail(join(blockPath, tagBlockDivergence), "Block Divergence is not PRESENT or ABSENT")
		}
		if block.NumberOfPoints < 0 || len(block.Contour) > 0 && (len(block.Contour) != block.NumberOfPoints || len(block.Contour) < 3) {
			r.fail(join(blockPath, tagBlockNumberOfPoints), "declared outline count does not describe a closed polygon")
		}
		for _, point := range block.Contour {
			if !finite(point[0]) || !finite(point[1]) {
				r.fail(join(blockPath, tagBlockData), "block coordinate is not finite")
				break
			}
		}
		if len(block.Contour) == 0 {
			r.warn(join(blockPath, tagBlockData), "block outline unavailable; no complete composite aperture can be inferred")
		}
		if block.Thickness != nil && *block.Thickness < 0 {
			r.fail(join(blockPath, tagBlockThickness), "Block Thickness is negative")
		}
		if block.Transmission != nil && (*block.Transmission < 0 || *block.Transmission > 1) {
			r.fail(join(blockPath, tagBlockTransmission), "Block Transmission is outside [0,1]")
		}
		if block.Type == "APERTURE" {
			if aperture != nil {
				if block.Thickness != nil && aperture.Thickness != nil && *block.Thickness != *aperture.Thickness {
					r.fail(join(blockPath, tagBlockThickness), "APERTURE blocks must have equal specified thickness")
				}
				if block.Transmission != nil && aperture.Transmission != nil && *block.Transmission != *aperture.Transmission {
					r.fail(join(blockPath, tagBlockTransmission), "APERTURE blocks must have equal specified transmission")
				}
				if aperture.Thickness == nil {
					aperture.Thickness = block.Thickness
				}
				if aperture.Transmission == nil {
					aperture.Transmission = block.Transmission
				}
			} else {
				copy := block
				aperture = &copy
			}
		}
	}
}

// staticChange reports whether a control point of a STATIC beam moves the
// gantry, collimator, couch or a beam limiting device away from the first
// control point.
func staticChange(first, cp ControlPoint) bool {
	if cp.GantryAngle != first.GantryAngle || cp.BeamLimitingDeviceAngle != first.BeamLimitingDeviceAngle ||
		cp.PatientSupportAngle != first.PatientSupportAngle || cp.TableTopEccentricAngle != first.TableTopEccentricAngle {
		return true
	}
	if len(cp.DevicePositions) != len(first.DevicePositions) {
		return true
	}
	for i, device := range cp.DevicePositions {
		other := first.DevicePositions[i]
		if device.DeviceType != other.DeviceType || len(device.Positions) != len(other.Positions) {
			return true
		}
		for k := range device.Positions {
			if math.Abs(device.Positions[k]-other.Positions[k]) > positionTolerance {
				return true
			}
		}
	}
	return false
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func validateFamilyDevices(r *reader, list []BeamLimitingDevice, path string, sequence core.Tag) {
	devices := map[string]bool{}
	for j, device := range list {
		devicePath := item(path, sequence, j)
		if devices[device.Type] {
			r.fail(join(devicePath, tagRTBeamLimitingDeviceType), "device %s is declared twice", device.Type)
		}
		devices[device.Type] = true
		switch device.Type {
		case DeviceX, DeviceY, DeviceAsymX, DeviceAsymY:
			if device.Pairs != 1 {
				r.fail(join(devicePath, tagNumberOfLeafJawPairs), "a jaw has %d pairs, want 1", device.Pairs)
			}
			if device.LeafBoundaries != nil {
				r.warn(join(devicePath, tagLeafPositionBoundaries), "jaws have no leaf position boundaries")
			}
		case DeviceMLCX, DeviceMLCY:
			if device.Pairs < 1 {
				r.fail(join(devicePath, tagNumberOfLeafJawPairs), "an MLC needs at least one leaf pair")
				break
			}
			if len(device.LeafBoundaries) != device.Pairs+1 {
				r.fail(join(devicePath, tagLeafPositionBoundaries), "has %d values, want %d (pairs + 1)", len(device.LeafBoundaries), device.Pairs+1)
				break
			}
			for k := 1; k < len(device.LeafBoundaries); k++ {
				if device.LeafBoundaries[k] <= device.LeafBoundaries[k-1] {
					r.fail(join(devicePath, tagLeafPositionBoundaries), "boundaries are not strictly increasing at value %d", k+1)
					break
				}
			}
		case "":
			r.fail(join(devicePath, tagRTBeamLimitingDeviceType), "RT Beam Limiting Device Type is missing")
		default:
			r.fail(join(devicePath, tagRTBeamLimitingDeviceType), "device type is not X, Y, ASYMX, ASYMY, MLCX or MLCY")
		}
	}
}
