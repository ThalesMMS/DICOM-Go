package rtplan

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// Summary is a presentation-neutral digest of a plan for inspectors.
type Summary struct {
	Label          string
	Name           string
	Intent         string
	Geometry       string
	ApprovalStatus string
	// Prescription lists the target prescription doses (Gy) of the dose
	// references that carry one, in plan order.
	Prescription   []float64
	FractionGroups []FractionGroupSummary
	Beams          []BeamSummary
	BrachySetups   int
	Warnings       int
	Profile        string
	IonBeams       []IonBeamSummary
	Sources        int
	Channels       int
}

// FractionGroupSummary digests a fraction group.
type FractionGroupSummary struct {
	Number           int
	FractionsPlanned *int
	Beams            int
	BrachySetups     int
	// MetersetPerFraction sums the Beam Meterset of the referenced beams;
	// nil when any of them has none.
	MetersetPerFraction *float64
	MetersetUnit        string
	// DosePerFraction sums the Beam Dose of the referenced beams; nil when
	// any of them has none.
	DosePerFraction *float64
}

// BeamSummary digests a beam at its first control point.
type BeamSummary struct {
	Number               int
	Name                 string
	Type                 string
	RadiationType        string
	TreatmentMachineName string
	DeliveryType         string
	NominalEnergy        *float64
	ControlPoints        int
	Arc                  GantryArc
	CollimatorAngle      float64
	CouchAngle           float64
	// Meterset is the Beam Meterset per fraction of the first fraction group
	// that references the beam, in MetersetUnit.
	Meterset     *float64
	MetersetUnit string
	// MLC is the type of the beam's multileaf collimator and MLCPairs its
	// number of leaf pairs; empty for a beam without MLC.
	MLC      string
	MLCPairs int
	// JawX and JawY are the jaw openings (mm) at the first control point.
	JawX, JawY *float64
	// Aperture is the MLC open area (mm²) at the first control point.
	Aperture *float64
	Wedges   int
	Blocks   int
}

// Summary digests the plan.
func (p *Plan) Summary() Summary {
	if p == nil {
		return Summary{}
	}
	s := Summary{
		Label: p.Label, Name: p.Name, Intent: p.Intent, Geometry: p.Geometry,
		ApprovalStatus: p.ApprovalStatus, BrachySetups: len(p.BrachySetups), Warnings: len(p.Warnings),
	}
	s.Profile = "Conventional RT Plan"
	if len(p.IonBeams) > 0 {
		s.Profile = "Ion PROTON/ION NONE/MODULATED"
	}
	if len(p.BrachySetups) > 0 {
		s.Profile = "Brachy " + p.BrachyTreatmentType + " STEPWISE/FIXED"
	}
	s.Sources = len(p.Sources)
	for _, v := range p.BrachySetups {
		s.Channels += len(v.Channels)
	}
	for _, ref := range p.DoseReferences {
		if ref.TargetPrescriptionDose != nil {
			s.Prescription = append(s.Prescription, *ref.TargetPrescriptionDose)
		}
	}
	for _, group := range p.FractionGroups {
		g := FractionGroupSummary{Number: group.Number, FractionsPlanned: group.FractionsPlanned, Beams: group.NumberOfBeams, BrachySetups: group.NumberOfBrachySetups}
		g.MetersetUnit = groupMetersetUnit(p, group)
		g.MetersetPerFraction = sumOf(group.ReferencedBeams, func(ref ReferencedBeam) *float64 { return ref.Meterset })
		if g.MetersetUnit == "declared mixed/unknown units" {
			g.MetersetPerFraction = nil
		}
		g.DosePerFraction = sumOf(group.ReferencedBeams, func(ref ReferencedBeam) *float64 { return ref.Dose })
		s.FractionGroups = append(s.FractionGroups, g)
	}
	for i := range p.Beams {
		beam := &p.Beams[i]
		b := BeamSummary{
			Number: beam.Number, Name: beam.Name, Type: beam.Type, RadiationType: beam.RadiationType,
			TreatmentMachineName: beam.TreatmentMachineName, DeliveryType: beam.TreatmentDeliveryType,
			ControlPoints: len(beam.ControlPoints), Arc: beam.Arc(), MetersetUnit: beam.PrimaryDosimeterUnit,
			Wedges: beam.NumberOfWedges, Blocks: beam.NumberOfBlocks,
		}
		if len(beam.ControlPoints) > 0 {
			first := beam.ControlPoints[0]
			b.NominalEnergy = first.NominalBeamEnergy
			b.CollimatorAngle = first.BeamLimitingDeviceAngle
			b.CouchAngle = first.PatientSupportAngle
			if x, y, okX, okY := beam.JawOpening(0); okX || okY {
				if okX {
					b.JawX = &x
				}
				if okY {
					b.JawY = &y
				}
			}
			if area, err := beam.ApertureArea(0); err == nil {
				b.Aperture = &area
			}
		}
		if mlc, ok := beam.MLC(); ok {
			b.MLC, b.MLCPairs = mlc.Type, mlc.Pairs
		}
	found:
		for _, group := range p.FractionGroups {
			for _, ref := range group.ReferencedBeams {
				if ref.BeamNumber == beam.Number {
					b.Meterset = ref.Meterset
					break found
				}
			}
		}
		s.Beams = append(s.Beams, b)
	}
	for _, b := range p.IonBeams {
		x := IonBeamSummary{Number: b.Number, Name: b.Name, RadiationType: b.RadiationType, ScanMode: b.ScanMode, Unit: b.PrimaryDosimeterUnit, ControlPoints: len(b.ControlPoints), RangeShifters: len(b.RangeShifters)}
		if len(b.ControlPoints) > 0 {
			x.Energy = b.ControlPoints[0].NominalBeamEnergy
		}
		for _, cp := range b.ControlPoints {
			x.Spots += len(cp.SpotWeights)
		}
		s.IonBeams = append(s.IonBeams, x)
	}
	return s
}

