package dimse

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Retrieve statuses of PS3.4 C.4.2.1.5 and C.4.3.1.4 used by the proxy.
const (
	// StatusRetrieveOutOfResourcesMatches is "Refused: Out of Resources -
	// Unable to calculate number of matches" (A701H).
	StatusRetrieveOutOfResourcesMatches uint16 = 0xA701
	// StatusRetrieveOutOfResourcesSubOperations is "Refused: Out of
	// Resources - Unable to perform sub-operations" (A702H).
	StatusRetrieveOutOfResourcesSubOperations uint16 = 0xA702
)

var (
	// ErrMoveDestinationUnknown reports a Move Destination the policy does
	// not know.
	ErrMoveDestinationUnknown = errors.New("dicom dimse: Move Destination unknown")
	// ErrMoveDestinationRefused reports a Move Destination the policy knows
	// but does not allow for the calling AE Title. The C-MOVE answer is the
	// same A801H as for an unknown one, so a caller cannot probe the policy.
	ErrMoveDestinationRefused = errors.New("dicom dimse: Move Destination refused by policy")
	// ErrRetrieveSourceInstance reports that the retrieve source did not
	// provide a located instance, or provided a different one.
	ErrRetrieveSourceInstance = errors.New("dicom dimse: retrieve source did not provide the instance")
)

// RetrieveSourceRequest is the retrieve a RetrieveSource resolves: the
// downstream C-MOVE or C-GET as the proxy received it.
type RetrieveSourceRequest struct {
	// SOPClassUID is the request's Query/Retrieve Information Model: a
	// Patient or Study Root MOVE or GET SOP Class.
	SOPClassUID        string
	QueryRetrieveLevel string
	Identifier         *object.Object
	Priority           uint16
	// CallingAETitle is the downstream requester.
	CallingAETitle string
	// MoveDestination is empty for C-GET.
	MoveDestination string
}

// RetrieveInstance identifies one instance a RetrieveSource located. The
// study and series UIDs and the transfer syntax are optional hints for the
// source's own Fetch and for presentation context negotiation.
// SOPClassUID may be empty when the upstream does not report it (DCMTK's
// dcmqrscp does not return it in C-FIND): the class is then read from the
// fetched instance, and the peers are offered the default Storage SOP
// Classes (DefaultCGetStorageSOPClassUIDs) besides the known ones.
type RetrieveInstance struct {
	SOPClassUID       string
	SOPInstanceUID    string
	StudyInstanceUID  string
	SeriesInstanceUID string
	TransferSyntaxUID string
}

// RetrieveSet is the located result of one retrieve. The proxy calls Fetch
// once per instance, in Instances order, from one goroutine, then Close
// exactly once.
type RetrieveSet interface {
	Instances() []RetrieveInstance
	// Fetch obtains one located instance. transferSyntaxUIDs lists, in
	// preference order, the syntaxes the downstream peer accepted for the
	// instance's SOP Class (empty while the class is unknown); a native
	// syntax may be converted to another accepted native one, compressed
	// pixel data is sent only as itself.
	Fetch(ctx context.Context, instance RetrieveInstance, transferSyntaxUIDs []string) (*object.File, error)
	Close() error
}

// RetrieveSource resolves retrieves against an upstream: another archive
// over DIMSE (NewDIMSERetrieveSource), WADO-RS, a cache. It is the clean-room
// counterpart of an instance locator: Locate lists the instances first, so
// every C-MOVE and C-GET response carries exact sub-operation counts.
type RetrieveSource interface {
	Locate(ctx context.Context, request RetrieveSourceRequest) (RetrieveSet, error)
}

// RetrieveSourceFuncs adapts a locate and a fetch function to RetrieveSource,
// for sources without per-retrieve state such as a WADO-RS client.
type RetrieveSourceFuncs struct {
	LocateFunc func(context.Context, RetrieveSourceRequest) ([]RetrieveInstance, error)
	FetchFunc  func(context.Context, RetrieveInstance, []string) (*object.File, error)
}

