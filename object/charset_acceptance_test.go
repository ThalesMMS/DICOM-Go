package object_test

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// The strict default documented in CHARACTER_SETS.md requires an error
// when a value ends in an incomplete character in its declared repertoire.
func TestReferenceAcceptanceTruncatedChineseText(t *testing.T) {
	for _, declaration := range []string{"GBK", "GB18030"} {
		t.Run(declaration, func(t *testing.T) {
			valid := charsetObject(object.TextOptions{},
				text(tagCharset, core.VRCS, declaration),
				text(tagStudyDesc, core.VRLO, "S\xa1\xa1X"),
			)
			if got, err := valid.LookupString(tagStudyDesc); err != nil || got != "S\u3000X" {
				t.Fatalf("complete value decoded as %q, error %v, want S followed by U+3000 and X", got, err)
			}

			truncated := charsetObject(object.TextOptions{},
				text(tagCharset, core.VRCS, declaration),
				text(tagStudyDesc, core.VRLO, "S\x82"),
			)
			if got, err := truncated.LookupString(tagStudyDesc); err == nil || got != "" {
				t.Fatalf("incomplete value decoded as %q, error %v, want a decoding error and no text", got, err)
			}
		})
	}
}
