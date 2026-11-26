package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type StoreOutcome string

const (
	StoreOutcomeNotSent  StoreOutcome = "not_sent"
	StoreOutcomeSuccess  StoreOutcome = "success"
	StoreOutcomeWarning  StoreOutcome = "warning"
	StoreOutcomeFailure  StoreOutcome = "failure"
	StoreOutcomeUnknown  StoreOutcome = "unknown"
	StoreOutcomeCanceled StoreOutcome = "canceled"
)

type StoreItemResult struct {
	AssociationID               string
	SourceIndex                 int
	Descriptor                  StoreDescriptor
	AssociationIndex            int
	Attempt                     int
	Attempted                   bool
	Status                      uint16
	StatusSet                   bool
	NegotiatedTransferSyntaxUID string
	Outcome                     StoreOutcome
	Err                         error
}

type StoreBatchResult struct {
	Plan         StorePlan
	Items        []StoreItemResult
	Associations int
	Succeeded    int
	Warnings     int
	Failed       int
	Unknown      int
	Canceled     int
	Complete     bool
	// ReleaseFailures counts associations whose A-RELEASE failed or went
	// unanswered after every C-STORE on them had its outcome; their items
	// keep those outcomes. With ContinueOnError the batch goes on to its next
	// association and StoreBatch returns a "release" StoreError once the
	// batch ends; without it the batch stops at the first failed release.
	ReleaseFailures int
}

// StoreProgress is deliberately origin-, UID-, and value-free. SourceIndex is
// the stable caller-owned correlation key.
type StoreProgress struct {
	// AssociationID is the opaque identity of the real accepted association.
	AssociationID    string
	SourceIndex      int
	Completed        int
	Total            int
	AssociationIndex int
	Attempt          int
	Outcome          StoreOutcome
	Status           uint16
	StatusSet        bool
}

type StoreProgressHandler func(context.Context, StoreProgress) error

type StoreSessionOptions struct {
	// DialOptions configure each association. Its CommonExtendedNegotiation
	// is a catalog: each association proposes the items whose SOP Class it
	// carries (see RelatedGeneralStorageSOPClasses), so a peer that lacks a
	// new or private Storage SOP Class can accept it as a related general one.
	DialOptions ul.DialOptions
	PlanOptions StorePlanOptions

	ContinueOnError        bool
	DisableReconnect       bool
	RetryUncertain         bool
	MaxAssociationAttempts int
	MaxStoreAttempts       int
	// MaxInFlightBytes bounds the payload bytes of the operations outstanding
	// at once on a pipelined association. An object larger than it is sent
	// alone; PlanOptions.Limits.MaxItemBytes bounds a single object.
	MaxInFlightBytes int64
	// MaxInvokedOperations caps outstanding C-STORE operations on one
	// association. Zero or one keeps the serial StoreClient loop. Values
	// greater than one opt into AsyncSession pipelining when the peer also
	// negotiates a window greater than one.
	MaxInvokedOperations int

	Priority                     uint16
	MoveOriginatorAETitle        string
	MoveOriginatorMessageIDOrNil *uint16
	ReleaseTimeout               time.Duration
	CleanupTimeout               time.Duration
	// OnProgress reports terminal item progress. On the serial path a confirmed
	// remote response is reported once even if cancellation arrives before the
	// callback; its context may already be canceled. No next source starts.
	OnProgress StoreProgressHandler
	// OnStorePrepared observes a validated source after presentation-context
	// selection and before its C-STORE command. It is opt-in, synchronous and
	// panic-contained, and does not change status or counters. ReadMetadata is
	// valid only during this callback; keep only its detached result.
	OnStorePrepared func(context.Context, StorePreparedObservation)
	// OnStoreResponse observes each correlated remote response before
	// cancellation accounting and association Finished. Panics are contained.
	// Outcome describes the remote status alone; Completed/Total remain zero.
	// Confirmed serial progress follows this observer, including when it cancels
	// the operation; uncertain exchanges are not promoted to terminal responses.
	OnStoreResponse func(context.Context, StoreProgress)
	// Association observers are opt-in, run outside session locks, and must
	// return promptly. Panics are contained. Finished follows release/abort or
	// transport termination and is called once per successfully started handle.
	OnAssociationStarted  func(context.Context, *ul.Association, int)
	OnAssociationFinished func(context.Context, *ul.Association, int)
}