// Locate calls LocateFunc.
func (f RetrieveSourceFuncs) Locate(ctx context.Context, request RetrieveSourceRequest) (RetrieveSet, error) {
	if f.LocateFunc == nil || f.FetchFunc == nil {
		return nil, errors.New("dicom dimse: retrieve source functions are not set")
	}
	instances, err := f.LocateFunc(ctx, request)
	if err != nil {
		return nil, err
	}
	return funcRetrieveSet{instances: instances, fetch: f.FetchFunc}, nil
}

type funcRetrieveSet struct {
	instances []RetrieveInstance
	fetch     func(context.Context, RetrieveInstance, []string) (*object.File, error)
}

func (s funcRetrieveSet) Instances() []RetrieveInstance { return s.instances }
func (s funcRetrieveSet) Fetch(ctx context.Context, instance RetrieveInstance, syntaxes []string) (*object.File, error) {
	return s.fetch(ctx, instance, syntaxes)
}
func (s funcRetrieveSet) Close() error { return nil }

// MoveDestinationAddress is where C-MOVE sub-operations for one AE Title go.
type MoveDestinationAddress struct {
	// Address is host:port.
	Address string
	// TLSConfig secures the destination association; nil uses the proxy's
	// DestinationDialOptions.TLSConfig.
	TLSConfig *tls.Config
}

// MoveDestinationRequest is what a MoveDestinationResolver decides on.
type MoveDestinationRequest struct {
	CallingAETitle  string
	CalledAETitle   string
	MoveDestination string
}

// MoveDestinationResolver maps a C-MOVE's Move Destination to an address
// and enforces the destination policy. Errors wrapping
// ErrMoveDestinationUnknown or ErrMoveDestinationRefused, and any other
// error, refuse the C-MOVE with A801H before the source is consulted.
type MoveDestinationResolver interface {
	ResolveMoveDestination(context.Context, MoveDestinationRequest) (MoveDestinationAddress, error)
}

// MoveDestinationPolicy is a static MoveDestinationResolver.
type MoveDestinationPolicy struct {
	// Destinations maps a Move Destination AE Title to its address.
	Destinations map[string]MoveDestinationAddress
	// Callers optionally restricts, per Move Destination, the calling AE
	// Titles that may move there; a destination absent from Callers is open
	// to every caller.
	Callers map[string][]string
	// CallerOnly allows a caller to move only to its own AE Title.
	CallerOnly bool
}

// ResolveMoveDestination applies the policy.
func (p MoveDestinationPolicy) ResolveMoveDestination(_ context.Context, request MoveDestinationRequest) (MoveDestinationAddress, error) {
	name := strings.TrimSpace(request.MoveDestination)
	caller := strings.TrimSpace(request.CallingAETitle)
	destination, ok := p.Destinations[name]
	if !ok || strings.TrimSpace(destination.Address) == "" {
		return MoveDestinationAddress{}, ErrMoveDestinationUnknown
	}
	if p.CallerOnly && name != caller {
		return MoveDestinationAddress{}, ErrMoveDestinationRefused
	}
	if allowed, restricted := p.Callers[name]; restricted {
		for _, candidate := range allowed {
			if strings.TrimSpace(candidate) == caller {
				return destination, nil
			}
		}
		return MoveDestinationAddress{}, ErrMoveDestinationRefused
	}
	return destination, nil
}

