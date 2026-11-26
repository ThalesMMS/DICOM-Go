package mrdiffusion

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Every fixture below is synthetic: only the attributes the reader looks at.

func newDataset(manufacturer string) *object.Object {
	dataset := object.New(std.Dictionary)
	if manufacturer != "" {
		_ = dataset.SetStrings(core.NewTag(0x0008, 0x0070), core.VRLO, manufacturer)
	}
	_ = dataset.SetStrings(core.NewTag(0x0008, 0x0060), core.VRCS, "MR")
	return dataset
}

func creator(dataset *object.Object, group uint16, name string) {
	dataset.Put(core.NewRawElement(core.NewTag(group, 0x0010), core.VRLO, []byte(name)))
}

func fl(values ...float32) []byte {
	var out []byte
	for _, v := range values {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(v))
	}
	return out
}

func fd(values ...float64) []byte {
	var out []byte
	for _, v := range values {
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
	}
	return out
}

func text(value string) []byte {
	if len(value)%2 != 0 {
		value += " "
	}
	return []byte(value)
}

func macroItem(b float64, directionality string, direction []float64) *object.Object {
	item := object.New(std.Dictionary)
	item.SetFD(core.NewTag(0x0018, 0x9087), b)
	_ = item.SetStrings(core.NewTag(0x0018, 0x9075), core.VRCS, directionality)
	if direction != nil {
		gradient := object.New(std.Dictionary)
		gradient.SetFD(core.NewTag(0x0018, 0x9089), direction...)
		item.SetSequence(core.NewTag(0x0018, 0x9076), gradient)
	}
	return item
}

// enhancedDWI is an Enhanced MR object with b=0, then b=1000 along x and y
// in the Per-Frame Functional Groups.
func enhancedDWI() *object.Object {
	dataset := newDataset("Philips")
	frames := []*object.Object{}
	for _, frame := range []struct {
		b         float64
		direction []float64
	}{{0, nil}, {1000, []float64{1, 0, 0}}, {1000, []float64{0, 1, 0}}} {
		directionality := "NONE"
		if frame.direction != nil {
			directionality = "DIRECTIONAL"
		}
		group := object.New(std.Dictionary)
		group.SetSequence(core.NewTag(0x0018, 0x9117), macroItem(frame.b, directionality, frame.direction))
		frames = append(frames, group)
	}
	dataset.SetSequence(core.NewTag(0x5200, 0x9230), frames...)
	return dataset
}

func siemensCSA(t *testing.T, elements ...siemens.CSAElement) *object.Object {
	t.Helper()
	blob, err := siemens.EncodeCSA(elements)
	if err != nil {
		t.Fatal(err)
	}
	dataset := newDataset("SIEMENS")
	creator(dataset, 0x0029, "SIEMENS CSA HEADER")
	dataset.Put(core.NewRawElement(core.NewTag(0x0029, 0x1010), core.VROB, blob))
	return dataset
}

