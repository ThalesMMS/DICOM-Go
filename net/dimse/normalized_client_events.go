package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DefaultMaxInterleavedEventReports limits notifications during one operation.
const DefaultMaxInterleavedEventReports = 32

// NormalizedEventReportSubscription declares the application-specific event
// types accepted for a SOP Class. An empty list accepts no types. The client
// copies subscriptions before sending the operation; callers must not mutate
// configuration concurrently with that copy.
type NormalizedEventReportSubscription struct {
	SOPClassUID  string
	EventTypeIDs []uint16
}

// NormalizedEventReportOptions explicitly enables synchronous event dispatch
// while another N-DIMSE operation is waiting for its response. The association
// operation gate remains held; handlers must not start another association
// operation or reader. Handlers must cooperate with the supplied context.
// There is no background listener for events arriving between operations.
type NormalizedEventReportOptions struct {
	Handler                   NormalizedEventReportHandler
	Subscriptions             []NormalizedEventReportSubscription
	PresentationContextPolicy NormalizedPresentationContextPolicy
	// MaxEvents bounds all received events, including refusals. Zero uses 32.
	MaxEvents int
	// MaxDataSetBytes bounds each decoded Event Information and encoded Event
	// Reply. Zero uses 4 MiB. Decoding also limits expansion, nodes and depth.
	MaxDataSetBytes int64
}

// NormalizedEventReportOutcome describes one response successfully sent to an
// interleaved event. It contains no dataset or SOP Instance UID. Err records a
// local validation/handler failure; the operation's own response and status
// remain independent. Transport failures instead return an OperationError.
type NormalizedEventReportOutcome struct {
	MessageID uint16
	Status    uint16
	Err       error
}

type normalizedEventReportState struct {
	options NormalizedEventReportOptions
	allowed map[string]map[uint16]struct{}
	count   int
}

func newNormalizedEventReportState(options *NormalizedEventReportOptions) (*normalizedEventReportState, error) {
	if options == nil {
		return nil, nil
	}
	s := &normalizedEventReportState{options: *options, allowed: make(map[string]map[uint16]struct{})}
	// Slices never remain aliased to caller-owned configuration.
	s.options.Subscriptions = nil
	if s.options.MaxEvents < 0 || s.options.MaxDataSetBytes < 0 {
		return nil, fmt.Errorf("dicom dimse: invalid interleaved event limits")
	}
	if s.options.MaxEvents == 0 {
		s.options.MaxEvents = DefaultMaxInterleavedEventReports
	}
	if s.options.MaxDataSetBytes == 0 {
		s.options.MaxDataSetBytes = MaxNormalizedDataSetBytes
	}
	for _, subscription := range options.Subscriptions {
		if !core.IsValidUID(subscription.SOPClassUID) {
			return nil, fmt.Errorf("dicom dimse: invalid interleaved event subscription SOP Class UID")
		}
		if _, exists := s.allowed[subscription.SOPClassUID]; exists {
			return nil, fmt.Errorf("dicom dimse: duplicate interleaved event subscription")
		}
		types := make(map[uint16]struct{}, len(subscription.EventTypeIDs))
		for _, eventType := range subscription.EventTypeIDs {
			if _, exists := types[eventType]; exists {
				return nil, fmt.Errorf("dicom dimse: duplicate interleaved event type")
			}
			types[eventType] = struct{}{}
		}
		s.allowed[subscription.SOPClassUID] = types
	}
	if s.options.Handler != nil && len(s.allowed) == 0 {
		return nil, fmt.Errorf("dicom dimse: interleaved event handler requires subscriptions")
	}
	return s, nil
}