type storeDialFunc func(context.Context, string, ul.DialOptions) (*ul.Association, error)

// StoreSession owns associations created for one or more StoreBatch calls.
// Calls are serialized. The default path keeps one outstanding C-STORE per
// association; MaxInvokedOperations greater than one opts into pipelining
// when the peer negotiates a matching window.
type StoreSession struct {
	address string
	options StoreSessionOptions
	dial    storeDialFunc
	runGate chan struct{}

	mu            sync.Mutex
	closed        bool
	active        *ul.Association
	activeContext context.Context
	activeIndex   int
}

func NewStoreSession(address string, options StoreSessionOptions) (*StoreSession, error) {
	if address == "" {
		return nil, ErrStoreInvalidOptions
	}
	fixedContexts, err := cloneStoreProfileContexts(options.PlanOptions.PresentationContexts)
	if err != nil {
		return nil, err
	}
	options = cloneStoreSessionOptions(options)
	options.PlanOptions.PresentationContexts = fixedContexts
	if options.Priority > 2 || options.MaxAssociationAttempts < 0 || options.MaxStoreAttempts < 0 || options.MaxInFlightBytes < 0 || options.MaxInvokedOperations < 0 || options.ReleaseTimeout < 0 || options.CleanupTimeout < 0 {
		return nil, ErrStoreInvalidOptions
	}
	if options.MaxAssociationAttempts == 0 {
		options.MaxAssociationAttempts = 1
	}
	if options.MaxStoreAttempts == 0 {
		options.MaxStoreAttempts = 1
	}
	if options.ReleaseTimeout == 0 {
		options.ReleaseTimeout = 5 * time.Second
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = time.Second
	}
	// MaxInFlightBytes is not a per-object bound (#1956): an object above it
	// is sent alone, and MaxItemBytes keeps its own value.
	limits, err := normalizeStoreLimits(options.PlanOptions.Limits)
	if err != nil {
		return nil, err
	}
	options.PlanOptions.Limits = limits
	session := &StoreSession{
		address: address,
		options: options,
		dial: func(ctx context.Context, address string, dialOptions ul.DialOptions) (*ul.Association, error) {
			return ul.DialContext(ctx, address, dialOptions)
		},
		runGate: make(chan struct{}, 1),
	}
	session.runGate <- struct{}{}
	return session, nil
}

func (s *StoreSession) Store(ctx context.Context, source StoreSource) (StoreItemResult, error) {
	result, err := s.StoreBatch(ctx, []StoreSource{source})
	if len(result.Items) == 0 {
		return StoreItemResult{}, err
	}
	if err == nil {
		err = result.Items[0].Err
	}
	return result.Items[0], err
}

func (s *StoreSession) StorePath(ctx context.Context, path string) (StoreItemResult, error) {
	return s.Store(ctx, NewPathStoreSource(path))
}