func TestReadVendorFixtures(t *testing.T) {
	sharedMacro := newDataset("SIEMENS")
	shared := object.New(std.Dictionary)
	shared.SetSequence(core.NewTag(0x0018, 0x9117), macroItem(800, "ISOTROPIC", nil))
	sharedMacro.SetSequence(core.NewTag(0x5200, 0x9229), shared)

	classic := newDataset("Philips Medical Systems")
	classic.SetFD(core.NewTag(0x0018, 0x9087), 1500)
	classic.SetFD(core.NewTag(0x0018, 0x9089), 0, 0, 1)
	// Philips private attributes are ignored when the standard ones exist.
	creator(classic, 0x2001, "Philips Imaging DD 001")
	classic.Put(core.NewRawElement(core.NewTag(0x2001, 0x1003), core.VRFL, fl(1)))

	siemensDirectional := siemensCSA(t,
		siemens.CSAElement{Name: "B_value", VM: 1, VR: "IS", SyngoDT: 6, Items: 6, Values: []string{"1000"}},
		siemens.CSAElement{Name: "DiffusionDirectionality", VM: 1, VR: "CS", SyngoDT: 16, Items: 6, Values: []string{"DIRECTIONAL"}},
		siemens.CSAElement{Name: "DiffusionGradientDirection", VM: 3, VR: "FD", SyngoDT: 4, Items: 6, Values: []string{"0.70710678", "0.00000000", "-0.70710678"}},
	)
	siemensB0 := siemensCSA(t,
		siemens.CSAElement{Name: "B_value", VM: 1, VR: "IS", SyngoDT: 6, Items: 6, Values: []string{"0"}},
		siemens.CSAElement{Name: "DiffusionGradientDirection", VM: 3, VR: "FD", SyngoDT: 4, Items: 6},
	)

	siemensMRHeader := newDataset("SIEMENS")
	creator(siemensMRHeader, 0x0019, "SIEMENS MR HEADER")
	siemensMRHeader.Put(core.NewRawElement(core.NewTag(0x0019, 0x100C), core.VRIS, text("500")))
	siemensMRHeader.Put(core.NewRawElement(core.NewTag(0x0019, 0x100D), core.VRCS, text("DIRECTIONAL")))
	siemensMRHeader.Put(core.NewRawElement(core.NewTag(0x0019, 0x100E), core.VRFD, fd(0, 1, 0)))

	ge := newDataset("GE MEDICAL SYSTEMS")
	creator(ge, 0x0043, "GEMS_PARM_01")
	ge.Put(core.NewRawElement(core.NewTag(0x0043, 0x1039), core.VRIS, text(`1000000800\8\0\0`)))
	creator(ge, 0x0019, "GEMS_ACQU_01")
	ge.Put(core.NewRawElement(core.NewTag(0x0019, 0x10BB), core.VRDS, text("0.5")))
	ge.Put(core.NewRawElement(core.NewTag(0x0019, 0x10BC), core.VRDS, text("-0.5")))
	ge.Put(core.NewRawElement(core.NewTag(0x0019, 0x10BD), core.VRDS, text("0.707107")))

	// An archive that lost the private VRs keeps them as UN.
	philips := newDataset("Philips Medical Systems")
	creator(philips, 0x2001, "Philips Imaging DD 001")
	philips.Put(core.NewRawElement(core.NewTag(0x2001, 0x1003), core.VRUN, fl(1000)))
	creator(philips, 0x2005, "Philips MR Imaging DD 001")
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B0), core.VRUN, fl(0)))
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B1), core.VRUN, fl(1)))
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B2), core.VRUN, fl(0)))

	canon := newDataset("TOSHIBA")
	_ = canon.SetStrings(core.NewTag(0x0020, 0x4000), core.VRLT, "b=1000(0.140,0.134,0.981)")
	canonTrace := newDataset("CANON_MEC")
	_ = canonTrace.SetStrings(core.NewTag(0x0020, 0x4000), core.VRLT, "DWI b=2000")

	for _, tc := range []struct {
		name           string
		dataset        *object.Object
		frame          int
		b              float64
		direction      []float64
		directionFrame DirectionFrame
		directionality string
		source         Source
		group          object.FunctionalGroupSource
	}{
		{"enhanced b0", enhancedDWI(), 0, 0, nil, FramePatient, "NONE", SourceMRDiffusionMacro, object.FunctionalGroupPerFrame},
		{"enhanced x", enhancedDWI(), 1, 1000, []float64{1, 0, 0}, FramePatient, "DIRECTIONAL", SourceMRDiffusionMacro, object.FunctionalGroupPerFrame},
		{"enhanced y", enhancedDWI(), 2, 1000, []float64{0, 1, 0}, FramePatient, "DIRECTIONAL", SourceMRDiffusionMacro, object.FunctionalGroupPerFrame},
		{"shared macro", sharedMacro, 3, 800, nil, FramePatient, "ISOTROPIC", SourceMRDiffusionMacro, object.FunctionalGroupShared},
		{"classic standard", classic, 0, 1500, []float64{0, 0, 1}, FramePatient, "", SourceStandardAttributes, ""},
		{"siemens csa", siemensDirectional, 0, 1000, []float64{0.70710678, 0, -0.70710678}, FramePatient, "DIRECTIONAL", SourceSiemensCSA, ""},
		{"siemens csa b0", siemensB0, 0, 0, nil, FramePatient, "", SourceSiemensCSA, ""},
		{"siemens mr header", siemensMRHeader, 0, 500, []float64{0, 1, 0}, FramePatient, "DIRECTIONAL", SourceSiemensMRHeader, ""},
		{"ge", ge, 0, 800, []float64{0.5, -0.5, 0.707107}, FrameGEGradient, "", SourceGE, ""},
		{"philips un", philips, 0, 1000, []float64{0, 1, 0}, FramePhilipsRLAPFH, "", SourcePhilips, ""},
		{"toshiba comments", canon, 0, 1000, []float64{0.140, 0.134, 0.981}, FrameUnspecified, "", SourceCanonImageComments, ""},
		{"canon trace comments", canonTrace, 0, 2000, nil, FrameUnspecified, "", SourceCanonImageComments, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Read(tc.dataset, tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			if got.BValue != tc.b || got.Source != tc.source || got.FrameIndex != tc.frame || got.FunctionalGroup != tc.group ||
				got.Directionality != tc.directionality || got.DirectionFrame != tc.directionFrame {
				t.Fatalf("Read = %+v", got)
			}
			if got.HasDirection != (tc.direction != nil) {
				t.Fatalf("HasDirection = %v, want %v", got.HasDirection, tc.direction != nil)
			}
			for i, want := range tc.direction {
				if math.Abs(got.Direction[i]-want) > 1e-6 {
					t.Fatalf("Direction = %v, want %v", got.Direction, tc.direction)
				}
			}
			if got.IsDiffusionWeighted() != (tc.b > 0) {
				t.Fatalf("IsDiffusionWeighted = %v", got.IsDiffusionWeighted())
			}
		})
	}
}

