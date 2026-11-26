package rtplan

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"testing"
)

func newBrachyFixture() *planFixture {
	f := newPlanFixture()
	f.beams = nil
	f.attrs.set(derivedio.Seq(ft(0x300a0206), derivedio.DataSet(derivedio.SH(tagTreatmentMachineName, "SYNTH_BRACHY"))), derivedio.CS(tagBrachyTreatmentTechnique, "INTRACAVITARY"), derivedio.CS(tagBrachyTreatmentType, "HDR"))
	sources := []core.DataSet{}
	for n := 1; n <= 2; n++ {
		sources = append(sources, derivedio.DataSet(derivedio.IS(ft(0x300a0212), n), derivedio.CS(ft(0x300a0214), "POINT"), derivedio.LO(ft(0x300a0226), "Ir-192"), derivedio.DS(ft(0x300a0228), 74), derivedio.CS(ft(0x300a0229), "AIR_KERMA_RATE"), derivedio.DS(ft(0x300a022a), 7.5), derivedio.Str(ft(0x300a022c), core.VRDA, "20260101"), derivedio.Str(ft(0x300a022e), core.VRTM, "120000")))
	}
	f.attrs.set(derivedio.Seq(ft(0x300a0210), sources...))
	for n := 1; n <= 2; n++ {
		channels := []core.DataSet{}
		for j := 1; j <= 2; j++ {
			points := []core.DataSet{}
			for k := 0; k < 2; k++ {
				points = append(points, derivedio.DataSet(derivedio.IS(tagControlPointIndex, k), derivedio.DS(ft(0x300a02d6), float64(k)), derivedio.DS(ft(0x300a02d2), float64(j)*2.5), derivedio.DS(ft(0x300a02d4), 1, float64(j), 3), familyNumeric(ft(0x300a0412), 0, 0, 1)))
			}
			channels = append(channels, derivedio.DataSet(derivedio.IS(ft(0x300a0282), j), derivedio.IS(tagReferencedSourceNumber, n), derivedio.CS(ft(0x300a0288), "STEPWISE"), derivedio.DS(ft(0x300a0284), 100), derivedio.DS(ft(0x300a0286), 60), derivedio.DS(ft(0x300a02a0), 2.5), derivedio.Str(ft(0x300a02a2), core.VRIS, ""), derivedio.DS(ft(0x300a02c8), 1), derivedio.IS(tagNumberOfControlPoints, 2), derivedio.Seq(ft(0x300a02d0), points...)))
		}
		f.brachy = append(f.brachy, newElems(derivedio.CS(tagApplicationSetupType, "FLETCHER_SUIT"), derivedio.IS(tagApplicationSetupNumber, n), derivedio.LO(tagApplicationSetupName, "SYNTH_APPLICATOR"), derivedio.DS(tagTotalReferenceAirKerma, 0.25), derivedio.Seq(tagChannelSequence, channels...)))
	}
	f.fractionGroups[0].attrs.set(derivedio.IS(tagNumberOfBeams, 0), derivedio.IS(tagNumberOfBrachySetups, 2), derivedio.Seq(tagReferencedBrachySetupSequence, derivedio.DataSet(derivedio.IS(tagReferencedBrachySetupNumber, 1), derivedio.DS(tagBrachySetupDose, 7)), derivedio.DataSet(derivedio.IS(tagReferencedBrachySetupNumber, 2), derivedio.DS(tagBrachySetupDose, 7))))
	f.fractionGroups[0].beams = nil
	return f
}
func familyReparse(t testing.TB, f *planFixture) *Plan {
	t.Helper()
	p, e := Read(f.file(t))
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func familyNumeric(tag core.Tag, v ...float64) core.Element {
	entry, _ := std.Dictionary.ByTag(tag)
	switch entry.VR {
	case core.VRFL:
		out := make(core.Float32Value, len(v))
		for i, x := range v {
			out[i] = float32(x)
		}
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: entry.VR}, Value: out}
	case core.VRFD:
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: entry.VR}, Value: core.Float64Value(v)}
	}
	return derivedio.DS(tag, v...)
}