func (s *normalizedEventReportState) receiveAndRespond(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object) (NormalizedEventReportOutcome, error) {
	var outcome NormalizedEventReportOutcome
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if s.count >= s.options.MaxEvents {
		return outcome, fmt.Errorf("dicom dimse: interleaved event count exceeded")
	}
	s.count++
	request, err := ParseNormalizedEventReportRequest(command)
	if err != nil {
		return outcome, err
	}
	if !core.IsValidUID(request.AffectedSOPClassUID) || !core.IsValidUID(request.AffectedSOPInstanceUID) {
		return outcome, fmt.Errorf("dicom dimse: invalid interleaved event SOP UID")
	}
	pc, err := AcceptedContextByID(assoc, pcID)
	if err != nil {
		return outcome, err
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		return outcome, err
	}
	var dataSet *object.Object
	if normalizedHasDataSet(request.CommandDataSetType) {
		dataSet, err = receiveInterleavedEventDataSet(ctx, assoc, pc.ID, syntax, s.options.MaxDataSetBytes)
		if err != nil {
			return outcome, err
		}
	}
	// Fully consume the bounded message before acknowledging either success or
	// refusal, so its fragments cannot be mistaken for the original response.
	response := NormalizedEventReportResponse{Status: StatusSuccess}
	if err := validateInterleavedEventContext(pc, request.AffectedSOPClassUID, s.options.PresentationContextPolicy); err != nil {
		response.Status, outcome.Err = StatusNoSuchSOPClass, err
	} else if s.options.Handler == nil {
		response.Status = StatusUnrecognizedOperation
	} else if allowedTypes, ok := s.allowed[request.AffectedSOPClassUID]; !ok {
		response.Status = StatusNoSuchSOPClass
	} else if _, ok := allowedTypes[request.EventTypeID]; !ok {
		response.Status = StatusNoSuchEventType
	} else {
		handlerCtx := withNormalizedRequestInfo(ctx, NormalizedRequestInfo{PresentationContext: pc, TransferSyntax: syntax})
		result, handlerErr := invokeInterleavedEventHandler(handlerCtx, s.options.Handler, *request, dataSet)
		response = result.Response
		correlationErr := errors.Join(
			correlateNormalizedResponse(&response.AffectedSOPClassUID, &response.AffectedSOPInstanceUID, &response.MessageIDBeingRespondedTo,
				request.AffectedSOPClassUID, request.AffectedSOPInstanceUID, request.MessageID, false),
			correlateNormalizedTypeID(&response.EventTypeIDOrNil, request.EventTypeID, "Event Type ID"),
		)
		if result.Response.MessageIDBeingRespondedTo != 0 && result.Response.MessageIDBeingRespondedTo != request.MessageID {
			correlationErr = errors.Join(correlationErr, fmt.Errorf("dicom dimse: interleaved handler response message ID mismatch"))
		}
		if correlationErr != nil {
			response.Status = StatusProcessingFailure
		}
		outcome.Err = normalizeHandlerStatus(&response.Status, errors.Join(handlerErr, correlationErr))
		var reply []byte
		if outcome.Err == nil && result.DataSet != nil {
			reply, err = encodeInterleavedEventReply(ctx, result.DataSet, syntax, s.options.MaxDataSetBytes)
			if err != nil {
				outcome.Err = err
				response.Status = StatusProcessingFailure
			}
		}
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		return sendInterleavedEventResponse(ctx, assoc, pc, syntax, *request, response, reply, outcome)
	}
	return sendInterleavedEventResponse(ctx, assoc, pc, syntax, *request, response, nil, outcome)
}

// Event Information has both a wire-byte and a logical/expanded-byte budget.
// On failure the exchange becomes uncertain; an unbounded drain is unsafe.
func receiveInterleavedEventDataSet(ctx context.Context, assoc *ul.Association, pcID byte, syntax transfer.Syntax, limit int64) (*object.Object, error) {
	r := newTypedPDataReaderWithContext(dataSetReadContext(ctx, assoc), assoc, pcID, false)
	wire := &interleavedDataSetReader{Reader: r, remaining: limit}
	dataSet, err := object.ReadDataSetWithOptions(wire, syntax, object.ReadFileOptions{
		MaxElementBytes: limit, MaxTotalBytes: limit,
		MaxElements: MaxNormalizedDataSetElements, MaxSequenceDepth: MaxNormalizedDataSetDepth,
		MaxFragments: MaxNormalizedDataSetElements,
	})
	if err != nil {
		return nil, err
	}
	// Consume trailing Deflated padding/fragments up to the message boundary.
	// This includes bytes after the inflater stopped producing logical data.
	if _, err := io.Copy(io.Discard, wire); err != nil {
		return nil, err
	}
	if err := validateInterleavedEventGraph(ctx, dataSet); err != nil {
		return nil, err
	}
	return dataSet, nil
}

func validateInterleavedEventContext(pc ul.AcceptedContext, class string, policy NormalizedPresentationContextPolicy) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("dicom dimse: interleaved event context policy panicked")
		}
	}()
	return validateNormalizedSCPPresentationContext(pc, class, policy)
}