// RetrieveProxyOptions configures a RetrieveProxy.
type RetrieveProxyOptions struct {
	// Source locates and fetches the instances. Required.
	Source RetrieveSource
	// Destinations resolves C-MOVE destinations; nil refuses every C-MOVE
	// with A801H.
	Destinations MoveDestinationResolver
	// AETitle is the proxy's calling AE Title toward Move Destinations.
	AETitle string
	// DestinationDialOptions is the template for destination associations
	// (timeouts, MaxPDU, TLS, user identity). Its AE Titles, presentation
	// contexts and role selections are set by the proxy.
	DestinationDialOptions ul.DialOptions
	// DestinationTransferSyntaxUIDs are proposed to Move Destinations for
	// every SOP Class, after the transfer syntax hints of the located
	// instances; empty is Explicit then Implicit VR Little Endian.
	DestinationTransferSyntaxUIDs []string
	// MaxInstances bounds the instances of one retrieve; zero or more than
	// 65535 (the largest sub-operation count) is 65535. A larger retrieve
	// is refused with A702H.
	MaxInstances int
}

// RetrieveProxy is a C-MOVE and C-GET SCP that forwards a retrieve to a
// RetrieveSource and delivers what it fetches: C-MOVE sub-operations go to
// the resolved Move Destination with the requester as Move Originator,
// C-GET sub-operations to the requester on its own association. Every
// sub-operation's status is the C-STORE status the receiving peer
// returned, so completed, warning and failed counts are the receiver's;
// an instance the source cannot provide is a failed sub-operation.
//
// Like the rest of the network stack, the proxy is meant for trusted
// networks: it adds no authentication, and its destination policy is only
// as strong as the AE Titles peers present unless the associations are
// authenticated (TLS, user identity) by the caller.
type RetrieveProxy struct {
	options RetrieveProxyOptions
}

// NewRetrieveProxy validates options and returns a proxy.
func NewRetrieveProxy(options RetrieveProxyOptions) (*RetrieveProxy, error) {
	if options.Source == nil {
		return nil, errors.New("dicom dimse: retrieve proxy source is required")
	}
	if options.MaxInstances <= 0 || options.MaxInstances > maxCMoveSubOperations {
		options.MaxInstances = maxCMoveSubOperations
	}
	if len(options.DestinationTransferSyntaxUIDs) == 0 {
		options.DestinationTransferSyntaxUIDs = []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}
	}
	options.DestinationTransferSyntaxUIDs = normalizedTransferSyntaxUIDs(options.DestinationTransferSyntaxUIDs)
	if strings.TrimSpace(options.AETitle) == "" {
		options.AETitle = "DICOMGO_PROXY"
	}
	return &RetrieveProxy{options: options}, nil
}

// Handler returns the C-MOVE and C-GET handler for one accepted
// association, for AssociationSCPOptions.CMoveHandler and CGetHandler. It
// reads the requester's AE Title and the C-GET storage contexts from assoc.
func (p *RetrieveProxy) Handler(assoc *ul.Association) *RetrieveProxyHandler {
	return &RetrieveProxyHandler{proxy: p, assoc: assoc}
}

// RetrieveProxyHandler serves one association; see RetrieveProxy.
type RetrieveProxyHandler struct {
	proxy *RetrieveProxy
	assoc *ul.Association
}

var (
	_ CMoveBatchHandler = (*RetrieveProxyHandler)(nil)
	_ CGetHandler       = (*RetrieveProxyHandler)(nil)
)

func (h *RetrieveProxyHandler) callingAETitle() string {
	if h == nil || h.assoc == nil {
		return ""
	}
	return strings.TrimSpace(h.assoc.CallingAETitle)
}

func (h *RetrieveProxyHandler) calledAETitle() string {
	if h == nil || h.assoc == nil {
		return ""
	}
	return strings.TrimSpace(h.assoc.CalledAETitle)
}

