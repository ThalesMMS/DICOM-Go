package ups

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/dicomweb"
	"github.com/ThalesMMS/dicom-go/object"
)

// DICOMwebOptions configures the UPS-RS adapter.
type DICOMwebOptions struct {
	// RequestingAETitle is recorded as the Requesting AE (0074,1236) of
	// cancellation requests received over DICOMweb, which carry no AE
	// Title of their own. Empty selects "DICOMWEB".
	RequestingAETitle string
	// NewUID assigns the UID of a workitem created without one. Nil uses
	// core.NewUID.
	NewUID func() (string, error)
	// MaxSearchScanned bounds the workitems one search examines (default
	// 100,000); a larger repository answers 503.
	MaxSearchScanned int
}

// DICOMwebBackend serves the UPS-RS (PS3.18 section 11) workitem and
// subscription resources of dicomweb.Server over Service, with the same
// state machine, validation, subscriptions and event outbox as the DIMSE
// SCPs. Configure it as dicomweb.ServerOptions.Workitems.
type DICOMwebBackend struct {
	service *Service
	options DICOMwebOptions
}

var (
	_ dicomweb.WorkitemBackend             = (*DICOMwebBackend)(nil)
	_ dicomweb.WorkitemSubscriptionBackend = (*DICOMwebBackend)(nil)
)

// NewDICOMwebBackend adapts service to dicomweb.WorkitemBackend.
func NewDICOMwebBackend(service *Service, options DICOMwebOptions) (*DICOMwebBackend, error) {
	if service == nil {
		return nil, ErrConflict
	}
	options.RequestingAETitle = strings.TrimSpace(options.RequestingAETitle)
	if options.RequestingAETitle == "" {
		options.RequestingAETitle = "DICOMWEB"
	}
	if !validAETitle(options.RequestingAETitle) {
		return nil, ErrInvalidDataSet
	}
	if options.NewUID == nil {
		options.NewUID = core.NewUID
	}
	if options.MaxSearchScanned <= 0 {
		options.MaxSearchScanned = 100_000
	}
	if options.MaxSearchScanned > maxQueryScannedSteps {
		return nil, ErrResourceLimit
	}
	return &DICOMwebBackend{service: service, options: options}, nil
}

// CreateWorkitem implements dicomweb.WorkitemBackend. An absent Transaction
// UID is added empty, as PS3.4 requires it zero-length on creation.
func (b *DICOMwebBackend) CreateWorkitem(ctx context.Context, request dicomweb.WorkitemCreateRequest) (dicomweb.WorkitemCreated, error) {
	if request.Workitem == nil {
		return dicomweb.WorkitemCreated{}, dicomweb.ErrInvalidRequest
	}
	uid := request.SOPInstanceUID
	if uid == "" {
		generated, err := b.options.NewUID()
		if err != nil {
			return dicomweb.WorkitemCreated{}, dicomweb.ErrBackend
		}
		uid = generated
	}
	attributes := object.FromDataSet(request.Workitem.ToDataSet(), std.Dictionary)
	if _, present := attributes.Get(TagTransactionUID); !present {
		attributes.Put(StringElement(TagTransactionUID, core.VRUI, ""))
	}
	_, err := b.service.Create(ctx, CreateRequest{SOPInstanceUID: uid, Attributes: attributes})
	result, err := dicomwebResult(err)
	if err != nil {
		return dicomweb.WorkitemCreated{}, err
	}
	return dicomweb.WorkitemCreated{WorkitemResult: result, SOPInstanceUID: uid}, nil
}

// RetrieveWorkitem implements dicomweb.WorkitemBackend. The Transaction UID
// is never returned (PS3.18 11.5).
func (b *DICOMwebBackend) RetrieveWorkitem(ctx context.Context, sopInstanceUID string) (dicomweb.Dataset, error) {
	step, err := b.service.Get(ctx, sopInstanceUID)
	if err != nil {
		_, mapped := dicomwebResult(err)
		return nil, mapped
	}
	attributes := step.Attributes
	removeElement(&attributes, TagTransactionUID)
	return datasetToDICOMweb(ctx, attributes)
}

// UpdateWorkitem implements dicomweb.WorkitemBackend.
func (b *DICOMwebBackend) UpdateWorkitem(ctx context.Context, request dicomweb.WorkitemUpdateRequest) (dicomweb.WorkitemResult, error) {
	_, err := b.service.Set(ctx, SetRequest{
		SOPInstanceUID: request.SOPInstanceUID, TransactionUID: request.TransactionUID, Modifications: request.Modifications,
	})
	return dicomwebResult(err)
}

