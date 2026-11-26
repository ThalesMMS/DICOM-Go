package rtplan

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func TestTagsMatchTheDictionary(t *testing.T) {
	for keyword, tag := range map[string]core.Tag{
		"RTPlanLabel": tagRTPlanLabel, "RTPlanGeometry": tagRTPlanGeometry, "PlanIntent": tagPlanIntent,
		"PrescriptionDescription": tagPrescriptionDescription, "DoseReferenceSequence": tagDoseReferenceSequence,
		"DoseReferenceNumber": tagDoseReferenceNumber, "DoseReferenceUID": tagDoseReferenceUID,
		"DoseReferenceStructureType": tagDoseReferenceStructureType, "DoseReferenceDescription": tagDoseReferenceDescription,
		"DoseReferencePointCoordinates": tagDoseReferencePointCoords, "DoseReferenceType": tagDoseReferenceType,
		"ConstraintWeight": tagConstraintWeight, "DeliveryMaximumDose": tagDeliveryMaximumDose,
		"TargetMinimumDose": tagTargetMinimumDose, "TargetPrescriptionDose": tagTargetPrescriptionDose,
		"TargetMaximumDose": tagTargetMaximumDose, "OrganAtRiskMaximumDose": tagOrganAtRiskMaximumDose,
		"ToleranceTableSequence": tagToleranceTableSequence, "ToleranceTableNumber": tagToleranceTableNumber,
		"ToleranceTableLabel": tagToleranceTableLabel, "GantryAngleTolerance": tagGantryAngleTolerance,
		"BeamLimitingDeviceAngleTolerance": tagBLDAngleTolerance, "BeamLimitingDeviceToleranceSequence": tagBLDToleranceSequence,
		"BeamLimitingDevicePositionTolerance": tagBLDPositionTolerance, "PatientSupportAngleTolerance": tagPatientSupportAngleTol,
		"TableTopEccentricAngleTolerance": tagTableTopEccentricAngleTol, "TableTopVerticalPositionTolerance": tagTableTopVerticalTol,
		"TableTopLongitudinalPositionTolerance": tagTableTopLongitudinalTol, "TableTopLateralPositionTolerance": tagTableTopLateralTol,
		"RTPlanRelationship": tagRTPlanRelationship, "FractionGroupSequence": tagFractionGroupSequence,
		"FractionGroupNumber": tagFractionGroupNumber, "FractionGroupDescription": tagFractionGroupDescription,
		"NumberOfFractionsPlanned": tagNumberOfFractionsPlanned, "NumberOfFractionPatternDigitsPerDay": tagFractionPatternDigitsPerDay,
		"RepeatFractionCycleLength": tagRepeatFractionCycleLength, "FractionPattern": tagFractionPattern,
		"NumberOfBeams": tagNumberOfBeams, "BeamDose": tagBeamDose, "BeamMeterset": tagBeamMeterset,
		"BeamDoseMeaning": tagBeamDoseMeaning, "NumberOfBrachyApplicationSetups": tagNumberOfBrachySetups,
		"BrachyApplicationSetupDose": tagBrachySetupDose, "BeamSequence": tagBeamSequence,
		"TreatmentMachineName": tagTreatmentMachineName, "PrimaryDosimeterUnit": tagPrimaryDosimeterUnit,
		"SourceAxisDistance": tagSourceAxisDistance, "BeamLimitingDeviceSequence": tagBLDSequence,
		"RTBeamLimitingDeviceType": tagRTBeamLimitingDeviceType, "SourceToBeamLimitingDeviceDistance": tagSourceToBLDDistance,
		"NumberOfLeafJawPairs": tagNumberOfLeafJawPairs, "LeafPositionBoundaries": tagLeafPositionBoundaries,
		"BeamNumber": tagBeamNumber, "BeamName": tagBeamName, "BeamDescription": tagBeamDescription, "BeamType": tagBeamType,
		"RadiationType": tagRadiationType, "HighDoseTechniqueType": tagHighDoseTechniqueType,
		"TreatmentDeliveryType": tagTreatmentDeliveryType, "NumberOfWedges": tagNumberOfWedges,
		"WedgeSequence": tagWedgeSequence, "WedgeNumber": tagWedgeNumber, "WedgeType": tagWedgeType, "WedgeID": tagWedgeID,
		"WedgeAngle": tagWedgeAngle, "WedgeFactor": tagWedgeFactor, "WedgeOrientation": tagWedgeOrientation,
		"NumberOfCompensators": tagNumberOfCompensators, "CompensatorSequence": tagCompensatorSequence,
		"NumberOfBoli": tagNumberOfBoli, "NumberOfBlocks": tagNumberOfBlocks, "BlockSequence": tagBlockSequence,
		"BlockType": tagBlockType, "BlockDivergence": tagBlockDivergence, "BlockNumber": tagBlockNumber, "BlockName": tagBlockName,
		"BlockNumberOfPoints": tagBlockNumberOfPoints, "BlockData": tagBlockData, "BlockThickness": tagBlockThickness, "BlockTransmission": tagBlockTransmission, "MaterialID": tagMaterialID, "ApplicatorSequence": tagApplicatorSequence, "ApplicatorID": tagApplicatorID,
		"ApplicatorType": tagApplicatorType, "CumulativeDoseReferenceCoefficient": tagCumulativeDoseRefCoefficient,
		"FinalCumulativeMetersetWeight": tagFinalCumulativeMetersetWeight, "NumberOfControlPoints": tagNumberOfControlPoints,
		"ControlPointSequence": tagControlPointSequence, "ControlPointIndex": tagControlPointIndex,
		"NominalBeamEnergy": tagNominalBeamEnergy, "DoseRateSet": tagDoseRateSet, "WedgePositionSequence": tagWedgePositionSequence,
		"WedgePosition": tagWedgePosition, "BeamLimitingDevicePositionSequence": tagBLDPositionSequence,
		"LeafJawPositions": tagLeafJawPositions, "GantryAngle": tagGantryAngle, "GantryRotationDirection": tagGantryRotationDirection,
		"BeamLimitingDeviceAngle": tagBLDAngle, "BeamLimitingDeviceRotationDirection": tagBLDRotationDirection,
		"PatientSupportAngle": tagPatientSupportAngle, "PatientSupportRotationDirection": tagPatientSupportRotationDir,
		"TableTopEccentricAngle": tagTableTopEccentricAngle, "TableTopEccentricRotationDirection": tagTableTopEccentricRotationDir,
		"TableTopVerticalPosition": tagTableTopVerticalPosition, "TableTopLongitudinalPosition": tagTableTopLongitudinalPosition,
		"TableTopLateralPosition": tagTableTopLateralPosition, "IsocenterPosition": tagIsocenterPosition,
		"SourceToSurfaceDistance": tagSourceToSurfaceDistance, "CumulativeMetersetWeight": tagCumulativeMetersetWeight,
		"TableTopPitchAngle": tagTableTopPitchAngle, "TableTopRollAngle": tagTableTopRollAngle,
		"PatientSetupSequence": tagPatientSetupSequence, "PatientSetupNumber": tagPatientSetupNumber,
		"PatientSetupLabel": tagPatientSetupLabel, "PatientAdditionalPosition": tagPatientAdditionalPosition,
		"SetupTechnique": tagSetupTechnique, "BrachyTreatmentTechnique": tagBrachyTreatmentTechnique,
		"BrachyTreatmentType": tagBrachyTreatmentType, "ApplicationSetupSequence": tagApplicationSetupSequence,
		"ApplicationSetupType": tagApplicationSetupType, "ApplicationSetupNumber": tagApplicationSetupNumber,
		"ApplicationSetupName": tagApplicationSetupName, "TotalReferenceAirKerma": tagTotalReferenceAirKerma,
		"ChannelSequence": tagChannelSequence, "ReferencedRTPlanSequence": tagReferencedRTPlanSequence,
		"ReferencedBeamSequence": tagReferencedBeamSequence, "ReferencedBeamNumber": tagReferencedBeamNumber,
		"ReferencedBrachyApplicationSetupSequence": tagReferencedBrachySetupSequence,
		"ReferencedBrachyApplicationSetupNumber":   tagReferencedBrachySetupNumber, "ReferencedSourceNumber": tagReferencedSourceNumber,
		"ReferencedDoseReferenceSequence": tagReferencedDoseReferenceSeq, "ReferencedDoseReferenceNumber": tagReferencedDoseReferenceNumber,
		"ReferencedStructureSetSequence": tagReferencedStructureSetSequence, "ReferencedPatientSetupNumber": tagReferencedPatientSetupNumber,
		"ReferencedDoseSequence": tagReferencedDoseSequence, "ReferencedToleranceTableNumber": tagReferencedToleranceTableNumber,
		"ReferencedBolusSequence": tagReferencedBolusSequence, "ReferencedWedgeNumber": tagReferencedWedgeNumber,
		"ApprovalStatus": tagApprovalStatus, "PatientPosition": tagPatientPosition, "ReferencedROINumber": tagReferencedROINumber,
	} {
		entry, ok := std.Dictionary.ByKeyword(keyword)
		if !ok {
			t.Errorf("keyword %s is not in the dictionary", keyword)
			continue
		}
		if entry.Tag != tag {
			t.Errorf("%s is %s in the dictionary, the package uses %s", keyword, entry.Tag, tag)
		}
	}
}

