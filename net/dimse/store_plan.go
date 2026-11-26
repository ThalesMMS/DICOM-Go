package dimse

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"syscall"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrStoreInvalidOptions              = errors.New("dicom dimse: invalid C-STORE session options")
	ErrStoreInvalidSource               = errors.New("dicom dimse: invalid C-STORE source")
	ErrStoreResourceLimit               = errors.New("dicom dimse: C-STORE resource limit exceeded")
	ErrStoreSourceChanged               = errors.New("dicom dimse: C-STORE source changed")
	ErrStoreTransferSyntax              = errors.New("dicom dimse: C-STORE transfer syntax is not writable")
	ErrStoreTransferSyntaxPolicy        = errors.New("dicom dimse: no writable C-STORE transfer syntax in the allowlist")
	ErrStorePresentationContextRejected = errors.New("dicom dimse: C-STORE presentation context was not accepted")
	ErrStoreAssociation                 = errors.New("dicom dimse: C-STORE association failed")
	ErrStoreRemoteFailure               = errors.New("dicom dimse: remote C-STORE failure")
	ErrStoreUncertain                   = errors.New("dicom dimse: C-STORE outcome is uncertain")
	// ErrStoreTimeout accompanies ErrStoreUncertain when the wait for the
	// peer timed out, so a caller can name the cause without the transport
	// error text.
	ErrStoreTimeout       = errors.New("dicom dimse: C-STORE timed out waiting for the peer")
	ErrStoreSessionClosed = errors.New("dicom dimse: C-STORE session is closed")
	ErrStoreCallback      = errors.New("dicom dimse: C-STORE progress callback failed")
)

const (
	DefaultStoreMaxItems                  = 100_000
	DefaultStoreMaxItemBytes        int64 = 64 << 30
	DefaultStoreMaxTotalBytes       int64 = 1 << 40
	DefaultStoreMaxAssociations           = 1024
	DefaultStoreMaxTransferSyntaxes       = 16
)

// StoreError is a value- and path-free error suitable for logs. SourceIndex is
// the only implicit correlation key; the underlying sentinel remains available
// through errors.Is.
type StoreError struct {
	Stage       string
	SourceIndex int
	Uncertain   bool
	err         error
}

func (e *StoreError) Error() string {
	if e == nil {
		return "dicom dimse: C-STORE batch failed"
	}
	if errors.Is(e.err, ErrStoreTransferSyntaxPolicy) {
		return ErrStoreTransferSyntaxPolicy.Error()
	}
	if e.Stage == "" {
		return "dicom dimse: C-STORE batch failed"
	}
	return "dicom dimse: C-STORE batch " + e.Stage + " failed"
}

