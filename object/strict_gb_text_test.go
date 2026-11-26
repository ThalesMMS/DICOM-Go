package object_test

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
)

// The strict default documented in CHARACTER_SETS.md requires an error
// when a value ends in an incomplete character in its declared repertoire.
func TestStrictGBTextRejectsIncompleteCharacters(t *testing.T) {
	for _, declaration := range []string{"GBK", "GB18030"} {
		t.Run(declaration, func(t *testing.T) {
			valid := charsetObject(object.TextOptions{},
				text(tagCharset, core.VRCS, declaration),
				text(tagStudyDesc, core.VRLO, "S\xa1\xa1X"),
			)
			if got, err := valid.LookupString(tagStudyDesc); err != nil || got != "S\u3000X" {
				t.Fatalf("complete value decoded as %q, error %v, want S followed by U+3000 and X", got, err)
			}

			for _, test := range []struct {
				raw    string
				offset int
			}{
				{"S\x82", 1},
				{"S\xa1\xa1\x82", 3},
				{"S\x82\x31", 1},
				{"S\x82\x31\x81", 1},
			} {
				truncated := charsetObject(object.TextOptions{},
					text(tagCharset, core.VRCS, declaration),
					text(tagStudyDesc, core.VRLO, test.raw),
				)
				got, err := truncated.LookupString(tagStudyDesc)
				var decodeErr *dicomenc.DecodeError
				if got != "" || !errors.As(err, &decodeErr) || decodeErr.Offset != test.offset || decodeErr.Byte != 0x82 {
					t.Fatalf("incomplete value %x decoded as %q, error %v, want a decoding error at byte %d", test.raw, got, err, test.offset)
				}
			}

			recovered := charsetObject(object.TextOptions{
				FallbackCharacterSet: dicomenc.ISOIR100,
				CharsetFallback:      object.FallbackUndecodable,
			}, text(tagCharset, core.VRCS, declaration), text(tagStudyDesc, core.VRLO, "S\x82"))
			if got, err := recovered.LookupString(tagStudyDesc); err != nil || got != "S\u0082" {
				t.Fatalf("authorized fallback decoded as %q, error %v, want Latin-1 value", got, err)
			}
		})
	}
}

func TestGB18030DecodesEncodedReplacementCharacter(t *testing.T) {
	obj := charsetObject(object.TextOptions{},
		text(tagCharset, core.VRCS, "GB18030"),
		text(tagStudyDesc, core.VRLO, "S\x84\x31\xa4\x37X"),
	)
	if got, err := obj.LookupString(tagStudyDesc); err != nil || got != "S\ufffdX" {
		t.Fatalf("valid U+FFFD spelling decoded as %q, error %v", got, err)
	}
}
