package rtplan

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

func blockFixture() *planFixture {
	f := newPlanFixture()
	f.beams[0].attrs.set(derivedio.IS(tagNumberOfBlocks, 1), derivedio.Seq(tagBlockSequence, derivedio.DataSet(
		derivedio.IS(tagBlockNumber, 1), derivedio.CS(tagBlockType, "SHIELDING"), derivedio.CS(tagBlockDivergence, "ABSENT"),
		derivedio.IS(tagBlockNumberOfPoints, 4), derivedio.DS(tagBlockData, -7, -3, 11, -2, 9, 13, -4, 8),
	)))
	return f
}

func TestBlockContourPreservedAndOwned(t *testing.T) {
	f := blockFixture().file(t)
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]float64{{-7, -3}, {11, -2}, {9, 13}, {-4, 8}}
	if !reflect.DeepEqual(p.Beams[0].Blocks[0].Contour, want) {
		t.Fatal("asymmetric polygon changed")
	}
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, f); err != nil {
		t.Fatal(err)
	}
	again, err := object.ReadFile(&wire)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Read(again)
	if err != nil || !reflect.DeepEqual(back.Beams[0].Blocks, p.Beams[0].Blocks) {
		t.Fatal("stored sequence round trip", err)
	}
	g, err := p.InspectAperture(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	g.Blocks[0].Contour[0][0] = 100
	if !reflect.DeepEqual(p.Beams[0].Blocks[0].Contour, want) {
		t.Fatal("inspection changed retained outline")
	}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: f}); out != nil || !errors.Is(err, ErrUnsupportedFeature) {
		t.Fatal("conventional authoring boundary", err)
	}
}

func TestBlockContourInvalidDataIsTyped(t *testing.T) {
	for name, element := range map[string]core.Element{
		"count":           derivedio.IS(tagBlockNumberOfPoints, 5),
		"incomplete pair": derivedio.DS(tagBlockData, 1, 2, 3),
		"invalid DS":      derivedio.Str(tagBlockData, core.VRDS, "wrong"),
		"nonfinite":       derivedio.Str(tagBlockData, core.VRDS, "NaN"),
		"hex DS":          derivedio.Str(tagBlockData, core.VRDS, `0x1p2\0\1\0\0\1`),
		"DS too long":     derivedio.Str(tagBlockData, core.VRDS, `12345678901234567\0\1\0\0\1`),
		"negative count":  derivedio.IS(tagBlockNumberOfPoints, -1),
		"wrong type":      derivedio.CS(tagBlockType, "OTHER"),
	} {
		t.Run(name, func(t *testing.T) {
			f := blockFixture()
			blocks := f.beams[0].attrs[tagBlockSequence].Value.(core.SequenceValue)
			b := newElems(blocks.Items[0].Elements...)
			b.set(element)
			f.beams[0].attrs.set(derivedio.Seq(tagBlockSequence, b.dataset()))
			// Malformed incoming values cannot be produced by the strict writer.
			p, err := ReadDataset(f.dataset(t))
			var invalid *ValidationError
			if p != nil || !errors.As(err, &invalid) || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("partial result or untyped error: %v", err)
			}
			if !strings.Contains(err.Error(), "(300A,") {
				t.Fatal("missing attribute path")
			}
		})
	}
}

