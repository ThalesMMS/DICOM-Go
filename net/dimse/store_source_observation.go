package dimse

import (
	"context"
	"sync"

	"github.com/ThalesMMS/dicom-go/index"
)

// StorePreparedObservation correlates metadata from a validated opened source
// with one attempt on the real accepted association. Descriptor is detached and
// can contain sensitive identity or origin values; observers must not log it.
// ReadMetadata is optional and valid only during OnStorePrepared. Preparation
// does not prove transmission or a successful remote response.
type StorePreparedObservation struct {
	AssociationID               string
	SourceIndex                 int
	AssociationIndex            int
	Attempt                     int
	Descriptor                  StoreDescriptor
	NegotiatedTransferSyntaxUID string
	ReadMetadata                func(context.Context, index.Options) (index.Result, error)
}

func (s *StoreSession) observeStorePrepared(ctx context.Context, item StoreItemResult, opened OpenedStoreSource, syntaxUID string) {
	if s.options.OnStorePrepared == nil {
		return
	}
	observation := StorePreparedObservation{
		AssociationID: item.AssociationID, SourceIndex: item.SourceIndex,
		AssociationIndex: item.AssociationIndex, Attempt: item.Attempt,
		Descriptor: cloneStoreDescriptor(item.Descriptor), NegotiatedTransferSyntaxUID: syntaxUID,
	}
	var mu sync.Mutex
	active := true
	if opened.ReadMetadata != nil {
		observation.ReadMetadata = func(readCtx context.Context, options index.Options) (index.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if !active {
				return index.Result{}, ErrStoreInvalidSource
			}
			return opened.ReadMetadata(readCtx, options)
		}
	}
	defer func() {
		mu.Lock()
		active = false
		mu.Unlock()
		_ = recover()
	}()
	s.options.OnStorePrepared(ctx, observation)
}