func (e *StoreError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// StoreLimits bound planning work and the number/size of replayable sources.
// Zero values select finite defaults; negative values are invalid.
type StoreLimits struct {
	MaxItems            int
	MaxItemBytes        int64
	MaxTotalBytes       int64
	MaxAssociations     int
	MaxTransferSyntaxes int
}

type StorePlanOptions struct {
	Limits StoreLimits
	// PresentationContexts, when non-nil, is the authoritative fixed wire
	// proposal, with canonical IDs 1,3,... as assigned by UL. Its order/IDs
	// and repeated SOP classes are preserved. Sources
	// absent from its independently producible SOP/syntax intersection fail
	// planning before Open/dial. Nil keeps automatic source-based contexts;
	// an empty non-nil template is invalid. This does not enable transcoding.
	PresentationContexts []ul.PresentationContext
}

// StorePlannedItem binds one input occurrence to a presentation context in a
// specific planned association.
type StorePlannedItem struct {
	SourceIndex           int
	Descriptor            StoreDescriptor
	PresentationContextID byte
}

type StoreAssociationPlan struct {
	Contexts []ul.PresentationContext
	Items    []StorePlannedItem
}

type StorePlanFailure struct {
	SourceIndex int
	Err         error
}

type StorePlan struct {
	Associations []StoreAssociationPlan
	Failures     []StorePlanFailure
	SourceCount  int
	// TotalBytes is planning admission, not negotiated wire size. With per-syntax
	// bounds it sums the smallest advertised representation; the session checks
	// selected bounds and actual total bytes separately after negotiation.
	TotalBytes int64
}

// PlanStoreBatch inspects every source before any dial, preserving first
// occurrence order. Source-local inspection failures are recorded in Failures;
// invalid options, cancellation, authorization refusal and global resource
// limits return an error.
func PlanStoreBatch(ctx context.Context, sources []StoreSource, options StorePlanOptions) (StorePlan, error) {
	return planStoreBatch(ctx, sources, options, nil)
}

func planStoreBatch(ctx context.Context, sources []StoreSource, options StorePlanOptions, allowlist []string) (StorePlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits, err := normalizeStoreLimits(options.Limits)
	if err != nil {
		return StorePlan{}, err
	}
	fixedContexts, err := cloneStoreProfileContexts(options.PresentationContexts)
	if err != nil {
		return StorePlan{}, err
	}
	if len(allowlist) != 0 {
		for i := range fixedContexts {
			fixedContexts[i].TransferSyntaxUIDs = transfer.FilterTransferSyntaxUIDs(fixedContexts[i].TransferSyntaxUIDs, allowlist)
			if len(fixedContexts[i].TransferSyntaxUIDs) == 0 {
				return StorePlan{}, ErrStoreInvalidOptions
			}
		}
	}
	plan := StorePlan{SourceCount: len(sources)}
	if len(sources) > limits.MaxItems {
		return plan, newStoreError("plan", -1, ErrStoreResourceLimit, false)
	}

	var current *StoreAssociationPlan
	contextIDs := map[string]byte{}
	for sourceIndex, source := range sources {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		if source == nil {
			plan.Failures = append(plan.Failures, StorePlanFailure{SourceIndex: sourceIndex, Err: newStoreError("inspect", sourceIndex, ErrStoreInvalidSource, false)})
			continue
		}
		descriptor, inspectErr := inspectStoreSource(ctx, source)
		if inspectErr != nil {
			if err := ctx.Err(); err != nil {
				return plan, err
			}
			if errors.Is(inspectErr, ErrStoreAccessDenied) {
				return plan, newStoreError("authorize", sourceIndex, ErrStoreAccessDenied, false)
			}
			plan.Failures = append(plan.Failures, StorePlanFailure{SourceIndex: sourceIndex, Err: newStoreError("inspect", sourceIndex, ErrStoreInvalidSource, false)})
			continue
		}
		offered := transfer.FilterTransferSyntaxUIDs(descriptor.WritableTransferSyntaxUIDs, allowlist)
		if len(offered) == 0 {
			plan.Failures = append(plan.Failures, StorePlanFailure{SourceIndex: sourceIndex,
				Err: newStoreError("transfer syntax policy", sourceIndex, ErrStoreTransferSyntaxPolicy, false)})
			continue
		}
		size := storeMinimumRepresentationSize(descriptor, offered)
		if size > limits.MaxItemBytes || size > math.MaxInt64-plan.TotalBytes || plan.TotalBytes+size > limits.MaxTotalBytes {
			return plan, newStoreError("plan", sourceIndex, ErrStoreResourceLimit, false)
		}
		if len(descriptor.WritableTransferSyntaxUIDs) > limits.MaxTransferSyntaxes {
			return plan, newStoreError("plan", sourceIndex, ErrStoreResourceLimit, false)
		}
		plan.TotalBytes += size

		if fixedContexts != nil {
			pcID, found := storeProfileContextID(fixedContexts, descriptor)
			if !found {
				plan.Failures = append(plan.Failures, StorePlanFailure{SourceIndex: sourceIndex, Err: newStoreError("profile", sourceIndex, ErrStorePresentationContextRejected, false)})
				continue
			}
			if current == nil {
				if len(plan.Associations) >= limits.MaxAssociations {
					return plan, newStoreError("plan", sourceIndex, ErrStoreResourceLimit, false)
				}
				plan.Associations = append(plan.Associations, StoreAssociationPlan{Contexts: fixedContexts})
				current = &plan.Associations[len(plan.Associations)-1]
			}
			current.Items = append(current.Items, StorePlannedItem{SourceIndex: sourceIndex, Descriptor: cloneStoreDescriptor(descriptor), PresentationContextID: pcID})
			continue
		}

		key := descriptor.SOPClassUID + "\x00" + strings.Join(offered, "\x00")
		pcID, exists := contextIDs[key]
		if current == nil || (!exists && len(current.Contexts) == ul.MaxPresentationContexts) {
			if len(plan.Associations) >= limits.MaxAssociations {
				return plan, newStoreError("plan", sourceIndex, ErrStoreResourceLimit, false)
			}
			plan.Associations = append(plan.Associations, StoreAssociationPlan{})
			current = &plan.Associations[len(plan.Associations)-1]
			contextIDs = map[string]byte{}
			exists = false
		}
		if !exists {
			pcID = byte(2*len(current.Contexts) + 1)
			contextIDs[key] = pcID
			current.Contexts = append(current.Contexts, ul.PresentationContext{
				ID:                 pcID,
				AbstractSyntaxUID:  descriptor.SOPClassUID,
				TransferSyntaxUIDs: append([]string(nil), offered...),
			})
		}
		current.Items = append(current.Items, StorePlannedItem{
			SourceIndex:           sourceIndex,
			Descriptor:            cloneStoreDescriptor(descriptor),
			PresentationContextID: pcID,
		})
	}
	return plan, nil
}

func inspectStoreSource(ctx context.Context, source StoreSource) (descriptor StoreDescriptor, err error) {
	defer func() {
		if recover() != nil {
			descriptor = StoreDescriptor{}
			err = ErrStoreInvalidSource
		}
	}()
	descriptor, err = source.Inspect(ctx)
	if err != nil {
		return StoreDescriptor{}, err
	}
	return normalizeStoreDescriptor(descriptor)
}

func normalizeStoreLimits(limits StoreLimits) (StoreLimits, error) {
	if limits.MaxItems < 0 || limits.MaxItemBytes < 0 || limits.MaxTotalBytes < 0 || limits.MaxAssociations < 0 || limits.MaxTransferSyntaxes < 0 {
		return StoreLimits{}, ErrStoreInvalidOptions
	}
	if limits.MaxItems == 0 {
		limits.MaxItems = DefaultStoreMaxItems
	}
	if limits.MaxItemBytes == 0 {
		limits.MaxItemBytes = DefaultStoreMaxItemBytes
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = DefaultStoreMaxTotalBytes
	}
	if limits.MaxAssociations == 0 {
		limits.MaxAssociations = DefaultStoreMaxAssociations
	}
	if limits.MaxTransferSyntaxes == 0 {
		limits.MaxTransferSyntaxes = DefaultStoreMaxTransferSyntaxes
	}
	return limits, nil
}

// valueFreeDialCause returns the class of a failed association attempt as an
// error that carries no address, host name or certificate, or nil when the
// cause has no such class. It lets a caller tell a refused connection, a
// silent peer and an untrusted certificate apart without exposing the peer.
func valueFreeDialCause(cause error) error {
	var errno syscall.Errno
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var dns *net.DNSError
	switch {
	case errors.Is(cause, ul.ErrAssociationTimeout):
		return ul.ErrAssociationTimeout
	case errors.As(cause, &unknownAuthority):
		return x509.UnknownAuthorityError{}
	case errors.As(cause, &invalid):
		return x509.CertificateInvalidError{Reason: invalid.Reason}
	case errors.As(cause, &dns):
		return &net.DNSError{Err: "host not found", IsNotFound: dns.IsNotFound, IsTimeout: dns.IsTimeout}
	case errors.As(cause, &errno):
		return errno
	case errors.Is(cause, io.EOF), errors.Is(cause, io.ErrUnexpectedEOF):
		return io.ErrUnexpectedEOF
	}
	return nil
}

// uncertainStoreCause is the value-free cause of a C-STORE whose outcome is
// unknown after err: ErrStoreUncertain, joined by ErrStoreTimeout when err
// is a timeout of the wait for the peer, or by the value-free way the peer
// ended the association: an A-ABORT (a *ul.AbortError with its source and
// reason), a reset (the syscall.Errno) or a close (io.ErrUnexpectedEOF).
func uncertainStoreCause(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ul.ErrAssociationTimeout) || errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %w", ErrStoreUncertain, ErrStoreTimeout)
	}
	if lost := valueFreeConnectionLoss(err); lost != nil {
		return fmt.Errorf("%w: %w", ErrStoreUncertain, lost)
	}
	return ErrStoreUncertain
}

