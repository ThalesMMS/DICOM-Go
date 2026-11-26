package object

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// LookupStringReplacing reads the first value of tag like LookupString, for a
// reader that must show every value it received. Where strict decoding, and
// any fallback TextOptions authorizes, fails on the value's bytes (an invalid
// or unsupported Specific Character Set, or bytes the declared set rejects),
// it returns the text that can be read with one
// encoding.ReplacementCharacter for each undecodable run, and replaced true,
// instead of an error. A missing or non-textual element is still an error.
func (o *Object) LookupStringReplacing(tag core.Tag) (value string, replaced bool, err error) {
	values, replaced, err := o.LookupStringsReplacing(tag)
	if err != nil || len(values) == 0 {
		return "", replaced, err
	}
	return values[0], replaced, nil
}

// LookupStringsReplacing is LookupStringReplacing for every value of tag.
func (o *Object) LookupStringsReplacing(tag core.Tag) (values []string, replaced bool, err error) {
	elem, ok := o.Get(tag)
	if !ok {
		return nil, false, fmt.Errorf("dicom: missing element %s", tag)
	}
	values, err = o.decodeTextValues(elem)
	if err == nil {
		return append([]string(nil), values...), false, nil
	}
	raw, ok := elem.Value.(core.RawValue)
	if !ok || !elem.VR().IsStringLike() {
		return nil, false, err
	}
	vr := elem.VR()
	text := core.TrimTextValueBytes(vr, raw.Bytes())
	var decoded string
	charset, charsetErr := o.characterSetForVR(vr)
	switch {
	case charsetErr != nil || dictionary.IsPrivateCreatorTag(elem.Tag()):
		decoded, replaced = dicomenc.ReadDefaultRepertoireReplacing(text)
	case vr == core.VRPN:
		decoded, replaced = charset.DecodePersonNameReplacing(text)
	case !vr.UsesTextValueDelimiter():
		decoded, replaced = charset.DecodeSingleValueReplacing(text)
	default:
		decoded, replaced = charset.DecodeReplacing(text)
	}
	return core.SplitTextMultiplicity(vr, decoded), replaced, nil
}
