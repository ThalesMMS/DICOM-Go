package dimse

import "github.com/ThalesMMS/dicom-go/net/ul"

// StorageServiceClassUID identifies the Storage Service Class (PS3.4 Annex B)
// in SOP Class Common Extended Negotiation.
const StorageServiceClassUID = "1.2.840.10008.4.2"

// RelatedGeneralStorageSOPClasses returns the SOP Class Common Extended
// Negotiation sub-item (PS3.7 D.3.3.6) that proposes a new or private Storage
// SOP Class together with its Related General SOP Classes, most specific
// first, so an acceptor that lacks sopClassUID may accept it as one of them
// (PS3.4 B.3.1.4). Put it in ul.DialOptions.CommonExtendedNegotiation, or in
// StoreSessionOptions.DialOptions for a StoreSession.
func RelatedGeneralStorageSOPClasses(sopClassUID string, related ...string) ul.SopClassCommonExtendedNegotiationItem {
	return ul.SopClassCommonExtendedNegotiationItem{
		SopClassUID:                sopClassUID,
		ServiceClassUID:            StorageServiceClassUID,
		RelatedGeneralSOPClassUIDs: append([]string(nil), related...),
	}
}

// commonExtendedNegotiationFor keeps the items whose SOP Class is proposed in
// contexts, so a StoreSession can hold one catalog of items for every
// association it plans.
func commonExtendedNegotiationFor(items []ul.SopClassCommonExtendedNegotiationItem, contexts []ul.PresentationContext) []ul.SopClassCommonExtendedNegotiationItem {
	if len(items) == 0 {
		return nil
	}
	proposed := make(map[string]struct{}, len(contexts))
	for _, context := range contexts {
		proposed[context.AbstractSyntaxUID] = struct{}{}
	}
	var out []ul.SopClassCommonExtendedNegotiationItem
	for _, item := range items {
		if _, ok := proposed[item.SopClassUID]; ok {
			item.RelatedGeneralSOPClassUIDs = append([]string(nil), item.RelatedGeneralSOPClassUIDs...)
			out = append(out, item)
		}
	}
	return out
}
