package rtplan

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
	"math"
	"testing"
)

func TestBrachyFullIODAndRoundTrip(t *testing.T) {
	source := newBrachyFixture().file(t)
	rep, err := iod.Validate(source.Dataset, iod.Options{CheckValues: true})
	if err != nil || len(rep.Errors()) != 0 {
		t.Fatalf("IOD %v %v", err, rep.Errors())
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			p, err := ReadWithOptions(context.Background(), source, ReadOptions{StrictIOD: true})
			if err != nil {
				t.Fatal(err)
			}
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: source, TransferSyntax: syntax})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := object.WriteFile(&buf, out); err != nil {
				t.Fatal(err)
			}
			read, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			actual, err := ReadWithOptions(context.Background(), read, ReadOptions{StrictIOD: true})
			if err != nil {
				t.Fatal(err)
			}
			if !semanticEqual(p, actual) {
				t.Fatalf("round-trip changed typed values")
			}
			if len(actual.Sources) != 2 || len(actual.BrachySetups) != 2 || len(actual.BrachySetups[1].Channels) != 2 {
				t.Fatal("lost entities")
			}
			actual.BrachySetups[0].Channels[0].ControlPoints[0].Position3D[0] = 99
			p2, err := Read(source)
			if err != nil || p2.BrachySetups[0].Channels[0].ControlPoints[0].Position3D[0] == 99 {
				t.Fatal("source aliased")
			}
		})
	}
}
func TestBrachyErrorsHaveAttributePaths(t *testing.T) {
	source := newBrachyFixture().file(t)
	mutate := func(tag core.Tag, e core.Element) *object.File {
		f, err := object.CloneFile(source)
		if err != nil {
			t.Fatal(err)
		}
		setups, _ := f.Dataset.GetSequence(tagApplicationSetupSequence)
		channels, _ := setups[0].GetSequence(tagChannelSequence)
		if tag == ft(0x300a02d2) || tag == tagControlPointIndex {
			points, _ := channels[0].GetSequence(ft(0x300a02d0))
			points[0].Put(e)
			putSequence(channels[0], ft(0x300a02d0), points)
		} else {
			channels[0].Put(e)
		}
		putSequence(setups[0], tagChannelSequence, channels)
		putSequence(f.Dataset, tagApplicationSetupSequence, setups)
		return f
	}
	cases := []struct {
		name string
		tag  core.Tag
		e    core.Element
	}{
		{"missing_source", tagReferencedSourceNumber, core.Element{Header: core.ElementHeader{Tag: tagReferencedSourceNumber, VR: core.VRIS}, Value: core.StringValue{"99"}}},
		{"bad_count", tagNumberOfControlPoints, core.Element{Header: core.ElementHeader{Tag: tagNumberOfControlPoints, VR: core.VRIS}, Value: core.StringValue{"3"}}},
		{"bad_index", tagControlPointIndex, core.Element{Header: core.ElementHeader{Tag: tagControlPointIndex, VR: core.VRIS}, Value: core.StringValue{"1"}}},
		{"nonfinite", ft(0x300a02d2), core.Element{Header: core.ElementHeader{Tag: ft(0x300a02d2), VR: core.VRFD}, Value: core.Float64Value{math.NaN()}}},
		{"bad_final", ft(0x300a02c8), core.Element{Header: core.ElementHeader{Tag: ft(0x300a02c8), VR: core.VRDS}, Value: core.StringValue{"2"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Read(mutate(c.tag, c.e))
			var invalid *ValidationError
			if p != nil || !errors.As(err, &invalid) || len(invalid.Problems) == 0 || invalid.Problems[0].Path == "" {
				t.Fatalf("%v %v", p, err)
			}
		})
	}
}
func TestBrachyWriterRequiresNewUIDAndHonorsCancel(t *testing.T) {
	source := newBrachyFixture().file(t)
	p := familyReparse(t, newBrachyFixture())
	*p.BrachySetups[0].Channels[0].TotalTime.Value = 61
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: source}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("%v %v", out, err)
	}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: source, SOPInstanceUID: "2.25.13290000001"}); err != nil || out == nil {
		t.Fatalf("%v %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := WritePlan(ctx, p, WriteOptions{Template: source}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v %v", out, err)
	}
}