func TestReadWithoutDiffusionAttributes(t *testing.T) {
	plain := newDataset("SIEMENS")
	// A CSA header of a non-diffusion sequence lists B_value without items.
	noValue := siemensCSA(t, siemens.CSAElement{Name: "B_value", VM: 1, VR: "IS", SyngoDT: 6, Items: 6})
	emptyType2 := newDataset("Philips")
	emptyType2.Put(core.NewRawElement(core.NewTag(0x0018, 0x9087), core.VRFD, nil))
	// Image Comments of other manufacturers are not interpreted.
	otherComments := newDataset("Acme")
	_ = otherComments.SetStrings(core.NewTag(0x0020, 0x4000), core.VRLT, "b=1000")
	for name, dataset := range map[string]*object.Object{"nil": nil, "plain": plain, "csa without value": noValue, "empty type 2": emptyType2, "other comments": otherComments} {
		if got, err := Read(dataset, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Read = %+v, %v; want ErrNotFound", name, got, err)
		}
	}
}

func TestReadRejectsInvalidValues(t *testing.T) {
	negative := newDataset("")
	negative.SetFD(core.NewTag(0x0018, 0x9087), -5)
	notANumber := newDataset("GE MEDICAL SYSTEMS")
	creator(notANumber, 0x0043, "GEMS_PARM_01")
	notANumber.Put(core.NewRawElement(core.NewTag(0x0043, 0x1039), core.VRIS, text(`abc\0`)))
	infiniteDirection := newDataset("")
	infiniteDirection.SetFD(core.NewTag(0x0018, 0x9087), 1000)
	infiniteDirection.SetFD(core.NewTag(0x0018, 0x9089), math.Inf(1), 0, 0)
	shortFloat := newDataset("Philips")
	creator(shortFloat, 0x2001, "Philips Imaging DD 001")
	shortFloat.Put(core.NewRawElement(core.NewTag(0x2001, 0x1003), core.VRUN, []byte{1, 2, 3}))
	for name, dataset := range map[string]*object.Object{"negative": negative, "not a number": notANumber, "infinite direction": infiniteDirection, "short float": shortFloat} {
		if got, err := Read(dataset, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Read = %+v, %v; want ErrInvalid", name, got, err)
		}
	}
}