// locate resolves the request and closes the set when ctx ends, which the
// SCP does when the operation is over.
func (h *RetrieveProxyHandler) locate(ctx context.Context, request RetrieveSourceRequest) (*proxyRetrieveSet, error) {
	if h == nil || h.proxy == nil {
		return nil, errors.New("dicom dimse: nil retrieve proxy")
	}
	set, err := h.proxy.options.Source.Locate(ctx, request)
	if err != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return nil, errors.Join(ctxErr, err)
		}
		return nil, retrieveProxyStatusError{status: StatusRetrieveOutOfResourcesMatches, comment: "Unable to calculate number of matches", err: err}
	}
	if set == nil {
		return nil, retrieveProxyStatusError{status: StatusRetrieveOutOfResourcesMatches, comment: "Unable to calculate number of matches", err: errors.New("dicom dimse: retrieve source returned no set")}
	}
	wrapped := &proxyRetrieveSet{set: set}
	context.AfterFunc(ctx, wrapped.close)
	instances := set.Instances()
	if len(instances) > h.proxy.options.MaxInstances {
		wrapped.close()
		return nil, retrieveProxyStatusError{status: StatusRetrieveOutOfResourcesSubOperations, comment: "Too many sub-operations", err: fmt.Errorf("dicom dimse: retrieve source located %d instances, limit %d", len(instances), h.proxy.options.MaxInstances)}
	}
	for i, instance := range instances {
		if (instance.SOPClassUID != "" && !core.IsValidUID(instance.SOPClassUID)) || !core.IsValidUID(instance.SOPInstanceUID) {
			wrapped.close()
			return nil, retrieveProxyStatusError{status: StatusRetrieveOutOfResourcesMatches, comment: "Unable to calculate number of matches", err: fmt.Errorf("dicom dimse: retrieve source instance %d has an invalid SOP Class or Instance UID", i)}
		}
	}
	wrapped.instances = append([]RetrieveInstance(nil), instances...)
	return wrapped, nil
}

type proxyRetrieveSet struct {
	set       RetrieveSet
	instances []RetrieveInstance
	once      sync.Once
}

func (s *proxyRetrieveSet) close() { s.once.Do(func() { _ = s.set.Close() }) }

// fetch obtains one instance and checks that it is the located one.
func (s *proxyRetrieveSet) fetch(ctx context.Context, instance RetrieveInstance, syntaxes []string) (*object.File, error) {
	file, err := s.set.Fetch(ctx, instance, syntaxes)
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return nil, errors.Join(ctxErr, ErrRetrieveSourceInstance)
		}
		return nil, retrieveSourceError{err: err}
	}
	if file == nil || file.Dataset == nil {
		return nil, ErrRetrieveSourceInstance
	}
	classUID, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0016))
	instanceUID, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if (instance.SOPClassUID != "" && classUID != instance.SOPClassUID) || !core.IsValidUID(classUID) || instanceUID != instance.SOPInstanceUID {
		_ = file.Close()
		return nil, fmt.Errorf("%w: fetched data set is a different instance", ErrRetrieveSourceInstance)
	}
	return file, nil
}

// retrieveSourceError keeps the source's error for errors.Is and errors.As
// but prints only ErrRetrieveSourceInstance: a C-GET answer carries the
// last sub-operation error as its Error Comment, and an upstream's error
// text may name patients.
type retrieveSourceError struct{ err error }

func (e retrieveSourceError) Error() string   { return ErrRetrieveSourceInstance.Error() }
func (e retrieveSourceError) Unwrap() []error { return []error{ErrRetrieveSourceInstance, e.err} }

// retrieveProxyStatusError carries a refusal status for either service.
type retrieveProxyStatusError struct {
	status  uint16
	comment string
	err     error
}

func (e retrieveProxyStatusError) Error() string {
	return fmt.Sprintf("dicom dimse: retrieve proxy status 0x%04X: %s: %v", e.status, e.comment, e.err)
}

func (e retrieveProxyStatusError) Unwrap() error { return e.err }

func (e retrieveProxyStatusError) move() error {
	return NewCMoveSCPError(e.status, e.comment, e.err)
}

func (e retrieveProxyStatusError) get() error {
	return NewCGetSCPError(e.status, e.comment, e.err)
}

