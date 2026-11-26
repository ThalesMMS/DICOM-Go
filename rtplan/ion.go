package rtplan

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"math"
)

// IonBeam supports single species PROTON/ION and NONE/MODULATED scanning.
// Machine-specific codes (notably range-shifter settings) are not interpreted.
type IonBeam struct {
	Number                                                                                                                                  int
	Name, Description, Type, RadiationType, ScanMode, TreatmentMachineName, PrimaryDosimeterUnit, TreatmentDeliveryType, PatientSupportType string
	AtomicNumber, MassNumber                                                                                                                Attribute[int]
	ChargeState                                                                                                                             Attribute[int]
	ReferencedPatientSetupNumber, ReferencedToleranceTableNumber                                                                            *int
	SnoutID                                                                                                                                 string
	VirtualSourceAxisDistances                                                                                                              []float64
	Devices                                                                                                                                 []BeamLimitingDevice
	RangeShifters                                                                                                                           []RangeShifter
	FinalCumulativeMetersetWeight                                                                                                           Attribute[float64]
	ControlPoints                                                                                                                           []IonControlPoint
}
type RangeShifter struct {
	Number                int
	ID, Type, Description string
}
type RangeShifterSetting struct {
	Number                             int
	Setting                            string
	Distance, WaterEquivalentThickness Attribute[float64]
}
type IonControlPoint struct {
	ControlPoint
	SnoutPosition, GantryPitchAngle, MetersetRate                                               Attribute[float64]
	EncodedSnoutPosition, EncodedGantryPitchAngle                                               bool
	GantryPitchRotationDirection, TableTopPitchRotationDirection, TableTopRollRotationDirection string
	RangeShifterSettings                                                                        []RangeShifterSetting
	EncodedRangeShifterSettings                                                                 bool
	ScanSpotTuneID                                                                              string
	SpotPositions, SpotWeights                                                                  []float32
	NumberOfPaintings                                                                           Attribute[int]
}