func (s *StoreSession) StoreBatch(ctx context.Context, sources []StoreSource) (result StoreBatchResult, returnErr error) {
	if s == nil {
		return StoreBatchResult{}, ErrStoreInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireRun(ctx); err != nil {
		return StoreBatchResult{}, err
	}
	defer s.releaseRun()
	if s.isClosed() {
		return StoreBatchResult{}, ErrStoreSessionClosed
	}
	sources = append([]StoreSource(nil), sources...)

	plan, err := planStoreBatch(ctx, sources, s.options.PlanOptions, s.options.DialOptions.TransferSyntaxAllowlist)
	result = newStoreBatchResult(plan)
	defer func() {
		result.Complete = storeBatchComplete(result.Items)
	}()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.cancelAllNotSent(&result, plan, err)
			result.Complete = true
		}
		return result, err
	}
	completed := 0
	for _, failure := range plan.Failures {
		item := &result.Items[failure.SourceIndex]
		item.Outcome = StoreOutcomeFailure
		item.Err = failure.Err
		result.Failed++
		completed++
		if progressErr := s.reportProgress(ctx, *item, completed, len(sources)); progressErr != nil {
			if ctx.Err() != nil {
				s.cancelAllNotSent(&result, plan, ctx.Err())
				result.Complete = storeBatchComplete(result.Items)
			}
			return result, progressErr
		}
	}
	if len(plan.Failures) > 0 && !s.options.ContinueOnError {
		result.Complete = storeBatchComplete(result.Items)
		return result, plan.Failures[0].Err
	}
	remainingTotalBytes := s.options.PlanOptions.Limits.MaxTotalBytes
	// deadAssociations counts consecutive associations that became unusable
	// without one stored object.
	deadAssociations := 0

	for associationIndex := range plan.Associations {
		associationPlan := plan.Associations[associationIndex]
		position := 0
		for position < len(associationPlan.Items) {
			if err := s.contextOrClosed(ctx); err != nil {
				s.cancelAllNotSent(&result, plan, err)
				result.Complete = storeBatchComplete(result.Items)
				return result, err
			}
			assoc, dialAttempt, dialErr := s.dialAssociation(ctx, associationPlan.Contexts)
			if dialErr != nil {
				if err := s.contextOrClosed(ctx); err != nil {
					s.cancelAllNotSent(&result, plan, err)
					result.Complete = storeBatchComplete(result.Items)
					return result, err
				}
				for _, planned := range associationPlan.Items[position:] {
					item := &result.Items[planned.SourceIndex]
					item.Descriptor = cloneStoreDescriptor(planned.Descriptor)
					item.AssociationIndex = associationIndex
					item.Attempt = dialAttempt
					item.Outcome = StoreOutcomeFailure
					// Keep the dial error in the chain so a caller can tell a
					// refused connection or a timeout from a rejection.
					cause := fmt.Errorf("%w: %w", ErrStoreAssociation, dialErr)
					if errors.Is(dialErr, ul.ErrNoAcceptedPresentationContexts) {
						cause = fmt.Errorf("%w: %w", ErrStorePresentationContextRejected, dialErr)
					}
					item.Err = newStoreError("associate", planned.SourceIndex, cause, false)
					result.Failed++
					completed++
					if progressErr := s.reportProgress(ctx, *item, completed, len(sources)); progressErr != nil {
						if ctx.Err() != nil {
							s.cancelAllNotSent(&result, plan, ctx.Err())
							result.Complete = storeBatchComplete(result.Items)
						}
						return result, progressErr
					}
				}
				if !s.options.ContinueOnError {
					return result, result.Items[associationPlan.Items[position].SourceIndex].Err
				}
				break
			}
			result.Associations++
			s.setActive(ctx, assoc, associationIndex)
			associationUsable := true
			succeededAtDial := result.Succeeded
			// stopReconnecting reports the second consecutive association
			// lost before storing anything. Each further attempt would cost
			// one more timeout against a peer that is not answering.
			stopReconnecting := func() bool {
				if result.Succeeded != succeededAtDial {
					deadAssociations = 0
					return false
				}
				deadAssociations++
				return deadAssociations >= maxDeadStoreAssociations
			}
			if invoked := effectiveStoreInvoked(storeSessionPipelineOptIn(s.options), s.options.MaxInvokedOperations, assoc.EffectiveAsynchronousOperationsWindow()); invoked > 1 {
				nextPos, nextCompleted, usable, pipeErr := s.storeAssociationPipelined(ctx, assoc, invoked, associationPlan, &result, sources, associationIndex, position, completed, &remainingTotalBytes)
				position = nextPos
				completed = nextCompleted
				associationUsable = usable
				if pipeErr != nil {
					return result, pipeErr
				}
				if !associationUsable {
					if stopReconnecting() || s.options.DisableReconnect || position >= len(associationPlan.Items) {
						for _, remaining := range associationPlan.Items[position:] {
							remainingResult := &result.Items[remaining.SourceIndex]
							if remainingResult.Outcome != StoreOutcomeNotSent {
								continue
							}
							remainingResult.Descriptor = cloneStoreDescriptor(remaining.Descriptor)
							remainingResult.Outcome = StoreOutcomeFailure
							remainingResult.Err = newStoreError("reconnect", remaining.SourceIndex, lostAssociationCause(&result, associationPlan.Items[:position]), false)
							result.Failed++
							completed++
						}
						position = len(associationPlan.Items)
					}
				}
				continue
			}
			client := NewStoreClient(assoc)
			for position < len(associationPlan.Items) {
				planned := associationPlan.Items[position]
				item := &result.Items[planned.SourceIndex]
				item.Descriptor = cloneStoreDescriptor(planned.Descriptor)
				item.AssociationIndex = associationIndex
				item.AssociationID = assoc.ID()
				item.Attempt++
				if err := s.contextOrClosed(ctx); err != nil {
					associationUsable = false
					s.abortAssociation(assoc, nil)
					s.clearActive(assoc)
					s.cancelAllNotSent(&result, plan, err)
					result.Complete = storeBatchComplete(result.Items)
					return result, err
				}
				if !storeContextAccepted(assoc, planned) {
					item.Outcome = StoreOutcomeFailure
					item.Err = newStoreError("negotiate", planned.SourceIndex, fmt.Errorf("%w: %w", ErrStorePresentationContextRejected, ExplainMissingPresentationContext(assoc, planned.Descriptor.SOPClassUID)), false)
					result.Failed++
					completed++
					position++
					if err := s.reportProgress(ctx, *item, completed, len(sources)); err != nil {
						_ = s.releaseAssociation(assoc)
						s.clearActive(assoc)
						if ctx.Err() != nil {
							s.cancelAllNotSent(&result, plan, ctx.Err())
							result.Complete = storeBatchComplete(result.Items)
						}
						return result, err
					}
					if !s.options.ContinueOnError {
						_ = s.releaseAssociation(assoc)
						s.clearActive(assoc)
						return result, item.Err
					}
					continue
				}
				opened, selectedPC, openErr := s.selectedStoreRepresentation(ctx, sources[planned.SourceIndex], assoc, planned.Descriptor, remainingTotalBytes)
				if openErr == nil {
					openErr = validateOpenedStoreSource(planned.Descriptor, opened)
				}
				if openErr == nil {
					s.observeStorePrepared(ctx, *item, opened, selectedPC.TransferSyntaxUID)
					openErr = checkStoreAccess(ctx, opened.BeforeStore)
				}
				if openErr != nil {
					if opened.Close != nil {
						_ = closeStoreSource(opened.Close)
					}
					if errors.Is(openErr, ErrStoreAccessDenied) {
						item.Err = newStoreError("authorize", planned.SourceIndex, ErrStoreAccessDenied, false)
						_ = s.releaseAssociation(assoc)
						s.clearActive(assoc)
						return result, item.Err
					}
					if err := ctx.Err(); err != nil && opened.BeforeStore != nil {
						s.abortAssociation(assoc, nil)
						s.clearActive(assoc)
						s.cancelAllNotSent(&result, plan, err)
						return result, err
					}
					item.Outcome = StoreOutcomeFailure
					item.Err = newStoreError("open", planned.SourceIndex, openErr, false)
					result.Failed++
					completed++
					position++
					if err := s.reportProgress(ctx, *item, completed, len(sources)); err != nil {
						_ = s.releaseAssociation(assoc)
						s.clearActive(assoc)
						if ctx.Err() != nil {
							s.cancelAllNotSent(&result, plan, ctx.Err())
							result.Complete = storeBatchComplete(result.Items)
						}
						return result, err
					}
					if !s.options.ContinueOnError {
						_ = s.releaseAssociation(assoc)
						s.clearActive(assoc)
						return result, item.Err
					}
					continue
				}

				item.Attempted = true
				writeDataSet := s.boundedStoreWriter(opened.WriteDataSet, &remainingTotalBytes, storeWriterRepresentationBound(planned.Descriptor, selectedPC.TransferSyntaxUID))
				storeResult, storeErr := client.StoreEncodedWithOptions(ctx, writeDataSet, CStoreOptions{
					AffectedSOPClassUID:          planned.Descriptor.SOPClassUID,
					AffectedSOPInstanceUID:       planned.Descriptor.SOPInstanceUID,
					TransferSyntaxUIDs:           []string{selectedPC.TransferSyntaxUID},
					Priority:                     s.options.Priority,
					MoveOriginatorAETitle:        s.options.MoveOriginatorAETitle,
					MoveOriginatorMessageIDOrNil: s.options.MoveOriginatorMessageIDOrNil,
				})
				if storeResult.Response != nil {
					s.observeStoreResponse(ctx, *item, storeResult.Response.Status)
				}
				closeErr := closeStoreSource(opened.Close)
				item.NegotiatedTransferSyntaxUID = storeResult.PresentationContext.TransferSyntaxUID
				if storeResult.Response != nil {
					item.Status = storeResult.Response.Status
					item.StatusSet = true
				}
				uncertain := errors.Is(storeErr, ErrAssociationStateUncertain)
				if uncertain && !s.options.DisableReconnect && s.options.RetryUncertain && item.Attempt < s.options.MaxStoreAttempts {
					associationUsable = false
					s.abortAssociation(assoc, storeErr)
					s.clearActive(assoc)
					item.Outcome = StoreOutcomeNotSent
					item.Err = nil
					break
				}
				switch {
				case uncertain:
					item.Outcome = StoreOutcomeUnknown
					cause := uncertainStoreCause(storeErr)
					if errors.Is(storeErr, ErrStoreResourceLimit) {
						cause = ErrStoreResourceLimit
					}
					item.Err = newStoreError("transfer", planned.SourceIndex, cause, true)
					result.Unknown++
					associationUsable = false
				case storeErr != nil:
					item.Outcome = StoreOutcomeFailure
					item.Err = newStoreError("response", planned.SourceIndex, ErrStoreRemoteFailure, false)
					result.Failed++
				case closeErr != nil:
					item.Outcome = StoreOutcomeFailure
					item.Err = newStoreError("close", planned.SourceIndex, ErrStoreInvalidSource, false)
					result.Failed++
				case IsCStoreWarningStatus(item.Status):
					item.Outcome = StoreOutcomeWarning
					result.Succeeded++
					result.Warnings++
				default:
					item.Outcome = StoreOutcomeSuccess
					result.Succeeded++
				}
				completed++
				position++
				// Preserve a correlated terminal exchange already received before
				// cancellation. Its completion must reach OnProgress exactly once;
				// the callback's cancellation check then stops the batch. An uncertain
				// exchange has no authoritative terminal result and still aborts here.
				if ctx.Err() != nil && (storeResult.Response == nil || uncertain) {
					associationUsable = false
					s.abortAssociation(assoc, storeErr)
					s.clearActive(assoc)
					s.cancelAllNotSent(&result, plan, ctx.Err())
					result.Complete = storeBatchComplete(result.Items)
					return result, ctx.Err()
				}
				progressErr := s.reportProgress(ctx, *item, completed, len(sources))
				if progressErr == nil {
					// A nil progress callback still must return the cancellation of
					// the last confirmed item, rather than finish the batch successfully.
					progressErr = ctx.Err()
				}
				if progressErr != nil {
					_ = s.releaseAssociation(assoc)
					s.clearActive(assoc)
					if ctx.Err() != nil {
						s.cancelAllNotSent(&result, plan, ctx.Err())
						result.Complete = storeBatchComplete(result.Items)
					}
					return result, progressErr
				}
				if item.Err != nil && !s.options.ContinueOnError {
					if associationUsable {
						_ = s.releaseAssociation(assoc)
					} else {
						s.abortAssociation(assoc, storeErr)
					}
					s.clearActive(assoc)
					return result, item.Err
				}
				if !associationUsable {
					s.abortAssociation(assoc, storeErr)
					s.clearActive(assoc)
					if stopReconnecting() || s.options.DisableReconnect || position >= len(associationPlan.Items) {
						for _, remaining := range associationPlan.Items[position:] {
							remainingResult := &result.Items[remaining.SourceIndex]
							remainingResult.Descriptor = cloneStoreDescriptor(remaining.Descriptor)
							remainingResult.Outcome = StoreOutcomeFailure
							remainingResult.Err = newStoreError("reconnect", remaining.SourceIndex, lostAssociationCause(&result, associationPlan.Items[:position]), false)
							result.Failed++
							completed++
						}
						position = len(associationPlan.Items)
					}
					break
				}
			}
			if associationUsable {
				releaseErr := s.releaseAssociation(assoc)
				s.clearActive(assoc)
				if releaseErr != nil {
					// Every item of this association has its outcome; a lost
					// release ends only this association (#2000).
					result.ReleaseFailures++
					if !s.options.ContinueOnError {
						return result, storeReleaseError()
					}
				}
			}
		}
	}
	result.Complete = storeBatchComplete(result.Items)
	if result.ReleaseFailures > 0 {
		return result, storeReleaseError()
	}
	return result, nil
}

