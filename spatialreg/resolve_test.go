package spatialreg

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func requireResolutionFailure(t *testing.T, err error, reason ResolutionReason, count int) {
	t.Helper()
	var failure *ResolutionError
	if !errors.As(err, &failure) || failure.Reason != reason || failure.Count != count {
		t.Fatalf("resolution error = %v; want %s with count %d", err, reason, count)
	}
}

func TestResolveFramesIdentityAndMissingUIDs(t *testing.T) {
	resolution, err := ResolveFrames(" 1.2.3 ", "1.2.3", nil)
	point := render.Vec3{X: 12, Y: 34, Z: 56}
	if err != nil || !resolution.SameFrame() || len(resolution.Hops()) != 0 {
		t.Fatalf("identity = %+v, %v", resolution, err)
	}
	if got, err := resolution.Map(point); err != nil || got != point {
		t.Fatalf("identity point = %v, %v; want %v", got, err, point)
	}
	if matrix, ok := resolution.Matrix(); !ok || matrix != identityAffine() {
		t.Fatalf("identity matrix = %v, %t", matrix, ok)
	}
	if _, err := resolution.Map(render.Vec3{X: math.NaN()}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("identity accepted nonfinite point: %v", err)
	}
	_, err = ResolveFrames(" ", "", nil)
	requireResolutionFailure(t, err, ResolutionMissingFrame, 0)
	_, err = ResolveFrames("1.2.3", " ", nil)
	requireResolutionFailure(t, err, ResolutionMissingFrame, 0)
}

func TestResolveFramesSelectsUniquePathAndPreservesProvenance(t *testing.T) {
	aToB := RegistrationCandidate{Affine: chainAffine(t, "1.2.1", "B", "A", TransformRigid, chainRigid(0, 10, 0, 0))}
	cToB := RegistrationCandidate{Affine: chainAffine(t, "1.2.2", "B", "C", TransformRigid, chainRigid(0, 0, 5, 0))}
	aToC := RegistrationCandidate{SOPInstanceUID: "1.2.3", Affine: chainAffine(t, "", "C", "A", TransformRigid, chainRigid(0, 7, 0, 0))}
	point := render.Vec3{X: 1, Y: 2, Z: 3}

	resolution, err := ResolveFrames("A", "C", []RegistrationCandidate{cToB, aToB})
	if err != nil {
		t.Fatal(err)
	}
	hops := resolution.Hops()
	if len(hops) != 2 || hops[0].SOPInstanceUID != "1.2.1" || !hops[0].SourceToRegistered ||
		hops[1].SOPInstanceUID != "1.2.2" || hops[1].SourceToRegistered {
		t.Fatalf("ordered hops = %+v", hops)
	}
	want := render.Vec3{X: 11, Y: -3, Z: 3}
	if got, err := resolution.Map(point); err != nil || got != want {
		t.Fatalf("two-hop mapping = %v, %v; want %v", got, err, want)
	}
	if matrix, ok := resolution.Matrix(); !ok || matrix.TransformPoint(point) != want || resolution.Type() != TransformRigid {
		t.Fatalf("two-hop matrix = %v, %t, type %s", matrix, ok, resolution.Type())
	}

	hops[0].SOPInstanceUID = "overwritten"
	if got := resolution.Hops()[0].SOPInstanceUID; got != "1.2.1" {
		t.Fatalf("returned provenance aliases selection: %s", got)
	}
	resolution, err = ResolveFrames("A", "C", []RegistrationCandidate{cToB, aToB, aToC})
	hops = resolution.Hops()
	if err != nil || len(hops) != 1 || hops[0].SOPInstanceUID != "1.2.3" {
		t.Fatalf("direct precedence = %+v, %v", hops, err)
	}
	if got, err := resolution.Map(point); err != nil || got != (render.Vec3{X: 8, Y: 2, Z: 3}) {
		t.Fatalf("direct mapping = %v, %v", got, err)
	}

	aToC2 := RegistrationCandidate{Affine: chainAffine(t, "1.2.4", "C", "A", TransformRigid, chainRigid(0, 8, 0, 0))}
	_, err = ResolveFrames("A", "C", []RegistrationCandidate{aToB, cToB, aToC, aToC2})
	requireResolutionFailure(t, err, ResolutionAmbiguousDirect, 2)
	aToB2 := RegistrationCandidate{Affine: chainAffine(t, "1.2.5", "B", "A", TransformRigid, chainRigid(0, 9, 0, 0))}
	_, err = ResolveFrames("A", "C", []RegistrationCandidate{aToB, cToB, aToB2})
	requireResolutionFailure(t, err, ResolutionAmbiguousChains, 2)
}