// lostAssociationCause is the error of an item the session gives up on after
// its associations kept being lost: ErrStoreAssociation joined by how the
// peer ended the last uncertain store among the items already attempted, so
// a caller can tell that the peer kept timing out, resetting or aborting.
// Only that transport cause is carried over. Whatever else the uncertain
// store reported, such as a local resource limit, belongs to that item: the
// items given up here were never transferred.
func lostAssociationCause(result *StoreBatchResult, attempted []StorePlannedItem) error {
	for i := len(attempted) - 1; i >= 0; i-- {
		index := attempted[i].SourceIndex
		if index < 0 || index >= len(result.Items) {
			continue
		}
		item := result.Items[index]
		if item.Outcome != StoreOutcomeUnknown || item.Err == nil {
			continue
		}
		if errors.Is(item.Err, ErrStoreResourceLimit) {
			// The session ended that association itself.
			return ErrStoreAssociation
		}
		if errors.Is(item.Err, ErrStoreTimeout) {
			return fmt.Errorf("%w: %w", ErrStoreAssociation, ErrStoreTimeout)
		}
		if lost := valueFreeConnectionLoss(item.Err); lost != nil {
			// ErrStoreUncertain marks the loss as one of a store, which
			// newStoreError keeps as the transport cause and then drops.
			return fmt.Errorf("%w: %w: %w", ErrStoreAssociation, ErrStoreUncertain, lost)
		}
		return ErrStoreAssociation
	}
	return ErrStoreAssociation
}

