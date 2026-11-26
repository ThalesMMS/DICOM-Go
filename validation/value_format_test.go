package validation_test

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/validation"
)

// TestValidateValueFormatsFromDCMTKCrossCheck pins the PS3.5 rules added when
// the DCMTK checkStringValue and UI vectors were replayed (#1053).
func TestValidateValueFormatsFromDCMTKCrossCheck(t *testing.T) {
	tag := core.NewTag(0x0011, 0x3010)
	tests := []struct {
		name    string
		element core.Element
		code    validation.Code
		valid   bool
	}{
		{name: "AE solely spaces", element: textElement(tag, core.VRAE, " "), code: validation.CodeValueFormat},
		{name: "AE padded title", element: textElement(tag, core.VRAE, " AE Title "), valid: true},
		{name: "UR trailing space", element: textElement(tag, core.VRUR, "http://example.org/ "), valid: true},
		{name: "UR percent-encoded", element: textElement(tag, core.VRUR, "http://example.org/a%20b"), valid: true},
		{name: "UR relative reference", element: textElement(tag, core.VRUR, "../a?b=c#d"), valid: true},
		{name: "UR leading space", element: textElement(tag, core.VRUR, " http://example.org/"), code: validation.CodeValueFormat},
		{name: "UR embedded space", element: textElement(tag, core.VRUR, "http://example.org/a b"), code: validation.CodeValueFormat},
		{name: "UR backslash", element: textElement(tag, core.VRUR, `http:\\example.org`), code: validation.CodeValueFormat},
		{name: "UR truncated percent", element: textElement(tag, core.VRUR, "http://example.org/%2"), code: validation.CodeValueFormat},
		{name: "TM leading space", element: textElement(tag, core.VRTM, " 1215"), code: validation.CodeValueFormat},
		{name: "TM trailing space", element: textElement(tag, core.VRTM, "1215 "), valid: true},
		{name: "DA leading space", element: core.NewRawElement(tag, core.VRDA, []byte(" 20260604 ")), code: validation.CodeValueFormat},
		{name: "DT leading space", element: textElement(tag, core.VRDT, " 2026"), code: validation.CodeValueFormat},
		{name: "UI one NUL", element: core.NewRawElement(tag, core.VRUI, []byte("1.2.3.4\x00")), valid: true},
		{name: "UI two NULs", element: core.NewRawElement(tag, core.VRUI, []byte("7.8.9.10\x00\x00")), code: validation.CodeValuePadding},
		{name: "UI odd space padding", element: core.NewRawElement(tag, core.VRUI, []byte("1.2.3.4  ")), code: validation.CodeValuePadding},
		{name: "UI space before NUL", element: core.NewRawElement(tag, core.VRUI, []byte("1.2.3 \x00")), code: validation.CodeValuePadding},
		// #2032: readers strip NUL padding from every text VR; validation
		// still reports it for the VRs PS3.5 pads with spaces.
		{name: "CS NUL padding", element: core.NewRawElement(tag, core.VRCS, []byte("PATIENT\x00")), code: validation.CodeValuePadding},
		{name: "CS NUL before space padding", element: core.NewRawElement(tag, core.VRCS, []byte("CT\x00 ")), code: validation.CodeValuePadding},
		{name: "CS NUL-padded first value", element: core.NewRawElement(tag, core.VRCS, []byte("CT\x00\\MR")), code: validation.CodeValuePadding},
		{name: "LO odd NUL padding", element: core.NewRawElement(tag, core.VRLO, []byte("ABC\x00\x00")), code: validation.CodeValuePadding},
		{name: "IS NUL padding", element: core.NewRawElement(tag, core.VRIS, []byte("12\x00 ")), code: validation.CodeValuePadding},
		{name: "LT NUL padding", element: core.NewRawElement(tag, core.VRLT, []byte("  text\x00 ")), code: validation.CodeValuePadding},
		{name: "CS space padding", element: core.NewRawElement(tag, core.VRCS, []byte("CT  ")), valid: true},
		// PS3.5 Table 6.2-1 limits count a value's own leading and trailing
		// spaces; only the even-length padding byte is left out.
		{name: "DS 16 bytes", element: core.NewRawElement(tag, core.VRDS, []byte("1.23456789012345")), valid: true},
		{name: "DS spaces and padding within 16", element: core.NewRawElement(tag, core.VRDS, []byte("  1.234567890123")), valid: true},
		{name: "DS spaces beyond 16", element: core.NewRawElement(tag, core.VRDS, []byte("   1.234567890123 ")), code: validation.CodeValueLength},
		{name: "DS second value too long", element: core.NewRawElement(tag, core.VRDS, []byte(`1\  1234567890123456 `)), code: validation.CodeValueLength},
		{name: "AE padded beyond 16", element: core.NewRawElement(tag, core.VRAE, []byte("  AE_TITLE_12345 ")), code: validation.CodeValueLength},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := validation.ValidateElement(context.Background(), test.element, validation.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if test.valid {
				if len(report.Findings) != 0 {
					t.Fatalf("findings %#v, want none", report.Findings)
				}
				return
			}
			if report.Count(test.code) == 0 {
				t.Fatalf("findings %#v, want %s", report.Findings, test.code)
			}
			// Padding is reported as padding, not as a repertoire violation.
			if test.code == validation.CodeValuePadding && report.Count(validation.CodeValueRepertoire) != 0 {
				t.Fatalf("findings %#v, want no %s", report.Findings, validation.CodeValueRepertoire)
			}
		})
	}
}
