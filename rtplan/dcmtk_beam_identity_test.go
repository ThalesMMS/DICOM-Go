package rtplan

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/derivedio"
)

// DCMTK dcmrt/tests/tsearch.cc looks up sparse Beam Numbers in sequence
// order. Give each beam distinct geometry and meterset to verify that the
// lookup also binds the correct fraction reference and control point.
func TestDCMTKReferenceRTPlanBeamIdentity(t *testing.T) {
	fixture := newPlanFixture()
	fixture.beams[0].attrs.set(derivedio.IS(tagBeamNumber, 42))
	fixture.beams[1].attrs.set(derivedio.IS(tagBeamNumber, 1337))
	third := newPlanFixture().beams[0]
	third.attrs.set(derivedio.IS(tagBeamNumber, 21), derivedio.LO(tagBeamName, "LATERAL"))
	third.cps[0].attrs.set(derivedio.DS(tagGantryAngle, 65))
	fixture.beams = append(fixture.beams, third)

	group := &fixture.fractionGroups[0]
	group.attrs.set(derivedio.IS(tagNumberOfBeams, 3), derivedio.IS(tagFractionGroupNumber, 8))
	group.beams[0].set(derivedio.IS(tagReferencedBeamNumber, 42))
	group.beams[1].set(derivedio.IS(tagReferencedBeamNumber, 1337))
	group.beams = append([]elems{newElems(
		derivedio.IS(tagReferencedBeamNumber, 21), derivedio.DS(tagBeamMeterset, 73),
	)}, group.beams...)

	plan, err := Read(fixture.file(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		beamNumber  int
		control     int
		controlSize int
		gantry      float64
		meterset    float64
	}{
		{name: "ARC", beamNumber: 1337, control: 1, controlSize: 3, gantry: 90, meterset: 250},
		{name: "AP", beamNumber: 42, control: 1, controlSize: 2, gantry: 0, meterset: 100.5},
		{name: "LATERAL", beamNumber: 21, control: 1, controlSize: 2, gantry: 65, meterset: 73},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beam, ok := plan.Beam(tc.beamNumber)
			if !ok || beam == nil || beam.Number != tc.beamNumber || beam.Name != tc.name {
				t.Fatalf("Beam(%d) = %+v, %t, want beam %s", tc.beamNumber, beam, ok, tc.name)
			}
			aperture, err := plan.InspectAperture(context.Background(), tc.beamNumber, tc.control)
			if err != nil {
				t.Fatal(err)
			}
			if aperture.BeamNumber != tc.beamNumber || aperture.BeamName != tc.name ||
				aperture.ControlPointCount != tc.controlSize || aperture.State.GantryAngle != tc.gantry {
				t.Fatalf("InspectAperture(%d, %d) selected %+v, want %s at %g degrees with %d control points",
					tc.beamNumber, tc.control, aperture, tc.name, tc.gantry, tc.controlSize)
			}
			meterset := aperture.FractionMetersets
			if len(meterset) != 1 || meterset[0].GroupNumber != 8 || meterset[0].Unit != "MU" ||
				meterset[0].Meterset == nil || *meterset[0].Meterset != tc.meterset {
				t.Fatalf("fraction binding = %+v, want group 8 with %g MU", meterset, tc.meterset)
			}
		})
	}
	t.Run("absent beam", func(t *testing.T) {
		if beam, ok := plan.Beam(100); beam != nil || ok {
			t.Fatalf("Beam(100) = %+v, %t, want nil, false", beam, ok)
		}
		if aperture, err := plan.InspectAperture(context.Background(), 100, 0); aperture != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("InspectAperture for absent beam = %+v, %v", aperture, err)
		}
	})
}