// ChangeWorkitemState implements dicomweb.WorkitemBackend.
func (b *DICOMwebBackend) ChangeWorkitemState(ctx context.Context, request dicomweb.WorkitemStateRequest) (dicomweb.WorkitemResult, error) {
	_, err := b.service.ChangeState(ctx, ChangeStateRequest{
		SOPInstanceUID: request.SOPInstanceUID, State: State(strings.TrimSpace(request.State)), TransactionUID: request.TransactionUID,
	})
	return dicomwebResult(err)
}

// RequestWorkitemCancellation implements dicomweb.WorkitemBackend. A
// Requesting AE in the body is used; otherwise RequestingAETitle.
func (b *DICOMwebBackend) RequestWorkitemCancellation(ctx context.Context, request dicomweb.WorkitemCancellationRequest) (dicomweb.WorkitemResult, error) {
	requester := b.options.RequestingAETitle
	var information *object.Object
	if request.Information != nil {
		information = object.FromDataSet(request.Information.ToDataSet(), std.Dictionary)
		if element, present := information.Get(TagRequestingAE); present {
			values := element.StringValues()
			if len(values) != 1 || !validAETitle(strings.TrimSpace(values[0])) {
				return dicomweb.WorkitemResult{}, dicomweb.ErrInvalidRequest
			}
			requester = strings.TrimSpace(values[0])
			information.Remove(TagRequestingAE)
		}
	}
	_, err := b.service.RequestCancel(ctx, CancelRequest{
		SOPInstanceUID: request.SOPInstanceUID, RequestingAETitle: requester, Information: information,
	})
	return dicomwebResult(err)
}

// SubscribeWorkitem implements dicomweb.WorkitemSubscriptionBackend.
func (b *DICOMwebBackend) SubscribeWorkitem(ctx context.Context, request dicomweb.WorkitemSubscriptionRequest) (dicomweb.WorkitemResult, error) {
	var matchingKeys map[string][]string
	if len(request.Filter) > 0 {
		matchingKeys = make(map[string][]string, len(request.Filter))
		for _, filter := range request.Filter {
			tag, ok := resolveDICOMwebAttribute(filter.Key)
			if !ok {
				return dicomweb.WorkitemResult{}, dicomweb.ErrInvalidRequest
			}
			key := tag.HexString()
			if _, duplicate := matchingKeys[key]; duplicate {
				return dicomweb.WorkitemResult{}, dicomweb.ErrInvalidRequest
			}
			matchingKeys[key] = append([]string(nil), filter.Values...)
		}
	}
	result, err := b.service.Subscribe(ctx, SubscribeRequest{
		SOPInstanceUID: request.SOPInstanceUID, ReceivingAETitle: request.AETitle,
		DeletionLock: request.DeletionLock, MatchingKeys: matchingKeys,
	})
	if err != nil {
		return dicomwebResult(err)
	}
	if result.Status == StatusDeletionLockNotGranted {
		return dicomweb.WorkitemResult{Warnings: []string{"Deletion lock not granted."}}, nil
	}
	return dicomweb.WorkitemResult{}, nil
}

// UnsubscribeWorkitem implements dicomweb.WorkitemSubscriptionBackend.
func (b *DICOMwebBackend) UnsubscribeWorkitem(ctx context.Context, request dicomweb.WorkitemSubscriptionRequest) (dicomweb.WorkitemResult, error) {
	return dicomwebResult(b.service.Unsubscribe(ctx, UnsubscribeRequest{
		SOPInstanceUID: request.SOPInstanceUID, ReceivingAETitle: request.AETitle,
	}))
}

// SuspendWorkitemSubscription implements dicomweb.WorkitemSubscriptionBackend.
func (b *DICOMwebBackend) SuspendWorkitemSubscription(ctx context.Context, request dicomweb.WorkitemSubscriptionRequest) (dicomweb.WorkitemResult, error) {
	return dicomwebResult(b.service.SuspendGlobal(ctx, request.AETitle))
}

// defaultDICOMwebReturnKeys are returned by a search without includefield,
// in addition to its matching keys.
var defaultDICOMwebReturnKeys = []core.Tag{
	TagSOPClassUID, TagSOPInstanceUID, TagPatientName, TagPatientID, TagPatientBirthDate, TagPatientSex,
	TagStudyInstanceUID, TagProcedureStepState, TagScheduledProcedureStepPriority, TagProcedureStepLabel,
	TagWorklistLabel, TagScheduledProcedureStepStartDateTime, TagExpectedCompletionDateTime,
	TagInputReadinessState, TagScheduledProcedureStepModificationDateTime, TagScheduledWorkitemCodeSequence,
}

