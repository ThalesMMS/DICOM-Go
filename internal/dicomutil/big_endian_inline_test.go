package dicomutil

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dicomutil -json writes the InlineBinary of an Explicit VR Big Endian
// palette (OW) and of OF, OD, OL and OV values in Little Endian, as for the
// Explicit VR Little Endian object with the same values (#1982).
func TestRunJSONBigEndianInlineBinaryIsLittleEndian(t *testing.T) {
	values := []struct {
		tag core.Tag
		vr  core.VR
	}{
		{core.NewTag(0x0028, 0x1201), core.VROW},
		{core.NewTag(0x0066, 0x0016), core.VROF},
		{core.NewTag(0x0066, 0x0022), core.VROD},
		{core.NewTag(0x0066, 0x0040), core.VROL},
		{core.NewTag(0x0072, 0x0081), core.VROV},
	}
	little := make([]byte, 16)
	for i := range little {
		little[i] = byte(i + 1)
	}
	dump := func(syntax transfer.Syntax) map[string]any {
		t.Helper()
		elements := dicomtest.MinimalDataset()
		for _, value := range values {
			data := little
			if syntax.UID == transfer.ExplicitVRBigEndian.UID {
				data = valueencode.ReverseFixedWidth(little, valueencode.ByteOrderWidth(value.vr))
			}
			elements = append(elements, core.NewRawElement(value.tag, value.vr, data))
		}
		file, err := dicomtest.Part10File(syntax, elements...)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"-path", writeFixturePath(t, file), "-json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("Run() exit code = %d, stderr=%q", code, stderr.String())
		}
		var got fileJSON
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.DataSet
	}
	want, got := dump(transfer.ExplicitVRLittleEndian), dump(transfer.ExplicitVRBigEndian)
	for _, value := range values {
		key := value.tag.HexString()
		wantInline, _ := want[key].(map[string]any)["InlineBinary"].(string)
		gotInline, _ := got[key].(map[string]any)["InlineBinary"].(string)
		decoded, err := base64.StdEncoding.DecodeString(gotInline)
		if wantInline == "" || gotInline != wantInline || err != nil || !bytes.Equal(decoded, little) {
			t.Errorf("%s %s InlineBinary %q (% X), want the Little Endian %q", key, value.vr, gotInline, decoded, wantInline)
		}
	}
}