func TestReadFrames(t *testing.T) {
	frames, err := ReadFrames(enhancedDWI(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].BValue != 0 || frames[1].BValue != 1000 || frames[2].FrameIndex != 2 {
		t.Fatalf("ReadFrames = %+v", frames)
	}
}

func TestFitADC(t *testing.T) {
	const s0 = 1000.0
	adcs := []float64{0.0007, 0.0012, 0.003, 0}
	images := func(b float64, scale float64) []float32 {
		out := make([]float32, len(adcs)+2)
		for i, adc := range adcs {
			out[i] = float32(scale * s0 * math.Exp(-b*adc))
		}
		out[len(adcs)] = 0 // background
		out[len(adcs)+1] = 5
		return out
	}
	t.Run("two points", func(t *testing.T) {
		got, err := FitADC([]float64{0, 1000}, [][]float32{images(0, 1), images(1000, 1)}, ADCOptions{MinSignal: 10})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range adcs {
			if math.Abs(got.Values[i]-want) > 1e-6 {
				t.Fatalf("ADC[%d] = %g, want %g", i, got.Values[i], want)
			}
		}
		if got.Fitted != len(adcs) || got.Values[len(adcs)] != 0 || got.Values[len(adcs)+1] != 0 {
			t.Fatalf("fit = %+v", got)
		}
	})
	t.Run("three b-values and two directions", func(t *testing.T) {
		got, err := FitADC([]float64{0, 500, 1000, 1000}, [][]float32{images(0, 1), images(500, 1), images(1000, 1), images(1000, 1)}, ADCOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range adcs {
			if math.Abs(got.Values[i]-want) > 1e-6 {
				t.Fatalf("ADC[%d] = %g, want %g", i, got.Values[i], want)
			}
		}
	})
	t.Run("mask and negative", func(t *testing.T) {
		high := images(1000, 1)
		high[0] = 2000 // the signal rises: noise
		got, err := FitADC([]float64{0, 1000}, [][]float32{images(0, 1), high}, ADCOptions{Mask: []bool{true, false, true, true, true, true}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Negative != 1 || got.Values[0] != 0 || got.Values[1] != 0 || math.Abs(got.Values[2]-0.003) > 1e-6 {
			t.Fatalf("fit = %+v", got)
		}
	})
	for name, tc := range map[string]struct {
		b      []float64
		images [][]float32
		opts   ADCOptions
	}{
		"one image":        {[]float64{0}, [][]float32{{1}}, ADCOptions{}},
		"same b-value":     {[]float64{1000, 1000}, [][]float32{{1}, {2}}, ADCOptions{}},
		"sizes differ":     {[]float64{0, 1000}, [][]float32{{1}, {2, 3}}, ADCOptions{}},
		"count mismatch":   {[]float64{0, 1000, 2000}, [][]float32{{1}, {2}}, ADCOptions{}},
		"negative b-value": {[]float64{-1, 1000}, [][]float32{{1}, {2}}, ADCOptions{}},
		"mask size":        {[]float64{0, 1000}, [][]float32{{1}, {2}}, ADCOptions{Mask: []bool{true, true}}},
	} {
		if _, err := FitADC(tc.b, tc.images, tc.opts); !errors.Is(err, ErrADC) {
			t.Errorf("%s: err = %v, want ErrADC", name, err)
		}
	}
}

// TestReadPrivateAttributesAfterImplicitVR reads the vendor fixtures back
// from Implicit VR Little Endian, where private values carry no VR.
func TestReadPrivateAttributesAfterImplicitVR(t *testing.T) {
	philips := newDataset("Philips Medical Systems")
	creator(philips, 0x2001, "Philips Imaging DD 001")
	philips.Put(core.NewRawElement(core.NewTag(0x2001, 0x1003), core.VRFL, fl(700)))
	creator(philips, 0x2005, "Philips MR Imaging DD 001")
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B0), core.VRFL, fl(1)))
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B1), core.VRFL, fl(0)))
	philips.Put(core.NewRawElement(core.NewTag(0x2005, 0x10B2), core.VRFL, fl(0)))
	ge := newDataset("GE MEDICAL SYSTEMS")
	creator(ge, 0x0043, "GEMS_PARM_01")
	ge.Put(core.NewRawElement(core.NewTag(0x0043, 0x1039), core.VRIS, text(`1000\8\0\0`)))
	siemensMR := newDataset("SIEMENS")
	creator(siemensMR, 0x0019, "SIEMENS MR HEADER")
	siemensMR.Put(core.NewRawElement(core.NewTag(0x0019, 0x100C), core.VRIS, text("50")))
	siemensMR.Put(core.NewRawElement(core.NewTag(0x0019, 0x100E), core.VRFD, fd(0, 0, 1)))
	for _, tc := range []struct {
		dataset *object.Object
		b       float64
		source  Source
	}{{philips, 700, SourcePhilips}, {ge, 1000, SourceGE}, {siemensMR, 50, SourceSiemensMRHeader}} {
		var buffer bytes.Buffer
		if err := object.WriteDataSet(&buffer, tc.dataset, transfer.ImplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		read, err := object.ReadDataSet(&buffer, transfer.ImplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Read(read, 0)
		if err != nil || got.BValue != tc.b || got.Source != tc.source {
			t.Fatalf("%s: Read = %+v, %v", tc.source, got, err)
		}
		if tc.source != SourceGE && !got.HasDirection {
			t.Fatalf("%s: no direction: %+v", tc.source, got)
		}
	}
}
