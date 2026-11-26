package ul

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// SopClassCommonExtendedNegotiationItem is the PS3.7 D.3.3.6 SOP Class Common
// Extended Negotiation sub-item (57H), sent only in the A-ASSOCIATE-RQ: for a
// proposed SOP Class it names the Service Class and the Related General SOP
// Classes (PS3.4 B.3.1.4) an acceptor may treat it as. Sub-item version 0 is
// written; a later version's additional fields are ignored on receipt.
type SopClassCommonExtendedNegotiationItem struct {
	SopClassUID                string
	ServiceClassUID            string
	RelatedGeneralSOPClassUIDs []string
}

func (SopClassCommonExtendedNegotiationItem) userVariableItem() {}

// CommonExtendedNegotiationHandler decides whether an acceptor that does not
// support a proposed abstract syntax accepts it as relatedGeneralSOPClassUID,
// a Related General SOP Class from item that the acceptor supports. Related
// classes are offered in the requestor's order; the first accepted one gives
// the context its transfer syntaxes and role policy, and the accepted
// context's AcceptedContext.RelatedGeneralSOPClassUID names it.
type CommonExtendedNegotiationHandler func(item SopClassCommonExtendedNegotiationItem, relatedGeneralSOPClassUID string) bool

// AcceptRelatedGeneralSOPClasses is a CommonExtendedNegotiationHandler that
// accepts every supported Related General SOP Class.
func AcceptRelatedGeneralSOPClasses(SopClassCommonExtendedNegotiationItem, string) bool { return true }

func readSopClassCommonExtendedNegotiation(data []byte) (SopClassCommonExtendedNegotiationItem, error) {
	var item SopClassCommonExtendedNegotiationItem
	r := bytes.NewReader(data)
	sopClassUID, err := readU16LengthUID(r, "SOP class UID")
	if err != nil {
		return item, err
	}
	serviceClassUID, err := readU16LengthUID(r, "service class UID")
	if err != nil {
		return item, err
	}
	relatedLength, err := readU16(r, "related general SOP class identification length")
	if err != nil {
		return item, err
	}
	if int(relatedLength) > r.Len() {
		return item, fmt.Errorf("%w: related general SOP class identification length %d exceeds remaining %d", ErrInvalidUserItem, relatedLength, r.Len())
	}
	related := make([]byte, relatedLength)
	_, _ = r.Read(related)
	relatedReader := bytes.NewReader(related)
	var relatedUIDs []string
	for relatedReader.Len() > 0 {
		uid, err := readU16LengthUID(relatedReader, "related general SOP class UID")
		if err != nil {
			return item, err
		}
		relatedUIDs = append(relatedUIDs, uid)
	}
	// Any remaining bytes are the Reserved field, zero length in version 0
	// and reserved for additional fields of later versions.
	item = SopClassCommonExtendedNegotiationItem{
		SopClassUID:                sopClassUID,
		ServiceClassUID:            serviceClassUID,
		RelatedGeneralSOPClassUIDs: relatedUIDs,
	}
	if err := validateSopClassCommonExtendedNegotiationItem(item); err != nil {
		return SopClassCommonExtendedNegotiationItem{}, err
	}
	return item, nil
}

func readU16(r *bytes.Reader, field string) (uint16, error) {
	if r.Len() < 2 {
		return 0, fmt.Errorf("%w: SOP class common extended negotiation %s truncated", ErrInvalidUserItem, field)
	}
	return dicomenc.NewBasicDecoder(binary.BigEndian).ReadU16(r)
}

func readU16LengthUID(r *bytes.Reader, field string) (string, error) {
	length, err := readU16(r, field+" length")
	if err != nil {
		return "", err
	}
	if int(length) > r.Len() {
		return "", fmt.Errorf("%w: SOP class common extended negotiation %s length %d exceeds remaining %d", ErrInvalidUserItem, field, length, r.Len())
	}
	value := make([]byte, length)
	_, _ = r.Read(value)
	return strings.TrimRight(strings.TrimSpace(string(value)), "\x00"), nil
}

func writeSopClassCommonExtendedNegotiationItem(buf *bytes.Buffer, item *SopClassCommonExtendedNegotiationItem) error {
	if item == nil {
		return fmt.Errorf("%w: nil *SopClassCommonExtendedNegotiationItem", ErrInvalidUserItem)
	}
	if err := validateSopClassCommonExtendedNegotiationItem(*item); err != nil {
		return err
	}
	var related bytes.Buffer
	for _, uid := range item.RelatedGeneralSOPClassUIDs {
		if err := writeU16LengthBytes(&related, []byte(uid)); err != nil {
			return err
		}
	}
	if related.Len() > math.MaxUint16 {
		return fmt.Errorf("%w: related general SOP class identification length %d exceeds uint16", ErrLengthOverflow, related.Len())
	}
	var data bytes.Buffer
	if err := writeU16LengthBytes(&data, []byte(item.SopClassUID)); err != nil {
		return err
	}
	if err := writeU16LengthBytes(&data, []byte(item.ServiceClassUID)); err != nil {
		return err
	}
	if err := writeU16LengthBytes(&data, related.Bytes()); err != nil {
		return err
	}
	// The sub-item header's second byte is the Sub-Item-version, 0 here.
	return writeItem(buf, SubItemSopClassCommonExtendedNegotiation, data.Bytes())
}