// storeReleaseError reports an association whose release failed after every
// C-STORE on it had its outcome.
func storeReleaseError() error {
	return newStoreError("release", -1, ErrStoreAssociation, false)
}

// maxDeadStoreAssociations is how many consecutive associations may be lost
// without a stored object before a batch stops reconnecting.
const maxDeadStoreAssociations = 2

func newStoreBatchResult(plan StorePlan) StoreBatchResult {
	items := make([]StoreItemResult, plan.SourceCount)
	for i := range items {
		items[i] = StoreItemResult{SourceIndex: i, AssociationIndex: -1, Outcome: StoreOutcomeNotSent}
	}
	return StoreBatchResult{Plan: plan, Items: items}
}

func (s *StoreSession) dialAssociation(ctx context.Context, contexts []ul.PresentationContext) (*ul.Association, int, error) {
	var lastErr error
	for attempt := 1; attempt <= s.options.MaxAssociationAttempts; attempt++ {
		if err := s.contextOrClosed(ctx); err != nil {
			return nil, attempt, err
		}
		opts := cloneStoreDialOptions(s.options.DialOptions)
		opts.Context = ctx
		opts.Contexts = cloneStorePresentationContexts(contexts)
		opts.CommonExtendedNegotiation = commonExtendedNegotiationFor(opts.CommonExtendedNegotiation, opts.Contexts)
		if storeSessionPipelineOptIn(s.options) && opts.AsynchronousOperationsWindow == nil {
			invoked := uint16(s.options.MaxInvokedOperations)
			opts.AsynchronousOperationsWindow = &ul.AsynchronousOperationsWindow{
				MaximumInvoked:   invoked,
				MaximumPerformed: 1,
			}
		}
		assoc, err := s.dial(ctx, s.address, opts)
		if err == nil {
			return assoc, attempt, nil
		}
		lastErr = err
	}
	return nil, s.options.MaxAssociationAttempts, lastErr
}

