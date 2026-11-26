package iod

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func dataset(elements ...core.Element) *object.Object {
	return object.FromElements(elements, nil)
}

func str(group, element uint16, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue(values)}
}

func TestParseCondition(t *testing.T) {
	var (
		padding      = str(0x0028, 0x0120, core.VRUS, "0")
		monochrome1  = str(0x0028, 0x0004, core.VRCS, "MONOCHROME1")
		rgb          = str(0x0028, 0x0004, core.VRCS, "RGB")
		original     = str(0x0008, 0x0008, core.VRCS, "ORIGINAL", "PRIMARY")
		derived      = str(0x0008, 0x0008, core.VRCS, "DERIVED", "PRIMARY")
		frames       = str(0x0028, 0x0008, core.VRIS, "3")
		oneFrame     = str(0x0028, 0x0008, core.VRIS, "1")
		emptySpacing = str(0x0028, 0x0030, core.VRDS)
		spacing      = str(0x0028, 0x0030, core.VRDS, "0.5", "0.5")
		full         = str(0x300A, 0x0638, core.VRCS, "FULL")
		minimal      = str(0x300A, 0x0638, core.VRCS, "MINIMAL")
		shadowOn     = str(0x0070, 0x0244, core.VRCS, "NORMAL")
		shadowOff    = str(0x0070, 0x0244, core.VRCS, "OFF")
		prospective  = str(0x0018, 0x9037, core.VRCS, "PROSPECTIVE")
		noCardiac    = str(0x0018, 0x9037, core.VRCS, "NONE")
		gated        = str(0x0054, 0x1000, core.VRCS, "GATED", "IMAGE")
		static       = str(0x0054, 0x1000, core.VRCS, "STATIC", "IMAGE")
		staged       = str(0x0040, 0x0261, core.VRCS, "STAGED")
		intensifier  = str(0x0018, 0x9420, core.VRCS, "IMG_INTENSIFIER")
		surfaceNo    = str(0x300A, 0x02EC, core.VRCS, "NO")
		surfaceYes   = str(0x300A, 0x02EC, core.VRCS, "YES")
		mixed        = str(0x0008, 0x9205, core.VRCS, "MIXED")
		pointer      = func(tag core.Tag) core.Element {
			return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0009), VR: core.VRAT}, Value: core.TagValue{tag}}
		}
		frameTimePointer = pointer(core.NewTag(0x0018, 0x1063))
		otherPointer     = pointer(core.NewTag(0x0018, 0x2002))
		frameExtraction  = core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1164), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{}}}}
		codeItem         = func(value, scheme string) core.Element {
			return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0022, 0x0015), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				str(0x0008, 0x0100, core.VRSH, value), str(0x0008, 0x0102, core.VRSH, scheme),
			}}}}}
		}
	)
	for _, tc := range []struct {
		text      string
		parsed    bool
		forbidden bool
		true_     []core.Element
		false_    []core.Element
	}{
		{text: "Required if Pixel Padding Value (0028,0120) is present.", parsed: true,
			true_: []core.Element{padding}, false_: nil},
		{text: "Required if Pixel Padding Value (0028,0120) is absent.", parsed: true,
			true_: nil, false_: []core.Element{padding}},
		{text: "Required if Photometric Interpretation (0028,0004) has a value of MONOCHROME1 or MONOCHROME2.", parsed: true,
			true_: []core.Element{monochrome1}, false_: []core.Element{rgb}},
		{text: "Required if Photometric Interpretation (0028,0004) is not RGB. May be present otherwise.", parsed: true,
			true_: []core.Element{monochrome1}, false_: []core.Element{rgb}},
		{text: "Required if Value 1 of Image Type (0008,0008) is ORIGINAL. Shall not be present otherwise.", parsed: true, forbidden: true,
			true_: []core.Element{original}, false_: []core.Element{derived}},
		{text: "Required if Number of Frames (0028,0008) is greater than 1.", parsed: true,
			true_: []core.Element{frames}, false_: []core.Element{oneFrame}},
		{text: "Required if Pixel Padding Value (0028,0120) is present and Photometric Interpretation (0028,0004) is MONOCHROME1.", parsed: true,
			true_: []core.Element{padding, monochrome1}, false_: []core.Element{padding, rgb}},
		{text: "Required if Pixel Padding Value (0028,0120) or Number of Frames (0028,0008) is present.", parsed: true,
			true_: []core.Element{frames}, false_: nil},
		{text: "Required if Pixel Spacing (0028,0030) is present and has a value.", parsed: true,
			true_: []core.Element{spacing}, false_: []core.Element{emptySpacing}},
		{text: "Required if the Patient is an animal and if Patient Species Code Sequence (0010,2202) is absent."},
		{text: "Required if the image has been calibrated."},
		{text: "Shall be present if Number of Frames (0028,0008) is greater than 1.", parsed: true,
			true_: []core.Element{frames}, false_: []core.Element{oneFrame}},
		// Formalized since the coverage report of ConditionCoverage.
		{text: "Required if Number of Frames is greater than 1", parsed: true,
			true_: []core.Element{frames}, false_: []core.Element{oneFrame}},
		{text: "Required if Image Type Value 1 is ORIGINAL. May be present otherwise.", parsed: true,
			true_: []core.Element{original}, false_: []core.Element{derived}},
		{text: "Required if RT Radiation Physical and Geometric Content Detail Flag (300A,0638) equals FULL. May be present otherwise.", parsed: true,
			true_: []core.Element{full}, false_: []core.Element{minimal}},
		{text: "Required if Shadow Style (0070,0244) Value is not OFF.", parsed: true,
			true_: []core.Element{shadowOn}, false_: []core.Element{shadowOff}},
		{text: "Required if Cardiac Synchronization Technique (0018,9037) equals other than NONE.", parsed: true,
			true_: []core.Element{prospective}, false_: []core.Element{noCardiac}},
		{text: "Required if Series Type (0054,1000), Value 1 is GATED.", parsed: true,
			true_: []core.Element{gated}, false_: []core.Element{static}},
		{text: "Required if Performed Protocol Type (0040,0261) is present with Value STAGED.", parsed: true,
			true_: []core.Element{staged}, false_: nil},
		{text: "Required if X-Ray Receptor Type (0018,9420) is present and equals IMG_INTENSIFIER.", parsed: true,
			true_: []core.Element{intensifier}, false_: nil},
		{text: "Required if Compensator Surface Representation Flag (300A,02EC) is absent or has the Value NO.", parsed: true,
			true_: []core.Element{surfaceNo}, false_: []core.Element{surfaceYes}},
		{text: "Required if Photometric Interpretation (0028,0004) has a Value of PALETTE COLOR or Pixel Presentation (0008,9205) at the image level equals COLOR or MIXED. See for further explanation.", parsed: true,
			true_: []core.Element{mixed}, false_: []core.Element{rgb}},
		{text: "Required if Absolute Channel Display Scale (003A,0248) is not present, may be present otherwise.", parsed: true,
			true_: nil, false_: []core.Element{str(0x003A, 0x0248, core.VRFL, "1")}},
		{text: "Required if Device Alternate Identifier (3010,001B) has a Value. Defined Terms: BARCODE RFID", parsed: true,
			true_: []core.Element{str(0x3010, 0x001B, core.VRUC, "id")}, false_: []core.Element{str(0x3010, 0x001B, core.VRUC)}},
		{text: "Required if Frame Increment Pointer (0028,0009) is Frame Time (0018,1063) or Frame Time Vector (0018,1065)", parsed: true,
			true_: []core.Element{frameTimePointer}, false_: []core.Element{otherPointer}},
		{text: "Required if pixel data is Multi-frame Cine data", parsed: true,
			true_: []core.Element{frameTimePointer}, false_: []core.Element{otherPointer}},
		{text: "Required if the SOP Instance was created in response to a Frame-Level retrieve request", parsed: true,
			true_: []core.Element{frameExtraction}, false_: nil},
		{text: "Required if contrast media was used in this image.", parsed: true,
			true_: []core.Element{str(0x0018, 0x0010, core.VRLO, "agent")}, false_: nil},
		{text: "Required if Lossy Compression has been performed on the Image. May be present otherwise.", parsed: true,
			true_: []core.Element{str(0x0028, 0x2110, core.VRCS, "01")}, false_: []core.Element{str(0x0028, 0x2110, core.VRCS, "00")}},
		{text: "Required if Acquisition Device Type Code Sequence (0022,0015) contains an Item with the value . May be present otherwise. (392012008, SCT, \"Optical Coherence Tomography Scanner\")", parsed: true,
			true_: []core.Element{codeItem("392012008", "SCT")}, false_: []core.Element{codeItem("392012008", "DCM")}},
		{text: "Required if the SOP Class UID of this instance is 1.2.840.10008.5.1.4.1.1.2.3 (CT Image Storage - For Processing). May be present otherwise.", parsed: true,
			true_: []core.Element{str(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.2.3")}, false_: []core.Element{str(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.2")}},
		// Prose that runs into a tag is not an AT Value.
		{text: "Required if the Value of Image Box Layout Type (0072,0304) is TILED, and the Value of Image Box Tile Horizontal Dimension (0072,0306) or Image Box Tile Vertical Dimension (0072,0308) is greater than 1."},
		{text: "Required if time synchronization was applied."},
		{text: "Required if present and consistent in the contributing SOP Instances."},
	} {
		c, ok := parseCondition(tc.text)
		if ok != tc.parsed {
			t.Errorf("%q: parsed %v, want %v (%+v)", tc.text, ok, tc.parsed, c)
			continue
		}
		if !ok {
			continue
		}
		if c.forbidden != tc.forbidden {
			t.Errorf("%q: forbidden %v, want %v", tc.text, c.forbidden, tc.forbidden)
		}
		if obj := dataset(tc.true_...); !c.eval(obj, obj) {
			t.Errorf("%q: false for %v", tc.text, tc.true_)
		}
		if obj := dataset(tc.false_...); c.eval(obj, obj) {
			t.Errorf("%q: true for %v", tc.text, tc.false_)
		}
	}
}

// TestConditionLooksUpItemThenRoot checks that a condition in a Sequence
// Item reads the Item's Attribute before the data set's.
func TestConditionLooksUpItemThenRoot(t *testing.T) {
	c, ok := parseCondition("Required if Photometric Interpretation (0028,0004) is MONOCHROME1.")
	if !ok {
		t.Fatal("not parsed")
	}
	root := dataset(str(0x0028, 0x0004, core.VRCS, "MONOCHROME1"))
	if !c.eval(dataset(), root) {
		t.Error("root Value not used for an Item without the Attribute")
	}
	if c.eval(dataset(str(0x0028, 0x0004, core.VRCS, "RGB")), root) {
		t.Error("Item Value not preferred")
	}
}