// SearchWorkitems implements dicomweb.WorkitemBackend with the UPS C-FIND
// matching rules (exact, wildcard, case-insensitive CS, date/time ranges and
// timezone). Matching keys must be in the UPS query attribute set; an
// unfiltered search lists every workitem. includefield=all returns the
// whole workitem except its Transaction UID.
func (b *DICOMwebBackend) SearchWorkitems(ctx context.Context, request dicomweb.SearchRequest, yield func(dicomweb.Dataset) error) (dicomweb.SearchResult, error) {
	query := ParsedQuery{Keys: make(map[core.Tag]QueryKey)}
	addKey := func(tag core.Tag, values []string) error {
		if !queryTags[tag] {
			return dicomweb.ErrInvalidRequest
		}
		if existing, present := query.Keys[tag]; present {
			if len(values) > 0 && len(existing.Values) > 0 {
				return dicomweb.ErrInvalidRequest
			}
			if len(values) == 0 {
				return nil
			}
		} else {
			query.order = append(query.order, tag)
		}
		query.Keys[tag] = QueryKey{Present: true, Values: values}
		return nil
	}
	for _, filter := range request.Filters {
		tag, ok := resolveDICOMwebAttribute(filter.Key)
		if !ok {
			return dicomweb.SearchResult{}, dicomweb.ErrInvalidRequest
		}
		entry, _ := queryEntry(tag)
		var values []string
		for _, value := range filter.Values {
			if entry.VR == core.VRSQ || value == "" {
				return dicomweb.SearchResult{}, dicomweb.ErrInvalidRequest
			}
			values = append(values, strings.Split(value, ",")...)
		}
		if tag == TagTimezoneOffsetFromUTC && (len(values) != 1 || !validUPSTimezoneOffset(values[0])) {
			return dicomweb.SearchResult{}, dicomweb.ErrInvalidRequest
		}
		if err := addKey(tag, values); err != nil {
			return dicomweb.SearchResult{}, err
		}
	}
	all := len(request.IncludeFields) == 1 && request.IncludeFields[0] == "all"
	if !all {
		returnKeys := defaultDICOMwebReturnKeys
		for _, field := range request.IncludeFields {
			tag, ok := resolveDICOMwebAttribute(field)
			if !ok {
				return dicomweb.SearchResult{}, dicomweb.ErrInvalidRequest
			}
			returnKeys = append(append([]core.Tag(nil), returnKeys...), tag)
		}
		for _, tag := range returnKeys {
			if err := addKey(tag, nil); err != nil {
				return dicomweb.SearchResult{}, err
			}
		}
	}

	limit := request.MaximumResults
	if limit <= 0 {
		limit = 1
	}
	matched, emitted := 0, 0
	var after uint64
	for scanned := 0; ; {
		steps, err := b.service.store.ListSteps(ctx, StepQuery{AfterSequence: after, Limit: maxQueryPageSize})
		if err != nil {
			_, mapped := dicomwebResult(safeRepositoryError(err))
			return dicomweb.SearchResult{}, mapped
		}
		for _, step := range steps {
			if err := ctx.Err(); err != nil {
				return dicomweb.SearchResult{}, err
			}
			scanned++
			if scanned > b.options.MaxSearchScanned {
				return dicomweb.SearchResult{}, dicomweb.ErrResourceLimit
			}
			after = step.Sequence
			if !queryMatchesStep(query, step) {
				continue
			}
			matched++
			if matched <= request.Offset || emitted >= limit {
				continue
			}
			var dataset dicomweb.Dataset
			if all {
				attributes := step.Attributes
				removeElement(&attributes, TagTransactionUID)
				dataset, err = datasetToDICOMweb(ctx, attributes)
			} else {
				projected, _ := projectQueryResult(query, step)
				dataset, err = datasetToDICOMweb(ctx, projected.ToDataSet())
			}
			if err != nil {
				return dicomweb.SearchResult{}, err
			}
			if err := yield(dataset); err != nil {
				return dicomweb.SearchResult{}, err
			}
			emitted++
		}
		if len(steps) < maxQueryPageSize {
			break
		}
	}
	result := dicomweb.SearchResult{}
	if emitted >= limit {
		remaining := matched - request.Offset - emitted
		if remaining < 0 {
			remaining = 0
		}
		result.Remaining = &remaining
	}
	return result, nil
}