func storeContextAccepted(assoc *ul.Association, planned StorePlannedItem) bool {
	if assoc == nil {
		return false
	}
	for _, syntaxUID := range planned.Descriptor.WritableTransferSyntaxUIDs {
		for _, accepted := range assoc.AcceptedContexts {
			if accepted.AbstractSyntaxUID == planned.Descriptor.SOPClassUID && accepted.TransferSyntaxUID == syntaxUID {
				return true
			}
		}
	}
	return false
}

func openStoreSource(ctx context.Context, source StoreSource) (opened OpenedStoreSource, err error) {
	if source == nil {
		return OpenedStoreSource{}, ErrStoreInvalidSource
	}
	defer func() {
		if recover() != nil {
			opened = OpenedStoreSource{}
			err = ErrStoreInvalidSource
		}
	}()
	return source.Open(ctx)
}

func validateOpenedStoreSource(planned StoreDescriptor, opened OpenedStoreSource) error {
	if opened.WriteDataSet == nil || opened.Close == nil {
		return ErrStoreInvalidSource
	}
	actual, err := normalizeStoreDescriptor(opened.Descriptor)
	if err != nil {
		return err
	}
	if actual.SOPClassUID != planned.SOPClassUID || actual.SOPInstanceUID != planned.SOPInstanceUID || actual.TransferSyntaxUID != planned.TransferSyntaxUID || (planned.Size > 0 && actual.Size != planned.Size) {
		return ErrStoreSourceChanged
	}
	if !equalStoreSyntaxUIDs(actual.WritableTransferSyntaxUIDs, planned.WritableTransferSyntaxUIDs) || !equalStoreRepresentationSizes(actual.TransferSyntaxSizes, planned.TransferSyntaxSizes) {
		return ErrStoreSourceChanged
	}
	if planned.PixelDataHeaderSet && (!actual.PixelDataHeaderSet || actual.PixelDataHeader != planned.PixelDataHeader) {
		return ErrStoreSourceChanged
	}
	return nil
}

func equalStoreSyntaxUIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func closeStoreSource(closeFunc func() error) (err error) {
	if closeFunc == nil {
		return ErrStoreInvalidSource
	}
	defer func() {
		if recover() != nil {
			err = ErrStoreInvalidSource
		}
	}()
	return closeFunc()
}

func (s *StoreSession) reportProgress(ctx context.Context, item StoreItemResult, completed, total int) (err error) {
	if s.options.OnProgress == nil {
		return nil
	}
	progress := StoreProgress{
		AssociationID: item.AssociationID, SourceIndex: item.SourceIndex, Completed: completed, Total: total,
		AssociationIndex: item.AssociationIndex, Attempt: item.Attempt,
		Outcome: item.Outcome, Status: item.Status, StatusSet: item.StatusSet,
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if ctx != nil && ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = newStoreError("progress", item.SourceIndex, ErrStoreCallback, false)
			}
		}
	}()
	err = s.options.OnProgress(ctx, progress)
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return newStoreError("progress", item.SourceIndex, ErrStoreCallback, false)
	}
	return nil
}

func (s *StoreSession) releaseAssociation(assoc *ul.Association) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.options.ReleaseTimeout)
	defer cancel()
	return assoc.Release(ctx)
}

// abortAssociation aborts after cause: a protocol violation is answered with
// an A-P-ABORT and its reason, anything else (nil included) with a
// service-user A-ABORT.
func (s *StoreSession) abortAssociation(assoc *ul.Association, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
	defer cancel()
	_ = assoc.AbortForError(ctx, cause)
}

