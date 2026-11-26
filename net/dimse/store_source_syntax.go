package dimse

import (
	"context"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
	"math"
)

func openStoreSourceForContext(ctx context.Context, source StoreSource, assoc *ul.Association, descriptor StoreDescriptor) (opened OpenedStoreSource, pc ul.AcceptedContext, err error) {
	pc, err = AcceptedContextForSOPClassTransferSyntaxes(assoc, descriptor.SOPClassUID, descriptor.WritableTransferSyntaxUIDs)
	if err != nil {
		return opened, pc, err
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		return opened, pc, err
	}
	opened, err = openStoreSourceForSyntax(ctx, source, syntax)
	return opened, pc, err
}

func openStoreSourceForSyntax(ctx context.Context, source StoreSource, syntax transfer.Syntax) (opened OpenedStoreSource, err error) {
	if source == nil {
		return opened, ErrStoreInvalidSource
	}
	defer func() {
		if recover() != nil {
			opened = OpenedStoreSource{}
			err = ErrStoreInvalidSource
		}
	}()
	if selected, ok := source.(StoreTransferSyntaxSource); ok {
		return selected.OpenForTransferSyntax(ctx, syntax)
	}
	return source.Open(ctx)
}

func storeRepresentationSize(descriptor StoreDescriptor, uid string) int64 {
	if descriptor.TransferSyntaxSizes != nil {
		return descriptor.TransferSyntaxSizes[uid]
	}
	return descriptor.Size
}

func storeMinimumRepresentationSize(descriptor StoreDescriptor, offered []string) int64 {
	if descriptor.TransferSyntaxSizes == nil {
		return descriptor.Size
	}
	minimum := int64(math.MaxInt64)
	for _, uid := range offered {
		minimum = min(minimum, descriptor.TransferSyntaxSizes[uid])
	}
	return minimum
}

func (s *StoreSession) selectedStoreRepresentation(ctx context.Context, source StoreSource, assoc *ul.Association, descriptor StoreDescriptor, remainingTotal int64) (opened OpenedStoreSource, pc ul.AcceptedContext, err error) {
	pc, err = AcceptedContextForSOPClassTransferSyntaxes(assoc, descriptor.SOPClassUID, descriptor.WritableTransferSyntaxUIDs)
	if err != nil {
		return opened, pc, err
	}
	bound := storeRepresentationSize(descriptor, pc.TransferSyntaxUID)
	if bound > s.options.PlanOptions.Limits.MaxItemBytes || bound > remainingTotal {
		return opened, pc, ErrStoreResourceLimit
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		return opened, pc, err
	}
	returnOpened, err := openStoreSourceForSyntax(ctx, source, syntax)
	return returnOpened, pc, err
}

func storeWriterRepresentationBound(descriptor StoreDescriptor, uid string) int64 {
	if descriptor.TransferSyntaxSizes == nil {
		return 0 // Legacy Size can be a source-size estimate, not a writer cap.
	}
	return descriptor.TransferSyntaxSizes[uid]
}
