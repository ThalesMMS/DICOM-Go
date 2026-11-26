package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func implicitLE(group, element uint16, value []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, group)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

// explicitVRs writes obj in Explicit VR Little Endian and reads the VRs back
// from the encoded headers.
func explicitVRs(t *testing.T, obj *object.Object) map[core.Tag]core.VR {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadDataSet(&encoded, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	vrs := map[core.Tag]core.VR{}
	for _, e := range back.Elements() {
		vrs[e.Tag()] = e.VR()
	}
	return vrs
}

// TestImplicitPrivateCreatorTranscodesAsLO transcodes an Implicit VR object
// like gdcmData MR_Philips_Intera_PrivateSequenceImplicitVR to Explicit VR:
// its Private Creator is LO (PS3.5 7.8.1) with or without a private
// dictionary, while an unknown private data element stays UN.
func TestImplicitPrivateCreatorTranscodesAsLO(t *testing.T) {
	source := bytes.Join([][]byte{
		implicitLE(0x0008, 0x0018, []byte("1.2.3.4\x00")),
		implicitLE(0x0028, 0x0002, []byte{1, 0}),
		implicitLE(0x0028, 0x0004, []byte("MONOCHROME2 ")),
		implicitLE(0x0028, 0x0010, []byte{2, 0}),
		implicitLE(0x0028, 0x0011, []byte{2, 0}),
		implicitLE(0x0028, 0x0100, []byte{8, 0}),
		implicitLE(0x0028, 0x0101, []byte{8, 0}),
		implicitLE(0x0028, 0x0102, []byte{7, 0}),
		implicitLE(0x0028, 0x0103, []byte{0, 0}),
		implicitLE(0x2001, 0x0010, []byte("Philips Imaging DD 001")),
		implicitLE(0x2001, 0x1003, []byte{0, 0, 0x80, 0x3f}),
		implicitLE(0x2001, 0x1004, []byte("xy")),
		implicitLE(0x7fe0, 0x0010, []byte{1, 2, 3, 4}),
	}, nil)
	catalog, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{{Creator: "Philips Imaging DD 001", Group: 0x2001, Offset: 0x03, VR: core.VRFL, VM: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dict dictionary.DataDictionary
		data core.VR
	}{
		{"standard dictionary", nil, core.VRUN},
		{"private dictionary", dictionary.Chain{std.Dictionary, catalog}, core.VRFL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := object.ReadDataSetWithOptions(bytes.NewReader(source), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{Dictionary: tc.dict})
			if err != nil {
				t.Fatal(err)
			}
			converted, report, err := TranscodeDataSet(context.Background(), obj, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if report.UNRecovery != nil {
				t.Fatal("UN recovery reported without being requested")
			}
			vrs := explicitVRs(t, converted)
			if vrs[core.NewTag(0x2001, 0x0010)] != core.VRLO || vrs[core.NewTag(0x2001, 0x1003)] != tc.data || vrs[core.NewTag(0x2001, 0x1004)] != core.VRUN {
				t.Fatalf("written VRs: creator %s, (2001,1003) %s, (2001,1004) %s", vrs[core.NewTag(0x2001, 0x0010)], vrs[core.NewTag(0x2001, 0x1003)], vrs[core.NewTag(0x2001, 0x1004)])
			}
		})
	}
}

// TestTranscodeRecoverUNIsOptInAndReported gives known VRs to UN elements
// received in Explicit VR only when asked, and reports each one.
func TestTranscodeRecoverUNIsOptInAndReported(t *testing.T) {
	source := func() *object.Object {
		obj := transcodeNativeObject([]byte{1, 2, 3, 4})
		obj.Put(core.NewRawElement(core.NewTag(0x0020, 0x0013), core.VRUN, []byte("7 ")))
		obj.Put(core.NewRawElement(core.NewTag(0x0019, 0x0010), core.VRUN, []byte("ACME 1")))
		obj.Put(core.NewRawElement(core.NewTag(0x0019, 0x1001), core.VRUN, []byte{1, 2}))
		return obj
	}
	instance, creator, data := core.NewTag(0x0020, 0x0013), core.NewTag(0x0019, 0x0010), core.NewTag(0x0019, 0x1001)
	converted, report, err := TranscodeDataSet(context.Background(), source(), transfer.ExplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if vrs := explicitVRs(t, converted); vrs[instance] != core.VRUN || vrs[creator] != core.VRUN || report.UNRecovery != nil {
		t.Fatalf("UN changed without RecoverUN: %v", vrs)
	}
	converted, report, err = TranscodeDataSet(context.Background(), source(), transfer.ExplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, TranscodeOptions{RecoverUN: &object.UNRecoveryOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	vrs := explicitVRs(t, converted)
	if vrs[instance] != core.VRIS || vrs[creator] != core.VRLO || vrs[data] != core.VRUN {
		t.Fatalf("recovered VRs: %v", vrs)
	}
	if v, _ := converted.GetString(instance); v != "7" {
		t.Fatalf("Instance Number %q", v)
	}
	if report.UNRecovery == nil || report.UNRecovery.Recovered != 2 || report.UNRecovery.Kept != 1 {
		t.Fatalf("report %+v", report.UNRecovery)
	}
	// Strict fails when a known VR cannot be applied (an AT of 2 bytes).
	bad := source()
	bad.Put(core.NewRawElement(core.NewTag(0x0028, 0x0009), core.VRUN, []byte{0x08, 0x00}))
	if _, _, err := TranscodeDataSet(context.Background(), bad, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, TranscodeOptions{RecoverUN: &object.UNRecoveryOptions{Strict: true}}); !errors.Is(err, object.ErrUNRecovery) {
		t.Fatalf("strict recovery of a 2-byte AT: %v", err)
	}
}
