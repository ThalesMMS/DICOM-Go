package rtplan

import (
	"bytes"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// The fixtures are synthetic: no element comes from a real plan or patient.

const (
	testStudyUID   = "2.25.126900000000000000000000000000000001"
	testSeriesUID  = "2.25.126900000000000000000000000000000002"
	testPlanUID    = "2.25.126900000000000000000000000000000003"
	testFrameUID   = "2.25.126900000000000000000000000000000004"
	testStructUID  = "2.25.126900000000000000000000000000000005"
	rtStructureSet = "1.2.840.10008.5.1.4.1.1.481.3"
)

// elems is a mutable attribute set that becomes a sorted data set.
type elems map[core.Tag]core.Element

func newElems(list ...core.Element) elems {
	e := elems{}
	for _, el := range list {
		e[el.Tag()] = el
	}
	return e
}

func (e elems) set(list ...core.Element) {
	for _, el := range list {
		e[el.Tag()] = el
	}
}

func (e elems) del(tags ...core.Tag) {
	for _, tag := range tags {
		delete(e, tag)
	}
}

func (e elems) list() []core.Element {
	out := make([]core.Element, 0, len(e))
	for _, el := range e {
		out = append(out, el)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Tag(), out[j].Tag()
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Element < b.Element
	})
	return out
}

func (e elems) dataset() core.DataSet { return derivedio.DataSet(e.list()...) }

type cpFixture struct {
	attrs     elems
	positions []elems // nil: no Beam Limiting Device Position Sequence
}

type beamFixture struct {
	attrs   elems
	devices []elems
	cps     []cpFixture
}

type fractionFixture struct {
	attrs elems
	beams []elems
}

type planFixture struct {
	sopClassUID    string
	attrs          elems
	doseRefs       []elems
	fractionGroups []fractionFixture
	beams          []beamFixture
	setups         []elems
	tolerances     []elems
	brachy         []elems
}

func device(deviceType string, pairs int, boundaries ...float64) elems {
	e := newElems(derivedio.CS(tagRTBeamLimitingDeviceType, deviceType), derivedio.IS(tagNumberOfLeafJawPairs, pairs))
	if len(boundaries) > 0 {
		e.set(derivedio.DS(tagLeafPositionBoundaries, boundaries...))
	}
	return e
}

func position(deviceType string, values ...float64) elems {
	return newElems(derivedio.CS(tagRTBeamLimitingDeviceType, deviceType), derivedio.DS(tagLeafJawPositions, values...))
}

// mlcBoundaries returns 11 boundaries from -50 to 50 mm (10 pairs, 10 mm).
func mlcBoundaries() []float64 {
	out := make([]float64, 11)
	for i := range out {
		out[i] = float64(-50 + 10*i)
	}
	return out
}

func banks(bank1, bank2 float64) []float64 {
	out := make([]float64, 20)
	for i := 0; i < 10; i++ {
		out[i], out[10+i] = bank1, bank2
	}
	return out
}

func firstControlPoint(gantry float64, direction string, collimator float64) elems {
	return newElems(
		derivedio.IS(tagControlPointIndex, 0),
		derivedio.DS(tagNominalBeamEnergy, 6),
		derivedio.DS(tagDoseRateSet, 600),
		derivedio.DS(tagGantryAngle, gantry),
		derivedio.CS(tagGantryRotationDirection, direction),
		derivedio.DS(tagBLDAngle, collimator),
		derivedio.CS(tagBLDRotationDirection, RotationNone),
		derivedio.DS(tagPatientSupportAngle, 0),
		derivedio.CS(tagPatientSupportRotationDir, RotationNone),
		derivedio.DS(tagTableTopEccentricAngle, 0),
		derivedio.CS(tagTableTopEccentricRotationDir, RotationNone),
		derivedio.Str(tagTableTopVerticalPosition, core.VRDS, ""),
		derivedio.Str(tagTableTopLongitudinalPosition, core.VRDS, ""),
		derivedio.Str(tagTableTopLateralPosition, core.VRDS, ""),
		derivedio.DS(tagIsocenterPosition, 0, 10, -20),
		derivedio.DS(tagSourceToSurfaceDistance, 900),
		derivedio.DS(tagCumulativeMetersetWeight, 0),
	)
}

