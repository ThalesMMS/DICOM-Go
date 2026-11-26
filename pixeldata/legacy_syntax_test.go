package pixeldata_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func leElement(g, e uint16, value []byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, g)
	b = binary.LittleEndian.AppendUint16(b, e)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(value)))
	return append(b, value...)
}

func leUS(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

// A recovered File decodes although File Meta still names the syntax it was
// recovered from; a File whose syntax was changed afterwards does not.
func TestDecodeRecoveredGEPrivateSyntax(t *testing.T) {
	obj := object.New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: obj, TransferSyntax: transfer.ImplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	offset := 132 + 12 + int(binary.LittleEndian.Uint32(data[140:144]))
	data = bytes.Replace(data[:offset], []byte("1.2.840.10008.1.2\x00"), []byte(transfer.PrivateGEImplicitVRBigEndianPixelData.UID), 1)
	for _, el := range [][]byte{
		leElement(0x0028, 0x0002, leUS(1)), leElement(0x0028, 0x0004, []byte("MONOCHROME2 ")),
		leElement(0x0028, 0x0010, leUS(1)), leElement(0x0028, 0x0011, leUS(2)),
		leElement(0x0028, 0x0100, leUS(16)), leElement(0x0028, 0x0101, leUS(16)),
		leElement(0x0028, 0x0102, leUS(15)), leElement(0x0028, 0x0103, leUS(0)),
		leElement(0x7fe0, 0x0010, []byte{0x01, 0x02, 0x03, 0x04}),
	} {
		data = append(data, el...)
	}
	f, _, err := object.ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), object.ReadFileOptions{},
		object.TransferSyntaxRecoveryOptions{AllowLegacySyntaxes: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(got, []byte{0x02, 0x01, 0x04, 0x03}) {
		t.Fatalf("pixel data % X", got)
	}
	f.TransferSyntax = transfer.ExplicitVRLittleEndian
	if _, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{}); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("changed syntax: %v", err)
	}
}