func TestResolveFramesChecksCyclesAndIncompleteCandidates(t *testing.T) {
	aToB := RegistrationCandidate{Affine: chainAffine(t, "1.2.1", "B", "A", TransformRigid, identityAffine())}
	bToA := RegistrationCandidate{Affine: chainAffine(t, "1.2.2", "A", "B", TransformRigid, identityAffine())}
	bToC := RegistrationCandidate{Affine: chainAffine(t, "1.2.3", "C", "B", TransformRigid, identityAffine())}
	_, err := ResolveFrames("A", "C", []RegistrationCandidate{aToB, bToA, bToC})
	requireResolutionFailure(t, err, ResolutionCycle, 0)

	xToY := RegistrationCandidate{Affine: chainAffine(t, "1.2.4", "Y", "X", TransformRigid, identityAffine())}
	yToX := RegistrationCandidate{Affine: chainAffine(t, "1.2.5", "X", "Y", TransformRigid, identityAffine())}
	if _, err := ResolveFrames("A", "B", []RegistrationCandidate{aToB, xToY, yToX}); err != nil {
		t.Fatalf("unrelated cycle refused a usable direct mapping: %v", err)
	}
	_, err = ResolveFrames("C", "D", []RegistrationCandidate{aToB})
	requireResolutionFailure(t, err, ResolutionNoLink, 0)
	incomplete := RegistrationCandidate{Affine: chainAffine(t, "1.2.6", "C", "", TransformRigid, identityAffine())}
	_, err = ResolveFrames("C", "D", []RegistrationCandidate{aToB, incomplete})
	requireResolutionFailure(t, err, ResolutionMissingSourceFrame, 0)
}

func TestResolveFramesDeformableInverseIsDirectOnly(t *testing.T) {
	vectors := make([]render.Vec3, 27)
	for i := range vectors {
		vectors[i].X = 2
	}
	grid, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 10, Y: 10, Z: 10}, [3]uint32{3, 3, 3}, vectors)
	if err != nil {
		t.Fatal(err)
	}
	identity := Matrix{Values: identityAffine()}
	deformable := &Object{SOPInstanceUID: "1.2.1", RegisteredFrameOfReferenceUID: "A", Registrations: []Registration{
		{SourceFrameOfReferenceUID: "B", Pre: identity, Post: identity, Grid: grid},
	}}
	bToC := chainAffine(t, "1.2.2", "C", "B", TransformRigid, chainRigid(0, 0, 3, 0))
	candidates := []RegistrationCandidate{{Deformable: deformable}, {Affine: bToC}}

	reverse, err := ResolveFrames("B", "A", candidates)
	if deform, inverse := reverse.DirectDeformation(); err != nil || deform != &deformable.Registrations[0] || !inverse {
		t.Fatalf("inverse selection = %+v, %v", reverse, err)
	}
	if mapped, err := reverse.Map(render.Vec3{X: 12, Y: 10, Z: 10}); err != nil || mapped.Sub(render.Vec3{X: 10, Y: 10, Z: 10}).Length() > 1e-6 {
		t.Fatalf("inverse mapping = %v, %v", mapped, err)
	}
	if _, err := reverse.Map(render.Vec3{X: 500}); !errors.Is(err, ErrOutsideDomain) {
		t.Fatalf("inverse domain error = %v", err)
	}

	forward, err := ResolveFrames("A", "C", candidates)
	if err != nil || forward.Type() != TransformDeformable {
		t.Fatalf("forward chain = %+v, %v", forward, err)
	}
	if _, ok := forward.Matrix(); ok {
		t.Fatal("deformable chain returned a matrix")
	}
	if _, err := forward.Map(render.Vec3{X: 500}); !errors.Is(err, ErrOutsideDomain) || !errors.Is(err, ErrChain) {
		t.Fatalf("chain domain error = %v", err)
	}
	_, err = ResolveFrames("C", "A", candidates)
	requireResolutionFailure(t, err, ResolutionNoLink, 0)
}
