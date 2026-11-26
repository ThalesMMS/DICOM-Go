package spatialreg

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func chainAffine(t *testing.T, sop, registered, source string, kind TransformType, sourceToRegistered render.GeometryAffine) *AffineObject {
	t.Helper()
	if err := ValidateMatrix(sourceToRegistered, kind); err != nil {
		t.Fatalf("%s matrix: %v", kind, err)
	}
	inverse, _, err := inverseAffine(sourceToRegistered)
	if err != nil {
		t.Fatal(err)
	}
	return &AffineObject{SOPInstanceUID: sop, RegisteredFrameOfReferenceUID: registered, Registrations: []AffineRegistration{
		{SourceFrameOfReferenceUID: registered, Type: TransformRigid, SourceToRegistered: identityAffine(), RegisteredToSource: identityAffine()},
		{SourceFrameOfReferenceUID: source, Type: kind, SourceToRegistered: sourceToRegistered, RegisteredToSource: inverse},
	}}
}

func chainRigid(degrees float64, tx, ty, tz float64) render.GeometryAffine {
	c, s := math.Cos(degrees*math.Pi/180), math.Sin(degrees*math.Pi/180)
	return render.GeometryAffine{c, -s, 0, tx, s, c, 0, ty, 0, 0, 1, tz, 0, 0, 0, 1}
}

func chainClose(a, b render.Vec3) bool { return a.Sub(b).Length() < 1e-9 }

// TestChainComposesTwoAffineHops: prior CT (A) is registered to the current
// CT (B) and the current MR (C) to the current CT. The chain from A to C
// goes through B, maps exactly as the two registrations in turn, multiplies
// their matrices, reports both SOP Instance UIDs, and runs back.
func TestChainComposesTwoAffineHops(t *testing.T) {
	// Registered B, source A: rigid.
	priorToCurrent := chainAffine(t, "1.2.3.1", "B", "A", TransformRigid, chainRigid(10, 5, -3, 2))
	// Registered B, source C: affine.
	mrToCurrent := chainAffine(t, "1.2.3.2", "B", "C", TransformAffine, render.GeometryAffine{1.1, 0.05, 0, -4, 0, 0.9, 0.02, 6, 0, 0, 1.05, 1, 0, 0, 0, 1})
	hops := Hops([]*AffineObject{priorToCurrent, mrToCurrent}, nil)
	if len(hops) != 4 {
		t.Fatalf("hops %d, want 4 (identity items skipped)", len(hops))
	}
	chains := FindChains(hops, "A", "C", 0)
	if len(chains) != 1 || len(chains[0].Hops) != 2 {
		t.Fatalf("chains A to C %v", chains)
	}
	chain := chains[0]
	if chain.From() != "A" || chain.To() != "C" || chain.Type() != TransformAffine {
		t.Fatalf("chain %s: %s to %s, type %s", chain, chain.From(), chain.To(), chain.Type())
	}
	if uids := chain.SOPInstanceUIDs(); len(uids) != 2 || uids[0] != "1.2.3.1" || uids[1] != "1.2.3.2" {
		t.Fatalf("UIDs %v", uids)
	}
	if got := chain.String(); got != "RIGID inverse (SOP 1.2.3.1) then AFFINE (SOP 1.2.3.2)" {
		t.Fatalf("String %q", got)
	}
	matrix, ok := chain.Matrix()
	if !ok {
		t.Fatal("affine chain has no matrix")
	}
	back := FindChains(hops, "C", "A", 2)
	if len(back) != 1 {
		t.Fatalf("chains C to A %v", back)
	}
	for _, p := range []render.Vec3{{}, {X: 12, Y: -30, Z: 7}, {X: -80, Y: 44, Z: 120}} {
		inB, err := priorToCurrent.Registrations[1].MapSourceToRegistered(p)
		if err != nil {
			t.Fatal(err)
		}
		want, err := mrToCurrent.Registrations[1].MapRegisteredToSource(inB)
		if err != nil {
			t.Fatal(err)
		}
		got, err := chain.Map(p)
		if err != nil || !chainClose(got, want) || !chainClose(matrix.TransformPoint(p), want) {
			t.Fatalf("A %v: chain %v (%v), matrix %v, want %v", p, got, err, matrix.TransformPoint(p), want)
		}
		round, err := back[0].Map(got)
		if err != nil || !chainClose(round, p) {
			t.Fatalf("round trip of %v: %v (%v)", p, round, err)
		}
	}
	// Two rigid hops compose to a rigid mapping.
	rigidToo := chainAffine(t, "1.2.3.3", "B", "D", TransformRigid, chainRigid(-25, 1, 2, 3))
	rigidChains := FindChains(Hops([]*AffineObject{priorToCurrent, rigidToo}, nil), "A", "D", 2)
	if len(rigidChains) != 1 || rigidChains[0].Type() != TransformRigid {
		t.Fatalf("rigid chains %v", rigidChains)
	}
}