func newIonFixture() *planFixture {
	f := newPlanFixture()
	f.sopClassUID = RTIonPlanStorage
	f.beams = nil
	f.tolerances = nil
	f.attrs.set(derivedio.SH(tagRTPlanLabel, "SYNTH_ION"), derivedio.LO(tagRTPlanName, "Ion sintético α"))
	f.fractionGroups[0].attrs.set(derivedio.IS(tagNumberOfBeams, 1))
	f.fractionGroups[0].beams = []elems{newElems(derivedio.IS(tagReferencedBeamNumber, 1), derivedio.DS(tagBeamDose, 2), derivedio.DS(tagBeamMeterset, 10))}
	first := firstControlPoint(0, RotationNone, 0)
	first.del(tagDoseRateSet, tagSourceToSurfaceDistance, tagTableTopEccentricAngle, tagTableTopEccentricRotationDir)
	first.set(familyNumeric(tagNominalBeamEnergy, 150), familyNumeric(ft(0x300a014a), 0), derivedio.CS(ft(0x300a014c), "NONE"), familyNumeric(tagTableTopPitchAngle, 0), derivedio.CS(ft(0x300a0142), "NONE"), familyNumeric(tagTableTopRollAngle, 0), derivedio.CS(ft(0x300a0146), "NONE"), familyNumeric(ft(0x300a030d), 300), familyNumeric(ft(0x300a035a), 10), sequence(tagBLDPositionSequence, []elems{position(DeviceAsymX, -50, 50)}), derivedio.Seq(ft(0x300a0360), derivedio.DataSet(derivedio.IS(ft(0x300c0100), 1), derivedio.LO(ft(0x300a0362), "101"), familyNumeric(ft(0x300a0364), 250), familyNumeric(ft(0x300a0366), 2.5))))
	cps := []core.DataSet{}
	for j := 0; j < 2; j++ {
		e := newElems(derivedio.IS(tagControlPointIndex, j), derivedio.DS(tagCumulativeMetersetWeight, float64(j)))
		if j == 0 {
			e = first
		}
		weights := []float64{.25, .75}
		if j == 1 {
			weights = []float64{0, 0}
		}
		e.set(derivedio.SH(ft(0x300a0390), "TUNE_A"), derivedio.IS(ft(0x300a0392), 2), familyNumeric(ft(0x300a0394), -10, 3, 10, 3), familyNumeric(ft(0x300a0396), weights...), derivedio.IS(ft(0x300a039a), 2))
		cps = append(cps, e.dataset())
	}
	b := derivedio.DataSet(derivedio.IS(tagBeamNumber, 1), derivedio.LO(tagBeamName, "PROTON"), derivedio.CS(tagBeamType, "STATIC"), derivedio.CS(tagRadiationType, "PROTON"), derivedio.CS(ft(0x300a0308), "MODULATED"), derivedio.SH(tagTreatmentMachineName, "SYNTH_ION"), derivedio.CS(tagPrimaryDosimeterUnit, "NP"), familyNumeric(ft(0x300a030a), 1000, 1100), derivedio.CS(tagTreatmentDeliveryType, "TREATMENT"), derivedio.CS(ft(0x300a0350), "TABLE"), derivedio.IS(tagReferencedPatientSetupNumber, 1), derivedio.IS(tagNumberOfWedges, 0), derivedio.IS(tagNumberOfCompensators, 0), derivedio.IS(tagNumberOfBoli, 0), derivedio.IS(tagNumberOfBlocks, 0), derivedio.IS(ft(0x300a0330), 0), derivedio.IS(ft(0x300a0340), 0), derivedio.IS(ft(0x300a0312), 1), derivedio.Seq(ft(0x300a0314), derivedio.DataSet(derivedio.IS(ft(0x300a0316), 1), derivedio.SH(ft(0x300a0318), "RS1"), derivedio.CS(ft(0x300a0320), "BINARY"))), derivedio.Seq(ft(0x300a030c), derivedio.DataSet(derivedio.SH(ft(0x300a030f), "SNOUT"), derivedio.LO(ft(0x300a00f9), "OPAQUE_CODE"))), derivedio.Seq(ft(0x300a03a4), derivedio.DataSet(derivedio.CS(tagRTBeamLimitingDeviceType, DeviceAsymX), derivedio.IS(tagNumberOfLeafJawPairs, 1), familyNumeric(ft(0x300a00bb)))), derivedio.DS(tagFinalCumulativeMetersetWeight, 1), derivedio.IS(tagNumberOfControlPoints, 2), derivedio.Seq(ft(0x300a03a8), cps...))
	f.attrs.set(derivedio.Seq(ft(0x300a03a2), b))
	return f
}
