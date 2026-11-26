package rtplan

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"math"
)

func floatAttribute(r *reader, o *object.Object, t core.Tag, p string) Attribute[float64] {
	_, ok := o.Get(t)
	return Attribute[float64]{ok, r.floatOpt(o, t, p)}
}
func intAttribute(r *reader, o *object.Object, t core.Tag, p string) Attribute[int] {
	_, ok := o.Get(t)
	return Attribute[int]{ok, r.intOpt(o, t, p)}
}
func requiredFloat(r *reader, o *object.Object, t core.Tag, p string) float64 {
	v := r.floatOpt(o, t, p)
	if v == nil {
		r.fail(join(p, t), "required numeric value is missing")
		return 0
	}
	return *v
}
func requiredText(r *reader, o *object.Object, t core.Tag, p string) string {
	v := r.str(o, t)
	if v == "" {
		r.fail(join(p, t), "required text is missing")
	}
	return v
}
func attrNonnegative(r *reader, a Attribute[float64], t core.Tag, p string, positive bool) {
	if a.Value != nil && (!finite(*a.Value) || *a.Value < 0 || positive && *a.Value == 0) {
		r.fail(join(p, t), "numeric value is outside the admitted domain")
	}
}
func readBrachy(r *reader, obj *object.Object, p *Plan) {
	setups := r.sequence(obj, ft(0x300a0230))
	if len(setups) == 0 {
		if obj.Has(ft(0x300a0210)) {
			r.fail(ft(0x300a0210).String(), "sources without application setups are outside profile")
		}
		return
	}
	if p.SOPClassUID == RTIonPlanStorage {
		r.fail(ft(0x300a0230).String(), "brachy is not permitted in Ion profile")
		return
	}
	machines := r.sequence(obj, ft(0x300a0206))
	if len(machines) != 1 || !machines[0].Has(tagTreatmentMachineName) {
		r.fail(ft(0x300a0206).String(), "one treatment machine with Type 2 name is required")
	}
	if !oneOf(p.BrachyTreatmentTechnique, "INTRACAVITARY", "INTERSTITIAL", "INTRALUMENARY", "CONTACT") {
		r.fail(ft(0x300a0200).String(), "brachy technique is outside temporary implant profile")
	}
	if !oneOf(p.BrachyTreatmentType, "HDR", "MDR", "LDR", "PDR", "MANUAL") {
		r.fail(ft(0x300a0202).String(), "brachy treatment type is outside profile")
	}
	sourceNumbers := map[int]bool{}
	betaSources := map[int]bool{}
	for i, o := range r.sequence(obj, ft(0x300a0210)) {
		path := item("", ft(0x300a0210), i)
		s := BrachySource{Number: r.requiredInt(o, ft(0x300a0212), path, "Source Number"), Type: requiredText(r, o, ft(0x300a0214), path), SerialNumber: r.str(o, ft(0x30080105)), IsotopeName: requiredText(r, o, ft(0x300a0226), path), StrengthUnits: r.code(o, ft(0x300a0229)), IsotopeHalfLife: floatAttribute(r, o, ft(0x300a0228), path), ReferenceAirKermaRate: floatAttribute(r, o, ft(0x300a022a), path), Strength: floatAttribute(r, o, ft(0x300a022b), path), StrengthReferenceDate: requiredText(r, o, ft(0x300a022c), path), StrengthReferenceTime: requiredText(r, o, ft(0x300a022e), path)}
		if sourceNumbers[s.Number] {
			r.fail(join(path, ft(0x300a0212)), "source number is duplicated")
		}
		sourceNumbers[s.Number] = true
		betaSources[s.Number] = s.StrengthUnits == "DOSE_RATE_WATER"
		if !oneOf(s.Type, "POINT", "LINE", "CYLINDER", "SPHERE") {
			r.fail(join(path, ft(0x300a0214)), "source type is outside profile")
		}
		if s.IsotopeHalfLife.Value == nil {
			r.fail(join(path, ft(0x300a0228)), "half life is missing")
		}
		attrNonnegative(r, s.IsotopeHalfLife, ft(0x300a0228), path, true)
		if s.StrengthUnits != "" && !oneOf(s.StrengthUnits, "AIR_KERMA_RATE", "DOSE_RATE_WATER") {
			r.fail(join(path, ft(0x300a0229)), "strength unit is unsupported")
		}
		if s.StrengthUnits == "DOSE_RATE_WATER" && s.ReferenceAirKermaRate.Value != nil && *s.ReferenceAirKermaRate.Value != 0 {
			r.fail(join(path, ft(0x300a022a)), "non-gamma reference air kerma rate must be zero")
		}
		if s.StrengthUnits == "DOSE_RATE_WATER" && s.Strength.Value == nil {
			r.fail(join(path, ft(0x300a022b)), "non-gamma source strength is missing")
		}
		if s.ReferenceAirKermaRate.Value == nil {
			r.fail(join(path, ft(0x300a022a)), "reference air kerma rate is missing")
		}
		attrNonnegative(r, s.ReferenceAirKermaRate, ft(0x300a022a), path, false)
		attrNonnegative(r, s.Strength, ft(0x300a022b), path, false)
		p.Sources = append(p.Sources, s)
	}
	if len(p.Sources) == 0 {
		r.fail(ft(0x300a0210).String(), "sources are missing")
	}
	for i, o := range setups {
		path := item("", ft(0x300a0230), i)
		s := BrachySetup{Number: r.requiredInt(o, ft(0x300a0234), path, "Application Setup Number"), Type: requiredText(r, o, ft(0x300a0232), path), Name: r.str(o, ft(0x300a0236)), TotalReferenceAirKerma: r.floatOpt(o, ft(0x300a0250), path)}
		if s.TotalReferenceAirKerma != nil && *s.TotalReferenceAirKerma < 0 {
			r.fail(join(path, ft(0x300a0250)), "total reference air kerma is negative")
		}
		if s.TotalReferenceAirKerma == nil {
			r.fail(join(path, ft(0x300a0250)), "total reference air kerma is missing")
		}
		channels := r.sequence(o, ft(0x300a0280))
		if len(channels) == 0 {
			r.fail(join(path, ft(0x300a0280)), "channels are missing")
		}
		seen := map[int]bool{}
		refs := map[int]bool{}
		for j, c := range channels {
			cp := item(path, ft(0x300a0280), j)
			channel := BrachyChannel{Number: r.requiredInt(c, ft(0x300a0282), cp, "Channel Number"), SourceNumber: r.requiredInt(c, ft(0x300c000e), cp, "Referenced Source Number"), Movement: r.code(c, ft(0x300a0288)), Length: floatAttribute(r, c, ft(0x300a0284), cp), TotalTime: floatAttribute(r, c, ft(0x300a0286), cp), StepSize: floatAttribute(r, c, ft(0x300a02a0), cp), PulseInterval: floatAttribute(r, c, ft(0x300a028c), cp), NumberOfPulses: intAttribute(r, c, ft(0x300a028a), cp), TransferTubeNumber: intAttribute(r, c, ft(0x300a02a2), cp), TransferTubeLength: floatAttribute(r, c, ft(0x300a02a4), cp), FinalCumulativeTimeWeight: floatAttribute(r, c, ft(0x300a02c8), cp)}
			if seen[channel.Number] {
				r.fail(join(cp, ft(0x300a0282)), "channel number is duplicated")
			}
			seen[channel.Number] = true
			if !sourceNumbers[channel.SourceNumber] {
				r.fail(join(cp, ft(0x300c000e)), "referenced source is missing")
			}
			if !refs[channel.SourceNumber] {
				s.ReferencedSourceNumbers = append(s.ReferencedSourceNumbers, channel.SourceNumber)
				refs[channel.SourceNumber] = true
			}
			if !oneOf(channel.Movement, "STEPWISE", "FIXED") {
				r.fail(join(cp, ft(0x300a0288)), "source movement is outside profile")
			}
			if channel.TotalTime.Value == nil {
				r.fail(join(cp, ft(0x300a0286)), "channel total time is missing")
			}
			attrNonnegative(r, channel.TotalTime, ft(0x300a0286), cp, false)
			attrNonnegative(r, channel.Length, ft(0x300a0284), cp, false)
			attrNonnegative(r, channel.TransferTubeLength, ft(0x300a02a4), cp, false)
			if channel.Movement == "STEPWISE" && channel.StepSize.Value == nil {
				r.fail(join(cp, ft(0x300a02a0)), "step size is missing")
			}
			attrNonnegative(r, channel.StepSize, ft(0x300a02a0), cp, true)
			if p.BrachyTreatmentType == "PDR" {
				if channel.NumberOfPulses.Value == nil || *channel.NumberOfPulses.Value <= 0 {
					r.fail(join(cp, ft(0x300a028a)), "positive pulse count is missing")
				}
				if channel.PulseInterval.Value == nil {
					r.fail(join(cp, ft(0x300a028c)), "pulse interval is missing")
				}
				attrNonnegative(r, channel.PulseInterval, ft(0x300a028c), cp, true)
				if channel.PulseInterval.Value != nil && channel.TotalTime.Value != nil && *channel.PulseInterval.Value < *channel.TotalTime.Value {
					r.fail(join(cp, ft(0x300a028c)), "pulse interval is less than channel time")
				}
			}
			if channel.TransferTubeNumber.Value != nil && !channel.TransferTubeLength.Present {
				r.fail(join(cp, ft(0x300a02a4)), "transfer tube length is missing")
			}
			points := r.sequence(c, ft(0x300a02d0))
			n := r.requiredInt(c, tagNumberOfControlPoints, cp, "Number of Control Points")
			if n != len(points) || n < 2 {
				r.fail(join(cp, ft(0x300a02d0)), "control point count is inconsistent")
			}
			var previous *float64
			for k, v := range points {
				pp := item(cp, ft(0x300a02d0), k)
				point := BrachyControlPoint{Index: r.requiredInt(v, tagControlPointIndex, pp, "Control Point Index"), RelativePosition: requiredFloat(r, v, ft(0x300a02d2), pp), CumulativeTimeWeight: floatAttribute(r, v, ft(0x300a02d6), pp), Position3D: r.numbers(v, ft(0x300a02d4), pp), Orientation: r.numbers(v, ft(0x300a0412), pp), Position3DPresent: v.Has(ft(0x300a02d4)), OrientationPresent: v.Has(ft(0x300a0412))}
				if point.Index != k {
					r.fail(join(pp, tagControlPointIndex), "control point index is inconsistent")
				}
				if point.CumulativeTimeWeight.Value != nil {
					w := *point.CumulativeTimeWeight.Value
					if w < 0 || previous != nil && w < *previous {
						r.fail(join(pp, ft(0x300a02d6)), "time weight decreases or is negative")
					}
					previous = point.CumulativeTimeWeight.Value
				}
				if k == 0 && (point.CumulativeTimeWeight.Value == nil || *point.CumulativeTimeWeight.Value != 0) {
					r.fail(join(pp, ft(0x300a02d6)), "first cumulative time weight must be zero")
				}
				if len(point.Position3D) != 0 && len(point.Position3D) != 3 {
					r.fail(join(pp, ft(0x300a02d4)), "patient position needs three components")
				}
				if len(point.Orientation) != 0 {
					sum := 0.0
					for _, x := range point.Orientation {
						sum += x * x
					}
					if len(point.Orientation) != 3 || math.Abs(sum-1) > 1e-5 {
						r.fail(join(pp, ft(0x300a0412)), "orientation must be a unit vector")
					}
				}
				channel.ControlPoints = append(channel.ControlPoints, point)
			}
			if len(channel.ControlPoints) > 0 && channel.ControlPoints[len(channel.ControlPoints)-1].CumulativeTimeWeight.Value == nil {
				r.fail(join(cp, ft(0x300a02d6)), "last time weight is empty in admitted weighted profile")
			}
			if previous != nil && (channel.FinalCumulativeTimeWeight.Value == nil || *previous != *channel.FinalCumulativeTimeWeight.Value) {
				r.fail(join(cp, ft(0x300a02c8)), "final time weight is inconsistent")
			}
			if channel.TotalTime.Value != nil && *channel.TotalTime.Value > 0 && (channel.FinalCumulativeTimeWeight.Value == nil || *channel.FinalCumulativeTimeWeight.Value <= 0) {
				r.fail(join(cp, ft(0x300a02c8)), "positive channel time needs positive final weight")
			}
			if channel.Movement == "STEPWISE" {
				if len(channel.ControlPoints)%2 != 0 {
					r.fail(join(cp, ft(0x300a02d0)), "stepwise control points need dwell pairs")
				}
				for k := 0; k+1 < len(channel.ControlPoints); k += 2 {
					if channel.ControlPoints[k].RelativePosition != channel.ControlPoints[k+1].RelativePosition {
						r.fail(item(cp, ft(0x300a02d0), k+1), "dwell pair positions differ")
					}
				}
			}
			s.Channels = append(s.Channels, channel)
		}
		allBeta := len(s.Channels) > 0
		for _, c := range s.Channels {
			allBeta = allBeta && betaSources[c.SourceNumber]
		}
		if allBeta && s.TotalReferenceAirKerma != nil && *s.TotalReferenceAirKerma != 0 {
			r.fail(join(path, tagTotalReferenceAirKerma), "non-gamma application air kerma must be zero")
		}
		s.NumberOfChannels = len(s.Channels)
		p.BrachySetups = append(p.BrachySetups, s)
	}
}