func TestReadResolvesBeamsControlPointsAndFractions(t *testing.T) {
	plan, err := Read(newPlanFixture().file(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", plan.Warnings)
	}
	if plan.Label != "SYNTH_PLAN" || plan.Geometry != GeometryPatient || plan.Intent != "CURATIVE" ||
		plan.ReferencedStructureSet == nil || plan.ReferencedStructureSet.SOPInstanceUID != testStructUID ||
		plan.FrameOfReferenceUID != testFrameUID {
		t.Fatalf("general plan = %+v", plan)
	}
	if len(plan.DoseReferences) != 1 || *plan.DoseReferences[0].TargetPrescriptionDose != 60 ||
		!reflect.DeepEqual(plan.DoseReferences[0].PointCoordinates, []float64{0, 10, -20}) {
		t.Fatalf("dose references = %+v", plan.DoseReferences)
	}
	group := plan.FractionGroups[0]
	if *group.FractionsPlanned != 30 || group.NumberOfBeams != 2 || len(group.ReferencedBeams) != 2 ||
		*group.ReferencedBeams[1].Meterset != 250 || group.ReferencedDoseReference[0].Number != 1 {
		t.Fatalf("fraction group = %+v", group)
	}
	if len(plan.PatientSetups) != 1 || plan.PatientSetups[0].PatientPosition != "HFS" ||
		len(plan.ToleranceTables) != 1 || plan.ToleranceTables[0].BeamLimitingDevicePositionLimit[0].Position != 2 {
		t.Fatalf("setups/tolerances = %+v / %+v", plan.PatientSetups, plan.ToleranceTables)
	}

	arc, ok := plan.Beam(2)
	if !ok || arc.Type != BeamTypeDynamic || len(arc.ControlPoints) != 3 {
		t.Fatalf("beam 2 = %+v", arc)
	}
	// The last control point encodes only its gantry angle and weight: the
	// collimator, energy, isocentre and every device position are inherited,
	// the MLC from control point 1 and the jaws from control point 0.
	last := arc.ControlPoints[2]
	if last.GantryAngle != 179 || last.BeamLimitingDeviceAngle != 30 || *last.NominalBeamEnergy != 6 ||
		!reflect.DeepEqual(last.IsocenterPosition, []float64{0, 10, -20}) || last.GantryRotationDirection != RotationClockwise {
		t.Fatalf("resolved last control point = %+v", last)
	}
	if !last.Encodes(FieldGantryAngle) || last.Encodes(FieldBeamLimitingDeviceAngle) || last.Encodes(FieldDevicePositions) {
		t.Fatalf("encoded fields of the last control point = %b", last.Encoded)
	}
	if mlc, _ := last.Device(DeviceMLCX); mlc.Positions[0] != -10 || mlc.Positions[19] != 10 {
		t.Fatalf("inherited MLC = %v", mlc.Positions)
	}
	if jaws, _ := last.Device(DeviceAsymY); !reflect.DeepEqual(jaws.Positions, []float64{-50, 50}) {
		t.Fatalf("inherited Y jaws = %v", jaws.Positions)
	}
	if last.SourceToSurfaceDistance != nil {
		t.Fatal("Source to Surface Distance must not be inherited")
	}
	if got := arc.Arc(); got.Start != 181 || got.Stop != 179 || got.Extent != 358 || got.Direction != RotationClockwise {
		t.Fatalf("arc = %+v, want 181 -> 179 CW over 358 degrees", got)
	}

	static, _ := plan.Beam(1)
	if static.IsArc() {
		t.Fatal("the STATIC beam is reported as an arc")
	}
	// Leaves open 40 mm (-20..20), the X jaws (-50..50) do not clip them and
	// the Y jaws (-30..30) keep 6 of the 10-mm leaf pairs: 6 x 10 x 40 mm².
	if area, err := static.ApertureArea(0); err != nil || area != 2400 {
		t.Fatalf("static aperture = %v, %v, want 2400", area, err)
	}
	// Arc control point 1: leaves at -10..10 inside X jaws -40..40, Y jaws
	// -50..50 keep all ten pairs: 10 x 10 x 20 mm².
	if area, err := arc.ApertureArea(1); err != nil || area != 2000 {
		t.Fatalf("arc aperture = %v, %v, want 2000", area, err)
	}
	if x, y, okX, okY := static.JawOpening(1); !okX || !okY || x != 100 || y != 60 {
		t.Fatalf("jaw opening = %v x %v (%v %v)", x, y, okX, okY)
	}

	summary := plan.Summary()
	if len(summary.Beams) != 2 || summary.Beams[1].Arc.Extent != 358 || *summary.Beams[0].Meterset != 100.5 ||
		summary.Beams[0].MLC != DeviceMLCX || summary.Beams[0].MLCPairs != 10 || *summary.Beams[0].Aperture != 2400 ||
		*summary.Beams[1].JawX != 80 || summary.Beams[1].CollimatorAngle != 30 {
		t.Fatalf("summary beams = %+v", summary.Beams)
	}
	g := summary.FractionGroups[0]
	if math.Abs(*g.MetersetPerFraction-350.5) > 1e-9 || math.Abs(*g.DosePerFraction-2) > 1e-9 ||
		!reflect.DeepEqual(summary.Prescription, []float64{60}) {
		t.Fatalf("summary fraction group = %+v, prescription %v", g, summary.Prescription)
	}
}

// The synthetic plan meets the module requirements of the RT Plan IOD as
// generated from PS3.3, so the semantic checks of Read are exercised on a
// conformant object.
func TestSyntheticPlanMeetsTheRTPlanIOD(t *testing.T) {
	file := newPlanFixture().file(t)
	report, err := iod.Validate(file.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.IOD != "RT Plan" {
		t.Fatalf("IOD = %q", report.IOD)
	}
	for _, finding := range report.Errors() {
		t.Errorf("IOD finding: %s", finding)
	}
	checked := map[string]bool{}
	for _, module := range report.Modules {
		checked[module.Module] = module.Checked
	}
	for _, module := range []string{"RT General Plan", "RT Prescription", "RT Tolerance Tables", "RT Patient Setup", "RT Fraction Scheme", "RT Beams"} {
		if !checked[module] {
			t.Errorf("module %s was not checked", module)
		}
	}
}

func TestReadRefusesStructurallyAmbiguousPlans(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*planFixture)
		want   string
	}{
		{"control point count", func(f *planFixture) {
			f.beams[1].attrs.set(derivedio.IS(tagNumberOfControlPoints, 4))
		}, "has 3 items, Number of Control Points is 4"},
		{"single control point", func(f *planFixture) {
			f.beams[0].cps = f.beams[0].cps[:1]
			f.beams[0].attrs.set(derivedio.IS(tagNumberOfControlPoints, 1), derivedio.DS(tagFinalCumulativeMetersetWeight, 0))
		}, "at least 2 control points"},
		{"control point index", func(f *planFixture) {
			f.beams[1].cps[2].attrs.set(derivedio.IS(tagControlPointIndex, 5))
		}, "Control Point Index is 5, want 2"},
		{"first control point without gantry", func(f *planFixture) {
			f.beams[0].cps[0].attrs.del(tagGantryAngle)
		}, "Gantry Angle is required in the first control point"},
		{"first control point without MLC", func(f *planFixture) {
			f.beams[0].cps[0].positions = f.beams[0].cps[0].positions[:2]
		}, "does not position device MLCX"},
		{"crossing leaves", func(f *planFixture) {
			values := banks(-20, 20)
			values[3] = 25
			f.beams[0].cps[0].positions[2] = position(DeviceMLCX, values...)
		}, "pair 4: bank 1 at 25 mm is beyond bank 2 at 20 mm"},
		{"leaf count", func(f *planFixture) {
			f.beams[0].cps[0].positions[2] = position(DeviceMLCX, banks(-20, 20)[:18]...)
		}, "has 18 values, want 20"},
		{"undeclared device", func(f *planFixture) {
			f.beams[1].cps[1].positions = append(f.beams[1].cps[1].positions, position(DeviceMLCY, -1, 1))
		}, "device MLCY is not in the Beam Limiting Device Sequence"},
		{"leaf boundaries", func(f *planFixture) {
			f.beams[0].devices[2] = device(DeviceMLCX, 10, mlcBoundaries()[:10]...)
		}, "has 10 values, want 11 (pairs + 1)"},
		{"decreasing meterset weight", func(f *planFixture) {
			f.beams[1].cps[1].attrs.set(derivedio.DS(tagCumulativeMetersetWeight, 1))
			f.beams[1].cps[2].attrs.set(derivedio.DS(tagCumulativeMetersetWeight, 0.9))
			f.beams[1].attrs.set(derivedio.DS(tagFinalCumulativeMetersetWeight, 0.9))
		}, "decreases from 1 to 0.9"},
		{"final weight", func(f *planFixture) {
			f.beams[1].attrs.set(derivedio.DS(tagFinalCumulativeMetersetWeight, 2))
		}, "is 2 but the last control point is at 1"},
		{"dangling beam reference", func(f *planFixture) {
			f.fractionGroups[0].beams[1].set(derivedio.IS(tagReferencedBeamNumber, 7))
		}, "beam 7 is not in the Beam Sequence"},
		{"number of beams", func(f *planFixture) {
			f.fractionGroups[0].attrs.set(derivedio.IS(tagNumberOfBeams, 3))
		}, "has 2 items, Number of Beams is 3"},
		{"duplicate beam number", func(f *planFixture) {
			f.beams[1].attrs.set(derivedio.IS(tagBeamNumber, 1))
		}, "Beam Number 1 is not unique"},
		{"patient geometry without structure set", func(f *planFixture) {
			f.attrs.del(tagReferencedStructureSetSequence)
		}, "is required when RT Plan Geometry is PATIENT"},
		{"coordinates dose reference", func(f *planFixture) {
			f.doseRefs[0].del(tagDoseReferencePointCoords)
		}, "needs 3 values for a COORDINATES dose reference"},
		{"dangling dose reference", func(f *planFixture) {
			f.fractionGroups[0].attrs.set(derivedio.Seq(tagReferencedDoseReferenceSeq, derivedio.DataSet(derivedio.IS(tagReferencedDoseReferenceNumber, 9))))
		}, "dose reference 9 is not in the Dose Reference Sequence"},
		{"wedge count", func(f *planFixture) {
			f.beams[0].attrs.set(derivedio.IS(tagNumberOfWedges, 1))
		}, "is missing although Number of Wedges is 1"},
		{"tolerance table", func(f *planFixture) {
			f.beams[0].attrs.set(derivedio.IS(tagReferencedToleranceTableNumber, 4))
		}, "tolerance table 4 is not in the Tolerance Table Sequence"},
		{"gantry angle range", func(f *planFixture) {
			f.beams[1].cps[1].attrs.set(derivedio.DS(tagGantryAngle, 360))
		}, "angle 360 is outside [0, 360)"},
		{"rotation direction", func(f *planFixture) {
			f.beams[1].cps[0].attrs.set(derivedio.CS(tagGantryRotationDirection, "LEFT"))
		}, "rotation direction is not CW, CC or NONE"},
		{"label", func(f *planFixture) {
			f.attrs.del(tagRTPlanLabel)
		}, "RT Plan Label is missing"},
		{"modality", func(f *planFixture) {
			f.attrs.set(derivedio.CS(tagModality, "RTSTRUCT"))
		}, "Modality is not RTPLAN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPlanFixture()
			test.mutate(fixture)
			_, err := Read(fixture.file(t))
			var validation *ValidationError
			if !errors.As(err, &validation) || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("Read error = %v, want a *ValidationError", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Read error = %v, want %q", err, test.want)
			}
			for _, problem := range validation.Problems {
				if strings.Contains(problem.Message, "SYNTH") || strings.Contains(problem.Message, "Synthetic") {
					t.Fatalf("problem quotes a free-text value: %v", problem)
				}
			}
		})
	}
}