// This also counts empty SQ items and fragments, which are not all primitive
// elements charged by the parser's element limit. A bounded depth check occurs
// before descending, including for a cyclic graph returned by a handler.
func validateInterleavedEventGraph(ctx context.Context, dataSet *object.Object) error {
	if dataSet.Len() > MaxNormalizedDataSetElements {
		return fmt.Errorf("dicom dimse: interleaved event graph node limit exceeded")
	}
	remaining := MaxNormalizedDataSetElements
	var visit func([]core.Element, int) error
	visit = func(elements []core.Element, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxNormalizedDataSetDepth {
			return fmt.Errorf("dicom dimse: interleaved event graph depth limit exceeded")
		}
		if len(elements) > remaining {
			return fmt.Errorf("dicom dimse: interleaved event graph node limit exceeded")
		}
		remaining -= len(elements)
		for _, element := range elements {
			switch value := element.Value.(type) {
			case core.SequenceValue:
				if depth+1 > MaxNormalizedDataSetDepth || len(value.Items) > remaining {
					return fmt.Errorf("dicom dimse: interleaved event sequence graph limit exceeded")
				}
				remaining -= len(value.Items)
				for _, item := range value.Items {
					if err := visit(item.Elements, depth+2); err != nil {
						return err
					}
				}
			case core.FragmentSequence:
				if len(value.Fragments) > remaining {
					return fmt.Errorf("dicom dimse: interleaved event fragment graph limit exceeded")
				}
				remaining -= len(value.Fragments)
			}
		}
		return nil
	}
	return visit(dataSet.Elements(), 0)
}

type interleavedDataSetReader struct {
	io.Reader
	remaining int64
}

func (r *interleavedDataSetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Read one extra byte to distinguish an exact-size message from overflow.
	if r.remaining < int64(len(p))-1 {
		p = p[:int(r.remaining)+1]
	}
	n, err := r.Reader.Read(p)
	if int64(n) > r.remaining {
		r.remaining = 0
		return 0, fmt.Errorf("dicom dimse: interleaved event wire dataset limit exceeded")
	}
	r.remaining -= int64(n)
	return n, err
}

func invokeInterleavedEventHandler(ctx context.Context, handler NormalizedEventReportHandler, request NormalizedEventReportRequest, dataSet *object.Object) (result NormalizedEventReportSCPResult, err error) {
	defer func() {
		if recover() != nil {
			result = NormalizedEventReportSCPResult{}
			err = fmt.Errorf("dicom dimse: interleaved event handler panicked")
		}
	}()
	return handler(ctx, request, dataSet)
}

func sendInterleavedEventResponse(ctx context.Context, assoc *ul.Association, pc ul.AcceptedContext, syntax transfer.Syntax, request NormalizedEventReportRequest, response NormalizedEventReportResponse, reply []byte, outcome NormalizedEventReportOutcome) (NormalizedEventReportOutcome, error) {
	response.AffectedSOPClassUID = request.AffectedSOPClassUID
	response.AffectedSOPInstanceUID = request.AffectedSOPInstanceUID
	response.MessageIDBeingRespondedTo = request.MessageID
	typeID := request.EventTypeID
	response.EventTypeIDOrNil = &typeID
	response.CommandDataSetType = NoDataSet
	var err error
	if reply != nil {
		response.CommandDataSetType = DataSetPresent
		err = sendMessageWithContext(ctx, assoc, pc.ID, response.CommandSet(), syntax, func(w io.Writer) error {
			_, err := w.Write(reply)
			return err
		})
	} else {
		err = SendCommandSetWithContext(ctx, assoc, pc.ID, response.CommandSet())
	}
	outcome.MessageID, outcome.Status = request.MessageID, response.Status
	return outcome, err
}

type interleavedReplyBuffer struct {
	bytes.Buffer
	ctx   context.Context
	limit int64
}

func (w *interleavedReplyBuffer) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-int64(w.Len()) {
		return 0, fmt.Errorf("dicom dimse: interleaved event reply exceeds dataset limit")
	}
	return w.Buffer.Write(p)
}

func encodeInterleavedEventReply(ctx context.Context, dataSet *object.Object, syntax transfer.Syntax, limit int64) ([]byte, error) {
	if err := validateInterleavedEventGraph(ctx, dataSet); err != nil {
		return nil, err
	}
	logicalSyntax := syntax
	if syntax.Deflated {
		logicalSyntax = transfer.ExplicitVRLittleEndian
	}
	w := &interleavedReplyBuffer{ctx: ctx, limit: limit}
	if err := object.WriteDataSet(w, dataSet, logicalSyntax); err != nil {
		return nil, err
	}
	if w.Len() == 0 {
		return nil, fmt.Errorf("dicom dimse: interleaved event reply dataset is empty")
	}
	// Bound the logical representation before compressing; a tiny Deflated
	// bitstream does not prove its expansion or object graph is bounded.
	validated, err := object.ReadDataSetWithOptions(bytes.NewReader(w.Bytes()), logicalSyntax, object.ReadFileOptions{
		MaxElementBytes: limit, MaxTotalBytes: limit,
		MaxElements: MaxNormalizedDataSetElements, MaxSequenceDepth: MaxNormalizedDataSetDepth,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if syntax.Deflated {
		wire := &interleavedReplyBuffer{ctx: ctx, limit: limit}
		if err := object.WriteDataSet(wire, validated, syntax); err != nil {
			return nil, err
		}
		return wire.Bytes(), nil
	}
	return w.Bytes(), nil
}
