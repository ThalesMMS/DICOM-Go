package printmgmt

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"strings"
	"unicode"
	"unicode/utf8"
)

func supportedAnnotationCharset(cs string) bool {
	return cs == "" || cs == "ISO_IR 6" || cs == "ISO_IR 192"
}

func validNativeText(value, charset string) bool {
	if !supportedAnnotationCharset(charset) || len(value) > 256 || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 64 {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) || c == '\\' || (charset != "ISO_IR 192" && c > 0x7e) {
			return false
		}
	}
	return true
}

func nativeText(ds *object.Object, tag core.Tag, charset string) (string, bool, error) {
	e, present := ds.Get(tag)
	if !present {
		return "", false, nil
	}
	if e.Header.VR != core.VRLO {
		return "", true, invalid(tag)
	}
	var s string
	switch v := e.Value.(type) {
	case core.RawValue:
		if len(v) > 256 {
			return "", true, invalid(tag)
		}
		s = string(v)
	case core.StringValue:
		if len(v) != 1 || len(v[0]) > 256 {
			return "", true, invalid(tag)
		}
		s = v[0]
	case nil:
		if !e.Header.LengthSet || e.Header.Length != 0 {
			return "", true, invalid(tag)
		}
	default:
		return "", true, invalid(tag)
	}
	// Validate before removing semantic padding, including the character limit.
	if !validNativeText(s, charset) {
		return "", true, invalid(tag)
	}
	return strings.TrimRight(s, " "), true, nil
}

func BuildAnnotationBox(position int, textValue, charset string) (*object.Object, error) {
	if position < 1 || position > MaxNativeAnnotationBoxes || !validNativeText(textValue, charset) || (len(textValue)%2 == 1 && utf8.RuneCountInString(textValue) == 64) {
		return nil, invalid(TagAnnotationPosition, TagAnnotationText)
	}
	// Raw UTF-8 bytes use the character set inherited from Film Session; no
	// undeclared per-N-SET charset or default encoder is silently introduced.
	return object.FromElements([]core.Element{us(TagAnnotationPosition, position), core.NewRawElement(TagAnnotationText, core.VRLO, []byte(textValue))}, nil), nil
}
