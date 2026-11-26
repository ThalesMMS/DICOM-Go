package dicomweb

import (
	"context"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// UPS-RS (PS3.18 section 11) Worklist Service identifiers.
const (
	// GlobalWorkitemSubscriptionUID is the well-known SOP Instance UID that
	// addresses the global subscription in the subscribers resources.
	GlobalWorkitemSubscriptionUID = "1.2.840.10008.5.1.4.34.5"
	// FilteredGlobalWorkitemSubscriptionUID addresses a filtered global
	// subscription.
	FilteredGlobalWorkitemSubscriptionUID = "1.2.840.10008.5.1.4.34.5.1"
	// UPSEventSOPClassUID is the Affected SOP Class UID of event messages.
	UPSEventSOPClassUID = "1.2.840.10008.5.1.4.34.6.4"
)

// Workitem (UPS-RS) operations.
const (
	OperationCreateWorkitem              Operation = "create_workitem"
	OperationRetrieveWorkitem            Operation = "retrieve_workitem"
	OperationUpdateWorkitem              Operation = "update_workitem"
	OperationChangeWorkitemState         Operation = "change_workitem_state"
	OperationRequestWorkitemCancellation Operation = "request_workitem_cancellation"
	OperationSearchWorkitems             Operation = "search_workitems"
	OperationSubscribeWorkitem           Operation = "subscribe_workitem"
	OperationUnsubscribeWorkitem         Operation = "unsubscribe_workitem"
	OperationSuspendWorkitemSubscription Operation = "suspend_workitem_subscription"
	OperationOpenEventChannel            Operation = "open_event_channel"
)

// SearchLevelWorkitem selects the UPS-RS workitem search resource.
const SearchLevelWorkitem SearchLevel = "workitem"

// WorkitemResult carries the outcome of a workitem transaction that
// succeeded, possibly with warnings. Each warning becomes a
// "Warning: 299 <service>: <text>" response header, for example when a
// requested deletion lock was not granted.
type WorkitemResult struct {
	Warnings []string
}

// WorkitemCreateRequest is a validated Create Workitem transaction. The
// server resolves SOPInstanceUID from the "workitem" (or
// "AffectedSOPInstanceUID") query parameter, a bare UID query, or the
// dataset's SOP Instance UID; it is empty when none was supplied and the
// backend assigns one.
type WorkitemCreateRequest struct {
	SOPInstanceUID string
	Workitem       *object.Object
}

// WorkitemCreated reports the UID a backend stored the workitem under.
type WorkitemCreated struct {
	WorkitemResult
	SOPInstanceUID string
}

// WorkitemUpdateRequest is an Update Workitem transaction.
type WorkitemUpdateRequest struct {
	SOPInstanceUID string
	// TransactionUID comes from the "transaction" query parameter; it is
	// required while the workitem is IN PROGRESS.
	TransactionUID string
	Modifications  *object.Object
}

// WorkitemStateRequest is a Change Workitem State transaction. State and
// TransactionUID come from the Procedure Step State (0074,1000) and
// Transaction UID (0008,1195) of the request dataset.
type WorkitemStateRequest struct {
	SOPInstanceUID string
	State          string
	TransactionUID string
}

// WorkitemCancellationRequest is a Request Workitem Cancellation
// transaction. Information is nil when the request had no body; otherwise it
// may carry Reason For Cancellation, the discontinuation reason code,
// Contact URI and Contact Display Name.
type WorkitemCancellationRequest struct {
	SOPInstanceUID string
	Information    *object.Object
}

// WorkitemSubscriptionRequest is a Subscribe, Unsubscribe or Suspend Global
// Subscription transaction for one Receiving AE Title.
type WorkitemSubscriptionRequest struct {
	SOPInstanceUID string
	AETitle        string
	DeletionLock   bool
	// Filter holds the matching keys of a filtered global subscription
	// ("filter" query parameters of the form attribute=value).
	Filter []SearchFilter
}

// WorkitemBackend implements the UPS-RS workitem resources. Errors use the
// package sentinels: ErrInvalidRequest (400), ErrNotFound (404),
// ErrConflict (409, for example a wrong transaction UID or a final state),
// ErrForbidden (403) and ErrResourceLimit (503). Package ups provides an
// implementation over its state machine (ups.NewDICOMwebBackend).
type WorkitemBackend interface {
	CreateWorkitem(context.Context, WorkitemCreateRequest) (WorkitemCreated, error)
	RetrieveWorkitem(context.Context, string) (Dataset, error)
	UpdateWorkitem(context.Context, WorkitemUpdateRequest) (WorkitemResult, error)
	ChangeWorkitemState(context.Context, WorkitemStateRequest) (WorkitemResult, error)
	RequestWorkitemCancellation(context.Context, WorkitemCancellationRequest) (WorkitemResult, error)
	SearchWorkitems(context.Context, SearchRequest, func(Dataset) error) (SearchResult, error)
}

// WorkitemSubscriptionBackend adds the subscription resources.
type WorkitemSubscriptionBackend interface {
	SubscribeWorkitem(context.Context, WorkitemSubscriptionRequest) (WorkitemResult, error)
	UnsubscribeWorkitem(context.Context, WorkitemSubscriptionRequest) (WorkitemResult, error)
	SuspendWorkitemSubscription(context.Context, WorkitemSubscriptionRequest) (WorkitemResult, error)
}

// WorkitemEvent is one UPS event report carried over an event channel.
// Information holds the event attributes of PS3.4 CC.2.4.3; the message
// also carries Affected SOP Class UID (0000,0002), Message ID (0000,0110),
// Affected SOP Instance UID (0000,1000) and Event Type ID (0000,1002).
type WorkitemEvent struct {
	SOPInstanceUID string
	EventTypeID    uint16
	Information    Dataset
}

// ValidAETitle reports whether value is a usable AE Title: 1 to 16
// characters of printable ASCII without backslash and not only spaces. It is
// ul.ValidateAETitle (PS3.5 Table 6.2-1) applied to a value as written in a
// data set or an HTTP parameter, which is never NUL padded; this package does
// not depend on net/ul, so the rule is repeated here and a ups test keeps the
// two in step.
func ValidAETitle(value string) bool {
	if len(value) == 0 || len(value) > 16 || strings.TrimSpace(value) == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7E || value[i] == '\\' {
			return false
		}
	}
	return true
}