// newPlanFixture is a two-beam photon plan: a STATIC MLC field and a
// DYNAMIC clockwise arc, one fraction group of 30 fractions.
func newPlanFixture() *planFixture {
	staticBeam := beamFixture{
		attrs: newElems(
			derivedio.IS(tagBeamNumber, 1),
			derivedio.LO(tagBeamName, "AP"),
			derivedio.CS(tagBeamType, BeamTypeStatic),
			derivedio.CS(tagRadiationType, "PHOTON"),
			derivedio.SH(tagTreatmentMachineName, "SYNTH_LINAC"),
			derivedio.LO(tagManufacturer, "SYNTHETIC"),
			derivedio.CS(tagPrimaryDosimeterUnit, "MU"),
			derivedio.DS(tagSourceAxisDistance, 1000),
			derivedio.CS(tagTreatmentDeliveryType, "TREATMENT"),
			derivedio.IS(tagReferencedPatientSetupNumber, 1),
			derivedio.IS(tagReferencedToleranceTableNumber, 1),
			derivedio.IS(tagNumberOfWedges, 0),
			derivedio.IS(tagNumberOfCompensators, 0),
			derivedio.IS(tagNumberOfBoli, 0),
			derivedio.IS(tagNumberOfBlocks, 0),
			derivedio.DS(tagFinalCumulativeMetersetWeight, 1),
			derivedio.IS(tagNumberOfControlPoints, 2),
		),
		devices: []elems{device(DeviceAsymX, 1), device(DeviceAsymY, 1), device(DeviceMLCX, 10, mlcBoundaries()...)},
		cps: []cpFixture{
			{attrs: firstControlPoint(0, RotationNone, 0), positions: []elems{
				position(DeviceAsymX, -50, 50), position(DeviceAsymY, -30, 30), position(DeviceMLCX, banks(-20, 20)...),
			}},
			{attrs: newElems(derivedio.IS(tagControlPointIndex, 1), derivedio.DS(tagCumulativeMetersetWeight, 1))},
		},
	}
	arcBeam := beamFixture{
		attrs: newElems(
			derivedio.IS(tagBeamNumber, 2),
			derivedio.LO(tagBeamName, "ARC"),
			derivedio.CS(tagBeamType, BeamTypeDynamic),
			derivedio.CS(tagRadiationType, "PHOTON"),
			derivedio.SH(tagTreatmentMachineName, "SYNTH_LINAC"),
			derivedio.CS(tagPrimaryDosimeterUnit, "MU"),
			derivedio.DS(tagSourceAxisDistance, 1000),
			derivedio.IS(tagNumberOfWedges, 0),
			derivedio.IS(tagNumberOfCompensators, 0),
			derivedio.IS(tagNumberOfBoli, 0),
			derivedio.IS(tagNumberOfBlocks, 0),
			derivedio.DS(tagFinalCumulativeMetersetWeight, 1),
			derivedio.IS(tagNumberOfControlPoints, 3),
		),
		devices: []elems{device(DeviceAsymX, 1), device(DeviceAsymY, 1), device(DeviceMLCX, 10, mlcBoundaries()...)},
		cps: []cpFixture{
			{attrs: firstControlPoint(181, RotationClockwise, 30), positions: []elems{
				position(DeviceAsymX, -40, 40), position(DeviceAsymY, -50, 50), position(DeviceMLCX, banks(-20, 20)...),
			}},
			{attrs: newElems(derivedio.IS(tagControlPointIndex, 1), derivedio.DS(tagGantryAngle, 90), derivedio.DS(tagCumulativeMetersetWeight, 0.5)),
				positions: []elems{position(DeviceMLCX, banks(-10, 10)...)}},
			{attrs: newElems(derivedio.IS(tagControlPointIndex, 2), derivedio.DS(tagGantryAngle, 179), derivedio.DS(tagCumulativeMetersetWeight, 1))},
		},
	}
	return &planFixture{
		sopClassUID: RTPlanStorage,
		attrs: newElems(
			derivedio.CS(core.NewTag(0x0008, 0x0005), "ISO_IR 192"),
			derivedio.Str(core.NewTag(0x0008, 0x0020), core.VRDA, "20260101"),
			derivedio.Str(core.NewTag(0x0008, 0x0030), core.VRTM, "120000"),
			derivedio.SH(core.NewTag(0x0008, 0x0050), ""),
			derivedio.CS(tagModality, "RTPLAN"),
			derivedio.LO(tagManufacturer, "SYNTHETIC"),
			derivedio.Str(core.NewTag(0x0008, 0x0090), core.VRPN, ""),
			derivedio.Str(core.NewTag(0x0008, 0x1070), core.VRPN, ""),
			derivedio.Str(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^PLAN"),
			derivedio.LO(core.NewTag(0x0010, 0x0020), "SYN-RTPLAN"),
			derivedio.Str(core.NewTag(0x0010, 0x0030), core.VRDA, ""),
			derivedio.CS(core.NewTag(0x0010, 0x0040), ""),
			derivedio.UI(tagStudyInstanceUID, testStudyUID),
			derivedio.UI(tagSeriesInstanceUID, testSeriesUID),
			derivedio.SH(core.NewTag(0x0020, 0x0010), "1"),
			derivedio.IS(core.NewTag(0x0020, 0x0011), 1),
			derivedio.UI(tagFrameOfReferenceUID, testFrameUID),
			derivedio.LO(core.NewTag(0x0020, 0x1040), ""),
			derivedio.SH(tagRTPlanLabel, "SYNTH_PLAN"),
			derivedio.LO(tagRTPlanName, "Synthetic two-beam plan"),
			derivedio.Str(tagRTPlanDate, core.VRDA, "20260101"),
			derivedio.Str(tagRTPlanTime, core.VRTM, "120000"),
			derivedio.CS(tagPlanIntent, "CURATIVE"),
			derivedio.CS(tagRTPlanGeometry, GeometryPatient),
			derivedio.Seq(tagReferencedStructureSetSequence, derivedio.DataSet(
				derivedio.UI(tagRefSOPClassUID, rtStructureSet), derivedio.UI(tagRefSOPInstanceUID, testStructUID))),
			derivedio.CS(tagApprovalStatus, "UNAPPROVED"),
		),
		doseRefs: []elems{newElems(
			derivedio.IS(tagDoseReferenceNumber, 1),
			derivedio.CS(tagDoseReferenceStructureType, "COORDINATES"),
			derivedio.LO(tagDoseReferenceDescription, "SYNTH_TARGET"),
			derivedio.DS(tagDoseReferencePointCoords, 0, 10, -20),
			derivedio.CS(tagDoseReferenceType, "TARGET"),
			derivedio.DS(tagTargetPrescriptionDose, 60),
		)},
		fractionGroups: []fractionFixture{{
			attrs: newElems(
				derivedio.IS(tagFractionGroupNumber, 1),
				derivedio.IS(tagNumberOfFractionsPlanned, 30),
				derivedio.IS(tagNumberOfBeams, 2),
				derivedio.IS(tagNumberOfBrachySetups, 0),
				derivedio.Seq(tagReferencedDoseReferenceSeq, derivedio.DataSet(derivedio.IS(tagReferencedDoseReferenceNumber, 1))),
			),
			beams: []elems{
				newElems(derivedio.IS(tagReferencedBeamNumber, 1), derivedio.DS(tagBeamDose, 0.8), derivedio.DS(tagBeamMeterset, 100.5)),
				newElems(derivedio.IS(tagReferencedBeamNumber, 2), derivedio.DS(tagBeamDose, 1.2), derivedio.DS(tagBeamMeterset, 250)),
			},
		}},
		beams: []beamFixture{staticBeam, arcBeam},
		setups: []elems{newElems(
			derivedio.CS(tagPatientPosition, "HFS"),
			derivedio.IS(tagPatientSetupNumber, 1),
			derivedio.CS(tagSetupTechnique, "ISOCENTRIC"),
		)},
		tolerances: []elems{newElems(
			derivedio.IS(tagToleranceTableNumber, 1),
			derivedio.SH(tagToleranceTableLabel, "T1"),
			derivedio.DS(tagGantryAngleTolerance, 1),
			derivedio.Seq(tagBLDToleranceSequence, derivedio.DataSet(
				derivedio.DS(tagBLDPositionTolerance, 2), derivedio.CS(tagRTBeamLimitingDeviceType, DeviceMLCX))),
		)},
	}
}

func sequence(tag core.Tag, items []elems) core.Element {
	sets := make([]core.DataSet, len(items))
	for i, it := range items {
		sets[i] = it.dataset()
	}
	return derivedio.Seq(tag, sets...)
}

// dataset builds the plan data set in memory.
func (f *planFixture) dataset(t testing.TB) *object.Object {
	t.Helper()
	top := newElems(f.attrs.list()...)
	top.set(derivedio.UI(tagSOPClassUID, f.sopClassUID), derivedio.UI(tagSOPInstanceUID, testPlanUID))
	if len(f.doseRefs) > 0 {
		top.set(sequence(tagDoseReferenceSequence, f.doseRefs))
	}
	if len(f.setups) > 0 {
		top.set(sequence(tagPatientSetupSequence, f.setups))
	}
	if len(f.tolerances) > 0 {
		top.set(sequence(tagToleranceTableSequence, f.tolerances))
	}
	if len(f.brachy) > 0 {
		top.set(sequence(tagApplicationSetupSequence, f.brachy))
	}
	if len(f.fractionGroups) > 0 {
		groups := make([]elems, len(f.fractionGroups))
		for i, g := range f.fractionGroups {
			groups[i] = newElems(g.attrs.list()...)
			if len(g.beams) > 0 {
				groups[i].set(sequence(tagReferencedBeamSequence, g.beams))
			}
		}
		top.set(sequence(tagFractionGroupSequence, groups))
	}
	if len(f.beams) > 0 {
		beams := make([]elems, len(f.beams))
		for i, b := range f.beams {
			beams[i] = newElems(b.attrs.list()...)
			if len(b.devices) > 0 {
				beams[i].set(sequence(tagBLDSequence, b.devices))
			}
			cps := make([]elems, len(b.cps))
			for j, cp := range b.cps {
				cps[j] = newElems(cp.attrs.list()...)
				if cp.positions != nil {
					cps[j].set(sequence(tagBLDPositionSequence, cp.positions))
				}
			}
			beams[i].set(sequence(tagControlPointSequence, cps))
		}
		top.set(sequence(tagBeamSequence, beams))
	}
	return derivedio.Object(top.list()...)
}

// file builds the Part 10 file, writes it and reads it back so the tests
// exercise the parser's view of the plan.
func (f *planFixture) file(t testing.TB) *object.File {
	t.Helper()
	file, err := derivedio.File(f.sopClassUID, testPlanUID, f.dataset(t))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	read, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return read
}

func (f *planFixture) bytes(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, f.file(t)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