func sumOf(refs []ReferencedBeam, value func(ReferencedBeam) *float64) *float64 {
	if len(refs) == 0 {
		return nil
	}
	total := 0.0
	for _, ref := range refs {
		v := value(ref)
		if v == nil {
			return nil
		}
		total += *v
		if !finite(total) {
			return nil
		}
	}
	return &total
}

// IonBeamSummary deliberately has no conventional aperture/dose calculation.
type IonBeamSummary struct {
	Number                              int
	Name, RadiationType, ScanMode, Unit string
	ControlPoints, RangeShifters, Spots int
	Energy                              *float64
}

func groupMetersetUnit(p *Plan, g FractionGroup) string {
	unit := ""
	for _, ref := range g.ReferencedBeams {
		u := ""
		for _, b := range p.Beams {
			if b.Number == ref.BeamNumber {
				u = b.PrimaryDosimeterUnit
			}
		}
		for _, b := range p.IonBeams {
			if b.Number == ref.BeamNumber {
				u = b.PrimaryDosimeterUnit
			}
		}
		if u == "" || unit != "" && u != unit {
			return "declared mixed/unknown units"
		}
		unit = u
	}
	return unit
}

// InspectionTarget is an entity selected by its semantic identifier.
type InspectionTarget struct {
	Kind, Label                            string
	BeamNumber, SetupNumber, ChannelNumber int
}
type InspectionValue struct{ Name, Value, Unit, Coordinates string }
type InspectionRow struct {
	Label  string
	Values []InspectionValue
}
type InspectionPage struct {
	Rows          []InspectionRow
	Offset, Total int
}