// A malformed Decimal String cannot be written, so it is read from the
// in-memory data set.
func TestReadReportsMalformedNumbers(t *testing.T) {
	fixture := newPlanFixture()
	fixture.beams[1].cps[1].attrs.set(derivedio.Str(tagGantryAngle, core.VRDS, "ninety"))
	_, err := ReadDataset(fixture.dataset(t))
	if !errors.Is(err, ErrInvalidObject) || !strings.Contains(err.Error(), "(300A,00B0)[1]/(300A,0111)[1]/(300A,011E): value 1 is not a number") {
		t.Fatalf("ReadDataset error = %v", err)
	}
}

func TestReadKeepsNonBlockingDeparturesAsWarnings(t *testing.T) {
	fixture := newPlanFixture()
	// A STATIC beam whose MLC moves, and an approval status outside the
	// enumerated values, leave the model unambiguous.
	fixture.beams[0].cps[1].positions = []elems{position(DeviceMLCX, banks(-10, 10)...)}
	fixture.attrs.set(derivedio.CS(tagApprovalStatus, "PENDING"))
	plan, err := Read(fixture.file(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var messages []string
	for _, warning := range plan.Warnings {
		messages = append(messages, warning.String())
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{"a STATIC beam changes its machine parameters", "Approval Status is not an enumerated value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q do not contain %q", joined, want)
		}
	}
	if plan.Summary().Warnings != len(plan.Warnings) {
		t.Fatal("summary does not count the warnings")
	}
}

func TestReadRefusesTreatmentRecordsAndMalformedIon(t *testing.T) {
	for _, sopClass := range []string{RTBeamsTreatmentRecordStorage, RTBrachyTreatmentRecordStorage, RTTreatmentSummaryRecordStorage, RTIonBeamsTreatmentRecordStorage, rtStructureSet} {
		fixture := newPlanFixture()
		fixture.sopClassUID = sopClass
		if _, err := Read(fixture.file(t)); !errors.Is(err, ErrUnsupportedSOPClass) {
			t.Errorf("Read(%s) error = %v, want ErrUnsupportedSOPClass", sopClass, err)
		}
	}
	fixture := newPlanFixture()
	fixture.sopClassUID = RTIonPlanStorage
	if _, err := Read(fixture.file(t)); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("malformed Ion error = %v", err)
	}
	if _, err := Read(nil); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("Read(nil) error = %v", err)
	}
}