func TestBrachyPDRFixedPresenceAndSemanticItemOwnership(t *testing.T) {
	for _, typ := range []string{"HDR", "PDR", "MDR", "LDR", "MANUAL"} {
		f := newBrachyFixture().file(t)
		f.Dataset.Put(derivedio.CS(tagBrachyTreatmentType, typ))
		ss, _ := f.Dataset.GetSequence(tagApplicationSetupSequence)
		for _, s := range ss {
			cc, _ := s.GetSequence(tagChannelSequence)
			for _, c := range cc {
				c.Put(derivedio.CS(ft(0x300a0288), "FIXED"))
				c.Remove(ft(0x300a02a0))
				if typ == "PDR" {
					c.Put(derivedio.IS(ft(0x300a028a), 5))
					c.Put(derivedio.DS(ft(0x300a028c), 3600))
				}
				cp, _ := c.GetSequence(ft(0x300a02d0))
				for _, p := range cp {
					p.Put(derivedio.Str(ft(0x300a02d4), core.VRDS, ""))
					p.Put(familyNumeric(ft(0x300a0412)))
				}
				putSequence(c, ft(0x300a02d0), cp)
			}
			putSequence(s, tagChannelSequence, cc)
		}
		putSequence(f.Dataset, tagApplicationSetupSequence, ss)
		f = rsReparse(t, f)
		p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
		if err != nil {
			t.Fatal(typ, err)
		}
		c := p.BrachySetups[0].Channels[0]
		if !c.ControlPoints[0].Position3DPresent || !c.ControlPoints[0].OrientationPresent || len(c.ControlPoints[0].Position3D) != 0 {
			t.Fatal("Type2 optional vector presence")
		}
		for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, TransferSyntax: ts})
			if err != nil {
				t.Fatal(typ, err)
			}
			actual, err := ReadWithOptions(context.Background(), rsReparse(t, out), ReadOptions{StrictIOD: true})
			if err != nil || !semanticEqual(p, actual) {
				t.Fatal("fixed/PDR round-trip", typ, err)
			}
		}
	}
	f := newBrachyFixture().file(t)
	ss, _ := f.Dataset.GetSequence(tagApplicationSetupSequence)
	cc, _ := ss[0].GetSequence(tagChannelSequence)
	cc[0].Put(derivedio.LO(ft(0x77771010), "CHANNEL_ONE"))
	putSequence(ss[0], tagChannelSequence, cc)
	putSequence(f.Dataset, tagApplicationSetupSequence, ss)
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	ch := p.BrachySetups[0].Channels
	ch[0], ch[1] = ch[1], ch[0]
	p.BrachySetups[0].Channels = ch
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, SOPInstanceUID: "2.25.1329008101"})
	if err != nil {
		t.Fatal(err)
	}
	ss, _ = out.Dataset.GetSequence(tagApplicationSetupSequence)
	cc, _ = ss[0].GetSequence(tagChannelSequence)
	if v, _ := cc[1].GetString(ft(0x77771010)); v != "CHANNEL_ONE" || cc[0].Has(ft(0x77771010)) {
		t.Fatal("unmodelled channel metadata moved with array position")
	}
}

func TestBrachyBetaStrengthAndKermaDomain(t *testing.T) {
	f := newBrachyFixture().file(t)
	ss, _ := f.Dataset.GetSequence(ft(0x300a0210))
	for _, s := range ss {
		s.Put(derivedio.LO(ft(0x300a0226), "P-32"))
		s.Put(derivedio.DS(ft(0x300a0228), 14.3))
		s.Put(derivedio.CS(ft(0x300a0229), "DOSE_RATE_WATER"))
		s.Put(derivedio.DS(ft(0x300a022a), 0))
		s.Put(derivedio.DS(ft(0x300a022b), 1))
	}
	putSequence(f.Dataset, ft(0x300a0210), ss)
	setups, _ := f.Dataset.GetSequence(tagApplicationSetupSequence)
	for _, s := range setups {
		s.Put(derivedio.DS(tagTotalReferenceAirKerma, 0))
	}
	putSequence(f.Dataset, tagApplicationSetupSequence, setups)
	f = rsReparse(t, f)
	p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: f}); out == nil || err != nil {
		t.Fatal(err)
	}
	ss, _ = f.Dataset.GetSequence(ft(0x300a0210))
	ss[0].Put(derivedio.DS(ft(0x300a022a), 1))
	putSequence(f.Dataset, ft(0x300a0210), ss)
	if p, err := Read(f); p != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("non-gamma nonzero rate", err)
	}
}
