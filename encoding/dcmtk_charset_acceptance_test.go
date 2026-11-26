package encoding_test

import (
	"testing"

	dicomencoding "github.com/ThalesMMS/dicom-go/encoding"
)

// The GB2312 row 1 fixture from DCMTK oficonv assigns these two cells
// different scalars than the corresponding GBK cells.
func TestReferenceAcceptanceIR58Punctuation(t *testing.T) {
	charset, err := dicomencoding.ParseCharacterSet("", "ISO 2022 IR 58")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		wire string
		want string
	}{
		{"katakana middle dot", "\x1b$)A\xa1\xa4", "\u30fb"},
		{"horizontal bar", "\x1b$)A\xa1\xaa", "\u2015"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDCMTKCharsetMapping(t, charset, []byte(tc.wire), tc.want)
		})
	}
}
