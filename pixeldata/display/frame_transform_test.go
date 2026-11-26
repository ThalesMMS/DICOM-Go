package display

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func ds(elements ...core.Element) core.DataSet { return core.DataSet{Elements: elements} }

func str(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

func seq(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

func rescaleItem(slope, intercept string) core.DataSet {
	return ds(seq(tagPixelValueTransformationSequence, ds(
		str(tagRescaleIntercept, core.VRDS, intercept), str(tagRescaleSlope, core.VRDS, slope))))
}

var (
	tagShared   = core.NewTag(0x5200, 0x9229)
	tagPerFrame = core.NewTag(0x5200, 0x9230)
)

// enhancedCT has per-frame rescale and shared windows; frame 2 also carries
// its own window, a conflict with the shared macro that is reported. The
// top-level attributes must be ignored.
func enhancedCT() *object.Object {
	sharedWindows := ds(seq(tagFrameVOILUTSequence, ds(
		str(tagWindowCenter, core.VRDS, "40", "400"), str(tagWindowWidth, core.VRDS, "400", "2000"),
		str(tagWindowCenterWidthExplanation, core.VRLO, "SOFT", "BONE"))))
	frame2 := rescaleItem("1", "-1000")
	frame2.Elements = append(frame2.Elements, seq(tagFrameVOILUTSequence, ds(
		str(tagWindowCenter, core.VRDS, "-600"), str(tagWindowWidth, core.VRDS, "1500"))))
	return object.FromElements([]core.Element{
		str(tagRescaleSlope, core.VRDS, "9"), str(tagRescaleIntercept, core.VRDS, "9"),
		str(tagWindowCenter, core.VRDS, "9"), str(tagWindowWidth, core.VRDS, "9"),
		seq(tagShared, sharedWindows),
		seq(tagPerFrame, rescaleItem("1", "-1024"), rescaleItem("2", "-2048"), frame2),
	}, nil)
}

func TestModalityAndVOIPerFrame(t *testing.T) {
	obj := enhancedCT()
	for i, want := range [][2]float64{{1, -1024}, {2, -2048}, {1, -1000}} {
		m, err := ModalityLUTAt(obj, i)
		if err != nil || m.Slope != want[0] || m.Intercept != want[1] {
			t.Fatalf("frame %d modality %+v %v", i, m, err)
		}
	}
	if voi, err := VOIAt(obj, 0); err != nil || voi.Center != 40 || voi.Width != 400 {
		t.Fatalf("frame 0 VOI %+v %v", voi, err)
	}
	if voi, err := VOIAt(obj, 2); !errors.Is(err, object.ErrFunctionalGroupConflict) || voi.Center != -600 || voi.Width != 1500 {
		t.Fatalf("frame 2 VOI %+v %v", voi, err)
	}
	windows, err := VOIsAt(obj, 1)
	if err != nil || len(windows) != 2 || windows[1].Center != 400 || windows[1].Explanation != "BONE" {
		t.Fatalf("frame 1 windows %+v %v", windows, err)
	}
}

func TestSingleFrameUnchanged(t *testing.T) {
	obj := object.FromElements([]core.Element{
		str(tagRescaleSlope, core.VRDS, "2"), str(tagRescaleIntercept, core.VRDS, "-10"),
		str(tagWindowCenter, core.VRDS, "50", "60"), str(tagWindowWidth, core.VRDS, "100", "200"),
	}, nil)
	for _, frame := range []int{0, 5} {
		m, _ := ModalityLUTAt(obj, frame)
		legacy, _ := ModalityLUTFromObject(obj)
		voi, _ := VOIAt(obj, frame)
		legacyVOI, _ := VOIFromObject(obj)
		if m != legacy || voi != legacyVOI {
			t.Fatalf("frame %d: %+v %+v vs %+v %+v", frame, m, voi, legacy, legacyVOI)
		}
	}
	if windows, _ := VOIsAt(obj, 0); len(windows) != 2 || windows[1].Width != 200 {
		t.Fatalf("windows %+v", windows)
	}
}

func TestFunctionalGroupConflictReported(t *testing.T) {
	obj := object.FromElements([]core.Element{
		seq(tagShared, rescaleItem("1", "0")),
		seq(tagPerFrame, rescaleItem("3", "-5")),
	}, nil)
	m, err := ModalityLUTAt(obj, 0)
	if !errors.Is(err, object.ErrFunctionalGroupConflict) || m.Slope != 3 {
		t.Fatalf("conflict: %+v %v", m, err)
	}
	// Frames without a per-frame item use the shared macro.
	if m, err := ModalityLUTAt(obj, 7); err != nil || m.Slope != 1 {
		t.Fatalf("shared fallback: %+v %v", m, err)
	}
}
