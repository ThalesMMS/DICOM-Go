package display

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func ovUS(group, element, v uint16) core.Element {
	return core.NewRawElement(core.NewTag(group, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
}

func ovText(group, element uint16, vr core.VR, v string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue{v}}
}

// bits packs set pixel indexes of consecutive frames LSB first.
func bits(n int, set ...int) []byte {
	out := make([]byte, (n+7)/8+(n+7)/8%2)
	for _, i := range set {
		out[i/8] |= 1 << uint(i%8)
	}
	return out
}

func TestMultiFrameOverlayAttributes(t *testing.T) {
	// 2x3 overlay, 3 frames starting at image frame 2; frame k sets pixel k.
	obj := object.FromElements([]core.Element{
		ovUS(0x6002, 0x0010, 2), ovUS(0x6002, 0x0011, 3),
		ovText(0x6002, 0x0015, core.VRIS, "3"), ovText(0x6002, 0x0022, core.VRLO, "tumour"),
		ovText(0x6002, 0x0040, core.VRCS, "R"), ovText(0x6002, 0x0045, core.VRLO, "USER"),
		ovUS(0x6002, 0x0051, 2), ovUS(0x6002, 0x0100, 1), ovUS(0x6002, 0x0102, 0),
		ovText(0x6002, 0x1301, core.VRIS, "42"), ovText(0x6002, 0x1302, core.VRDS, "12.5"),
		ovText(0x6002, 0x1303, core.VRDS, "3.25"), ovText(0x6002, 0x1500, core.VRLO, "LESION"),
		core.NewRawElement(core.NewTag(0x6002, 0x3000), core.VROB, bits(18, 0, 6+1, 12+2)),
	}, nil)
	overlays, err := OverlaysFromObject(obj)
	if err != nil || len(overlays) != 1 {
		t.Fatalf("%v %d", err, len(overlays))
	}
	o := overlays[0]
	if o.Group != 0x6002 || o.Type != "R" || o.Subtype != "USER" || o.Label != "LESION" || o.Description != "tumour" ||
		o.FrameCount() != 3 || o.FrameOrigin != 2 || !o.HasROIStatistics || o.ROIArea != 42 || o.ROIMean != 12.5 || o.ROIStandardDeviation != 3.25 {
		t.Fatalf("overlay %+v", o)
	}
	if _, ok := o.ForImageFrame(0); ok {
		t.Fatal("image frame 1 precedes Image Frame Origin")
	}
	for imageFrame, want := range map[int]int{1: 0, 2: 1, 3: 2} {
		frame, ok := o.ForImageFrame(imageFrame)
		if !ok || !bytes.Equal(frame.Unpacked(), unpackedWith(6, want)) {
			t.Fatalf("image frame %d: %v %v", imageFrame, ok, frame.Unpacked())
		}
	}
	if _, ok := o.ForImageFrame(4); ok {
		t.Fatal("image frame beyond the overlay frames")
	}
}

func unpackedWith(n int, set int) []byte {
	out := make([]byte, n)
	out[set] = 255
	return out
}

func TestBigEndianOWOverlayData(t *testing.T) {
	// Bits 0 and 9 set; OW words stored big endian.
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x6000, 0x0010), core.VRUS, []byte{0, 4}),
		core.NewRawElement(core.NewTag(0x6000, 0x0011), core.VRUS, []byte{0, 4}),
		core.NewRawElement(core.NewTag(0x6000, 0x3000), core.VROW, []byte{0x02, 0x01}),
	}, nil)
	obj.SetValueByteOrder(binary.BigEndian)
	overlays, _ := OverlaysFromObject(obj)
	if len(overlays) != 1 || !bytes.Equal(overlays[0].Data, []byte{0x01, 0x02}) {
		t.Fatalf("%+v", overlays)
	}
}

func TestEmbeddedOverlays(t *testing.T) {
	// 2x2 image, 2 frames, 16-bit samples with 12 bits stored; bit 15 holds
	// the overlay.
	samples := []uint16{0x8001, 0x0002, 0x0003, 0x8004, 0x0005, 0x8006, 0x0007, 0x0008}
	var native []byte
	for _, v := range samples {
		native = binary.LittleEndian.AppendUint16(native, v)
	}
	obj := object.FromElements([]core.Element{
		ovUS(0x0028, 0x0010, 2), ovUS(0x0028, 0x0011, 2), ovUS(0x0028, 0x0100, 16),
		ovText(0x0028, 0x0008, core.VRIS, "2"),
		ovUS(0x6000, 0x0010, 2), ovUS(0x6000, 0x0011, 2), ovText(0x6000, 0x0040, core.VRCS, "G"),
		ovUS(0x6000, 0x0100, 16), ovUS(0x6000, 0x0102, 15),
	}, nil)
	if plain, _ := OverlaysFromObject(obj); len(plain) != 0 {
		t.Fatal("embedded overlay read without pixels")
	}
	overlays, err := EmbeddedOverlays(obj, native, binary.LittleEndian)
	if err != nil || len(overlays) != 1 {
		t.Fatalf("%v %+v", err, overlays)
	}
	o := overlays[0]
	if !o.Embedded || o.BitPosition != 15 || o.FrameCount() != 2 || o.Type != "G" {
		t.Fatalf("%+v", o)
	}
	first, _ := o.ForImageFrame(0)
	second, _ := o.ForImageFrame(1)
	if !bytes.Equal(first.Unpacked(), []byte{255, 0, 0, 255}) || !bytes.Equal(second.Unpacked(), []byte{0, 255, 0, 0}) {
		t.Fatalf("frames %v %v", first.Unpacked(), second.Unpacked())
	}
	if _, err := EmbeddedOverlays(obj, native[:6], binary.LittleEndian); err == nil {
		t.Fatal("short pixel data accepted")
	}
	// One decoded frame at a time.
	perFrame, err := EmbeddedOverlaysForFrame(obj, native[8:], binary.LittleEndian, 1)
	if err != nil || len(perFrame) != 1 || !bytes.Equal(perFrame[0].Unpacked(), []byte{0, 255, 0, 0}) {
		t.Fatalf("frame 2 %+v %v", perFrame, err)
	}
}
