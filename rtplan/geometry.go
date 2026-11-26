package rtplan

import (
	"errors"
	"fmt"
	"math"
)

// ErrNoAperture reports a control point whose aperture cannot be computed
// (no MLC, or no position for it).
var ErrNoAperture = errors.New("dicom/rtplan: no MLC aperture")

// GantryArc describes the gantry motion of a beam over its control points.
type GantryArc struct {
	Start float64
	Stop  float64
	// Extent is the total gantry rotation in degrees, following each
	// segment's rotation direction (a CW segment from 350 to 10 is 20).
	Extent float64
	// Direction is CW or CC for an arc that turns one way, NONE for a fixed
	// gantry, MIXED when the arc reverses.
	Direction string
}

// Arc returns the gantry motion of the beam. The rotation direction of a
// control point applies to the segment that follows it.
func (b *Beam) Arc() GantryArc {
	if b == nil || len(b.ControlPoints) == 0 {
		return GantryArc{Direction: RotationNone}
	}
	cps := b.ControlPoints
	arc := GantryArc{Start: cps[0].GantryAngle, Stop: cps[len(cps)-1].GantryAngle, Direction: RotationNone}
	for i := 0; i+1 < len(cps); i++ {
		from, to := cps[i].GantryAngle, cps[i+1].GantryAngle
		var step float64
		direction := cps[i].GantryRotationDirection
		switch direction {
		case RotationClockwise:
			step = math.Mod(to-from+360, 360)
		case RotationCounterClockwise:
			step = math.Mod(from-to+360, 360)
		default:
			step = math.Abs(to - from)
			if step > 180 {
				step = 360 - step
			}
		}
		if step == 0 {
			continue
		}
		arc.Extent += step
		switch {
		case direction != RotationClockwise && direction != RotationCounterClockwise:
		case arc.Direction == RotationNone:
			arc.Direction = direction
		case arc.Direction != direction:
			arc.Direction = "MIXED"
		}
	}
	return arc
}

// IsArc reports whether the gantry rotates during the beam.
func (b *Beam) IsArc() bool { return b.Arc().Extent > 0 }

// JawOpening returns the field size (mm) defined by the X and Y jaws
// (symmetric or asymmetric) at a control point, in IEC BEAM LIMITING DEVICE
// coordinates. ok is false when the beam has no jaw on an axis.
func (b *Beam) JawOpening(controlPoint int) (x, y float64, okX, okY bool) {
	if b == nil || controlPoint < 0 || controlPoint >= len(b.ControlPoints) {
		return 0, 0, false, false
	}
	cp := b.ControlPoints[controlPoint]
	x1, x2, okX := jaw(cp, DeviceX, DeviceAsymX)
	y1, y2, okY := jaw(cp, DeviceY, DeviceAsymY)
	return x2 - x1, y2 - y1, okX, okY
}

func jaw(cp ControlPoint, types ...string) (float64, float64, bool) {
	for _, deviceType := range types {
		if d, ok := cp.Device(deviceType); ok && len(d.Positions) == 2 {
			return d.Positions[0], d.Positions[1], true
		}
	}
	return 0, 0, false
}

// ApertureArea returns the open area (mm²) of the MLC at a control point,
// clipped by the jaws when the beam has them: for each leaf pair, the gap
// between the banks times the width of the pair, both restricted to the jaw
// opening. Positions are those of the plan, in IEC BEAM LIMITING DEVICE
// coordinates; no projection to another plane is applied.
func (b *Beam) ApertureArea(controlPoint int) (float64, error) {
	if b == nil || controlPoint < 0 || controlPoint >= len(b.ControlPoints) {
		return 0, fmt.Errorf("%w: control point %d does not exist", ErrNoAperture, controlPoint)
	}
	mlc, ok := b.MLC()
	if !ok {
		return 0, ErrNoAperture
	}
	cp := b.ControlPoints[controlPoint]
	leaves, ok := cp.Device(mlc.Type)
	if !ok || len(leaves.Positions) != 2*mlc.Pairs || len(mlc.LeafBoundaries) != mlc.Pairs+1 {
		return 0, fmt.Errorf("%w: control point %d has no position for %s", ErrNoAperture, controlPoint, mlc.Type)
	}
	// travel is the axis the leaves move along; across holds the boundaries.
	travelTypes, acrossTypes := []string{DeviceX, DeviceAsymX}, []string{DeviceY, DeviceAsymY}
	if mlc.Type == DeviceMLCY {
		travelTypes, acrossTypes = acrossTypes, travelTypes
	}
	travelLow, travelHigh := math.Inf(-1), math.Inf(1)
	if lo, hi, ok := jaw(cp, travelTypes...); ok {
		travelLow, travelHigh = lo, hi
	}
	acrossLow, acrossHigh := math.Inf(-1), math.Inf(1)
	if lo, hi, ok := jaw(cp, acrossTypes...); ok {
		acrossLow, acrossHigh = lo, hi
	}
	area := 0.0
	for k := 0; k < mlc.Pairs; k++ {
		gap := math.Min(leaves.Positions[mlc.Pairs+k], travelHigh) - math.Max(leaves.Positions[k], travelLow)
		width := math.Min(mlc.LeafBoundaries[k+1], acrossHigh) - math.Max(mlc.LeafBoundaries[k], acrossLow)
		if gap > 0 && width > 0 {
			area += gap * width
		}
	}
	return area, nil
}
