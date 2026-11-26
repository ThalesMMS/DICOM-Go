package dicomjson

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestMarshalReadsBitsAllocatedInTheEffectiveByteOrder checks an object whose
// raw values are Big Endian while its own value byte order is the Little
// Endian default: with Options.ByteOrder Big Endian, Bits Allocated 32 is read
// in that order, not as 8192, so OW Pixel Data is reversed by sample, and
// Unmarshal with that ByteOrder restores it.
func TestMarshalReadsBitsAllocatedInTheEffectiveByteOrder(t *testing.T) {
	little := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	big := valueencode.ReverseFixedWidth(little, 4)
	bits := make([]byte, 2)
	binary.BigEndian.PutUint16(bits, 32)
	obj := object.FromElements([]core.Element{
		core.NewRawElement(valueencode.BitsAllocatedTag, core.VRUS, bits),
		core.NewRawElement(core.TagPixelData, core.VROW, big),
	}, std.Dictionary)
	encoded, err := Marshal(obj, Options{ByteOrder: binary.BigEndian})
	if err != nil {
		t.Fatal(err)
	}
	var dataset Dataset
	if err := json.Unmarshal(encoded, &dataset); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(dataset["7FE00010"].InlineBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, little) {
		t.Fatalf("Pixel Data InlineBinary % X, want % X", got, little)
	}
	decoded, err := UnmarshalWithOptions(encoded, std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := decoded.GetRaw(core.TagPixelData); !bytes.Equal(raw, big) {
		t.Fatalf("round trip Pixel Data % X, want % X", raw, big)
	}
}
