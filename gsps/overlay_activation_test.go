package gsps

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func TestOverlayActivationRoundTripAndSelection(t *testing.T) {
	own := display.Overlay{Group: 0x6004, Rows: 2, Columns: 2, OriginRow: 1, OriginCol: 1, Type: "R", Label: "CAD", Data: []byte{0x09, 0x00}}
	state := &State{
		SOPInstanceUID: "1.2.3.4.5", StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.4",
		DisplayedArea:      DisplayedArea{TopLeftX: 1, TopLeftY: 1, BottomRightX: 2, BottomRightY: 2},
		OverlayActivations: []OverlayActivation{{Group: 0x6002, Layer: "IMAGE OVERLAYS"}, {Group: 0x6004, Layer: "CAD"}},
		Overlays:           []display.Overlay{own},
	}
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	read, err := Read(back.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.OverlayActivations) != 2 || read.OverlayActivations[1].Layer != "CAD" || len(read.Overlays) != 1 ||
		read.Overlays[0].Group != 0x6004 || read.Overlays[0].Label != "CAD" || read.Overlays[0].Type != "R" {
		t.Fatalf("read back %+v %+v", read.OverlayActivations, read.Overlays)
	}
	image := []display.Overlay{{Group: 0x6000, Label: "hidden"}, {Group: 0x6002, Label: "image"}, {Group: 0x6004, Label: "replaced"}}
	active := ActiveOverlays(read, image)
	if len(active) != 2 || active[0].Label != "image" || active[1].Label != "CAD" {
		t.Fatalf("active %+v", active)
	}
	if got := ActiveOverlays(&State{}, image); len(got) != 0 {
		t.Fatalf("a state without activations displayed %+v", got)
	}
}

func TestOverlayActivationRejectsInvalidGroup(t *testing.T) {
	state := &State{SOPInstanceUID: "1.2", StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.4",
		DisplayedArea:      DisplayedArea{TopLeftX: 1, TopLeftY: 1, BottomRightX: 2, BottomRightY: 2},
		OverlayActivations: []OverlayActivation{{Group: 0x6001}}}
	if _, err := Write(state); err == nil {
		t.Fatal("odd overlay group written")
	}
}
