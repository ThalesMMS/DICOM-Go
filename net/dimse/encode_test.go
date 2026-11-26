package dimse

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeCommandSetUsesImplicitVRLittleEndianGroupLength(t *testing.T) {
	data, err := EncodeCommandSet(CEchoRequest{MessageID: 7}.CommandSet())
	if err != nil {
		t.Fatalf("EncodeCommandSet() error = %v", err)
	}
	if len(data) < 12 {
		t.Fatalf("encoded command set length = %d, want at least 12", len(data))
	}
	if got, want := binary.LittleEndian.Uint16(data[0:2]), uint16(0x0000); got != want {
		t.Fatalf("group length group = 0x%04X, want 0x%04X", got, want)
	}
	if got, want := binary.LittleEndian.Uint16(data[2:4]), uint16(0x0000); got != want {
		t.Fatalf("group length element = 0x%04X, want 0x%04X", got, want)
	}
	if got, want := binary.LittleEndian.Uint32(data[4:8]), uint32(4); got != want {
		t.Fatalf("implicit VR length bytes = %d, want %d", got, want)
	}
	if got, want := binary.LittleEndian.Uint32(data[8:12]), uint32(len(data)-12); got != want {
		t.Fatalf("CommandGroupLength = %d, want %d", got, want)
	}
	if bytes.Contains(data[:12], []byte("UL")) {
		t.Fatal("encoded command group length contains explicit VR bytes; command sets must use Implicit VR Little Endian")
	}

	command, err := DecodeCommandSet(data)
	if err != nil {
		t.Fatalf("DecodeCommandSet() error = %v", err)
	}
	field, err := CommandUint16(command, CommandField)
	if err != nil {
		t.Fatalf("CommandField error = %v", err)
	}
	if field != CEchoRQ {
		t.Fatalf("CommandField = 0x%04X, want 0x%04X", field, CEchoRQ)
	}
	messageID, err := CommandUint16(command, MessageID)
	if err != nil {
		t.Fatalf("MessageID error = %v", err)
	}
	if messageID != 7 {
		t.Fatalf("MessageID = %d, want 7", messageID)
	}
}

// Command UIDs repeat a dataset's identity, which may be a legacy value read
// from a file; forwarding it must not fail on the command copy, which has to
// match the dataset it announces.
func TestEncodeCommandSetCarriesLegacyUIDsUnchanged(t *testing.T) {
	const legacy = "1.2.826.0.1.03.RTPLAN"
	data, err := EncodeCommandSet(CStoreRequest{AffectedSOPClassUID: "1.2.840.10008.5.1.4.1.1.7", AffectedSOPInstanceUID: legacy, MessageID: 3}.CommandSet())
	if err != nil {
		t.Fatalf("EncodeCommandSet() error = %v", err)
	}
	command, err := DecodeCommandSet(data)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ParseCStoreRequest(command)
	if err != nil {
		t.Fatal(err)
	}
	if request.AffectedSOPInstanceUID != legacy || request.AffectedSOPClassUID != "1.2.840.10008.5.1.4.1.1.7" {
		t.Fatalf("decoded request %+v", request)
	}
	if !bytes.Contains(data, append([]byte(legacy), 0)) {
		t.Fatal("odd-length UID not NUL-padded")
	}
}
