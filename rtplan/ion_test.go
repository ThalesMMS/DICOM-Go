package rtplan

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
	"testing"
)

func TestIonStrictRoundTrip(t *testing.T) {
	for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(ts.UID, func(t *testing.T) {
			source := newIonFixture().file(t)
			rep, err := iod.Validate(source.Dataset, iod.Options{CheckValues: true})
			if err != nil || len(rep.Errors()) > 0 {
				t.Fatalf("fixture IOD: %v %+v", err, rep.Errors())
			}
			vrep, _ := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: source.Dataset.Elements()}, validation.Options{Dictionary: std.Dictionary, ByteOrder: source.Dataset.ValueByteOrder()})
			if vrep.Report.HasErrors() {
				t.Fatalf("VRVM %+v", vrep.Report.Findings)
			}
			p, err := ReadWithOptions(context.Background(), source, ReadOptions{StrictIOD: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.IonBeams) != 1 || len(p.Beams) != 0 || p.IonBeams[0].ControlPoints[1].RangeShifterSettings[0].Setting != "101" || p.IonBeams[0].ControlPoints[1].GantryPitchRotationDirection != "NONE" {
				t.Fatalf("model: %+v", p.IonBeams)
			}
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: source, TransferSyntax: ts})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err = object.WriteFile(&buf, out); err != nil {
				t.Fatal(err)
			}
			again, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			q, err := ReadWithOptions(context.Background(), again, ReadOptions{StrictIOD: true})
			if err != nil {
				t.Fatal(err)
			}
			if !semanticEqual(p, q) {
				t.Fatal("typed state changed")
			}
			beams, _ := again.Dataset.GetSequence(ft(0x300a03a2))
			snouts, _ := beams[0].GetSequence(ft(0x300a030c))
			if v, _ := snouts[0].GetString(ft(0x300a00f9)); v != "OPAQUE_CODE" {
				t.Fatal("opaque semantic item attribute lost")
			}
			// IEEE FL sample oracle is independent of the reader/writer model comparison.
			cp := q.IonBeams[0].ControlPoints[0]
			if cp.SpotPositions[0] != -10 || cp.SpotPositions[3] != 3 || cp.SpotWeights[0] != .25 || cp.SpotWeights[1] != .75 {
				t.Fatal("FL spots changed")
			}
			cp.SpotPositions[0] = 999
			if p.IonBeams[0].ControlPoints[0].SpotPositions[0] != -10 {
				t.Fatal("returned model aliases prior model")
			}
		})
	}
}
func TestIonNegativeRelationships(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*object.Object)
	}{
		{"species", func(b *object.Object) { b.Put(derivedio.CS(tagRadiationType, "ION")) }},
		{"count", func(b *object.Object) { b.Put(derivedio.IS(tagNumberOfControlPoints, 3)) }},
		{"sum", func(b *object.Object) {
			c, _ := b.GetSequence(ft(0x300a03a8))
			c[0].Put(familyNumeric(ft(0x300a0396), .25, .5))
			putSequence(b, ft(0x300a03a8), c)
		}},
		{"map", func(b *object.Object) {
			c, _ := b.GetSequence(ft(0x300a03a8))
			c[0].Put(familyNumeric(ft(0x300a0394), 1, 2))
			putSequence(b, ft(0x300a03a8), c)
		}},
		{"reference", func(b *object.Object) {
			c, _ := b.GetSequence(ft(0x300a03a8))
			s, _ := c[0].GetSequence(ft(0x300a0360))
			s[0].Put(derivedio.IS(ft(0x300c0100), 99))
			putSequence(c[0], ft(0x300a0360), s)
			putSequence(b, ft(0x300a03a8), c)
		}},
		{"missingType2", func(b *object.Object) {
			c, _ := b.GetSequence(ft(0x300a03a8))
			c[0].Remove(ft(0x300a014a))
			putSequence(b, ft(0x300a03a8), c)
		}},
		{"duplicateDevice", func(b *object.Object) {
			d, _ := b.GetSequence(ft(0x300a03a4))
			putSequence(b, ft(0x300a03a4), []*object.Object{d[0], d[0]})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newIonFixture().dataset(t)
			b, _ := o.GetSequence(ft(0x300a03a2))
			tc.edit(b[0])
			putSequence(o, ft(0x300a03a2), b)
			p, err := ReadDataset(o)
			if p != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("%v %v", p, err)
			}
		})
	}
}
func TestIonCarbonChargeSS(t *testing.T) {
	f := newIonFixture()
	o := f.dataset(t)
	beams, _ := o.GetSequence(ft(0x300a03a2))
	b := beams[0]
	b.Put(derivedio.CS(tagRadiationType, "ION"))
	b.Put(derivedio.CS(ft(0x300a0308), "NONE"))
	b.Put(derivedio.IS(ft(0x300a0302), 12))
	b.Put(derivedio.IS(ft(0x300a0304), 6))
	b.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x300a0306), VR: core.VRSS}, Value: core.Int16Value{6}})
	cp, _ := b.GetSequence(ft(0x300a03a8))
	for _, c := range cp {
		for _, v := range []uint32{0x300a0390, 0x300a0392, 0x300a0394, 0x300a0396, 0x300a039a} {
			c.Remove(ft(v))
		}
	}
	putSequence(b, ft(0x300a03a8), cp)
	putSequence(o, ft(0x300a03a2), beams)
	source, err := derivedio.File(RTIonPlanStorage, testPlanUID, o)
	if err != nil {
		t.Fatal(err)
	}
	vrep, _ := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: source.Dataset.Elements()}, validation.Options{Dictionary: std.Dictionary, ByteOrder: source.Dataset.ValueByteOrder()})
	if vrep.Report.HasErrors() {
		t.Fatalf("VRVM %+v", vrep.Report.Findings)
	}
	p, err := ReadWithOptions(context.Background(), source, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	if *p.IonBeams[0].ChargeState.Value != 6 {
		t.Fatal("SS charge")
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: source})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := out.Dataset.GetSequence(ft(0x300a03a2))
	e, _ := b2[0].Get(ft(0x300a0306))
	if e.VR() != core.VRSS {
		t.Fatal("charge VR")
	}
}