func (s *StoreSession) cancelAllNotSent(result *StoreBatchResult, plan StorePlan, cause error) {
	for _, association := range plan.Associations {
		for _, planned := range association.Items {
			result.Items[planned.SourceIndex].Descriptor = cloneStoreDescriptor(planned.Descriptor)
		}
	}
	for i := range result.Items {
		item := &result.Items[i]
		if item.Outcome != StoreOutcomeNotSent {
			continue
		}
		item.Outcome = StoreOutcomeCanceled
		item.Err = newStoreError("cancel", item.SourceIndex, cause, false)
		result.Canceled++
	}
}

func (s *StoreSession) boundedStoreWriter(writeDataSet CStoreDataSetWriter, remainingTotal *int64, representationBound int64) CStoreDataSetWriter {
	limit := s.options.PlanOptions.Limits.MaxItemBytes
	if representationBound > 0 {
		limit = min(limit, representationBound)
	}
	return func(ctx context.Context, destination io.Writer, syntax transfer.Syntax) error {
		writer := &storeByteLimitWriter{destination: destination, remaining: limit, remainingTotal: remainingTotal}
		return writeDataSet(ctx, writer, syntax)
	}
}

type storeByteLimitWriter struct {
	destination    io.Writer
	remaining      int64
	remainingTotal *int64
}

func (w *storeByteLimitWriter) Write(data []byte) (int, error) {
	if w == nil || w.destination == nil || int64(len(data)) > w.remaining || w.remainingTotal == nil || int64(len(data)) > *w.remainingTotal {
		return 0, ErrStoreResourceLimit
	}
	n, err := w.destination.Write(data)
	w.remaining -= int64(n)
	*w.remainingTotal -= int64(n)
	return n, err
}

func storeBatchComplete(items []StoreItemResult) bool {
	for _, item := range items {
		if item.Outcome == StoreOutcomeNotSent {
			return false
		}
	}
	return true
}

func (s *StoreSession) acquireRun(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.runGate:
		return nil
	}
}

func (s *StoreSession) releaseRun() { s.runGate <- struct{}{} }

func (s *StoreSession) contextOrClosed(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if s.isClosed() {
		return ErrStoreSessionClosed
	}
	return nil
}

func (s *StoreSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *StoreSession) setActive(ctx context.Context, assoc *ul.Association, index int) {
	s.mu.Lock()
	s.active = assoc
	s.activeContext = ctx
	s.activeIndex = index
	s.mu.Unlock()
	observeStoreAssociation(s.options.OnAssociationStarted, ctx, assoc, index)
}

