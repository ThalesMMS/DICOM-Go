package interopcheck

import (
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Fixture geometry: three 16-bit frames, so removing a frame and altering the
// last sample are both far from anything a header check would see.
const (
	FixtureFrames  = 3
	FixtureRows    = 24
	FixtureColumns = 32
	// FixtureSOPClassUID is Multi-frame Grayscale Word Secondary Capture.
	FixtureSOPClassUID    = "1.2.840.10008.5.1.4.1.1.7.3"
	FixtureSOPInstanceUID = "1.2.826.0.1.3680043.10.543.945.1.1"
)

// Fixture returns a synthetic, non-PHI multi-frame image with a coded
// sequence; every negative control applies to it.
func Fixture() *object.Object {
	obj := object.New(std.Dictionary)
	text := func(group, element uint16, vr core.VR, value string) {
		obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue{value}})
	}
	us := func(element uint16, value uint16) {
		raw := make([]byte, 2)
		binary.LittleEndian.PutUint16(raw, value)
		obj.Put(core.NewRawElement(core.NewTag(0x0028, element), core.VRUS, raw))
	}
	text(0x0008, 0x0016, core.VRUI, FixtureSOPClassUID)
	text(0x0008, 0x0018, core.VRUI, FixtureSOPInstanceUID)
	text(0x0008, 0x0020, core.VRDA, "20260925")
	text(0x0008, 0x0060, core.VRCS, "OT")
	text(0x0008, 0x0064, core.VRCS, "SYN")
	text(0x0008, 0x1030, core.VRLO, "Synthetic interop fixture")
	text(0x0010, 0x0010, core.VRPN, "SYNTHETIC^INTEROP")
	text(0x0010, 0x0020, core.VRLO, "SYNTHETIC-945")
	text(0x0020, 0x000D, core.VRUI, "1.2.826.0.1.3680043.10.543.945.2")
	text(0x0020, 0x000E, core.VRUI, "1.2.826.0.1.3680043.10.543.945.3")
	text(0x0020, 0x0013, core.VRIS, "1")
	text(0x0028, 0x0004, core.VRCS, "MONOCHROME2")
	text(0x0028, 0x0008, core.VRIS, "3")
	code := core.DataSet{Elements: []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0100), VR: core.VRSH}, Value: core.StringValue{"SYNTH01"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0102), VR: core.VRSH}, Value: core.StringValue{"99SYNTH"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0104), VR: core.VRLO}, Value: core.StringValue{"Synthetic procedure"}},
	}}
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1032), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{code}}})
	us(0x0002, 1)
	us(0x0010, FixtureRows)
	us(0x0011, FixtureColumns)
	us(0x0100, 16)
	us(0x0101, 12)
	us(0x0102, 11)
	us(0x0103, 0)
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROW, FixturePixels()))
	return obj
}

// FixturePixels returns the fixture's native little-endian Pixel Data: a
// distinct 12-bit ramp per frame.
func FixturePixels() []byte {
	samples := FixtureFrames * FixtureRows * FixtureColumns
	raw := make([]byte, 2*samples)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(raw[2*i:], uint16((i*37+i/(FixtureRows*FixtureColumns)*1000)%4096))
	}
	return raw
}