func TestBlockContourUnavailableAndNormativeRelations(t *testing.T) {
	f := blockFixture()
	blocks := f.beams[0].attrs[tagBlockSequence].Value.(core.SequenceValue)
	b := newElems(blocks.Items[0].Elements...)
	b.set(derivedio.Str(tagBlockData, core.VRDS, ""))
	f.beams[0].attrs.set(derivedio.Seq(tagBlockSequence, b.dataset()))
	p, err := Read(f.file(t))
	if err != nil || len(p.Beams[0].Blocks[0].Contour) != 0 {
		t.Fatal("Type 2 empty outline", err)
	}
	g, err := p.InspectAperture(context.Background(), 1, 0)
	if err != nil || !strings.Contains(strings.Join(g.Limitations, " "), "outline unavailable") {
		t.Fatal("unavailable outline claimed complete", err)
	}
	for _, mismatch := range []core.Tag{tagBlockNumber, tagBlockThickness, tagBlockTransmission} {
		f := blockFixture()
		first := newElems(blocks.Items[0].Elements...)
		first.set(derivedio.CS(tagBlockType, "APERTURE"), derivedio.DS(tagBlockThickness, 10), derivedio.DS(tagBlockTransmission, 0.01))
		second := newElems(first.list()...)
		second.set(derivedio.IS(tagBlockNumber, 2))
		if mismatch == tagBlockNumber {
			second.set(derivedio.IS(tagBlockNumber, 1))
		} else {
			second.set(derivedio.DS(mismatch, 0.1))
		}
		f.beams[0].attrs.set(derivedio.IS(tagNumberOfBlocks, 2), derivedio.Seq(tagBlockSequence, first.dataset(), second.dataset()))
		if p, err := Read(f.file(t)); p != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatal("block relation accepted", mismatch, err)
		}
	}
}

func TestConventionalApertureKnownIECControlPoints(t *testing.T) {
	p, err := Read(newPlanFixture().file(t))
	if err != nil {
		t.Fatal(err)
	}
	for cp, area := range []float64{4000, 2000, 2000} {
		g, err := p.InspectAperture(context.Background(), 2, cp)
		if err != nil || g.State.Index != cp || g.State.CumulativeMetersetWeight == nil || *g.State.CumulativeMetersetWeight != float64(cp)/2 || g.DerivedArea == nil || *g.DerivedArea != area {
			t.Fatalf("CP %d area/state: %+v %v", cp, g, err)
		}
		if cp == 2 && g.State.Encodes(FieldDevicePositions) {
			t.Fatal("inherited leaves presented as newly declared")
		}
		if cp == 1 && !reflect.DeepEqual(g.State.EncodedDeviceTypes, []string{DeviceMLCX}) || cp == 2 && len(g.State.EncodedDeviceTypes) != 0 {
			t.Fatal("per-device declared/inherited state changed")
		}
		if cp == 1 && (g.Leaves[0].Min != [2]float64{-10, -50} || g.Leaves[0].Max != [2]float64{10, -40}) {
			t.Fatal("IEC MLCX axes/sign changed")
		}
	}
	// MLCY swaps travel/across axes; the fixture has explicit asymmetric jaws.
	b := &p.Beams[1]
	b.Devices[2].Type = DeviceMLCY
	for i := range b.ControlPoints {
		b.ControlPoints[i].DevicePositions[2].DeviceType = DeviceMLCY
	}
	g, err := p.InspectAperture(context.Background(), 2, 1)
	if err != nil || g.Leaves[1].Min != [2]float64{-40, -10} || g.Leaves[1].Max != [2]float64{-30, 10} || *g.DerivedArea != 1600 {
		t.Fatalf("IEC MLCY/clipping: %+v %v", g, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g, err := p.InspectAperture(ctx, 2, 0); g != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled inspection", err)
	}
	if g, err := p.InspectAperture(context.Background(), 2, 3); g != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("absent CP", err)
	}
	if g, err := p.InspectAperture(context.Background(), 2, 0, Limits{MaxModelBytes: 1}); g != nil || !errors.Is(err, ErrResourceLimit) {
		t.Fatal("unbounded inspection", err)
	}
	b.ControlPoints[0].DevicePositions[2].Positions = []float64{1}
	if g, err := p.InspectAperture(context.Background(), 2, 0); g != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("partial invalid aperture", err)
	}
}

func TestConventionalApertureMissingFieldsStayUnavailable(t *testing.T) {
	p, err := Read(newPlanFixture().file(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Beams {
		p.Beams[i].FinalCumulativeMetersetWeight = nil
		for j := range p.Beams[i].ControlPoints {
			p.Beams[i].ControlPoints[j].CumulativeMetersetWeight = nil
		}
	}
	g, err := p.InspectAperture(context.Background(), 2, 2)
	if err != nil || g.State.CumulativeMetersetWeight != nil || g.State.SourceToSurfaceDistance != nil {
		t.Fatal("missing non-inherited fields were synthesized", err)
	}
}