func TestReadSummarisesBrachyApplicationSetups(t *testing.T) {
	fixture := newBrachyFixture()
	plan, err := Read(fixture.file(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	setup := plan.BrachySetups[0]
	if plan.BrachyTreatmentType != "HDR" || setup.NumberOfChannels != 2 || !reflect.DeepEqual(setup.ReferencedSourceNumbers, []int{1}) ||
		*setup.TotalReferenceAirKerma != 0.25 || *plan.FractionGroups[0].ReferencedBrachySetups[0].Dose != 7 {
		t.Fatalf("brachy = %+v / %+v", plan, setup)
	}
	if summary := plan.Summary(); summary.BrachySetups != 2 || len(summary.Beams) != 0 {
		t.Fatalf("summary = %+v", summary)
	}

	fixture.fractionGroups[0].attrs.set(derivedio.Seq(tagReferencedBrachySetupSequence, derivedio.DataSet(derivedio.IS(tagReferencedBrachySetupNumber, 3))))
	if _, err := Read(fixture.file(t)); err == nil || !strings.Contains(err.Error(), "application setup 3 is not in the Application Setup Sequence") {
		t.Fatalf("dangling brachy reference error = %v", err)
	}
}

func TestApertureAreaFollowsMLCYAndRefusesBeamsWithoutMLC(t *testing.T) {
	beam := &Beam{
		Devices: []BeamLimitingDevice{{Type: DeviceX, Pairs: 1}, {Type: DeviceY, Pairs: 1}, {Type: DeviceMLCY, Pairs: 2, LeafBoundaries: []float64{-10, 0, 10}}},
		ControlPoints: []ControlPoint{{DevicePositions: []DevicePosition{
			{DeviceType: DeviceX, Positions: []float64{-5, 10}},  // across the leaves
			{DeviceType: DeviceY, Positions: []float64{-3, 100}}, // along leaf travel
			{DeviceType: DeviceMLCY, Positions: []float64{-6, -4, 4, 6}},
		}}},
	}
	// Pair 1 spans x -10..0 (clipped to -5..0, 5 mm) and opens y -6..4,
	// clipped to -3..4 (7 mm); pair 2 spans x 0..10 (10 mm) and opens y
	// -4..6, clipped to -3..6 (9 mm).
	if area, err := beam.ApertureArea(0); err != nil || area != 5*7+10*9 {
		t.Fatalf("MLCY aperture = %v, %v, want 125", area, err)
	}
	if _, err := (&Beam{ControlPoints: []ControlPoint{{}}}).ApertureArea(0); !errors.Is(err, ErrNoAperture) {
		t.Fatalf("beam without MLC error = %v", err)
	}
	if _, err := beam.ApertureArea(3); !errors.Is(err, ErrNoAperture) {
		t.Fatalf("missing control point error = %v", err)
	}
}

func TestArcFollowsEachSegmentDirection(t *testing.T) {
	cps := func(points ...any) []ControlPoint {
		var out []ControlPoint
		for i := 0; i < len(points); i += 2 {
			out = append(out, ControlPoint{GantryAngle: points[i].(float64), GantryRotationDirection: points[i+1].(string)})
		}
		return out
	}
	for _, test := range []struct {
		points []ControlPoint
		want   GantryArc
	}{
		{cps(350.0, "CW", 10.0, "NONE"), GantryArc{Start: 350, Stop: 10, Extent: 20, Direction: "CW"}},
		{cps(10.0, "CC", 350.0, "NONE"), GantryArc{Start: 10, Stop: 350, Extent: 20, Direction: "CC"}},
		{cps(0.0, "CW", 90.0, "CC", 0.0, "NONE"), GantryArc{Start: 0, Stop: 0, Extent: 180, Direction: "MIXED"}},
		{cps(45.0, "NONE", 45.0, "NONE"), GantryArc{Start: 45, Stop: 45, Extent: 0, Direction: "NONE"}},
	} {
		if got := (&Beam{ControlPoints: test.points}).Arc(); got != test.want {
			t.Errorf("Arc(%v) = %+v, want %+v", test.points, got, test.want)
		}
	}
}