func (s *StoreSession) clearActive(assoc *ul.Association) {
	s.mu.Lock()
	active := s.active == assoc
	ctx, index := s.activeContext, s.activeIndex
	if active {
		s.active = nil
		s.activeContext = nil
	}
	s.mu.Unlock()
	if active {
		observeStoreAssociation(s.options.OnAssociationFinished, context.WithoutCancel(ctx), assoc, index)
	}
}

func observeStoreAssociation(observer func(context.Context, *ul.Association, int), ctx context.Context, assoc *ul.Association, index int) {
	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer(ctx, assoc, index)
}

// Close prevents new batches. It waits for an active batch to finish and is
// idempotent; StoreBatch itself performs the protocol release.
func (s *StoreSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireRun(ctx); err != nil {
		return err
	}
	defer s.releaseRun()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// Abort prevents new batches and aborts the active association, if any.
func (s *StoreSession) Abort(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.closed = true
	assoc := s.active
	s.mu.Unlock()
	if assoc == nil {
		return nil
	}
	return assoc.AbortWithContext(ctx, ul.AbortReasonNotSpecified)
}

func cloneStoreSessionOptions(options StoreSessionOptions) StoreSessionOptions {
	options.DialOptions = cloneStoreDialOptions(options.DialOptions)
	if options.MoveOriginatorMessageIDOrNil != nil {
		value := *options.MoveOriginatorMessageIDOrNil
		options.MoveOriginatorMessageIDOrNil = &value
	}
	return options
}

func cloneStoreDialOptions(options ul.DialOptions) ul.DialOptions {
	options.Transport = options.Transport.Clone()
	options.Contexts = cloneStorePresentationContexts(options.Contexts)
	options.TransferSyntaxAllowlist = append([]string(nil), options.TransferSyntaxAllowlist...)
	options.RoleSelections = append([]ul.RoleSelectionItem(nil), options.RoleSelections...)
	options.ExtendedNegotiation = append([]ul.SopClassExtendedNegotiationItem(nil), options.ExtendedNegotiation...)
	for i := range options.ExtendedNegotiation {
		options.ExtendedNegotiation[i].Data = append([]byte(nil), options.ExtendedNegotiation[i].Data...)
	}
	options.CommonExtendedNegotiation = append([]ul.SopClassCommonExtendedNegotiationItem(nil), options.CommonExtendedNegotiation...)
	for i := range options.CommonExtendedNegotiation {
		options.CommonExtendedNegotiation[i].RelatedGeneralSOPClassUIDs = append([]string(nil), options.CommonExtendedNegotiation[i].RelatedGeneralSOPClassUIDs...)
	}
	if options.UserIdentity != nil {
		identity := *options.UserIdentity
		identity.PrimaryField = append([]byte(nil), identity.PrimaryField...)
		identity.SecondaryField = append([]byte(nil), identity.SecondaryField...)
		options.UserIdentity = &identity
	}
	if options.AsynchronousOperationsWindow != nil {
		window := *options.AsynchronousOperationsWindow
		options.AsynchronousOperationsWindow = &window
	}
	if options.TLSConfig != nil {
		options.TLSConfig = options.TLSConfig.Clone()
	}
	return options
}

func cloneStorePresentationContexts(contexts []ul.PresentationContext) []ul.PresentationContext {
	clone := append([]ul.PresentationContext(nil), contexts...)
	for i := range clone {
		clone[i].TransferSyntaxUIDs = append([]string(nil), clone[i].TransferSyntaxUIDs...)
	}
	return clone
}

func equalStoreRepresentationSizes(left, right map[string]int64) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for uid, bound := range left {
		if right[uid] != bound {
			return false
		}
	}
	return true
}

func (s *StoreSession) observeStoreResponse(ctx context.Context, item StoreItemResult, status uint16) {
	if s.options.OnStoreResponse == nil {
		return
	}
	outcome := StoreOutcomeFailure
	if status == StatusSuccess {
		outcome = StoreOutcomeSuccess
	} else if IsCStoreWarningStatus(status) {
		outcome = StoreOutcomeWarning
	}
	progress := StoreProgress{AssociationID: item.AssociationID, SourceIndex: item.SourceIndex, AssociationIndex: item.AssociationIndex, Attempt: item.Attempt, Outcome: outcome, Status: status, StatusSet: true}
	defer func() { _ = recover() }()
	s.options.OnStoreResponse(context.WithoutCancel(ctx), progress)
}
