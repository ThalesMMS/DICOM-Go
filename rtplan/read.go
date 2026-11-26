package rtplan

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// reader accumulates problems while it builds the model.
type reader struct {
	ctx         context.Context
	maxProblems int
	errors      []Problem
	warnings    []Problem
}

func (r *reader) fail(path, format string, args ...any) {
	if r.maxProblems > 0 && len(r.errors) >= r.maxProblems {
		return
	}
	r.errors = append(r.errors, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (r *reader) warn(path, format string, args ...any) {
	if r.maxProblems > 0 && len(r.warnings) >= r.maxProblems {
		return
	}
	r.warnings = append(r.warnings, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func join(prefix string, tag core.Tag) string {
	if prefix == "" {
		return tag.String()
	}
	return prefix + "/" + tag.String()
}

func item(prefix string, tag core.Tag, index int) string {
	return fmt.Sprintf("%s[%d]", join(prefix, tag), index)
}

func clean(value string) string {
	return strings.TrimFunc(value, func(r rune) bool { return r == 0 || unicode.IsSpace(r) })
}

func (r *reader) str(obj *object.Object, tag core.Tag) string {
	value, ok := obj.GetString(tag)
	if !ok {
		return ""
	}
	return clean(value)
}

func (r *reader) code(obj *object.Object, tag core.Tag) string {
	return strings.ToUpper(r.str(obj, tag))
}

func (r *reader) uid(obj *object.Object, tag core.Tag) string {
	value, ok := obj.GetUID(tag)
	if !ok {
		return ""
	}
	return clean(value)
}

// numbers returns the values of a DS, IS, FL or FD element. A present
// element with a malformed value is a problem; an absent or empty element
// returns nil.
func (r *reader) numbers(obj *object.Object, tag core.Tag, prefix string) []float64 {
	element, ok := obj.Get(tag)
	if !ok {
		return nil
	}
	path := join(prefix, tag)
	switch element.VR() {
	case core.VRFL, core.VRFD:
		values, err := obj.GetFloats(tag)
		if err != nil {
			r.fail(path, "binary value cannot be decoded")
			return nil
		}
		return r.finite(values, path)
	}
	texts, ok := obj.GetStrings(tag)
	if !ok {
		// An element without a value (Type 2 empty) decodes to nothing.
		return nil
	}
	nonEmpty := 0
	for _, text := range texts {
		if r.ctx != nil && r.ctx.Err() != nil {
			return nil
		}
		if clean(text) != "" {
			nonEmpty++
		}
	}
	if nonEmpty == 0 {
		return nil
	}
	if nonEmpty != len(texts) {
		r.fail(path, "value list has empty entries")
		return nil
	}
	out := make([]float64, len(texts))
	for i, text := range texts {
		if r.ctx != nil && r.ctx.Err() != nil {
			return nil
		}
		value, err := strconv.ParseFloat(clean(text), 64)
		if err != nil {
			r.fail(path, "value %d is not a number", i+1)
			return nil
		}
		out[i] = value
	}
	return r.finite(out, path)
}

func (r *reader) finite(values []float64, path string) []float64 {
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			r.fail(path, "value %d is not finite", i+1)
			return nil
		}
	}
	return values
}

func (r *reader) floatOpt(obj *object.Object, tag core.Tag, prefix string) *float64 {
	values := r.numbers(obj, tag, prefix)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		r.fail(join(prefix, tag), "has %d values, want 1", len(values))
		return nil
	}
	value := values[0]
	return &value
}

func (r *reader) intOpt(obj *object.Object, tag core.Tag, prefix string) *int {
	if e, ok := obj.Get(tag); ok {
		switch e.VR() {
		case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
			// Object's scalar helper supports raw SS but not every typed binary
			// integer value. Admit these owned scalar representations locally.
			var scalar int64
			var count int
			var typed bool
			switch values := e.Value.(type) {
			case core.Int16Value:
				typed = true
				count = len(values)
				if count == 1 {
					scalar = int64(values[0])
				}
			case core.Int64Value:
				typed = true
				count = len(values)
				if count == 1 {
					scalar = values[0]
				}
			case core.Uint64Value:
				typed = true
				count = len(values)
				if count == 1 {
					if values[0] > math.MaxInt32 {
						r.fail(join(prefix, tag), "integer is outside signed 32-bit range")
						return nil
					}
					scalar = int64(values[0])
				}
			}
			if typed {
				if count == 0 {
					return nil
				}
				if count != 1 || scalar < math.MinInt32 || scalar > math.MaxInt32 {
					r.fail(join(prefix, tag), "integer cardinality or range is invalid")
					return nil
				}
				n := int(scalar)
				return &n
			}
			values, err := obj.GetInts(tag)
			if err != nil || len(values) > 1 {
				r.fail(join(prefix, tag), "integer value cannot be decoded")
				return nil
			}
			if len(values) == 0 {
				return nil
			}
			if values[0] < math.MinInt32 || values[0] > math.MaxInt32 {
				r.fail(join(prefix, tag), "integer is outside signed 32-bit range")
				return nil
			}
			n := int(values[0])
			return &n
		}
	}

	value := r.floatOpt(obj, tag, prefix)
	if value == nil {
		return nil
	}
	if *value != math.Trunc(*value) || math.Abs(*value) > math.MaxInt32 {
		r.fail(join(prefix, tag), "is not an integer")
		return nil
	}
	n := int(*value)
	return &n
}

// requiredInt reads a Type 1 integer and reports its absence.
func (r *reader) requiredInt(obj *object.Object, tag core.Tag, prefix, name string) int {
	value := r.intOpt(obj, tag, prefix)
	if value == nil {
		if !r.hasProblem(join(prefix, tag)) {
			r.fail(join(prefix, tag), "%s is missing", name)
		}
		return 0
	}
	return *value
}

func (r *reader) hasProblem(path string) bool {
	for _, p := range r.errors {
		if p.Path == path {
			return true
		}
	}
	return false
}

func (r *reader) sequence(obj *object.Object, tag core.Tag) []*object.Object {
	if r.ctx != nil && r.ctx.Err() != nil {
		return nil
	}
	items, ok := obj.GetSequence(tag)
	if !ok {
		return nil
	}
	return items
}

func (r *reader) references(obj *object.Object, tag core.Tag) []Reference {
	var out []Reference
	for _, it := range r.sequence(obj, tag) {
		out = append(out, Reference{SOPClassUID: r.uid(it, tagRefSOPClassUID), SOPInstanceUID: r.uid(it, tagRefSOPInstanceUID)})
	}
	return out
}

func (r *reader) readPlan(obj *object.Object) *Plan {
	plan := &Plan{
		SOPInstanceUID:           r.uid(obj, tagSOPInstanceUID),
		StudyInstanceUID:         r.uid(obj, tagStudyInstanceUID),
		SeriesInstanceUID:        r.uid(obj, tagSeriesInstanceUID),
		FrameOfReferenceUID:      r.uid(obj, tagFrameOfReferenceUID),
		Label:                    r.str(obj, tagRTPlanLabel),
		Name:                     r.str(obj, tagRTPlanName),
		Description:              r.str(obj, tagRTPlanDescription),
		Date:                     r.str(obj, tagRTPlanDate),
		Time:                     r.str(obj, tagRTPlanTime),
		Intent:                   r.code(obj, tagPlanIntent),
		Geometry:                 r.code(obj, tagRTPlanGeometry),
		ApprovalStatus:           r.code(obj, tagApprovalStatus),
		PrescriptionDescription:  r.str(obj, tagPrescriptionDescription),
		BrachyTreatmentTechnique: r.code(obj, tagBrachyTreatmentTechnique),
		BrachyTreatmentType:      r.code(obj, tagBrachyTreatmentType),
	}
	if modality := r.code(obj, tagModality); modality != "RTPLAN" {
		r.fail(tagModality.String(), "Modality is not RTPLAN")
	}
	if refs := r.references(obj, tagReferencedStructureSetSequence); len(refs) > 0 {
		if len(refs) > 1 {
			r.fail(tagReferencedStructureSetSequence.String(), "has %d items, want 1", len(refs))
		}
		plan.ReferencedStructureSet = &refs[0]
	}
	plan.ReferencedDoses = r.references(obj, tagReferencedDoseSequence)
	for _, it := range r.sequence(obj, tagReferencedRTPlanSequence) {
		plan.ReferencedPlans = append(plan.ReferencedPlans, PlanReference{
			Reference:    Reference{SOPClassUID: r.uid(it, tagRefSOPClassUID), SOPInstanceUID: r.uid(it, tagRefSOPInstanceUID)},
			Relationship: r.code(it, tagRTPlanRelationship),
		})
	}
	for i, it := range r.sequence(obj, tagDoseReferenceSequence) {
		plan.DoseReferences = append(plan.DoseReferences, r.readDoseReference(it, item("", tagDoseReferenceSequence, i)))
	}
	toleranceSequence := tagToleranceTableSequence
	if sop, _ := obj.GetUID(tagSOPClassUID); sop == RTIonPlanStorage {
		toleranceSequence = ft(0x300a03a0)
	}
	for i, it := range r.sequence(obj, toleranceSequence) {
		plan.ToleranceTables = append(plan.ToleranceTables, r.readToleranceTable(it, item("", toleranceSequence, i)))
	}
	for i, it := range r.sequence(obj, tagPatientSetupSequence) {
		path := item("", tagPatientSetupSequence, i)
		plan.PatientSetups = append(plan.PatientSetups, PatientSetup{
			Number:                    r.requiredInt(it, tagPatientSetupNumber, path, "Patient Setup Number"),
			Label:                     r.str(it, tagPatientSetupLabel),
			PatientPosition:           r.code(it, tagPatientPosition),
			PatientAdditionalPosition: r.str(it, tagPatientAdditionalPosition),
			SetupTechnique:            r.code(it, tagSetupTechnique),
		})
	}
	for i, it := range r.sequence(obj, tagFractionGroupSequence) {
		plan.FractionGroups = append(plan.FractionGroups, r.readFractionGroup(it, item("", tagFractionGroupSequence, i)))
	}
	for i, it := range r.sequence(obj, tagBeamSequence) {
		plan.Beams = append(plan.Beams, r.readBeam(it, item("", tagBeamSequence, i)))
	}

	return plan
}

func (r *reader) readDoseReference(it *object.Object, path string) DoseReference {
	return DoseReference{
		Number:                 r.requiredInt(it, tagDoseReferenceNumber, path, "Dose Reference Number"),
		UID:                    r.uid(it, tagDoseReferenceUID),
		StructureType:          r.code(it, tagDoseReferenceStructureType),
		Description:            r.str(it, tagDoseReferenceDescription),
		Type:                   r.code(it, tagDoseReferenceType),
		ReferencedROINumber:    r.intOpt(it, tagReferencedROINumber, path),
		PointCoordinates:       r.numbers(it, tagDoseReferencePointCoords, path),
		DeliveryMaximumDose:    r.floatOpt(it, tagDeliveryMaximumDose, path),
		TargetMinimumDose:      r.floatOpt(it, tagTargetMinimumDose, path),
		TargetPrescriptionDose: r.floatOpt(it, tagTargetPrescriptionDose, path),
		TargetMaximumDose:      r.floatOpt(it, tagTargetMaximumDose, path),
		OrganAtRiskMaximumDose: r.floatOpt(it, tagOrganAtRiskMaximumDose, path),
	}
}

func (r *reader) readToleranceTable(it *object.Object, path string) ToleranceTable {
	table := ToleranceTable{
		Number:                       r.requiredInt(it, tagToleranceTableNumber, path, "Tolerance Table Number"),
		Label:                        r.str(it, tagToleranceTableLabel),
		GantryAngle:                  r.floatOpt(it, tagGantryAngleTolerance, path),
		BeamLimitingDeviceAngle:      r.floatOpt(it, tagBLDAngleTolerance, path),
		PatientSupportAngle:          r.floatOpt(it, tagPatientSupportAngleTol, path),
		TableTopEccentricAngle:       r.floatOpt(it, tagTableTopEccentricAngleTol, path),
		TableTopVerticalPosition:     r.floatOpt(it, tagTableTopVerticalTol, path),
		TableTopLongitudinalPosition: r.floatOpt(it, tagTableTopLongitudinalTol, path),
		TableTopLateralPosition:      r.floatOpt(it, tagTableTopLateralTol, path),
	}
	for j, device := range r.sequence(it, tagBLDToleranceSequence) {
		devicePath := item(path, tagBLDToleranceSequence, j)
		position := r.floatOpt(device, tagBLDPositionTolerance, devicePath)
		if position == nil {
			if !r.hasProblem(join(devicePath, tagBLDPositionTolerance)) {
				r.fail(join(devicePath, tagBLDPositionTolerance), "Beam Limiting Device Position Tolerance is missing")
			}
			continue
		}
		table.BeamLimitingDevicePositionLimit = append(table.BeamLimitingDevicePositionLimit, DeviceTolerance{
			DeviceType: r.code(device, tagRTBeamLimitingDeviceType),
			Position:   *position,
		})
	}
	return table
}

func (r *reader) readFractionGroup(it *object.Object, path string) FractionGroup {
	group := FractionGroup{
		Number:                    r.requiredInt(it, tagFractionGroupNumber, path, "Fraction Group Number"),
		Description:               r.str(it, tagFractionGroupDescription),
		FractionsPlanned:          r.intOpt(it, tagNumberOfFractionsPlanned, path),
		FractionPatternDigitsDay:  r.intOpt(it, tagFractionPatternDigitsPerDay, path),
		RepeatFractionCycleLength: r.intOpt(it, tagRepeatFractionCycleLength, path),
		FractionPattern:           r.str(it, tagFractionPattern),
		BeamDoseMeaning:           r.code(it, tagBeamDoseMeaning),
		NumberOfBeams:             r.requiredInt(it, tagNumberOfBeams, path, "Number of Beams"),
		NumberOfBrachySetups:      r.requiredInt(it, tagNumberOfBrachySetups, path, "Number of Brachy Application Setups"),
	}
	for j, ref := range r.sequence(it, tagReferencedBeamSequence) {
		refPath := item(path, tagReferencedBeamSequence, j)
		group.ReferencedBeams = append(group.ReferencedBeams, ReferencedBeam{
			BeamNumber: r.requiredInt(ref, tagReferencedBeamNumber, refPath, "Referenced Beam Number"),
			Dose:       r.floatOpt(ref, tagBeamDose, refPath),
			Meterset:   r.floatOpt(ref, tagBeamMeterset, refPath),
		})
	}
	for j, ref := range r.sequence(it, tagReferencedBrachySetupSequence) {
		refPath := item(path, tagReferencedBrachySetupSequence, j)
		group.ReferencedBrachySetups = append(group.ReferencedBrachySetups, ReferencedBrachySetup{
			SetupNumber: r.requiredInt(ref, tagReferencedBrachySetupNumber, refPath, "Referenced Brachy Application Setup Number"),
			Dose:        r.floatOpt(ref, tagBrachySetupDose, refPath),
		})
	}
	for j, ref := range r.sequence(it, tagReferencedDoseReferenceSeq) {
		refPath := item(path, tagReferencedDoseReferenceSeq, j)
		group.ReferencedDoseReference = append(group.ReferencedDoseReference, ReferencedDoseReference{
			Number:                 r.requiredInt(ref, tagReferencedDoseReferenceNumber, refPath, "Referenced Dose Reference Number"),
			ConstraintWeight:       r.floatOpt(ref, tagConstraintWeight, refPath),
			DeliveryMaximumDose:    r.floatOpt(ref, tagDeliveryMaximumDose, refPath),
			TargetPrescriptionDose: r.floatOpt(ref, tagTargetPrescriptionDose, refPath),
		})
	}
	return group
}

func (r *reader) readBeam(it *object.Object, path string) Beam {
	beam := Beam{
		Number:                         r.requiredInt(it, tagBeamNumber, path, "Beam Number"),
		Name:                           r.str(it, tagBeamName),
		Description:                    r.str(it, tagBeamDescription),
		Type:                           r.code(it, tagBeamType),
		RadiationType:                  r.code(it, tagRadiationType),
		HighDoseTechniqueType:          r.code(it, tagHighDoseTechniqueType),
		TreatmentMachineName:           r.str(it, tagTreatmentMachineName),
		Manufacturer:                   r.str(it, tagManufacturer),
		PrimaryDosimeterUnit:           r.code(it, tagPrimaryDosimeterUnit),
		SourceAxisDistance:             r.floatOpt(it, tagSourceAxisDistance, path),
		TreatmentDeliveryType:          r.code(it, tagTreatmentDeliveryType),
		ReferencedPatientSetupNumber:   r.intOpt(it, tagReferencedPatientSetupNumber, path),
		ReferencedToleranceTableNumber: r.intOpt(it, tagReferencedToleranceTableNumber, path),
		NumberOfWedges:                 r.requiredInt(it, tagNumberOfWedges, path, "Number of Wedges"),
		NumberOfCompensators:           r.requiredInt(it, tagNumberOfCompensators, path, "Number of Compensators"),
		NumberOfBoli:                   r.requiredInt(it, tagNumberOfBoli, path, "Number of Boli"),
		NumberOfBlocks:                 r.requiredInt(it, tagNumberOfBlocks, path, "Number of Blocks"),
		FinalCumulativeMetersetWeight:  r.floatOpt(it, tagFinalCumulativeMetersetWeight, path),
	}
	for j, device := range r.sequence(it, tagBLDSequence) {
		devicePath := item(path, tagBLDSequence, j)
		beam.Devices = append(beam.Devices, BeamLimitingDevice{
			Type:           r.code(device, tagRTBeamLimitingDeviceType),
			SourceDistance: r.floatOpt(device, tagSourceToBLDDistance, devicePath),
			Pairs:          r.requiredInt(device, tagNumberOfLeafJawPairs, devicePath, "Number of Leaf/Jaw Pairs"),
			LeafBoundaries: r.numbers(device, tagLeafPositionBoundaries, devicePath),
		})
	}
	beforeDevices := len(r.errors)
	validateFamilyDevices(r, beam.Devices, path, tagBLDSequence)
	if len(r.errors) > beforeDevices {
		return beam
	}
	for j, wedge := range r.sequence(it, tagWedgeSequence) {
		wedgePath := item(path, tagWedgeSequence, j)
		w := Wedge{
			Number: r.requiredInt(wedge, tagWedgeNumber, wedgePath, "Wedge Number"),
			Type:   r.code(wedge, tagWedgeType),
			ID:     r.str(wedge, tagWedgeID),
			Factor: r.floatOpt(wedge, tagWedgeFactor, wedgePath),
		}
		if angle := r.floatOpt(wedge, tagWedgeAngle, wedgePath); angle != nil {
			w.Angle = *angle
		}
		if orientation := r.floatOpt(wedge, tagWedgeOrientation, wedgePath); orientation != nil {
			w.Orientation = *orientation
		}
		beam.Wedges = append(beam.Wedges, w)
	}
	for j, block := range r.sequence(it, tagBlockSequence) {
		blockPath := item(path, tagBlockSequence, j)
		b := Block{
			Number:       r.requiredInt(block, tagBlockNumber, blockPath, "Block Number"),
			Type:         r.code(block, tagBlockType),
			Name:         r.str(block, tagBlockName),
			Divergence:   r.code(block, tagBlockDivergence),
			MaterialID:   r.str(block, tagMaterialID),
			Thickness:    r.floatOpt(block, tagBlockThickness, blockPath),
			Transmission: r.floatOpt(block, tagBlockTransmission, blockPath),
		}
		if points := r.intOpt(block, tagBlockNumberOfPoints, blockPath); points != nil {
			b.NumberOfPoints = *points
		}
		if element, present := block.Get(tagBlockData); present {
			ctx := r.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			report, err := validation.ValidateElement(ctx, element, validation.Options{MaxFindings: r.maxProblems, ByteOrder: block.ValueByteOrder()})
			if element.VR() != core.VRDS || err != nil || report.HasErrors() || report.Truncated {
				r.fail(join(blockPath, tagBlockData), "Block Data is not a valid DS value list")
			} else {
				values := r.numbers(block, tagBlockData, blockPath)
				if len(values) != 0 && (len(values)%2 != 0 || len(values)/2 != b.NumberOfPoints) {
					r.fail(join(blockPath, tagBlockData), "has %d coordinates, want twice Block Number of Points (%d)", len(values), b.NumberOfPoints)
				} else {
					b.Contour = make([][2]float64, len(values)/2)
					for k := range b.Contour {
						b.Contour[k] = [2]float64{values[2*k], values[2*k+1]}
					}
				}
			}
		}
		beam.Blocks = append(beam.Blocks, b)
	}
	for _, applicator := range r.sequence(it, tagApplicatorSequence) {
		beam.Applicators = append(beam.Applicators, Applicator{ID: r.str(applicator, tagApplicatorID), Type: r.code(applicator, tagApplicatorType)})
	}
	r.checkCount(it, tagWedgeSequence, path, "Number of Wedges", beam.NumberOfWedges)
	r.checkCount(it, tagCompensatorSequence, path, "Number of Compensators", beam.NumberOfCompensators)
	r.checkCount(it, tagReferencedBolusSequence, path, "Number of Boli", beam.NumberOfBoli)
	r.checkCount(it, tagBlockSequence, path, "Number of Blocks", beam.NumberOfBlocks)

	declared := r.requiredInt(it, tagNumberOfControlPoints, path, "Number of Control Points")
	items := r.sequence(it, tagControlPointSequence)
	if declared != len(items) {
		r.fail(join(path, tagControlPointSequence), "has %d items, Number of Control Points is %d", len(items), declared)
	}
	beam.ControlPoints = r.readControlPoints(&beam, items, path)
	return beam
}

// checkCount compares a "Number of X" attribute with the items of the
// sequence it counts. The sequence is required when the count is positive.
func (r *reader) checkCount(obj *object.Object, tag core.Tag, prefix, name string, count int) {
	n := len(r.sequence(obj, tag))
	switch {
	case count < 0:
		r.fail(join(prefix, tag), "%s is negative", name)
	case !obj.Has(tag) && count > 0:
		r.fail(join(prefix, tag), "is missing although %s is %d", name, count)
	case obj.Has(tag) && n != count:
		r.fail(join(prefix, tag), "has %d items, %s is %d", n, name, count)
	}
}

// readControlPoints resolves the control points of a beam: each one starts
// from the state of the previous one and applies what its item encodes.
func (r *reader) readControlPoints(beam *Beam, items []*object.Object, beamPath string) []ControlPoint {
	return r.readControlPointsAt(beam, items, beamPath, tagControlPointSequence)
}
func (r *reader) readControlPointsAt(beam *Beam, items []*object.Object, beamPath string, seqTag core.Tag) []ControlPoint {
	out := make([]ControlPoint, 0, len(items))
	var state ControlPoint
	for i, it := range items {
		path := item(beamPath, seqTag, i)
		if r.ctx != nil && r.ctx.Err() != nil {
			return out
		}
		cp := state
		cp.Encoded = 0
		cp.EncodedDeviceTypes = nil
		cp.WedgePositions = nil
		cp.DoseReferences = nil
		cp.Index = r.requiredInt(it, tagControlPointIndex, path, "Control Point Index")
		cp.CumulativeMetersetWeight = r.floatOpt(it, tagCumulativeMetersetWeight, path)
		cp.SourceToSurfaceDistance = r.floatOpt(it, tagSourceToSurfaceDistance, path)

		setFloatPtr := func(tag core.Tag, field Field, dst **float64) {
			if value := r.floatOpt(it, tag, path); value != nil {
				*dst = value
				cp.Encoded |= field
			}
		}
		setFloat := func(tag core.Tag, field Field, dst *float64) {
			if value := r.floatOpt(it, tag, path); value != nil {
				*dst = *value
				cp.Encoded |= field
			}
		}
		setCode := func(tag core.Tag, field Field, dst *string) {
			if value := r.code(it, tag); value != "" {
				*dst = value
				cp.Encoded |= field
			}
		}
		setFloatPtr(tagNominalBeamEnergy, FieldNominalBeamEnergy, &cp.NominalBeamEnergy)
		setFloatPtr(tagDoseRateSet, FieldDoseRateSet, &cp.DoseRateSet)
		setFloat(tagGantryAngle, FieldGantryAngle, &cp.GantryAngle)
		setCode(tagGantryRotationDirection, FieldGantryRotationDirection, &cp.GantryRotationDirection)
		setFloat(tagBLDAngle, FieldBeamLimitingDeviceAngle, &cp.BeamLimitingDeviceAngle)
		setCode(tagBLDRotationDirection, FieldBeamLimitingDeviceRotationDirection, &cp.BeamLimitingDeviceRotationDirection)
		setFloat(tagPatientSupportAngle, FieldPatientSupportAngle, &cp.PatientSupportAngle)
		setCode(tagPatientSupportRotationDir, FieldPatientSupportRotationDirection, &cp.PatientSupportRotationDirection)
		setFloat(tagTableTopEccentricAngle, FieldTableTopEccentricAngle, &cp.TableTopEccentricAngle)
		setCode(tagTableTopEccentricRotationDir, FieldTableTopEccentricRotationDirection, &cp.TableTopEccentricRotationDirection)
		setFloatPtr(tagTableTopPitchAngle, FieldTableTopPitchAngle, &cp.TableTopPitchAngle)
		setFloatPtr(tagTableTopRollAngle, FieldTableTopRollAngle, &cp.TableTopRollAngle)
		setFloatPtr(tagTableTopVerticalPosition, FieldTableTopVerticalPosition, &cp.TableTopVerticalPosition)
		setFloatPtr(tagTableTopLongitudinalPosition, FieldTableTopLongitudinalPosition, &cp.TableTopLongitudinalPosition)
		setFloatPtr(tagTableTopLateralPosition, FieldTableTopLateralPosition, &cp.TableTopLateralPosition)
		if values := r.numbers(it, tagIsocenterPosition, path); values != nil {
			if len(values) != 3 {
				r.fail(join(path, tagIsocenterPosition), "has %d values, want 3", len(values))
			} else {
				cp.IsocenterPosition = values
				cp.Encoded |= FieldIsocenterPosition
			}
		}

		if positions, ok := it.GetSequence(tagBLDPositionSequence); ok {
			cp.Encoded |= FieldDevicePositions
			cp.DevicePositions = mergeDevicePositions(r, beam, cp.DevicePositions, positions, path)
			for _, position := range positions {
				cp.EncodedDeviceTypes = append(cp.EncodedDeviceTypes, r.code(position, tagRTBeamLimitingDeviceType))
			}
		}
		for j, wedge := range r.sequence(it, tagWedgePositionSequence) {
			wedgePath := item(path, tagWedgePositionSequence, j)
			cp.WedgePositions = append(cp.WedgePositions, WedgePosition{
				WedgeNumber: r.requiredInt(wedge, tagReferencedWedgeNumber, wedgePath, "Referenced Wedge Number"),
				Position:    r.code(wedge, tagWedgePosition),
			})
		}
		for j, ref := range r.sequence(it, tagReferencedDoseReferenceSeq) {
			refPath := item(path, tagReferencedDoseReferenceSeq, j)
			cp.DoseReferences = append(cp.DoseReferences, ControlPointDoseReference{
				Number:      r.requiredInt(ref, tagReferencedDoseReferenceNumber, refPath, "Referenced Dose Reference Number"),
				Coefficient: r.floatOpt(ref, tagCumulativeDoseRefCoefficient, refPath),
			})
		}
		out = append(out, ownControlPoint(cp))
		state = cp
	}
	return out
}

// mergeDevicePositions applies a control point's Beam Limiting Device
// Position Sequence to the inherited positions. The result lists the
// beam's devices in Beam Limiting Device Sequence order; a device not yet
// positioned is omitted (validation reports it at the first control point).
func mergeDevicePositions(r *reader, beam *Beam, inherited []DevicePosition, items []*object.Object, path string) []DevicePosition {
	current := map[string][]float64{}
	for _, d := range inherited {
		current[d.DeviceType] = d.Positions
	}
	seen := map[string]bool{}
	for j, it := range items {
		itemPath := item(path, tagBLDPositionSequence, j)
		deviceType := r.code(it, tagRTBeamLimitingDeviceType)
		if deviceType == "" {
			r.fail(join(itemPath, tagRTBeamLimitingDeviceType), "RT Beam Limiting Device Type is missing")
			continue
		}
		if seen[deviceType] {
			r.fail(join(itemPath, tagRTBeamLimitingDeviceType), "device %s is positioned twice", deviceType)
			continue
		}
		seen[deviceType] = true
		device, ok := beam.Device(deviceType)
		if !ok {
			r.fail(join(itemPath, tagRTBeamLimitingDeviceType), "device %s is not in the Beam Limiting Device Sequence", deviceType)
			continue
		}
		positions := r.numbers(it, tagLeafJawPositions, itemPath)
		if positions == nil {
			if !r.hasProblem(join(itemPath, tagLeafJawPositions)) {
				r.fail(join(itemPath, tagLeafJawPositions), "Leaf/Jaw Positions are missing")
			}
			continue
		}
		if device.Pairs <= 0 || len(positions) != 2*device.Pairs {
			r.fail(join(itemPath, tagLeafJawPositions), "has %d values, want %d (2 x %d pairs)", len(positions), 2*device.Pairs, device.Pairs)
			continue
		}
		for k := 0; k < device.Pairs; k++ {
			if positions[k] > positions[device.Pairs+k]+positionTolerance {
				r.fail(join(itemPath, tagLeafJawPositions), "pair %d: bank 1 at %g mm is beyond bank 2 at %g mm", k+1, positions[k], positions[device.Pairs+k])
				break
			}
		}
		current[deviceType] = positions
	}
	out := make([]DevicePosition, 0, len(beam.Devices))
	for _, device := range beam.Devices {
		if positions, ok := current[device.Type]; ok {
			out = append(out, DevicePosition{DeviceType: device.Type, Positions: positions})
		}
	}
	return out
}

// positionTolerance absorbs decimal-string rounding when opposing leaves or
// jaws touch.
const positionTolerance = 1e-6

func ownedFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func ownControlPoint(cp ControlPoint) ControlPoint {
	cp.EncodedDeviceTypes = append([]string(nil), cp.EncodedDeviceTypes...)
	cp.NominalBeamEnergy = ownedFloat(cp.NominalBeamEnergy)
	cp.DoseRateSet = ownedFloat(cp.DoseRateSet)
	cp.TableTopPitchAngle = ownedFloat(cp.TableTopPitchAngle)
	cp.TableTopRollAngle = ownedFloat(cp.TableTopRollAngle)
	cp.TableTopVerticalPosition = ownedFloat(cp.TableTopVerticalPosition)
	cp.TableTopLongitudinalPosition = ownedFloat(cp.TableTopLongitudinalPosition)
	cp.TableTopLateralPosition = ownedFloat(cp.TableTopLateralPosition)
	cp.IsocenterPosition = append([]float64(nil), cp.IsocenterPosition...)
	cp.DevicePositions = append([]DevicePosition(nil), cp.DevicePositions...)
	for i := range cp.DevicePositions {
		cp.DevicePositions[i].Positions = append([]float64(nil), cp.DevicePositions[i].Positions...)
	}
	return cp
}
func ownFloatAttribute(a Attribute[float64]) Attribute[float64] {
	a.Value = ownedFloat(a.Value)
	return a
}
