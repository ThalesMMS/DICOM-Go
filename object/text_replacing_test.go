package object_test

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestLookupStringReplacingMarksUndecodableText(t *testing.T) {
	tagPatientID := core.NewTag(0x0010, 0x0020)
	latin1 := object.TextOptions{FallbackCharacterSet: dicomenc.ISOIR100, CharsetFallback: object.FallbackUndeclared}
	for _, tc := range []struct {
		name     string
		elements []core.Element
		options  object.TextOptions
		tag      core.Tag
		want     string
		replaced bool
	}{
		{"decodable text is unchanged", []core.Element{text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, japaneseName)}, object.TextOptions{}, tagPatientName, japaneseName, false},
		{"an authorized fallback still applies", []core.Element{text(tagPatientName, core.VRPN, latinNameISO)}, latin1, tagPatientName, latinName, false},
		{"bytes the declared set rejects", []core.Element{text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, latinNameISO)}, latin1, tagPatientName, "Jos�^�ez", true},
		{"high bytes without a declaration or fallback", []core.Element{text(tagPatientName, core.VRPN, latinNameISO)}, object.TextOptions{}, tagPatientName, "Jos�^�ez", true},
		// Orthanc in JapaneseKanji mode declares a single-valued Code
		// Extension term, which PS3.5 does not allow.
		{"an invalid declaration keeps the default repertoire", []core.Element{text(tagCharset, core.VRCS, "ISO 2022 IR 87"), text(tagPatientName, core.VRPN, japaneseRaw)}, object.TextOptions{}, tagPatientName, "Yamada^Tarou=�^�=�^�", true},
		{"ASCII under an invalid declaration is intact", []core.Element{text(tagCharset, core.VRCS, "ISO 2022 IR 87"), text(tagPatientID, core.VRLO, "PID-1")}, object.TextOptions{}, tagPatientID, "PID-1", false},
		{"an unsupported declaration", []core.Element{text(tagCharset, core.VRCS, "ISO_IR 999"), text(tagPatientName, core.VRPN, latinNameISO)}, object.TextOptions{}, tagPatientName, "Jos�^�ez", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := charsetObject(tc.options, tc.elements...)
			got, replaced, err := obj.LookupStringReplacing(tc.tag)
			if err != nil || got != tc.want || replaced != tc.replaced {
				t.Fatalf("LookupStringReplacing = %q, %t, %v; want %q, %t", got, replaced, err, tc.want, tc.replaced)
			}
		})
	}
}

func TestLookupStringsReplacingKeepsErrorsForMissingAndBinaryElements(t *testing.T) {
	obj := charsetObject(object.TextOptions{}, core.NewRawElement(tagRows, core.VRUS, []byte{1, 0}))
	if _, _, err := obj.LookupStringsReplacing(tagPatientName); err == nil {
		t.Fatal("a missing element returned no error")
	}
	if _, _, err := obj.LookupStringsReplacing(tagRows); err == nil {
		t.Fatal("a binary element returned no error")
	}
	multi := charsetObject(object.TextOptions{}, text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagInstitution, core.VRLO, "A\xff\\B"))
	values, replaced, err := multi.LookupStringsReplacing(tagInstitution)
	if err != nil || !replaced || len(values) != 2 || values[0] != "A�" || values[1] != "B" {
		t.Fatalf("LookupStringsReplacing = %q, %t, %v; want [A� B] true", values, replaced, err)
	}
}
