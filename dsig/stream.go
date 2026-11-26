package dsig

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagItem                   = core.NewTag(0xFFFE, 0xE000)
	tagItemDelimitation       = core.NewTag(0xFFFE, 0xE00D)
	tagSequenceDelimitation   = core.NewTag(0xFFFE, 0xE0DD)
	tagLengthToEnd            = core.NewTag(0x0008, 0x0001)
	tagDataSetTrailingPadding = core.NewTag(0xFFFC, 0xFFFC)
)

// macTransferSyntax resolves a MAC Calculation Transfer Syntax UID: Implicit
// or Explicit VR Little Endian, or an encapsulated syntax (whose byte stream
// is Explicit VR Little Endian with the Pixel Data fragments as stored). Big
// endian and deflated syntaxes are not supported, since raw binary values are
// kept in little endian order.
func macTransferSyntax(uid string) (transfer.Syntax, error) {
	syntax, ok := transfer.DefaultRegistry.Get(uid)
	if !ok || !syntax.IsLittleEndian() || syntax.Deflated || (!syntax.ExplicitVR && syntax.UID != transfer.ImplicitVRLittleEndian.UID) {
		return transfer.Syntax{}, fmt.Errorf("%w: MAC Calculation Transfer Syntax %q", ErrUnsupported, uid)
	}
	return syntax, nil
}

// ErrEncapsulatedMACSyntax reports encapsulated Pixel Data signed with a
// native MAC Calculation Transfer Syntax: PS3.15 computes the MAC of
// encapsulated data in the transfer syntax it is stored in, and DCMTK refuses
// the native form.
var ErrEncapsulatedMACSyntax = errors.New("dicom dsig: encapsulated Pixel Data needs an encapsulated MAC Calculation Transfer Syntax")

// sortedElements returns elements in ascending tag order, the data set order
// of the byte stream, whatever order they were added in.
func sortedElements(elements []core.Element) []core.Element {
	out := append([]core.Element(nil), elements...)
	sort.SliceStable(out, func(i, j int) bool { return tagLess(out[i].Tag(), out[j].Tag()) })
	return out
}

// excluded reports the tags PS3.3 C.12.1.1.3.1.1 bars from Data Elements
// Signed: group lengths, Length to End, groups below 0008, UN elements,
// sequences holding UN, group FFFA, the MAC Parameters Sequence and Data Set
// Trailing Padding.
func excluded(element core.Element) bool {
	tag := element.Tag()
	switch {
	case tag.Element == 0, tag == tagLengthToEnd, tag.Group < 0x0008, tag.Group == 0xFFFA,
		tag == tagMACParametersSequence, tag == tagDataSetTrailingPadding, tag == tagItemDelimitation:
		return true
	}
	return containsUN(element)
}

func containsUN(element core.Element) bool {
	if element.VR() == core.VRUN {
		return true
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok {
		return false
	}
	for _, item := range sequence.Items {
		for _, nested := range item.Elements {
			if containsUN(nested) {
				return true
			}
		}
	}
	return false
}

// streamWriter encodes elements into the MAC byte stream.
type streamWriter struct {
	w      io.Writer
	syntax transfer.Syntax
}

func (s streamWriter) tag(tag core.Tag) error {
	var buf [4]byte
	binary.LittleEndian.PutUint16(buf[0:], uint16(tag.Group))
	binary.LittleEndian.PutUint16(buf[2:], uint16(tag.Element))
	_, err := s.w.Write(buf[:])
	return err
}

// header writes the Tag, VR and reserved field of a sequence or undefined
// length OB, without the Value Length.
func (s streamWriter) header(tag core.Tag, vr core.VR) error {
	if err := s.tag(tag); err != nil {
		return err
	}
	if !s.syntax.ExplicitVR {
		return nil
	}
	_, err := s.w.Write([]byte{vr[0], vr[1], 0, 0})
	return err
}

// sequenceEnd adds the Sequence Delimitation Item Tag, which the byte stream
// carries whether or not the data set had one; like the Item Tags, it goes in
// without a Value Length (DCMTK dcmsign agrees).
func (s streamWriter) sequenceEnd() error {
	return s.tag(tagSequenceDelimitation)
}

func (s streamWriter) element(element core.Element) error {
	switch value := element.Value.(type) {
	case core.SequenceValue:
		if err := s.header(element.Tag(), core.VRSQ); err != nil {
			return err
		}
		for _, item := range value.Items {
			if err := s.tag(tagItem); err != nil {
				return err
			}
			for _, nested := range sortedElements(item.Elements) {
				if excluded(nested) {
					continue
				}
				if err := s.element(nested); err != nil {
					return err
				}
			}
		}
		return s.sequenceEnd()
	case core.FragmentSequence:
		if !s.syntax.Encapsulated {
			return ErrEncapsulatedMACSyntax
		}
		if err := s.header(element.Tag(), element.VR()); err != nil {
			return err
		}
		if err := s.tag(tagItem); err != nil {
			return err
		}
		if _, err := s.w.Write(value.OffsetTable); err != nil {
			return err
		}
		for _, fragment := range value.Fragments {
			if err := s.tag(tagItem); err != nil {
				return err
			}
			if _, err := s.w.Write(fragment); err != nil {
				return err
			}
		}
		return s.sequenceEnd()
	default:
		return parser.NewWriter(s.w, s.syntax).WriteElement(element)
	}
}

// writeSignedStream writes the MAC byte stream of a signature: the listed
// elements of the data set in data set order, then the signature item's
// elements other than the certificate, signature and certified timestamp.
func writeSignedStream(w io.Writer, dataSet []core.Element, signed []core.Tag, signatureItem []core.Element, syntax transfer.Syntax) error {
	listed := make(map[core.Tag]bool, len(signed))
	for _, tag := range signed {
		listed[tag] = true
	}
	stream := streamWriter{w: w, syntax: syntax}
	for _, element := range sortedElements(dataSet) {
		if !listed[element.Tag()] || excluded(element) {
			continue
		}
		if err := stream.element(element); err != nil {
			return fmt.Errorf("dicom dsig: encode %s: %w", element.Tag(), err)
		}
	}
	for _, element := range sortedElements(signatureItem) {
		switch element.Tag() {
		case tagCertificateOfSigner, tagSignature, tagCertifiedTimestampType, tagCertifiedTimestamp:
			continue
		}
		if err := stream.element(element); err != nil {
			return fmt.Errorf("dicom dsig: encode %s: %w", element.Tag(), err)
		}
	}
	return nil
}
