package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// An undefined-length UN sequence keeps Implicit VR Little Endian items in a
// Big Endian data set (PS3.5 6.2.2). Converting the data set to Little Endian
// and back must keep the nested values and write the items as Implicit VR
// Little Endian again.
func TestTranscodeKeepsUndefinedLengthUNSequenceValues(t *testing.T) {
	var ds bytes.Buffer
	be := func(v ...any) {
		for _, x := range v {
			_ = binary.Write(&ds, binary.BigEndian, x)
		}
	}
	le := func(v ...any) {
		for _, x := range v {
			_ = binary.Write(&ds, binary.LittleEndian, x)
		}
	}
	be(uint16(0x0008), uint16(0x1115))
	ds.WriteString("UN")
	be(uint16(0), uint32(0xFFFFFFFF))
	le(uint16(0xFFFE), uint16(0xE000), uint32(0xFFFFFFFF))
	le(uint16(0x0028), uint16(0x0011), uint32(2), uint16(256))
	le(uint16(0x0028), uint16(0x0030), uint32(4))
	ds.WriteString("0.5 ")
	le(uint16(0xFFFE), uint16(0xE00D), uint32(0))
	le(uint16(0xFFFE), uint16(0xE0DD), uint32(0))
	source := ds.Bytes()

	big, err := object.ReadDataSet(bytes.NewReader(source), transfer.ExplicitVRBigEndian)
	if err != nil {
		t.Fatal(err)
	}
	little, _, err := pixeldata.TranscodeDataSet(context.Background(), big, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := little.GetSequence(core.NewTag(0x0008, 0x1115))
	if !ok || len(items) != 1 {
		t.Fatal("sequence lost")
	}
	if raw, _ := items[0].GetRaw(core.NewTag(0x0028, 0x0011)); !bytes.Equal(raw, []byte{0x00, 0x01}) {
		t.Fatalf("nested Columns after BE->LE = % X, want 00 01", raw)
	}
	back, _, err := pixeldata.TranscodeDataSet(context.Background(), little, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := object.WriteDataSet(&out, back, transfer.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), source) {
		t.Fatalf("BE->LE->BE changed the bytes:\n% X\n% X", out.Bytes(), source)
	}
}
