package rtplan

import (
	"context"
	"fmt"
	"math"
	"reflect"
)

// ApertureCoordinates is the geometric contract of conventional aperture
// inspection. No IEC-to-patient transform or anatomical projection is applied.
const ApertureCoordinates = "IEC BEAM LIMITING DEVICE; machine isocentric plane; mm"

// JawGeometry is one stored pair of opposing jaw positions on an IEC axis.
type JawGeometry struct {
	DeviceType string
	Axis       int // 0 is X, 1 is Y
	Positions  [2]float64
}

// LeafOpening is one leaf-pair gap, clipped by available jaws. Closed gaps
// remain present with Open false; Pair is the one-based IEC leaf-pair number.
type LeafOpening struct {
	DeviceType string
	Pair       int
	Min, Max   [2]float64
	Open       bool
}

// FractionMeterset retains the beam's stored meterset for a fraction group.
// It is not calculated from the control point's normalized weight.
type FractionMeterset struct {
	GroupNumber int
	Meterset    *float64
	Unit        string
}

// ApertureGeometry retains declared device positions in State and separately
// exposes derived leaf openings and area. Blocks remain individual closed
// contours, with APERTURE/SHIELDING semantics; no composite block area is inferred.
type ApertureGeometry struct {
	BeamNumber        int
	BeamName          string
	ControlPointCount int
	State             ControlPoint
	Jaws              []JawGeometry
	Leaves            []LeafOpening
	Blocks            []Block
	DerivedArea       *float64 // mm², excluding block contours/transmission
	AreaBasis         string
	Limitations       []string
	FractionMetersets []FractionMeterset
}

// InspectAperture validates an admitted model off the UI thread and returns
// owned geometry for exactly one conventional control point. Missing values
// remain unavailable; only the reader's normative CP inheritance is retained.
func (p *Plan) InspectAperture(ctx context.Context, beamNumber, controlPoint int, options ...Limits) (*ApertureGeometry, error) {
	if ctx == nil || p == nil || controlPoint < 0 {
		return nil, ErrInvalidObject
	}
	l, err := inspectionLimits(options)
	if err != nil {
		return nil, err
	}
	if err := admitModel(ctx, reflect.ValueOf(p), l); err != nil {
		return nil, err
	}
	r := &reader{ctx: ctx, maxProblems: l.MaxProblems}
	validatePlan(r, p)
	if len(r.errors) > 0 {
		return nil, &ValidationError{Problems: r.errors}
	}
	for i := range p.Beams {
		b := &p.Beams[i]
		if b.Number != beamNumber {
			continue
		}
		if controlPoint >= len(b.ControlPoints) {
			return nil, fmt.Errorf("%w: control point does not exist", ErrInvalidObject)
		}
		cp := b.ControlPoints[controlPoint]
		g := &ApertureGeometry{BeamNumber: b.Number, BeamName: b.Name, ControlPointCount: len(b.ControlPoints), State: ownControlPoint(cp)}
		for _, group := range p.FractionGroups {
			for _, ref := range group.ReferencedBeams {
				if ref.BeamNumber == b.Number {
					g.FractionMetersets = append(g.FractionMetersets, FractionMeterset{group.Number, ownedFloat(ref.Meterset), b.PrimaryDosimeterUnit})
				}
			}
		}
		for _, device := range b.Devices {
			position, ok := cp.Device(device.Type)
			if !ok || device.Pairs < 1 || len(position.Positions) != 2*device.Pairs {
				return nil, fmt.Errorf("%w: selected control point has incomplete device positions", ErrInvalidObject)
			}
			for k := 0; k < device.Pairs; k++ {
				if position.Positions[k] > position.Positions[device.Pairs+k]+positionTolerance {
					return nil, fmt.Errorf("%w: selected leaf/jaw banks cross", ErrInvalidObject)
				}
			}
			if !device.IsMLC() {
				axis := 0
				if device.Type == DeviceY || device.Type == DeviceAsymY {
					axis = 1
				}
				g.Jaws = append(g.Jaws, JawGeometry{device.Type, axis, [2]float64{position.Positions[0], position.Positions[1]}})
			}
		}
		xLow, xHigh, xOK := jaw(cp, DeviceX, DeviceAsymX)
		yLow, yHigh, yOK := jaw(cp, DeviceY, DeviceAsymY)
		mlcs := 0
		for _, device := range b.Devices {
			if !device.IsMLC() {
				continue
			}
			mlcs++
			position, _ := cp.Device(device.Type)
			for k := 0; k < device.Pairs; k++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				gap := LeafOpening{DeviceType: device.Type, Pair: k + 1, Min: [2]float64{position.Positions[k], device.LeafBoundaries[k]}, Max: [2]float64{position.Positions[device.Pairs+k], device.LeafBoundaries[k+1]}}
				if device.Type == DeviceMLCY {
					gap.Min[0], gap.Min[1] = gap.Min[1], gap.Min[0]
					gap.Max[0], gap.Max[1] = gap.Max[1], gap.Max[0]
				}
				if xOK {
					gap.Min[0], gap.Max[0] = math.Max(gap.Min[0], xLow), math.Min(gap.Max[0], xHigh)
				}
				if yOK {
					gap.Min[1], gap.Max[1] = math.Max(gap.Min[1], yLow), math.Min(gap.Max[1], yHigh)
				}
				gap.Open = gap.Max[0] > gap.Min[0] && gap.Max[1] > gap.Min[1]
				g.Leaves = append(g.Leaves, gap)
			}
		}
		if mlcs == 1 {
			area, err := b.ApertureArea(controlPoint)
			if err != nil || !finite(area) {
				return nil, fmt.Errorf("%w: derived MLC area is unavailable or non-finite", ErrInvalidObject)
			}
			g.DerivedArea, g.AreaBasis = &area, "MLC clipped by available jaws, excluding blocks"
		} else if mlcs == 0 && xOK && yOK {
			area := (xHigh - xLow) * (yHigh - yLow)
			if !finite(area) {
				return nil, fmt.Errorf("%w: derived jaw area is non-finite", ErrInvalidObject)
			}
			g.DerivedArea, g.AreaBasis = &area, "jaws only, excluding blocks"
		} else {
			g.Limitations = append(g.Limitations, "Combined aperture area unavailable for this device arrangement.")
		}
		for _, block := range b.Blocks {
			block.Contour = append([][2]float64(nil), block.Contour...)
			block.Thickness, block.Transmission = ownedFloat(block.Thickness), ownedFloat(block.Transmission)
			g.Blocks = append(g.Blocks, block)
			if len(block.Contour) == 0 {
				g.Limitations = append(g.Limitations, fmt.Sprintf("Block %d outline unavailable.", block.Number))
			}
		}
		if len(b.Blocks) > 0 {
			g.Limitations = append(g.Limitations, "Block contours are declared outlines; composite aperture, transmission and block-corrected area are not calculated.")
		}
		if !xOK || !yOK {
			g.Limitations = append(g.Limitations, "One or both jaw axes are unavailable; available device geometry only.")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return g, nil
	}
	return nil, fmt.Errorf("%w: conventional beam does not exist", ErrInvalidObject)
}