// InspectionTargets lists all supported Ion beams, brachy sources and channels.
func (p *Plan) InspectionTargets(ctx context.Context, options ...Limits) ([]InspectionTarget, error) {
	if ctx == nil || p == nil {
		return nil, ErrInvalidObject
	}
	l, err := inspectionLimits(options)
	if err != nil {
		return nil, err
	}
	if err := admitModel(ctx, reflect.ValueOf(p), l); err != nil {
		return nil, err
	}
	out := []InspectionTarget{}
	for _, b := range p.IonBeams {
		out = append(out, InspectionTarget{Kind: "ion", BeamNumber: b.Number, Label: fmt.Sprintf("Ion beam %d %s", b.Number, b.Name)})
	}
	if len(p.Sources) > 0 {
		out = append(out, InspectionTarget{Kind: "sources", Label: "Brachy sources"})
	}
	for _, s := range p.BrachySetups {
		for _, c := range s.Channels {
			out = append(out, InspectionTarget{Kind: "brachy", SetupNumber: s.Number, ChannelNumber: c.Number, Label: fmt.Sprintf("Setup %d / channel %d", s.Number, c.Number)})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectPage produces at most 256 presentation rows. CP state, every spot,
// range-shifter setting and leaf/jaw pair remain accessible through pagination.
func (p *Plan) InspectPage(ctx context.Context, target InspectionTarget, offset, limit int, options ...Limits) (InspectionPage, error) {
	if ctx == nil || p == nil || offset < 0 || limit < 1 || limit > 256 {
		return InspectionPage{}, ErrInvalidObject
	}
	l, err := inspectionLimits(options)
	if err != nil {
		return InspectionPage{}, err
	}
	if err := admitModel(ctx, reflect.ValueOf(p), l); err != nil {
		return InspectionPage{}, err
	}
	page := InspectionPage{Offset: offset}
	matched := false
	emit := func(label string, build func() []InspectionValue) {
		n := page.Total
		page.Total++
		if n >= offset && n-offset < limit {
			page.Rows = append(page.Rows, InspectionRow{label, build()})
		}
	}
	number := func(v float64) string { return fmt.Sprintf("%g", v) }
	opt := func(v *float64) string {
		if v == nil {
			return "unavailable"
		}
		return number(*v)
	}
	attr := func(v Attribute[float64]) string { return opt(v.Value) }
	vec := func(v []float64) string {
		if len(v) == 0 {
			return "unavailable"
		}
		text := make([]string, len(v))
		for i, x := range v {
			text[i] = number(x)
		}
		return strings.Join(text, ", ")
	}
	val := func(n, v, u, c string) InspectionValue { return InspectionValue{n, v, u, c} }
	switch target.Kind {
	case "sources":
		matched = true
		for _, source := range p.Sources {
			s := source
			emit(fmt.Sprintf("Source %d", s.Number), func() []InspectionValue {
				return []InspectionValue{val("Type", s.Type, "", ""), val("Isotope", s.IsotopeName, "", ""), val("Half-life", attr(s.IsotopeHalfLife), "day", ""), val("Reference air kerma rate", attr(s.ReferenceAirKermaRate), "µGy/h at 1 m", ""), val("Strength", attr(s.Strength), s.StrengthUnits, ""), val("Strength reference", s.StrengthReferenceDate+" "+s.StrengthReferenceTime, "", "declared precision")}
			})
		}
	case "brachy":
		for _, s := range p.BrachySetups {
			if s.Number != target.SetupNumber {
				continue
			}
			for _, c := range s.Channels {
				if c.Number != target.ChannelNumber {
					continue
				}
				matched = true
				emit("Channel", func() []InspectionValue {
					return []InspectionValue{val("Source", fmt.Sprint(c.SourceNumber), "", "reference"), val("Movement", c.Movement, "", ""), val("Total time", attr(c.TotalTime), "s", "one pulse for PDR"), val("Length", attr(c.Length), "mm", "channel"), val("Step size", attr(c.StepSize), "mm", "channel"), val("Final weight", attr(c.FinalCumulativeTimeWeight), "weight", ""), val("Pulse interval", attr(c.PulseInterval), "s", ""), val("Pulses", func() string {
						if c.NumberOfPulses.Value == nil {
							return "unavailable"
						}
						return fmt.Sprint(*c.NumberOfPulses.Value)
					}(), "count", "")}
				})
				for _, cp := range c.ControlPoints {
					if err := ctx.Err(); err != nil {
						return InspectionPage{}, err
					}
					x := cp
					emit(fmt.Sprintf("CP %d", x.Index), func() []InspectionValue {
						return []InspectionValue{val("Time weight", attr(x.CumulativeTimeWeight), "weight", ""), val("Relative position", number(x.RelativePosition), "mm", "distal-most possible position"), val("3D position", vec(x.Position3D), "mm", "patient"), val("Orientation", vec(x.Orientation), "direction cosines", "patient")}
					})
				}
			}
		}
	case "ion":
		for _, b := range p.IonBeams {
			if b.Number != target.BeamNumber {
				continue
			}
			matched = true
			emit("Beam", func() []InspectionValue {
				return []InspectionValue{val("Radiation", b.RadiationType, "", ""), val("Scan", b.ScanMode, "", ""), val("Dosimeter", b.PrimaryDosimeterUnit, "", "declared"), val("Snout", b.SnoutID, "", "machine identifier"), val("Virtual source-axis distances", vec(b.VirtualSourceAxisDistances), "mm", "IEC GANTRY X, Y")}
			})
			for _, cp := range b.ControlPoints {
				if err := ctx.Err(); err != nil {
					return InspectionPage{}, err
				}
				x := cp
				emit(fmt.Sprintf("CP %d", x.Index), func() []InspectionValue {
					return []InspectionValue{val("Cumulative meterset", opt(x.CumulativeMetersetWeight), "weight", "plan normalized"), val("Energy", opt(x.NominalBeamEnergy), "MeV/nucleon", "nozzle entrance"), val("Gantry", number(x.GantryAngle)+" "+x.GantryRotationDirection, "degree", "IEC GANTRY"), val("Gantry pitch", attr(x.GantryPitchAngle)+" "+x.GantryPitchRotationDirection, "degree", "IEC GANTRY"), val("BLD angle", number(x.BeamLimitingDeviceAngle)+" "+x.BeamLimitingDeviceRotationDirection, "degree", "IEC BEAM LIMITING DEVICE"), val("Patient support", number(x.PatientSupportAngle)+" "+x.PatientSupportRotationDirection, "degree", "IEC PATIENT SUPPORT"), val("Table pitch", opt(x.TableTopPitchAngle)+" "+x.TableTopPitchRotationDirection, "degree", "IEC TABLE TOP"), val("Table roll", opt(x.TableTopRollAngle)+" "+x.TableTopRollRotationDirection, "degree", "IEC TABLE TOP"), val("Table vertical/longitudinal/lateral", opt(x.TableTopVerticalPosition)+", "+opt(x.TableTopLongitudinalPosition)+", "+opt(x.TableTopLateralPosition), "mm", "IEC TABLE TOP"), val("Isocentre", vec(x.IsocenterPosition), "mm", "patient"), val("Snout position", attr(x.SnoutPosition), "mm", "from isocentre"), val("Meterset rate", attr(x.MetersetRate), b.PrimaryDosimeterUnit+"/min", "declared"), val("Tune", x.ScanSpotTuneID, "", "machine code"), val("Paintings", func() string {
						if x.NumberOfPaintings.Value == nil {
							return "unavailable"
						}
						return fmt.Sprint(*x.NumberOfPaintings.Value)
					}(), "count", "")}
				})
				for _, rs := range x.RangeShifterSettings {
					r := rs
					emit(fmt.Sprintf("CP %d / range shifter %d", x.Index, r.Number), func() []InspectionValue {
						return []InspectionValue{val("Setting", r.Setting, "", "machine code"), val("Distance", attr(r.Distance), "mm", "from isocentre"), val("Water equivalent thickness", attr(r.WaterEquivalentThickness), "mm", "")}
					})
				}
				for i, weight := range x.SpotWeights {
					if i&255 == 0 {
						if err := ctx.Err(); err != nil {
							return InspectionPage{}, err
						}
					}
					j, w := i, weight
					emit(fmt.Sprintf("CP %d / spot %d", x.Index, j), func() []InspectionValue {
						position := "unavailable"
						if 2*j+1 < len(x.SpotPositions) {
							position = fmt.Sprintf("%g, %g", x.SpotPositions[2*j], x.SpotPositions[2*j+1])
						}
						return []InspectionValue{val("Position", position, "mm", "IEC GANTRY isocentric plane"), val("Meterset", fmt.Sprint(w), "weight", "")}
					})
				}
				for _, device := range x.DevicePositions {
					d := device
					n := len(d.Positions) / 2
					for i := 0; i < n; i++ {
						j := i
						emit(fmt.Sprintf("CP %d / %s pair %d", x.Index, d.DeviceType, j+1), func() []InspectionValue {
							return []InspectionValue{val("Banks 1, 2", fmt.Sprintf("%g, %g", d.Positions[j], d.Positions[j+n]), "mm", "IEC BEAM LIMITING DEVICE")}
						})
					}
				}
			}
		}
	default:
		return InspectionPage{}, ErrUnsupportedFeature
	}
	if !matched {
		return InspectionPage{}, pathError(ErrInvalidObject, "", "inspection entity was not found")
	}
	if offset > page.Total {
		return InspectionPage{}, pathError(ErrInvalidObject, "", "inspection offset is out of range")
	}
	if err := ctx.Err(); err != nil {
		return InspectionPage{}, err
	}
	return page, nil
}

func inspectionLimits(options []Limits) (Limits, error) {
	if len(options) > 1 {
		return Limits{}, ErrInvalidObject
	}
	if len(options) == 1 {
		return options[0].normalized()
	}
	return DefaultLimits(), nil
}