func asMoveError(err error) error {
	var status retrieveProxyStatusError
	if errors.As(err, &status) {
		return status.move()
	}
	return err
}

func asGetError(err error) error {
	var status retrieveProxyStatusError
	if errors.As(err, &status) {
		return status.get()
	}
	return err
}

// resolveDestination applies the destination policy before anything else.
func (h *RetrieveProxyHandler) resolveDestination(ctx context.Context, request CMoveRequestContext) (MoveDestinationAddress, error) {
	if h.proxy.options.Destinations == nil {
		return MoveDestinationAddress{}, NewCMoveSCPError(StatusCMoveMoveDestinationUnknown, "Move Destination unknown", ErrMoveDestinationUnknown)
	}
	destination, err := h.proxy.options.Destinations.ResolveMoveDestination(ctx, MoveDestinationRequest{
		CallingAETitle:  h.callingAETitle(),
		CalledAETitle:   h.calledAETitle(),
		MoveDestination: strings.TrimSpace(request.Request.MoveDestination),
	})
	if err == nil && strings.TrimSpace(destination.Address) == "" {
		err = ErrMoveDestinationUnknown
	}
	if err != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return MoveDestinationAddress{}, errors.Join(ctxErr, err)
		}
		return MoveDestinationAddress{}, NewCMoveSCPError(StatusCMoveMoveDestinationUnknown, "Move Destination unknown", err)
	}
	return destination, nil
}

func (h *RetrieveProxyHandler) prepareMove(ctx context.Context, request CMoveRequestContext) (*moveForwarder, error) {
	destination, err := h.resolveDestination(ctx, request)
	if err != nil {
		return nil, err
	}
	set, err := h.locate(ctx, RetrieveSourceRequest{
		SOPClassUID:        request.Request.AffectedSOPClassUID,
		QueryRetrieveLevel: request.QueryRetrieveLevel,
		Identifier:         request.Identifier,
		Priority:           request.Request.Priority,
		CallingAETitle:     h.callingAETitle(),
		MoveDestination:    strings.TrimSpace(request.Request.MoveDestination),
	})
	if err != nil {
		return nil, asMoveError(err)
	}
	forwarder := &moveForwarder{
		proxy:           h.proxy,
		set:             set,
		destination:     destination,
		destinationAE:   strings.TrimSpace(request.Request.MoveDestination),
		originatorAE:    h.callingAETitle(),
		originatorMsgID: request.Request.MessageID,
		priority:        request.Request.Priority,
	}
	context.AfterFunc(ctx, forwarder.release)
	return forwarder, nil
}