func validateSopClassCommonExtendedNegotiationItem(item SopClassCommonExtendedNegotiationItem) error {
	if item.SopClassUID == "" {
		return fmt.Errorf("%w: common extended negotiation SOP class UID", ErrMissingPDUField)
	}
	if item.ServiceClassUID == "" {
		return fmt.Errorf("%w: common extended negotiation service class UID for SOP class %q", ErrMissingPDUField, item.SopClassUID)
	}
	for _, uid := range append([]string{item.SopClassUID, item.ServiceClassUID}, item.RelatedGeneralSOPClassUIDs...) {
		if uid == "" {
			return fmt.Errorf("%w: empty related general SOP class UID for SOP class %q", ErrInvalidUserItem, item.SopClassUID)
		}
		if len(uid) > math.MaxUint16 {
			return fmt.Errorf("%w: common extended negotiation UID length %d exceeds uint16", ErrLengthOverflow, len(uid))
		}
	}
	return nil
}

func asSopClassCommonExtendedNegotiationItem(item UserVariableItem) (SopClassCommonExtendedNegotiationItem, bool) {
	switch it := item.(type) {
	case SopClassCommonExtendedNegotiationItem:
		return it, true
	case *SopClassCommonExtendedNegotiationItem:
		if it != nil {
			return *it, true
		}
	}
	return SopClassCommonExtendedNegotiationItem{}, false
}

// commonExtendedNegotiationFromUserInfo returns the sub-items of an
// A-ASSOCIATE-RQ; PS3.7 D.3.3.6 allows one per SOP Class.
func commonExtendedNegotiationFromUserInfo(items []UserVariableItem) ([]SopClassCommonExtendedNegotiationItem, error) {
	var out []SopClassCommonExtendedNegotiationItem
	seen := make(map[string]struct{})
	for _, item := range items {
		common, ok := asSopClassCommonExtendedNegotiationItem(item)
		if !ok {
			continue
		}
		if err := validateSopClassCommonExtendedNegotiationItem(common); err != nil {
			return nil, err
		}
		if _, ok := seen[common.SopClassUID]; ok {
			return nil, fmt.Errorf("%w: duplicate common extended negotiation for SOP class %q", ErrInvalidUserItem, common.SopClassUID)
		}
		seen[common.SopClassUID] = struct{}{}
		out = append(out, cloneSopClassCommonExtendedNegotiationItem(common))
	}
	return out, nil
}

func cloneSopClassCommonExtendedNegotiationItem(item SopClassCommonExtendedNegotiationItem) SopClassCommonExtendedNegotiationItem {
	item.RelatedGeneralSOPClassUIDs = append([]string(nil), item.RelatedGeneralSOPClassUIDs...)
	return item
}

func cloneSopClassCommonExtendedNegotiationItems(items []SopClassCommonExtendedNegotiationItem) []SopClassCommonExtendedNegotiationItem {
	if items == nil {
		return nil
	}
	out := make([]SopClassCommonExtendedNegotiationItem, len(items))
	for i, item := range items {
		out[i] = cloneSopClassCommonExtendedNegotiationItem(item)
	}
	return out
}

// relatedGeneralSOPClassFor returns the Related General SOP Class under which
// the acceptor takes an abstract syntax it does not support: the first one
// in the requestor's order that the acceptor supports (by rule or in
// SupportedAbstractSyntaxes) and the handler accepts.
func relatedGeneralSOPClassFor(abstractSyntaxUID string, common []SopClassCommonExtendedNegotiationItem, opts AcceptOptions) (string, bool) {
	if opts.CommonExtendedNegotiationHandler == nil {
		return "", false
	}
	for _, item := range common {
		if item.SopClassUID != abstractSyntaxUID {
			continue
		}
		for _, general := range item.RelatedGeneralSOPClassUIDs {
			_, ruled := presentationContextRule(opts, general)
			if !ruled && !containsString(opts.SupportedAbstractSyntaxes, general) {
				continue
			}
			if opts.CommonExtendedNegotiationHandler(cloneSopClassCommonExtendedNegotiationItem(item), general) {
				return general, true
			}
		}
		return "", false
	}
	return "", false
}
