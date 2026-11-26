package quirkcorpus

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// CheckRetainedValues checks authored source values before any decoder runs;
// the preservation comparison must not simply bless whatever the parser gave us.
func CheckRetainedValues(file *object.File, f Fixture) error {
	check := func(ds *object.Object, t core.Tag, vr core.VR, want []byte) error {
		e, ok := ds.Get(t)
		if !ok || e.VR() != vr {
			return fmt.Errorf("preservation: %s missing or wrong VR", t)
		}
		raw, ok := e.RawBytes()
		if !ok || !bytes.Equal(raw, want) {
			return fmt.Errorf("preservation: %s authored bytes changed", t)
		}
		return nil
	}
	if f.ID == "VRUNInMetaHeader" {
		if e := check(file.Meta, tag(0x0002, 0x0002), core.VRUN, []byte(scUID+"\x00")); e != nil {
			return e
		}
	}
	switch f.ID {
	case "ElemZeroNotGroupLength":
		if e := check(file.Dataset, tag(0x0020, 0), core.VRLO, []byte("NOTGL ")); e != nil {
			return e
		}
	case "FLAIR-wrong-group-length":
		if e := check(file.Dataset, tag(0x0028, 0), core.VRUL, []byte{0x78, 0x56, 0x34, 0x12}); e != nil {
			return e
		}
	case "RMI_Mattes_unevenLengthTags":
		if e := check(file.Dataset, tag(0x0011, 0x1010), core.VRLO, []byte("ODD")); e != nil {
			return e
		}
	case "OverlayPrivateDataInBetween":
		if e := check(file.Dataset, tag(0x6001, 0x1001), core.VROB, []byte{1, 2, 3, 4}); e != nil {
			return e
		}
	}
	for i := 0; i < f.Oracle.OverlayGroups; i++ {
		g := uint16(0x6000 + 2*i)
		if e := check(file.Dataset, tag(g, 0x3000), core.VROW, []byte{byte(1 << uint(i%4)), 0}); e != nil {
			return e
		}
	}
	if !file.TransferSyntax.Encapsulated {
		t := core.TagPixelData
		if f.ID == "SinglePrecisionSC" {
			t = tag(0x7fe0, 0x0008)
		}
		if e, ok := file.Dataset.Get(t); ok {
			want, err := hex.DecodeString(f.Oracle.PixelHex)
			if err != nil {
				return err
			}
			if f.ID == "PhilipsByteSwapping" {
				want = []byte{0, 0, 0, 64, 0, 128, 0, 255}
			}
			raw, ok := e.RawBytes()
			if !ok || !bytes.Equal(raw, want) {
				return fmt.Errorf("preservation: source pixels differ from authored carrier")
			}
		}
	}
	return nil
}