// PrepareCMoveBatch locates the instances, then forwards them to the Move
// Destination over one association. A destination that cannot be reached
// refuses the C-MOVE with A702H.
func (h *RetrieveProxyHandler) PrepareCMoveBatch(ctx context.Context, request CMoveRequestContext) (CMoveBatch, error) {
	forwarder, err := h.prepareMove(ctx, request)
	if err != nil {
		return CMoveBatch{}, err
	}
	descriptors := make([]CMoveSubOperationDescriptor, 0, len(forwarder.set.instances))
	for _, instance := range forwarder.set.instances {
		descriptors = append(descriptors, CMoveSubOperationDescriptor{AffectedSOPClassUID: instance.SOPClassUID, AffectedSOPInstanceUID: instance.SOPInstanceUID})
	}
	return CMoveBatch{SubOperations: descriptors, Run: func(ctx context.Context, yield func(CMoveSubOperationResult) error) error {
		defer forwarder.release()
		if len(forwarder.set.instances) == 0 {
			return nil
		}
		if err := forwarder.connect(ctx); err != nil {
			if ctxErr := context.Cause(ctx); ctxErr != nil {
				return errors.Join(ctxErr, err)
			}
			return NewCMoveSCPError(StatusRetrieveOutOfResourcesSubOperations, "Unable to perform sub-operations", err)
		}
		for _, instance := range forwarder.set.instances {
			if err := yield(forwarder.forward(ctx, instance)); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

// Move is the per-sub-operation form of PrepareCMoveBatch, for callers that
// drive CMoveHandler directly; the sub-operations share one destination
// association, opened by the first.
func (h *RetrieveProxyHandler) Move(ctx context.Context, request CMoveRequestContext) ([]CMoveSubOperation, error) {
	forwarder, err := h.prepareMove(ctx, request)
	if err != nil {
		return nil, err
	}
	ops := make([]CMoveSubOperation, 0, len(forwarder.set.instances))
	for _, instance := range forwarder.set.instances {
		instance := instance
		ops = append(ops, CMoveSubOperation{
			AffectedSOPClassUID:    instance.SOPClassUID,
			AffectedSOPInstanceUID: instance.SOPInstanceUID,
			Store: func(ctx context.Context) CMoveSubOperationResult {
				if err := forwarder.connect(ctx); err != nil {
					return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: err}
				}
				return forwarder.forward(ctx, instance)
			},
		})
	}
	return ops, nil
}

// Get locates the instances and returns one sub-operation each, sent on the
// requester's association in the SOP Classes it accepted with the SCP role.
func (h *RetrieveProxyHandler) Get(ctx context.Context, request CGetRequestContext) ([]CGetSubOperation, error) {
	set, err := h.locate(ctx, RetrieveSourceRequest{
		SOPClassUID:        request.Request.AffectedSOPClassUID,
		QueryRetrieveLevel: request.QueryRetrieveLevel,
		Identifier:         request.Identifier,
		Priority:           request.Request.Priority,
		CallingAETitle:     h.callingAETitle(),
	})
	if err != nil {
		return nil, asGetError(err)
	}
	ops := make([]CGetSubOperation, 0, len(set.instances))
	for _, instance := range set.instances {
		instance := instance
		ops = append(ops, CGetSubOperation{
			AffectedSOPClassUID:    instance.SOPClassUID,
			AffectedSOPInstanceUID: instance.SOPInstanceUID,
			LoadFile: func(ctx context.Context) (*object.File, error) {
				return set.fetch(ctx, instance, acceptedTransferSyntaxUIDs(h.assoc, instance.SOPClassUID))
			},
		})
	}
	return ops, nil
}

// moveForwarder delivers one C-MOVE's instances to its destination.
type moveForwarder struct {
	proxy           *RetrieveProxy
	set             *proxyRetrieveSet
	destination     MoveDestinationAddress
	destinationAE   string
	originatorAE    string
	originatorMsgID uint16
	priority        uint16

	mu        sync.Mutex
	assoc     *ul.Association
	client    *StoreClient
	dialErr   error
	broken    error
	messageID uint16
	released  bool
}

// connect opens the destination association once, proposing every located
// SOP Class.
func (f *moveForwarder) connect(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assoc != nil || f.dialErr != nil {
		return f.dialErr
	}
	if f.released {
		f.dialErr = errors.New("dicom dimse: C-MOVE forwarding already finished")
		return f.dialErr
	}
	contexts, err := destinationContexts(f.set.instances, f.proxy.options.DestinationTransferSyntaxUIDs)
	if err != nil {
		f.dialErr = err
		return err
	}
	options := f.proxy.options.DestinationDialOptions
	options.CallingAETitle = f.proxy.options.AETitle
	options.CalledAETitle = f.destinationAE
	options.Contexts = contexts
	options.RoleSelections = nil
	options.ExtendedNegotiation = nil
	options.CommonExtendedNegotiation = nil
	options.AsynchronousOperationsWindow = nil
	if f.destination.TLSConfig != nil {
		options.TLSConfig = f.destination.TLSConfig
	}
	assoc, err := ul.DialContext(ctx, strings.TrimSpace(f.destination.Address), options)
	if err != nil {
		f.dialErr = fmt.Errorf("dicom dimse: Move Destination association failed: %w", err)
		return f.dialErr
	}
	f.assoc = assoc
	f.client = NewStoreClient(assoc)
	return nil
}

// forward fetches one instance and stores it at the destination; the
// result's status is the destination's C-STORE status.
func (f *moveForwarder) forward(ctx context.Context, instance RetrieveInstance) CMoveSubOperationResult {
	f.mu.Lock()
	assoc, client, broken := f.assoc, f.client, f.broken
	f.mu.Unlock()
	if assoc == nil {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: errors.New("dicom dimse: no Move Destination association")}
	}
	if broken != nil {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: broken}
	}
	classUID := instance.SOPClassUID
	accepted := acceptedTransferSyntaxUIDs(assoc, classUID)
	if classUID != "" && len(accepted) == 0 {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: fmt.Errorf("%w: SOP Class not accepted by the Move Destination", ErrStorePresentationContextRejected)}
	}
	file, err := f.set.fetch(ctx, instance, accepted)
	if err != nil {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: err}
	}
	defer file.Close()
	if classUID == "" {
		classUID, _ = file.Dataset.GetUID(core.NewTag(0x0008, 0x0016))
		if accepted = acceptedTransferSyntaxUIDs(assoc, classUID); len(accepted) == 0 {
			return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: fmt.Errorf("%w: SOP Class not accepted by the Move Destination", ErrStorePresentationContextRejected)}
		}
	}
	syntaxes := StoreTransferSyntaxUIDs(file.TransferSyntax.UID, accepted)
	if len(syntaxes) == 0 {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: fmt.Errorf("%w: no accepted context for the fetched transfer syntax", ErrStoreTransferSyntax)}
	}
	f.mu.Lock()
	f.messageID++
	if f.messageID == 0 {
		f.messageID = 1
	}
	messageID := f.messageID
	f.mu.Unlock()
	originatorMsgID := f.originatorMsgID
	result, err := client.StoreWithOptions(ctx, file.Dataset, CStoreOptions{
		AffectedSOPClassUID:          classUID,
		AffectedSOPInstanceUID:       instance.SOPInstanceUID,
		TransferSyntaxUIDs:           syntaxes,
		MessageID:                    messageID,
		Priority:                     f.priority,
		MoveOriginatorAETitle:        f.originatorAE,
		MoveOriginatorMessageIDOrNil: &originatorMsgID,
	})
	if result.Response != nil {
		// The receiver answered: its status is the sub-operation's, warning
		// and failure included.
		return CMoveSubOperationResult{Status: result.Response.Status}
	}
	if err == nil {
		err = errors.New("dicom dimse: C-STORE returned no response")
	}
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: errors.Join(ctxErr, err)}
	}
	var missing *MissingPresentationContextError
	if !errors.As(err, &missing) && !errors.Is(err, ErrStoreTransferSyntax) {
		// The association state is unknown after a transport failure: the
		// remaining sub-operations fail without fetching.
		f.mu.Lock()
		f.broken = fmt.Errorf("dicom dimse: Move Destination association failed: %w", err)
		f.mu.Unlock()
		_ = assoc.Abort(ul.AbortReasonNotSpecified)
	}
	return CMoveSubOperationResult{Status: StatusCStoreOutOfResources, Err: err}
}

