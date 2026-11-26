package dimse

import (
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// A profile's syntax offers are independent of the source representation cap.
// 256 covers the official catalogs while bounding pre-copy validation.
const maxStoreProfileTransferSyntaxes = 256

func cloneStoreProfileContexts(contexts []ul.PresentationContext) ([]ul.PresentationContext, error) {
	if contexts == nil {
		return nil, nil
	}
	if len(contexts) == 0 || len(contexts) > ul.MaxPresentationContexts {
		return nil, ErrStoreInvalidOptions
	}
	var seen [256]bool
	for index, pc := range contexts {
		// UL assigns sequential odd IDs at proposal construction. Requiring
		// that canonical template here prevents silent wire renumbering.
		if pc.ID != byte(2*index+1) || seen[pc.ID] || !core.IsValidUID(pc.AbstractSyntaxUID) || len(pc.TransferSyntaxUIDs) == 0 || len(pc.TransferSyntaxUIDs) > maxStoreProfileTransferSyntaxes {
			return nil, ErrStoreInvalidOptions
		}
		seen[pc.ID] = true
		wire := 8 + len(pc.AbstractSyntaxUID)
		for i, ts := range pc.TransferSyntaxUIDs {
			if !core.IsValidUID(ts) {
				return nil, ErrStoreInvalidOptions
			}
			for _, prior := range pc.TransferSyntaxUIDs[:i] {
				if prior == ts {
					return nil, ErrStoreInvalidOptions
				}
			}
			wire += 4 + len(ts)
		}
		if wire > math.MaxUint16 {
			return nil, ErrStoreInvalidOptions
		}
	}
	return cloneStorePresentationContexts(contexts), nil
}

func storeProfileContextID(contexts []ul.PresentationContext, descriptor StoreDescriptor) (byte, bool) {
	for _, pc := range contexts {
		if pc.AbstractSyntaxUID != descriptor.SOPClassUID {
			continue
		}
		for _, offered := range pc.TransferSyntaxUIDs {
			for _, producible := range descriptor.WritableTransferSyntaxUIDs {
				if offered == producible {
					return pc.ID, true
				}
			}
		}
	}
	return 0, false
}