func resolveDICOMwebAttribute(key string) (core.Tag, bool) {
	if len(key) == 8 {
		if tag, err := core.ParseTag(key); err == nil {
			return tag, true
		}
	}
	entry, ok := std.Dictionary.ByKeyword(key)
	if !ok {
		return core.Tag{}, false
	}
	return entry.Tag, true
}

func datasetToDICOMweb(ctx context.Context, attributes core.DataSet) (dicomweb.Dataset, error) {
	data, err := dicomjson.MarshalContext(ctx, object.FromDataSet(attributes, std.Dictionary), dicomjson.DefaultOptions())
	if err != nil {
		return nil, dicomweb.ErrBackend
	}
	var dataset dicomweb.Dataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, dicomweb.ErrBackend
	}
	return dataset, nil
}

// dicomwebResult maps a Service outcome onto the dicomweb sentinels. The
// DIMSE warning statuses B304 (already canceled) and B306 (already
// completed) are successes with a warning, as in N-ACTION.
func dicomwebResult(err error) (dicomweb.WorkitemResult, error) {
	if err == nil {
		return dicomweb.WorkitemResult{}, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return dicomweb.WorkitemResult{}, err
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		switch {
		case errors.Is(err, ErrResourceLimit):
			return dicomweb.WorkitemResult{}, dicomweb.ErrResourceLimit
		case errors.Is(err, ErrConcurrentUpdate), errors.Is(err, ErrConflict):
			return dicomweb.WorkitemResult{}, dicomweb.ErrConflict
		}
		return dicomweb.WorkitemResult{}, dicomweb.ErrBackend
	}
	switch statusErr.Status {
	case StatusAlreadyCanceled:
		return dicomweb.WorkitemResult{Warnings: []string{"The workitem is already in the CANCELED state."}}, nil
	case StatusAlreadyCompleted:
		return dicomweb.WorkitemResult{Warnings: []string{"The workitem is already in the COMPLETED state."}}, nil
	case StatusNoSuchSOPInstance, StatusUPSNotFound:
		return dicomweb.WorkitemResult{}, dicomweb.ErrNotFound
	case StatusDuplicateSOPInstance, StatusMayNoLongerBeUpdated, StatusIncorrectTransactionUID,
		StatusAlreadyInProgress, StatusFinalStateRequirementsNotMet, StatusNotInProgress,
		StatusAlreadyCompletedCancelRequest, StatusPerformerCannotBeContacted, StatusPerformerChoosesNotToCancel:
		return dicomweb.WorkitemResult{}, dicomweb.ErrConflict
	case StatusNotAuthorized:
		return dicomweb.WorkitemResult{}, dicomweb.ErrForbidden
	case StatusResourceLimitation:
		return dicomweb.WorkitemResult{}, dicomweb.ErrResourceLimit
	default:
		return dicomweb.WorkitemResult{}, dicomweb.ErrInvalidRequest
	}
}

// dicomwebEventSink publishes UPS deliveries to a WebSocket hub.
type dicomwebEventSink struct {
	hub *dicomweb.WorkitemEventHub
}

// NewDICOMwebEventSink returns an EventSink that delivers each event to the
// open event channels of its Receiving AE Title. With no open channel the
// delivery stays pending (or goes to the DIMSE CallbackResolver when one is
// configured). Pair it with WorkitemEventHubOptions.OnConnect calling
// Service.WakeDeliveries and a running Service.RunDeliveries loop.
func NewDICOMwebEventSink(hub *dicomweb.WorkitemEventHub) EventSink {
	return dicomwebEventSink{hub: hub}
}

func (sink dicomwebEventSink) DeliverUPSEvent(ctx context.Context, delivery Delivery) (bool, error) {
	if sink.hub == nil || sink.hub.Connections(delivery.ReceivingAETitle) == 0 {
		return false, nil
	}
	information, err := datasetToDICOMweb(ctx, delivery.Event.Information)
	if err != nil {
		return true, err
	}
	sent, err := sink.hub.Publish(ctx, delivery.ReceivingAETitle, dicomweb.WorkitemEvent{
		SOPInstanceUID: delivery.SOPInstanceUID, EventTypeID: uint16(delivery.EventType), Information: information,
	})
	if sent > 0 {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	return false, nil
}