// release ends the destination association and closes the set.
func (f *moveForwarder) release() {
	f.mu.Lock()
	assoc, broken := f.assoc, f.broken
	f.assoc, f.client = nil, nil
	f.released = true
	f.mu.Unlock()
	if assoc != nil {
		if broken == nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeoutOrDefault(f.proxy.options.DestinationDialOptions))
			if err := assoc.Release(releaseCtx); err != nil {
				_ = assoc.Abort(ul.AbortReasonNotSpecified)
			}
			cancel()
		}
		_ = assoc.Close()
	}
	f.set.close()
}

const defaultProxyReleaseTimeout = 5 * time.Second

func releaseTimeoutOrDefault(options ul.DialOptions) time.Duration {
	if options.ReleaseTimeout > 0 {
		return options.ReleaseTimeout
	}
	return defaultProxyReleaseTimeout
}

// destinationContexts proposes, per SOP Class, one context per transfer
// syntax (the instances' hints first) so each is accepted or rejected on
// its own; when that exceeds 128 contexts, one context per SOP Class. An
// instance of unknown class adds the default Storage SOP Classes, as many
// as fit.
func destinationContexts(instances []RetrieveInstance, syntaxes []string) ([]ul.PresentationContext, error) {
	var classes []string
	perClass := map[string][]string{}
	unknown := false
	for _, instance := range instances {
		if instance.SOPClassUID == "" {
			unknown = true
			continue
		}
		if _, ok := perClass[instance.SOPClassUID]; !ok {
			classes = append(classes, instance.SOPClassUID)
			perClass[instance.SOPClassUID] = nil
		}
		if hint := transfer.NormalizeUID(instance.TransferSyntaxUID); hint != "" {
			perClass[instance.SOPClassUID] = append(perClass[instance.SOPClassUID], hint)
		}
	}
	if unknown {
		for _, class := range DefaultCGetStorageSOPClassUIDs() {
			if len(classes) >= 128 {
				break
			}
			if _, ok := perClass[class]; !ok {
				classes = append(classes, class)
				perClass[class] = nil
			}
		}
	}
	total := 0
	for _, class := range classes {
		perClass[class] = normalizedTransferSyntaxUIDs(append(perClass[class], syntaxes...))
		total += len(perClass[class])
	}
	if len(classes) > 128 {
		return nil, fmt.Errorf("dicom dimse: %d SOP Classes exceed the 128 presentation contexts of one association", len(classes))
	}
	var contexts []ul.PresentationContext
	id := byte(1)
	for _, class := range classes {
		if total <= 128 {
			for _, syntax := range perClass[class] {
				contexts = append(contexts, ul.PresentationContext{ID: id, AbstractSyntaxUID: class, TransferSyntaxUIDs: []string{syntax}})
				id += 2
			}
			continue
		}
		contexts = append(contexts, ul.PresentationContext{ID: id, AbstractSyntaxUID: class, TransferSyntaxUIDs: perClass[class]})
		id += 2
	}
	return contexts, nil
}