// valueFreeConnectionLoss returns how the peer ended an association, as an
// error that carries no address or peer text, or nil when err does not say.
func valueFreeConnectionLoss(err error) error {
	var abort *ul.AbortError
	var errno syscall.Errno
	switch {
	case errors.As(err, &abort) && abort != nil:
		return &ul.AbortError{Source: abort.Source, Reason: abort.Reason}
	case errors.Is(err, ul.ErrAssociationAborted):
		return ul.ErrAssociationAborted
	case errors.As(err, &errno):
		return errno
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return io.ErrUnexpectedEOF
	}
	return nil
}

func newStoreError(stage string, sourceIndex int, cause error, uncertain bool) error {
	if cause == nil {
		cause = ErrStoreInvalidSource
	}
	safe := cause
	switch {
	case errors.Is(cause, context.Canceled):
		safe = context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		safe = context.DeadlineExceeded
	case errors.Is(cause, ErrStoreAccessDenied):
		safe = ErrStoreAccessDenied
	case errors.Is(cause, ErrStoreInvalidOptions):
		safe = ErrStoreInvalidOptions
	case errors.Is(cause, ErrStoreResourceLimit):
		safe = ErrStoreResourceLimit
	case errors.Is(cause, ErrStoreSourceChanged):
		safe = ErrStoreSourceChanged
	case errors.Is(cause, ErrStoreTransferSyntaxPolicy):
		safe = ErrStoreTransferSyntaxPolicy
	case errors.Is(cause, ErrStoreTransferSyntax):
		safe = ErrStoreTransferSyntax
	case errors.Is(cause, ErrStorePresentationContextRejected):
		if safeCause := sanitizedPresentationContextCause(cause); safeCause != nil {
			safe = safeCause
		} else {
			safe = ErrStorePresentationContextRejected
		}
	case errors.Is(cause, ErrStoreAssociation):
		safe = ErrStoreAssociation
		// The A-ASSOCIATE-RJ codes carry no peer data and tell the caller
		// why the receiver refused the association.
		var rejection *ul.RejectionError
		if errors.As(cause, &rejection) {
			safe = fmt.Errorf("%w: %w", ErrStoreAssociation, &ul.RejectionError{Result: rejection.Result, Source: rejection.Source, Reason: rejection.Reason})
		} else if errors.Is(cause, ErrStoreTimeout) {
			safe = fmt.Errorf("%w: %w", ErrStoreAssociation, ErrStoreTimeout)
		} else if lost := valueFreeConnectionLoss(cause); lost != nil && errors.Is(cause, ErrStoreUncertain) {
			// The associations kept being lost the way this one was.
			safe = fmt.Errorf("%w: %w", ErrStoreAssociation, lost)
		} else if transport := valueFreeDialCause(cause); transport != nil {
			safe = fmt.Errorf("%w: %w", ErrStoreAssociation, transport)
		}
	case errors.Is(cause, ErrStoreRemoteFailure):
		safe = ErrStoreRemoteFailure
	case errors.Is(cause, ErrStoreUncertain):
		safe = ErrStoreUncertain
		if errors.Is(cause, ErrStoreTimeout) {
			safe = fmt.Errorf("%w: %w", ErrStoreUncertain, ErrStoreTimeout)
		} else if lost := valueFreeConnectionLoss(cause); lost != nil {
			safe = fmt.Errorf("%w: %w", ErrStoreUncertain, lost)
		}
	case errors.Is(cause, ErrStoreSessionClosed):
		safe = ErrStoreSessionClosed
	case errors.Is(cause, ErrStoreCallback):
		safe = ErrStoreCallback
	default:
		safe = ErrStoreInvalidSource
	}
	return &StoreError{Stage: stage, SourceIndex: sourceIndex, Uncertain: uncertain, err: safe}
}

func sanitizedPresentationContextCause(cause error) error {
	var missing *MissingPresentationContextError
	if errors.As(cause, &missing) && missing != nil {
		cloned := &MissingPresentationContextError{
			SOPClassUID: missing.SOPClassUID,
			Outcomes:    append([]ul.PresentationContextOutcome(nil), missing.Outcomes...),
		}
		return fmt.Errorf("%w: %w", ErrStorePresentationContextRejected, cloned)
	}
	var none *ul.NoAcceptedPresentationContextsError
	if errors.As(cause, &none) && none != nil {
		return fmt.Errorf("%w: %w", ErrStorePresentationContextRejected, &ul.NoAcceptedPresentationContextsError{Outcomes: none.Rejected()})
	}
	return nil
}