// TestChainUsesDeformableRegistrationsOnlyRegisteredToSource: a deformable
// registration joins a chain in its stored direction only.
func TestChainUsesDeformableRegistrationsOnlyRegisteredToSource(t *testing.T) {
	shift := render.Vec3{X: 1.5, Y: -0.5, Z: 0.25}
	vectors := make([]render.Vec3, 5*5*5)
	for index := range vectors {
		vectors[index] = shift
	}
	grid, err := NewGrid(render.Vec3{X: -20, Y: -20, Z: -20}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 10, Y: 10, Z: 10}, [3]uint32{5, 5, 5}, vectors)
	if err != nil {
		t.Fatal(err)
	}
	identity := Matrix{Values: identityAffine()}
	deformable := &Object{SOPInstanceUID: "1.2.3.10", RegisteredFrameOfReferenceUID: "A", Registrations: []Registration{
		{SourceFrameOfReferenceUID: "B", Pre: identity, Post: identity, Grid: grid},
	}}
	affine := chainAffine(t, "1.2.3.11", "B", "C", TransformRigid, chainRigid(5, 0, 0, 10))
	hops := Hops([]*AffineObject{affine}, []*Object{deformable})
	chains := FindChains(hops, "A", "C", 2)
	if len(chains) != 1 || chains[0].Type() != TransformDeformable {
		t.Fatalf("chains A to C %v", chains)
	}
	if _, ok := chains[0].Matrix(); ok {
		t.Fatal("a deformable chain has a matrix")
	}
	p := render.Vec3{X: 3, Y: 4, Z: -5}
	want, err := affine.Registrations[1].MapRegisteredToSource(p.Add(shift))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := chains[0].Map(p); err != nil || !chainClose(got, want) {
		t.Fatalf("mapped %v (%v), want %v", got, err, want)
	}
	if back := FindChains(hops, "C", "A", 2); len(back) != 0 {
		t.Fatalf("the reverse needs the deformable inverse: %v", back)
	}
	// Outside the grid, the chain fails with the hop's error.
	if _, err := chains[0].Map(render.Vec3{X: 500}); !errors.Is(err, ErrChain) || !errors.Is(err, ErrOutsideDomain) {
		t.Fatalf("outside the grid: %v", err)
	}
}

// TestFindChainsReportsEveryPath: several paths are all returned, shortest
// first, so that callers can refuse the ambiguity.
func TestFindChainsReportsEveryPath(t *testing.T) {
	ab := chainAffine(t, "1.2.3.20", "B", "A", TransformRigid, chainRigid(3, 0, 0, 0))
	ab2 := chainAffine(t, "1.2.3.21", "B", "A", TransformRigid, chainRigid(4, 0, 0, 0))
	bc := chainAffine(t, "1.2.3.22", "B", "C", TransformRigid, chainRigid(5, 0, 0, 0))
	ac := chainAffine(t, "1.2.3.23", "C", "A", TransformRigid, chainRigid(8, 0, 0, 0))
	if chains := FindChains(Hops([]*AffineObject{ab, ab2, bc}, nil), "A", "C", 2); len(chains) != 2 {
		t.Fatalf("two registrations of A to B: %d chains, want 2", len(chains))
	}
	chains := FindChains(Hops([]*AffineObject{ab, bc, ac}, nil), "A", "C", 2)
	if len(chains) != 2 || len(chains[0].Hops) != 1 || len(chains[1].Hops) != 2 {
		t.Fatalf("direct and chained: %v", chains)
	}
	if chains := FindChains(Hops([]*AffineObject{ab, bc}, nil), "A", "C", 1); len(chains) != 0 {
		t.Fatalf("one hop allowed: %v", chains)
	}
	if chains := FindChains(Hops([]*AffineObject{ab, bc}, nil), "A", "A", 2); len(chains) != 0 {
		t.Fatalf("same frame: %v", chains)
	}
}

func TestNewChainRejectsBrokenChains(t *testing.T) {
	hops := Hops([]*AffineObject{
		chainAffine(t, "1.2.3.30", "B", "A", TransformRigid, chainRigid(3, 0, 0, 0)),
		chainAffine(t, "1.2.3.31", "B", "C", TransformRigid, chainRigid(5, 0, 0, 0)),
	}, nil)
	byRoute := func(from, to string) Hop {
		for _, hop := range hops {
			if hop.From == from && hop.To == to {
				return hop
			}
		}
		t.Fatalf("no hop %s to %s", from, to)
		return Hop{}
	}
	if chain, err := NewChain(byRoute("A", "B"), byRoute("B", "C")); err != nil || chain.To() != "C" {
		t.Fatalf("valid chain: %v", err)
	}
	for name, broken := range map[string][]Hop{
		"empty":        nil,
		"disconnected": {byRoute("A", "B"), byRoute("C", "B")},
		"revisits":     {byRoute("A", "B"), byRoute("B", "A")},
		"no transform": {{From: "A", To: "B"}},
	} {
		if _, err := NewChain(broken...); !errors.Is(err, ErrChain) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (Chain{}).Map(render.Vec3{}); !errors.Is(err, ErrChain) {
		t.Fatalf("empty chain maps: %v", err)
	}
}