// acceptedTransferSyntaxUIDs lists the transfer syntaxes assoc accepted for
// a SOP Class, in context order.
func acceptedTransferSyntaxUIDs(assoc *ul.Association, sopClassUID string) []string {
	if assoc == nil {
		return nil
	}
	var syntaxes []string
	for _, pc := range assoc.AcceptedContexts {
		if pc.AbstractSyntaxUID == sopClassUID {
			syntaxes = append(syntaxes, pc.TransferSyntaxUID)
		}
	}
	return normalizedTransferSyntaxUIDs(syntaxes)
}

// StoreTransferSyntaxUIDs returns the transfer syntaxes a data set held in
// sourceUID can be sent in without transcoding pixel data: sourceUID
// itself, and for a native syntax the other native little endian ones.
// When allowed is not empty, the result keeps only the syntaxes it lists.
func StoreTransferSyntaxUIDs(sourceUID string, allowed []string) []string {
	candidates := transfer.ProposedStoreTransferSyntaxUIDs(sourceUID, transfer.NativeStoreSourceFirst)
	if len(allowed) == 0 {
		return candidates
	}
	permitted := map[string]bool{}
	for _, uid := range allowed {
		permitted[transfer.NormalizeUID(uid)] = true
	}
	out := candidates[:0:0]
	for _, uid := range candidates {
		if permitted[uid] {
			out = append(out, uid)
		}
	}
	return out
}
