package dimse

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

func storeSessionPipelineOptIn(options StoreSessionOptions) bool {
	return options.MaxInvokedOperations > 1
}

func effectiveStoreInvoked(optIn bool, configuredMax int, negotiated ul.AsynchronousOperationsWindow) int {
	if !optIn || configuredMax <= 1 {
		return 1
	}
	if negotiated.MaximumInvoked == 0 {
		return configuredMax
	}
	limit := int(negotiated.MaximumInvoked)
	if limit <= 1 {
		return 1
	}
	if configuredMax < limit {
		return configuredMax
	}
	return limit
}

type pipelineCompletion struct {
	sourceIndex int
	syntaxUID   string
	response    *CStoreResponse
	err         error
	closeErr    error
	uncertain   bool
}

type pipelineFlight struct {
	sourceIndex int
	reserved    int64
	close       func() error
}

func (s *StoreSession) storeAssociationPipelined(
	ctx context.Context,
	assoc *ul.Association,
	effectiveInvoked int,
	associationPlan StoreAssociationPlan,
	result *StoreBatchResult,
	sources []StoreSource,
	associationIndex int,
	position int,
	completed int,
	remainingTotalBytes *int64,
) (int, int, bool, error) {
	asyncSess, err := NewAsyncSession(assoc, AsyncSessionOptions{MaxInvokedOperations: effectiveInvoked})
	if err != nil {
		s.abortAssociation(assoc, err)
		s.clearActive(assoc)
		return position, completed, false, newStoreError("associate", -1, err, false)
	}

	completions := make(chan pipelineCompletion, effectiveInvoked)
	var responseWorkers sync.WaitGroup
	inFlight := make(map[int]pipelineFlight)
	bytesInFlight := int64(0)
	stopAdmit := false
	usable := true
	var accessErr error
	// Finish cannot overtake a parsed correlated response. After abort, Wait is
	// unblocked by the operation context/AsyncSession; drain only actual responses,
	// without committing canceled batch counters or claiming remote rollback.
	drainResponses := func() {
		responseWorkers.Wait()
		for {
			select {
			case completion := <-completions:
				if completion.response != nil {
					s.observeStoreResponse(ctx, result.Items[completion.sourceIndex], completion.response.Status)
				}
			default:
				return
			}
		}
	}

	abortInFlight := func(cause error) (int, int, bool, error) {
		for sourceIndex, flight := range inFlight {
			item := &result.Items[sourceIndex]
			if item.Outcome == StoreOutcomeNotSent || item.Outcome == "" {
				item.Outcome = StoreOutcomeUnknown
				item.Err = newStoreError("transfer", sourceIndex, uncertainStoreCause(cause), true)
				result.Unknown++
				completed++
			}
			if flight.close != nil {
				_ = flight.close()
			}
			_ = cause
		}
		inFlight = map[int]pipelineFlight{}
		abortCtx, cancel := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
		_ = asyncSess.Abort(abortCtx)
		cancel()
		drainResponses()
		s.clearActive(assoc)
		s.cancelAllNotSent(result, result.Plan, cause)
		result.Complete = storeBatchComplete(result.Items)
		return position, completed, false, cause
	}

	for position < len(associationPlan.Items) || len(inFlight) > 0 {
		if err := s.contextOrClosed(ctx); err != nil {
			return abortInFlight(err)
		}

		for !stopAdmit && position < len(associationPlan.Items) && len(inFlight) < effectiveInvoked {
			if err := s.contextOrClosed(ctx); err != nil {
				return abortInFlight(err)
			}
			planned := associationPlan.Items[position]
			item := &result.Items[planned.SourceIndex]
			item.Descriptor = cloneStoreDescriptor(planned.Descriptor)
			item.AssociationIndex = associationIndex
			item.AssociationID = assoc.ID()
			selectedPC, selectionErr := AcceptedContextForSOPClassTransferSyntaxes(assoc, planned.Descriptor.SOPClassUID, planned.Descriptor.WritableTransferSyntaxUIDs)
			reserve := storeRepresentationSize(planned.Descriptor, selectedPC.TransferSyntaxUID)
			if reserve < 0 {
				reserve = 0
			}
			if s.options.MaxInFlightBytes > 0 && len(inFlight) > 0 && reserve > s.options.MaxInFlightBytes-bytesInFlight {
				break
			}

			item.Attempt++
			if selectionErr != nil {
				item.Outcome = StoreOutcomeFailure
				item.Err = newStoreError("negotiate", planned.SourceIndex, fmt.Errorf("%w: %w", ErrStorePresentationContextRejected, ExplainMissingPresentationContext(assoc, planned.Descriptor.SOPClassUID)), false)
				result.Failed++
				completed++
				position++
				if progressErr := s.reportProgress(ctx, *item, completed, len(sources)); progressErr != nil {
					return abortInFlight(progressErr)
				}
				if !s.options.ContinueOnError {
					stopAdmit = true
					if len(inFlight) == 0 {
						releaseErr := s.releasePipelinedSession(asyncSess)
						s.clearActive(assoc)
						if releaseErr != nil {
							result.ReleaseFailures++
							return position, completed, false, storeReleaseError()
						}
						return position, completed, true, item.Err
					}
					break
				}
				continue
			}

			opened, pc, openErr := s.selectedStoreRepresentation(ctx, sources[planned.SourceIndex], assoc, planned.Descriptor, *remainingTotalBytes)
			if openErr == nil {
				openErr = validateOpenedStoreSource(planned.Descriptor, opened)
			}
			if openErr == nil {
				s.observeStorePrepared(ctx, *item, opened, pc.TransferSyntaxUID)
				openErr = checkStoreAccess(ctx, opened.BeforeStore)
			}
			if openErr != nil {
				if opened.Close != nil {
					_ = closeStoreSource(opened.Close)
				}
				if errors.Is(openErr, ErrStoreAccessDenied) {
					accessErr = newStoreError("authorize", planned.SourceIndex, ErrStoreAccessDenied, false)
					item.Err = accessErr
					stopAdmit = true
					break
				}
				if err := ctx.Err(); err != nil && opened.BeforeStore != nil {
					return abortInFlight(err)
				}
				item.Outcome = StoreOutcomeFailure
				item.Err = newStoreError("open", planned.SourceIndex, openErr, false)
				result.Failed++
				completed++
				position++
				if progressErr := s.reportProgress(ctx, *item, completed, len(sources)); progressErr != nil {
					return abortInFlight(progressErr)
				}
				if !s.options.ContinueOnError {
					stopAdmit = true
					if len(inFlight) == 0 {
						releaseErr := s.releasePipelinedSession(asyncSess)
						s.clearActive(assoc)
						if releaseErr != nil {
							result.ReleaseFailures++
							return position, completed, false, storeReleaseError()
						}
						return position, completed, true, item.Err
					}
					break
				}
				continue
			}

			item.Attempted = true
			writeDataSet := s.boundedStoreWriter(opened.WriteDataSet, remainingTotalBytes, storeWriterRepresentationBound(planned.Descriptor, pc.TransferSyntaxUID))
			sourceIndex := planned.SourceIndex
			closeFn := onceStoreCloser(opened.Close)
			operation, startErr := asyncSess.StartCStoreEncoded(ctx, pc.ID, CStoreRequest{
				AffectedSOPClassUID:          planned.Descriptor.SOPClassUID,
				AffectedSOPInstanceUID:       planned.Descriptor.SOPInstanceUID,
				Priority:                     s.options.Priority,
				MoveOriginatorAETitle:        s.options.MoveOriginatorAETitle,
				MoveOriginatorMessageIDOrNil: s.options.MoveOriginatorMessageIDOrNil,
			}, writeDataSet)
			if startErr != nil {
				_ = closeFn()
				if err := s.contextOrClosed(ctx); err != nil {
					return abortInFlight(err)
				}
				if errors.Is(startErr, ErrAssociationStateUncertain) {
					item.Outcome = StoreOutcomeUnknown
					item.Err = newStoreError("transfer", sourceIndex, uncertainStoreCause(startErr), true)
					result.Unknown++
					completed++
					position++
					abortCtx, cancelAbort := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
					_ = asyncSess.Abort(abortCtx)
					cancelAbort()
					drainResponses()
					s.clearActive(assoc)
					return position, completed, false, nil
				}
				item.Outcome = StoreOutcomeFailure
				item.Err = newStoreError("response", sourceIndex, ErrStoreRemoteFailure, false)
				result.Failed++
				completed++
				position++
				if !s.options.ContinueOnError {
					stopAdmit = true
					break
				}
				continue
			}

			inFlight[sourceIndex] = pipelineFlight{sourceIndex: sourceIndex, reserved: reserve, close: closeFn}
			bytesInFlight += reserve
			position++
			responseWorkers.Add(1)
			go func(op *AsyncOperation, sourceIndex int, closeFn func() error, syntaxUID string) {
				defer responseWorkers.Done()
				message, waitErr := op.Wait(ctx)
				closeErr := closeFn()
				// StartCStoreEncoded already admitted/sent the command. Without
				// a valid final response, a transport/protocol failure cannot
				// establish whether the peer stored the instance.
				completion := pipelineCompletion{sourceIndex: sourceIndex, syntaxUID: syntaxUID, err: waitErr, closeErr: closeErr, uncertain: waitErr != nil}
				if waitErr == nil {
					parsed, parseErr := ParseCStoreResponse(message.Command)
					if parseErr != nil {
						completion.err = parseErr
						completion.uncertain = true
					} else {
						completion.response = parsed
						completion.err = CheckCStoreStatus(parsed)
					}
				}
				completions <- completion
			}(operation, sourceIndex, closeFn, pc.TransferSyntaxUID)
		}

		if len(inFlight) == 0 {
			break
		}

		var completion pipelineCompletion
		select {
		case completion = <-completions:
		case <-ctx.Done():
			return abortInFlight(ctx.Err())
		}
		if completion.response != nil {
			s.observeStoreResponse(ctx, result.Items[completion.sourceIndex], completion.response.Status)
		}
		if err := s.contextOrClosed(ctx); err != nil {
			return abortInFlight(err)
		}
		if errors.Is(completion.err, context.Canceled) || errors.Is(completion.err, context.DeadlineExceeded) {
			return abortInFlight(completion.err)
		}

		flight, ok := inFlight[completion.sourceIndex]
		if ok {
			bytesInFlight -= flight.reserved
			if bytesInFlight < 0 {
				bytesInFlight = 0
			}
			delete(inFlight, completion.sourceIndex)
		}
		item := &result.Items[completion.sourceIndex]
		item.NegotiatedTransferSyntaxUID = completion.syntaxUID
		if completion.response != nil {
			item.Status = completion.response.Status
			item.StatusSet = true
		}

		uncertain := completion.uncertain || errors.Is(completion.err, ErrAssociationStateUncertain)
		if uncertain && accessErr == nil && !s.options.DisableReconnect && s.options.RetryUncertain && item.Attempt < s.options.MaxStoreAttempts {
			item.Outcome = StoreOutcomeNotSent
			item.Err = nil
			item.Attempted = false
			position = firstNotSentPosition(associationPlan, result, completion.sourceIndex)
			abortCtx, cancel := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
			_ = asyncSess.Abort(abortCtx)
			cancel()
			drainResponses()
			s.clearActive(assoc)
			for _, remaining := range inFlight {
				if remaining.close != nil {
					_ = remaining.close()
				}
			}
			return position, completed, false, nil
		}

		switch {
		case uncertain || completion.err != nil && errors.Is(completion.err, context.Canceled):
			if errors.Is(completion.err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return abortInFlight(ctx.Err())
			}
			item.Outcome = StoreOutcomeUnknown
			cause := uncertainStoreCause(completion.err)
			if errors.Is(completion.err, ErrStoreResourceLimit) {
				cause = ErrStoreResourceLimit
			}
			item.Err = newStoreError("transfer", completion.sourceIndex, cause, true)
			result.Unknown++
			usable = false
			stopAdmit = true
		case completion.err != nil:
			item.Outcome = StoreOutcomeFailure
			item.Err = newStoreError("response", completion.sourceIndex, ErrStoreRemoteFailure, false)
			result.Failed++
			if !s.options.ContinueOnError {
				stopAdmit = true
			}
		case completion.closeErr != nil:
			item.Outcome = StoreOutcomeFailure
			item.Err = newStoreError("close", completion.sourceIndex, ErrStoreInvalidSource, false)
			result.Failed++
			if !s.options.ContinueOnError {
				stopAdmit = true
			}
		case IsCStoreWarningStatus(item.Status):
			item.Outcome = StoreOutcomeWarning
			result.Succeeded++
			result.Warnings++
		default:
			item.Outcome = StoreOutcomeSuccess
			result.Succeeded++
		}
		completed++
		if progressErr := s.reportProgress(ctx, *item, completed, len(sources)); progressErr != nil {
			return abortInFlight(progressErr)
		}
		if usable && accessErr == nil && item.Err != nil && !s.options.ContinueOnError && len(inFlight) == 0 {
			releaseErr := s.releasePipelinedSession(asyncSess)
			s.clearActive(assoc)
			if releaseErr != nil && usable {
				result.ReleaseFailures++
				return position, completed, false, storeReleaseError()
			}
			return position, completed, usable, item.Err
		}
		if !usable {
			abortCtx, cancel := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
			_ = asyncSess.Abort(abortCtx)
			cancel()
			drainResponses()
			s.clearActive(assoc)
			for _, remaining := range inFlight {
				remItem := &result.Items[remaining.sourceIndex]
				if remItem.Outcome == StoreOutcomeNotSent {
					remItem.Outcome = StoreOutcomeUnknown
					remItem.Err = newStoreError("transfer", remaining.sourceIndex, uncertainStoreCause(completion.err), true)
					result.Unknown++
					completed++
				}
				if remaining.close != nil {
					_ = remaining.close()
				}
			}
			if accessErr != nil {
				return position, completed, false, accessErr
			}
			if !s.options.ContinueOnError {
				return position, completed, false, item.Err
			}
			return position, completed, false, nil
		}
	}

	if !usable {
		abortCtx, cancel := context.WithTimeout(context.Background(), s.options.CleanupTimeout)
		_ = asyncSess.Abort(abortCtx)
		cancel()
		drainResponses()
		s.clearActive(assoc)
		return position, completed, false, nil
	}
	if err := s.releasePipelinedSession(asyncSess); err != nil {
		s.clearActive(assoc)
		result.ReleaseFailures++
		releaseErr := storeReleaseError()
		if accessErr != nil {
			return position, completed, false, errors.Join(accessErr, releaseErr)
		}
		if s.options.ContinueOnError {
			// Every admitted item has its outcome: the lost release ends
			// only this association, not the batch (#2000). StoreBatch
			// reports it once the batch ends.
			return position, completed, true, nil
		}
		return position, completed, false, releaseErr
	}
	s.clearActive(assoc)
	return position, completed, true, accessErr
}

func (s *StoreSession) releasePipelinedSession(session *AsyncSession) error {
	if session == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.options.ReleaseTimeout)
	defer cancel()
	return session.Release(ctx, AsyncReleaseWait)
}

func firstNotSentPosition(plan StoreAssociationPlan, result *StoreBatchResult, sourceIndex int) int {
	for i, planned := range plan.Items {
		if planned.SourceIndex == sourceIndex {
			return i
		}
		if result.Items[planned.SourceIndex].Outcome == StoreOutcomeNotSent {
			return i
		}
	}
	return len(plan.Items)
}

func onceStoreCloser(closeFn func() error) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			err = closeStoreSource(closeFn)
		})
		return err
	}
}
