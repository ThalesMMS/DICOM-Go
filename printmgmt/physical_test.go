package printmgmt

import (
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"math"
	"strings"
	"testing"
)

func TestPhysicalPlanGeometry(t *testing.T) {
	area := ImageBoxArea{WidthMM: 100, HeightMM: 80, NativePixelPitchMM: 1}
	cases := []struct {
		name            string
		rows, cols      int
		aspect          [2]int
		p               ImageBoxPresentation
		magn            string
		outcome         DecimateCropBehavior
		w, h, left, top float64
		fail            bool
	}{
		{"fit", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 100, DecimateCropBehavior: Fail}, "BILINEAR", "", 100, 40, 0, 0, false},
		{"decimate-wide", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: Decimate}, "BILINEAR", Decimate, 100, 40, 0, 0, false},
		{"crop-wide", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: Crop}, "BILINEAR", Crop, 100, 80, .25, 0, false},
		{"crop-tall", 100, 40, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 80, DecimateCropBehavior: Crop}, "BILINEAR", Crop, 80, 80, 0, .3, false},
		{"aspect", 40, 100, [2]int{2, 1}, ImageBoxPresentation{RequestedWidthMM: 100, DecimateCropBehavior: Fail}, "BILINEAR", "", 100, 80, 0, 0, false},
		{"fail-oversize", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: Fail}, "BILINEAR", "", 0, 0, 0, 0, true},
		{"none-decimate", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: Decimate}, "NONE", "", 0, 0, 0, 0, true},
		{"image-override-none", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: Decimate, MagnificationType: "NONE"}, "CUBIC", "", 0, 0, 0, 0, true},
		{"default-fail", 40, 100, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 200}, "BILINEAR", "", 0, 0, 0, 0, true},
		{"native-crop", 100, 200, [2]int{}, ImageBoxPresentation{DecimateCropBehavior: Crop}, "BILINEAR", Crop, 100, 80, .25, .1, false},
		{"optimal", 40, 100, [2]int{}, ImageBoxPresentation{}, "BILINEAR", "", 100, 40, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanImageBox(c.rows, c.cols, c.aspect, c.p, area, c.magn, "")
			if c.fail {
				var e *StatusError
				if !errors.As(err, &e) || e.Status != 0xC603 || plan.Resolved {
					t.Fatalf("refusal %v %+v", err, plan)
				}
				return
			}
			if err != nil || !plan.Resolved || plan.Outcome != c.outcome || math.Abs(plan.AcceptedWidthMM-c.w) > 1e-10 || math.Abs(plan.AcceptedHeightMM-c.h) > 1e-10 || math.Abs(plan.SourceLeft-c.left) > 1e-10 || math.Abs(plan.SourceTop-c.top) > 1e-10 {
				t.Fatalf("%+v %v", plan, err)
			}
		})
	}
}
func TestPhysicalMalformedAndBounds(t *testing.T) {
	for _, v := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), 10001} {
		t.Run("width", func(t *testing.T) {
			if (ImageBoxPresentation{RequestedWidthMM: v}).Validate() == nil {
				t.Fatal("width accepted")
			}
		})
	}
	if (ImageBoxPresentation{DecimateCropBehavior: "ZOOM"}).Validate() == nil {
		t.Fatal("enum accepted")
	}
	for _, v := range []string{"0", "-1", "NaN", "Inf", "0x1p1", "1\\2", "1e999", "", "12345678901234567"} {
		ds := object.FromElements([]core.Element{text(TagRequestedImageSize, core.VRDS, v)}, nil)
		if _, err := parseImagePresentation(newReader(ds), ImageBoxPresentation{}); err == nil {
			t.Errorf("malformed DS accepted: %q", v)
		}
	}
	for _, e := range []core.Element{text(TagRequestedImageSize, core.VRLO, "10"), {Header: core.ElementHeader{Tag: TagRequestedImageSize, VR: core.VRDS}, Value: core.StringValue{"10", "11"}}, text(TagRequestedDecimateCrop, core.VRLO, "FAIL"), text(TagRequestedDecimateCrop, core.VRCS, "UNKNOWN")} {
		if _, err := parseImagePresentation(newReader(object.FromElements([]core.Element{e}, nil)), ImageBoxPresentation{}); err == nil {
			t.Fatal("malformed tag accepted")
		}
	}
	for _, v := range []string{"1", "+1.25", " .5 ", "1E+2", "1e-9"} {
		if _, err := parseImagePresentation(newReader(object.FromElements([]core.Element{text(TagRequestedImageSize, core.VRDS, v)}, nil)), ImageBoxPresentation{}); err != nil {
			t.Errorf("valid DS %q: %v", v, err)
		}
	}
	for _, a := range []ImageBoxArea{{}, {WidthMM: math.NaN(), HeightMM: 10}, {WidthMM: 1, HeightMM: math.Inf(1)}, {WidthMM: 10, HeightMM: 10, NativePixelPitchMM: -1}} {
		if _, err := PlanImageBox(2, 2, [2]int{}, ImageBoxPresentation{RequestedWidthMM: 1}, a, "", ""); err == nil {
			t.Fatal("bad area accepted")
		}
	}
	if _, err := PlanImageBox(2, 2, [2]int{}, ImageBoxPresentation{DecimateCropBehavior: Fail}, ImageBoxArea{WidthMM: 10, HeightMM: 10}, "", ""); !errors.Is(err, ErrPhysicalGeometry) {
		t.Fatal("unknown native geometry was guessed")
	}
	for _, a := range [][2]int{{0, 1}, {1, -1}, {2147483648, 1}} {
		if _, err := PlanImageBox(2, 2, a, ImageBoxPresentation{RequestedWidthMM: 1}, ImageBoxArea{WidthMM: 10, HeightMM: 10}, "", ""); err == nil {
			t.Fatal("bad aspect accepted")
		}
	}
}
func TestPhysicalImageEncodingAndClone(t *testing.T) {
	img := &Image{Rows: 2, Columns: 2, Bits: 8, Photometric: "MONOCHROME2", PixelAspectRatio: [2]int{2, 1}, PixelData: []byte{1, 2, 3, 4}}
	ds, err := BuildImageBoxWithPresentation(1, img, "", ImageBoxPresentation{RequestedWidthMM: 125.5, DecimateCropBehavior: Crop})
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseImagePresentation(newReader(ds), ImageBoxPresentation{})
	if err != nil || p.RequestedWidthMM != 125.5 || p.DecimateCropBehavior != Crop {
		t.Fatalf("%+v %v", p, err)
	}
	items, _ := ds.GetSequence(TagBasicGrayscaleImageSequence)
	got, err := parseImage(items[0], false, 8192, 64<<20)
	if err != nil || got.PixelAspectRatio != [2]int{2, 1} {
		t.Fatalf("%+v %v", got, err)
	}
	f := FilmBox{ImageBoxes: []ImageBox{{Image: got, Presentation: p, Plan: PhysicalPlan{Resolved: true, SourceLeft: .25}}}}
	copy := f.clone()
	f.ImageBoxes[0].Plan.SourceLeft = .4
	f.ImageBoxes[0].Presentation.RequestedWidthMM = 50
	f.ImageBoxes[0].Image.PixelData[0] = 8
	if copy.ImageBoxes[0].Plan.SourceLeft != .25 || copy.ImageBoxes[0].Presentation.RequestedWidthMM != 125.5 || copy.ImageBoxes[0].Image.PixelData[0] != 1 {
		t.Fatal("job aliases working image")
	}
	inherited, err := parseImagePresentation(newReader(object.FromElements(nil, nil)), p)
	if err != nil || inherited != p {
		t.Fatal("omitted attributes did not retain presentation")
	}
}

