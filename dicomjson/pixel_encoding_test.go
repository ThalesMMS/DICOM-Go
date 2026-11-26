package dicomjson

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestEncapsulatedInlineBinaryRequiresTransferSyntax(t *testing.T) {
	for _, data := range [][]byte{
		{0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0},
		{0xff, 0xfe, 0xe0, 0x00, 0, 0, 0, 0},
	} {
		doc := []byte(`{"7FE00010":{"vr":"OB","InlineBinary":"` + base64.StdEncoding.EncodeToString(data) + `"}}`)
		for _, syntax := range []transfer.Syntax{{}, transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
			_, err := UnmarshalWithOptions(doc, nil, UnmarshalOptions{TransferSyntax: syntax})
			if !errors.Is(err, object.ErrPixelDataEncodingMismatch) || !strings.Contains(err.Error(), "7FE00010/InlineBinary") {
				t.Fatalf("syntax %s: %v", syntax.UID, err)
			}
		}
	}
	if _, err := Unmarshal([]byte(`{"7FE00010":{"vr":"OB","InlineBinary":"AQIDBA=="}}`), nil); err != nil {
		t.Fatalf("native InlineBinary: %v", err)
	}
}