func readFL(r *reader, o *object.Object, t core.Tag, p string) []float32 {
	e, ok := o.Get(t)
	if !ok {
		return nil
	}
	if e.VR() != core.VRFL {
		r.fail(join(p, t), "FL representation is required")
		return nil
	}
	values := r.numbers(o, t, p)
	out := make([]float32, len(values))
	for i, v := range values {
		out[i] = float32(v)
	}
	return out
}
func readIon(r *reader, obj *object.Object, p *Plan) {
	items := r.sequence(obj, ft(0x300a03a2))
	if len(items) == 0 {
		r.fail(ft(0x300a03a2).String(), "ion beams are missing")
	}
	if len(p.Beams) != 0 {
		r.fail(tagBeamSequence.String(), "conventional beams are outside Ion profile")
	}
	for i, o := range items {
		path := item("", ft(0x300a03a2), i)
		b := IonBeam{Number: r.requiredInt(o, tagBeamNumber, path, "Beam Number"), Name: requiredText(r, o, tagBeamName, path), Description: r.str(o, tagBeamDescription), Type: r.code(o, tagBeamType), RadiationType: r.code(o, tagRadiationType), ScanMode: r.code(o, ft(0x300a0308)), TreatmentMachineName: r.str(o, tagTreatmentMachineName), PrimaryDosimeterUnit: r.code(o, tagPrimaryDosimeterUnit), TreatmentDeliveryType: r.code(o, tagTreatmentDeliveryType), PatientSupportType: r.code(o, ft(0x300a0350)), AtomicNumber: intAttribute(r, o, ft(0x300a0304), path), MassNumber: intAttribute(r, o, ft(0x300a0302), path), ChargeState: intAttribute(r, o, ft(0x300a0306), path), ReferencedPatientSetupNumber: r.intOpt(o, tagReferencedPatientSetupNumber, path), ReferencedToleranceTableNumber: r.intOpt(o, tagReferencedToleranceTableNumber, path), FinalCumulativeMetersetWeight: floatAttribute(r, o, tagFinalCumulativeMetersetWeight, path), VirtualSourceAxisDistances: r.numbers(o, ft(0x300a030a), path)}
		if len(b.VirtualSourceAxisDistances) != 2 || len(b.VirtualSourceAxisDistances) == 2 && (b.VirtualSourceAxisDistances[0] <= 0 || b.VirtualSourceAxisDistances[1] <= 0) {
			r.fail(join(path, ft(0x300a030a)), "virtual source-axis distances require two positive values")
		}
		if !oneOf(b.PrimaryDosimeterUnit, "MU", "NP") {
			r.fail(join(path, tagPrimaryDosimeterUnit), "dosimeter unit is outside profile")
		}
		if !oneOf(b.Type, "STATIC", "DYNAMIC") {
			r.fail(join(path, tagBeamType), "beam type is outside profile")
		}
		if !oneOf(b.RadiationType, "PROTON", "ION") {
			r.fail(join(path, tagRadiationType), "radiation type is outside profile")
		}
		if b.PatientSupportType == "" {
			r.fail(join(path, ft(0x300a0350)), "patient support type is missing")
		}
		if !oneOf(b.ScanMode, "NONE", "MODULATED") {
			r.fail(join(path, ft(0x300a0308)), "scan mode is outside profile")
		}
		if b.RadiationType == "ION" {
			if b.AtomicNumber.Value == nil || b.MassNumber.Value == nil || b.ChargeState.Value == nil {
				r.fail(join(path, ft(0x300a0302)), "ion species is incomplete")
			} else if *b.AtomicNumber.Value <= 0 || *b.MassNumber.Value < *b.AtomicNumber.Value || *b.ChargeState.Value == 0 || math.Abs(float64(*b.ChargeState.Value)) > float64(*b.AtomicNumber.Value) {
				r.fail(join(path, ft(0x300a0306)), "ion species is inconsistent")
			}
		}
		for _, tag := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks, ft(0x300a0330), ft(0x300a0340)} {
			if v := r.intOpt(o, tag, path); v != nil && *v != 0 {
				r.fail(join(path, tag), "accessory is outside profile")
			}
		}
		for _, tag := range []core.Tag{ft(0x300800a1), ft(0x300a03aa), ft(0x300a02ea), ft(0x300a03a6), ft(0x300c00b0), ft(0x300a0332), ft(0x300a0342), ft(0x300a0505)} {
			if o.Has(tag) {
				r.fail(join(path, tag), "advanced accessory is outside profile")
			}
		}
		if flag := r.code(o, ft(0x300800a3)); flag != "" && flag != "NO" {
			r.fail(join(path, ft(0x300800a3)), "enhanced devices are outside profile")
		}
		snouts := r.sequence(o, ft(0x300a030c))
		if len(snouts) > 1 {
			r.fail(join(path, ft(0x300a030c)), "snout item count")
		}
		if len(snouts) == 1 {
			b.SnoutID = requiredText(r, snouts[0], ft(0x300a030f), item(path, ft(0x300a030c), 0))
		}
		for j, d := range r.sequence(o, ft(0x300a03a4)) {
			dp := item(path, ft(0x300a03a4), j)
			b.Devices = append(b.Devices, BeamLimitingDevice{Type: r.code(d, tagRTBeamLimitingDeviceType), Pairs: r.requiredInt(d, tagNumberOfLeafJawPairs, dp, "Number of Leaf/Jaw Pairs"), LeafBoundaries: r.numbers(d, tagLeafPositionBoundaries, dp), SourceDistance: r.floatOpt(d, ft(0x300a00bb), dp)})
		}
		validateFamilyDevices(r, b.Devices, path, ft(0x300a03a4))
		if len(r.errors) > 0 {
			return
		}
		shifters := map[int]bool{}
		for j, s := range r.sequence(o, ft(0x300a0314)) {
			sp := item(path, ft(0x300a0314), j)
			rs := RangeShifter{Number: r.requiredInt(s, ft(0x300a0316), sp, "Range Shifter Number"), ID: requiredText(r, s, ft(0x300a0318), sp), Type: r.code(s, ft(0x300a0320)), Description: r.str(s, ft(0x300a0322))}
			if shifters[rs.Number] {
				r.fail(join(sp, ft(0x300a0316)), "range shifter number is duplicated")
			}
			shifters[rs.Number] = true
			if !oneOf(rs.Type, "ANALOG", "BINARY") {
				r.fail(join(sp, ft(0x300a0320)), "range shifter type is outside profile")
			}
			b.RangeShifters = append(b.RangeShifters, rs)
		}
		r.checkCount(o, ft(0x300a0314), path, "Number of Range Shifters", r.requiredInt(o, ft(0x300a0312), path, "Number of Range Shifters"))
		cpItems := r.sequence(o, ft(0x300a03a8))
		n := r.requiredInt(o, tagNumberOfControlPoints, path, "Number of Control Points")
		if n != len(cpItems) || n < 2 {
			r.fail(join(path, ft(0x300a03a8)), "control point count is inconsistent")
		}
		generic := r.readControlPointsAt(&Beam{Devices: b.Devices}, cpItems, path, ft(0x300a03a8))
		var snout, pitch Attribute[float64]
		settings := []RangeShifterSetting(nil)
		var pitchDirection, tablePitchDirection, tableRollDirection string
		for j, c := range cpItems {
			cpPath := item(path, ft(0x300a03a8), j)
			cp := IonControlPoint{ControlPoint: generic[j], ScanSpotTuneID: r.str(c, ft(0x300a0390)), NumberOfPaintings: intAttribute(r, c, ft(0x300a039a), cpPath), MetersetRate: floatAttribute(r, c, ft(0x300a035a), cpPath)}
			if v := floatAttribute(r, c, ft(0x300a030d), cpPath); v.Present {
				snout = v
				cp.EncodedSnoutPosition = true
			}
			cp.SnoutPosition = ownFloatAttribute(snout)
			if v := floatAttribute(r, c, ft(0x300a014a), cpPath); v.Present {
				pitch = v
				cp.EncodedGantryPitchAngle = true
			}
			cp.GantryPitchAngle = ownFloatAttribute(pitch)
			if c.Has(ft(0x300a014c)) {
				pitchDirection = r.code(c, ft(0x300a014c))
			}
			cp.GantryPitchRotationDirection = pitchDirection
			if c.Has(ft(0x300a0142)) {
				tablePitchDirection = r.code(c, ft(0x300a0142))
			}
			cp.TableTopPitchRotationDirection = tablePitchDirection
			if c.Has(ft(0x300a0146)) {
				tableRollDirection = r.code(c, ft(0x300a0146))
			}
			cp.TableTopRollRotationDirection = tableRollDirection
			if c.Has(ft(0x300a0360)) {
				cp.EncodedRangeShifterSettings = true
				seenSettings := map[int]bool{}
				for k, s := range r.sequence(c, ft(0x300a0360)) {
					sp := item(cpPath, ft(0x300a0360), k)
					rs := RangeShifterSetting{Number: r.requiredInt(s, ft(0x300c0100), sp, "Referenced Range Shifter Number"), Setting: requiredText(r, s, ft(0x300a0362), sp), Distance: floatAttribute(r, s, ft(0x300a0364), sp), WaterEquivalentThickness: floatAttribute(r, s, ft(0x300a0366), sp)}
					if seenSettings[rs.Number] {
						r.fail(join(sp, ft(0x300c0100)), "range shifter setting is duplicated")
					}
					seenSettings[rs.Number] = true
					attrNonnegative(r, rs.WaterEquivalentThickness, ft(0x300a0366), sp, false)
					if !shifters[rs.Number] {
						r.fail(join(sp, ft(0x300c0100)), "range shifter reference is missing")
					}
					replaced := false
					for m := range settings {
						if settings[m].Number == rs.Number {
							settings[m] = rs
							replaced = true
							break
						}
					}
					if !replaced {
						settings = append(settings, rs)
					}
				}
			}
			cp.RangeShifterSettings = append([]RangeShifterSetting(nil), settings...)
			for k := range cp.RangeShifterSettings {
				cp.RangeShifterSettings[k].Distance = ownFloatAttribute(cp.RangeShifterSettings[k].Distance)
				cp.RangeShifterSettings[k].WaterEquivalentThickness = ownFloatAttribute(cp.RangeShifterSettings[k].WaterEquivalentThickness)
			}
			if j == 0 && len(settings) != len(b.RangeShifters) {
				r.fail(join(cpPath, ft(0x300a0360)), "first range shifter settings are incomplete")
			}
			if cp.Index != j {
				r.fail(join(cpPath, tagControlPointIndex), "control point index is inconsistent")
			}
			if j == 0 {
				for _, field := range []Field{FieldNominalBeamEnergy, FieldGantryAngle, FieldGantryRotationDirection, FieldBeamLimitingDeviceAngle, FieldBeamLimitingDeviceRotationDirection, FieldPatientSupportAngle, FieldPatientSupportRotationDirection} {
					if !cp.Encodes(field) {
						r.fail(cpPath, "first machine state is incomplete")
					}
				}
			}
			if j == 0 {
				for _, t := range []core.Tag{ft(0x300a014a), ft(0x300a014c), tagTableTopPitchAngle, ft(0x300a0142), tagTableTopRollAngle, ft(0x300a0146), tagTableTopVerticalPosition, tagTableTopLongitudinalPosition, tagTableTopLateralPosition, ft(0x300a030d), tagIsocenterPosition} {
					if !c.Has(t) {
						r.fail(join(cpPath, t), "first Type 2 machine state is missing")
					}
				}
				if len(cp.DevicePositions) != len(b.Devices) {
					r.fail(join(cpPath, tagBLDPositionSequence), "first device positions are incomplete")
				}
			}
			for _, dir := range []string{cp.GantryRotationDirection, cp.BeamLimitingDeviceRotationDirection, cp.PatientSupportRotationDirection} {
				if !oneOf(dir, "CW", "CC", "NONE") {
					r.fail(cpPath, "rotation direction is outside profile")
				}
			}
			for _, dir := range []string{pitchDirection, tablePitchDirection, tableRollDirection} {
				if dir != "" && !oneOf(dir, "CW", "CC", "NONE") {
					r.fail(cpPath, "pitch or roll direction is outside profile")
				}
			}
			if cp.NominalBeamEnergy == nil || *cp.NominalBeamEnergy <= 0 {
				r.fail(join(cpPath, tagNominalBeamEnergy), "energy must be positive")
			}
			attrNonnegative(r, cp.MetersetRate, ft(0x300a035a), cpPath, false)
			for _, t := range []core.Tag{ft(0x300a03ac), ft(0x300a0370), ft(0x300a0380), ft(0x300800a2), ft(0x300a0302), ft(0x300a0304), ft(0x300a0306)} {
				if c.Has(t) {
					r.fail(join(cpPath, t), "control-point accessory or varying species is outside profile")
				}
			}
			if b.ScanMode == "NONE" {
				for _, t := range []core.Tag{ft(0x300a0390), ft(0x300a0392), ft(0x300a0394), ft(0x300a0396), ft(0x300a039a)} {
					if c.Has(t) {
						r.fail(join(cpPath, t), "spot attributes are outside non-scanning profile")
					}
				}
			}
			if b.ScanMode == "MODULATED" {
				count := r.requiredInt(c, ft(0x300a0392), cpPath, "Number of Scan Spot Positions")
				cp.SpotPositions = readFL(r, c, ft(0x300a0394), cpPath)
				cp.SpotWeights = readFL(r, c, ft(0x300a0396), cpPath)
				if count <= 0 || len(cp.SpotPositions) != 2*count || len(cp.SpotWeights) != count {
					r.fail(join(cpPath, ft(0x300a0394)), "spot count is inconsistent")
				}
				if cp.ScanSpotTuneID == "" || cp.NumberOfPaintings.Value == nil || *cp.NumberOfPaintings.Value <= 0 {
					r.fail(join(cpPath, ft(0x300a039a)), "spot tune or positive paintings are missing")
				}
				for _, v := range cp.SpotWeights {
					if v < 0 {
						r.fail(join(cpPath, ft(0x300a0396)), "spot weight is negative")
					}
				}
			}
			b.ControlPoints = append(b.ControlPoints, cp)
		}
		for j, cp := range b.ControlPoints {
			path := item(path, ft(0x300a03a8), j)
			w := cp.CumulativeMetersetWeight
			if w == nil || *w < 0 || j == 0 && *w != 0 {
				r.fail(join(path, tagCumulativeMetersetWeight), "cumulative weight is missing or inconsistent")
			}
			if j+1 < len(b.ControlPoints) && w != nil {
				next := b.ControlPoints[j+1].CumulativeMetersetWeight
				if next != nil {
					if *next < *w {
						r.fail(join(path, tagCumulativeMetersetWeight), "cumulative weight decreases")
					}
					if b.ScanMode == "MODULATED" {
						sum := 0.0
						abs := 0.0
						for _, v := range cp.SpotWeights {
							sum += float64(v)
							abs += math.Abs(float64(v))
						}
						tolerance := math.Ldexp(abs, -23) + math.Abs(math.Nextafter(*next, math.Inf(1))-*next) + math.Abs(math.Nextafter(*w, math.Inf(1))-*w)
						if math.Abs(sum-(*next-*w)) > tolerance {
							r.fail(join(path, ft(0x300a0396)), "spot sum differs from segment weight")
						}
					}
				}
			}
		}
		if len(b.ControlPoints) > 0 {
			last := b.ControlPoints[len(b.ControlPoints)-1].CumulativeMetersetWeight
			if last == nil || b.FinalCumulativeMetersetWeight.Value == nil || *last != *b.FinalCumulativeMetersetWeight.Value || *last <= 0 {
				r.fail(join(path, tagFinalCumulativeMetersetWeight), "final weight is inconsistent")
			}
		}
		p.IonBeams = append(p.IonBeams, b)
	}
}