func TestPhysicalDSResidentLengthBeforeTrim(t *testing.T) {
	over := "1" + strings.Repeat(" ", 17)
	for _, e := range []core.Element{
		core.NewRawElement(TagRequestedImageSize, core.VRDS, []byte(over)),
		{Header: core.ElementHeader{Tag: TagRequestedImageSize, VR: core.VRDS}, Value: core.StringValue{over}},
	} {
		t.Run(fmt.Sprintf("%T", e.Value), func(t *testing.T) {
			if _, err := parseImagePresentation(newReader(object.FromElements([]core.Element{e}, nil)), ImageBoxPresentation{}); err == nil {
				t.Fatal("overlength resident DS accepted after trim")
			}
		})
	}
	// Legal single padding byte for an odd one-character DS stays accepted.
	e := core.NewRawElement(TagRequestedImageSize, core.VRDS, []byte("1 "))
	if p, err := parseImagePresentation(newReader(object.FromElements([]core.Element{e}, nil)), ImageBoxPresentation{}); err != nil || p.RequestedWidthMM != 1 {
		t.Fatal("legal DS padding refused", p, err)
	}
}
func TestPhysicalPixelAspectRequiresISAndNonNilValue(t *testing.T) {
	img := &Image{Rows: 2, Columns: 2, Bits: 8, Photometric: "MONOCHROME2", PixelData: []byte{1, 2, 3, 4}}
	ds, err := BuildImageBox(1, img, "")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := ds.GetSequence(TagBasicGrayscaleImageSequence)
	for _, e := range []core.Element{
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRLO}, Value: core.StringValue{"2", "1"}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: nil},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{"1\t", "1"}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{"0000000000001", "1"}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{"1", "000000000001  "}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{"1", "0000000000001"}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{"2\\1"}},
		{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: physicalOpaqueValue{}},
	} {
		t.Run(fmt.Sprintf("%s-%T", e.Header.VR, e.Value), func(t *testing.T) {
			item := object.FromElements(items[0].Elements(), nil)
			item.Put(e)
			if _, err := parseImage(item, false, 8192, 64<<20); err == nil {
				t.Fatal("malformed aspect changed physical geometry")
			}
		})
	}
	for _, value := range []core.Value{
		core.RawValue(`000000000001\000000000001 `),
		core.StringValue{"000000000001", "000000000001 "},
		core.StringValue{" +2 ", "1"},
	} {
		item := object.FromElements(items[0].Elements(), nil)
		item.Put(core.Element{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: value})
		if got, err := parseImage(item, false, 8192, 64<<20); err != nil || got.PixelAspectRatio[1] != 1 {
			t.Fatalf("legal IS or whole-field padding refused: %T %+v %v", value, got, err)
		}
	}
}

type physicalOpaqueValue struct{}

func (physicalOpaqueValue) Kind() core.ValueKind { panic("opaque Kind must not be invoked") }
func (physicalOpaqueValue) EncodedLength() (core.Length, bool) {
	panic("opaque EncodedLength must not be invoked")
}

func TestPhysicalTextRejectsOpaqueAndMalformedResidentCS(t *testing.T) {
	for _, e := range []core.Element{
		{Header: core.ElementHeader{Tag: TagRequestedImageSize, VR: core.VRDS}, Value: physicalOpaqueValue{}},
		{Header: core.ElementHeader{Tag: TagRequestedImageSize, VR: core.VRDS}, Value: nil},
		text(TagRequestedDecimateCrop, core.VRCS, "FAIL"+strings.Repeat(" ", 14)),
		text(TagRequestedDecimateCrop, core.VRCS, "FAIL\t"),
		text(TagMagnificationType, core.VRCS, "NONE\x00"),
	} {
		if _, err := parseImagePresentation(newReader(object.FromElements([]core.Element{e}, nil)), ImageBoxPresentation{}); err == nil {
			t.Fatal("opaque or malformed resident presentation accepted")
		}
	}
}