func TestIonToleranceReferencesAndControlStateDomains(t *testing.T) {
	f := newIonFixture().file(t)
	table := derivedio.Object(derivedio.IS(tagToleranceTableNumber, 1), derivedio.SH(tagToleranceTableLabel, "SYNTH_TOL"), derivedio.DS(tagGantryAngleTolerance, 1), familyNumeric(ft(0x300a004b), 2))
	putSequence(f.Dataset, ft(0x300a03a0), []*object.Object{table})
	b, _ := f.Dataset.GetSequence(ft(0x300a03a2))
	b[0].Put(derivedio.IS(tagReferencedToleranceTableNumber, 1))
	putSequence(f.Dataset, ft(0x300a03a2), b)
	f = rsReparse(t, f)
	p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ToleranceTables) != 1 || p.ToleranceTables[0].Number != 1 {
		t.Fatal("Ion tolerance sequence ignored")
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: f})
	if err != nil {
		t.Fatal(err)
	}
	if out.Dataset.Has(tagToleranceTableSequence) {
		t.Fatal("classic tolerance tag substituted")
	}
	tt, _ := out.Dataset.GetSequence(ft(0x300a03a0))
	if v, _ := tt[0].GetFloats(ft(0x300a004b)); len(v) != 1 || v[0] != 2 {
		t.Fatal("opaque FL Ion tolerance lost")
	}
	for _, edit := range []func(*object.Object){func(c *object.Object) { c.Put(derivedio.DS(tagGantryAngle, 360)) }, func(c *object.Object) {
		putSequence(c, tagReferencedDoseReferenceSeq, []*object.Object{derivedio.Object(derivedio.IS(tagReferencedDoseReferenceNumber, 99))})
	}} {
		f := newIonFixture().file(t)
		b, _ := f.Dataset.GetSequence(ft(0x300a03a2))
		cp, _ := b[0].GetSequence(ft(0x300a03a8))
		edit(cp[0])
		putSequence(b[0], ft(0x300a03a8), cp)
		putSequence(f.Dataset, ft(0x300a03a2), b)
		if p, err := Read(f); p != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("invalid Ion state accepted %v %v", p, err)
		}
	}
}
