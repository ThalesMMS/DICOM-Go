package ian

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Received is one validated notification handed to a Handler.
type Received struct {
	// SOPInstanceUID is the notification UID, from the request or assigned
	// by the Service.
	SOPInstanceUID string
	// CallingAETitle and CalledAETitle identify the association; they are
	// empty when Service.Receive is called directly.
	CallingAETitle string
	CalledAETitle  string
	Notification   Notification
	// DataSet is the Attribute List as received, owned by the Handler.
	DataSet *object.Object
}

// Handler consumes a notification. A nil error answers Success. A
// *StatusError answers its status; any other error answers 0110H Processing
// Failure. The Handler runs on the association's goroutine and should hand
// long work (a retrieve) to the application's own queue.
type Handler func(ctx context.Context, received Received) error

// Limits bound one notification. Zero fields select the defaults.
type Limits struct {
	// MaxDataSetBytes bounds a received Attribute List (default 4 MiB).
	MaxDataSetBytes int64
	// MaxSeries bounds the Referenced Series Sequence (default 1000).
	MaxSeries int
	// MaxInstances bounds the Referenced SOP Sequence items over all series
	// (default 100000).
	MaxInstances int
}

// ServiceOptions configure a Service.
type ServiceOptions struct {
	Limits Limits
	// UIDGenerator assigns the notification UID when N-CREATE omits it;
	// nil uses a random 2.25 UID.
	UIDGenerator func() (string, error)
	// RememberedNotifications bounds how many accepted notification UIDs
	// the Service remembers to answer a repeated UID with 0111H Duplicate
	// SOP Instance (default 4096; the oldest are forgotten first).
	RememberedNotifications int
	// AcceptIdenticalReplay answers a repeated notification UID whose
	// Attribute List is byte for byte the one already accepted with success,
	// without calling the Handler again. It lets an SCU resend a
	// notification whose outcome it could not observe.
	AcceptIdenticalReplay bool
}

// Service is the Instance Availability Notification SCP. It validates each
// N-CREATE, refuses a repeated or concurrent notification UID and hands the
// rest to its Handler. It is safe for concurrent use.
type Service struct {
	handler  Handler
	limits   Limits
	uids     func() (string, error)
	replay   bool
	capacity int

	mu       sync.Mutex
	accepted map[string][sha256.Size]byte
	order    []string
	inflight map[string]bool
}

// NewService returns an SCP that passes each valid notification to handler.
func NewService(handler Handler, options ServiceOptions) (*Service, error) {
	if handler == nil {
		return nil, errors.New("ian: nil handler")
	}
	limits := options.Limits
	if limits.MaxDataSetBytes < 0 || limits.MaxSeries < 0 || limits.MaxInstances < 0 || options.RememberedNotifications < 0 {
		return nil, ErrResourceLimit
	}
	if limits.MaxDataSetBytes == 0 {
		limits.MaxDataSetBytes = 4 << 20
	}
	if limits.MaxSeries == 0 {
		limits.MaxSeries = 1000
	}
	if limits.MaxInstances == 0 {
		limits.MaxInstances = 100000
	}
	capacity := options.RememberedNotifications
	if capacity == 0 {
		capacity = 4096
	}
	uids := options.UIDGenerator
	if uids == nil {
		uids = core.NewUID
	}
	return &Service{
		handler: handler, limits: limits, uids: uids, replay: options.AcceptIdenticalReplay, capacity: capacity,
		accepted: map[string][sha256.Size]byte{}, inflight: map[string]bool{},
	}, nil
}

// Receive validates one notification and calls the Handler. An empty
// sopInstanceUID is assigned by the Service.
func (s *Service) Receive(ctx context.Context, sopInstanceUID string, attributes *object.Object) (Received, error) {
	return s.receive(ctx, Received{SOPInstanceUID: sopInstanceUID, DataSet: attributes})
}

func (s *Service) receive(ctx context.Context, received Received) (Received, error) {
	if err := ctx.Err(); err != nil {
		return Received{}, err
	}
	uid := strings.TrimSpace(received.SOPInstanceUID)
	if uid == "" {
		generated, err := s.uids()
		if err != nil {
			return Received{}, err
		}
		uid = generated
	}
	if !core.IsValidUID(uid) {
		return Received{}, &StatusError{Status: DIMSEStatusInvalidObjectInstance, Err: ErrInvalidDataSet}
	}
	received.SOPInstanceUID = uid
	notification, err := Parse(received.DataSet)
	if err != nil {
		return Received{}, err
	}
	if len(notification.Series) > s.limits.MaxSeries || notification.Count() > s.limits.MaxInstances {
		return Received{}, &StatusError{Status: DIMSEStatusResourceLimitation, Tags: []core.Tag{TagReferencedSeriesSequence}, Err: ErrResourceLimit}
	}
	received.Notification = notification
	digest, err := dataSetDigest(received.DataSet)
	if err != nil {
		return Received{}, err
	}
	replayed, err := s.begin(uid, digest)
	if err != nil || replayed {
		return received, err
	}
	err = s.handler(ctx, received)
	s.finish(uid, digest, err == nil)
	if err != nil {
		return Received{}, err
	}
	return received, nil
}

// begin reserves uid for one handler call; replayed reports an identical
// repetition of an accepted notification.
func (s *Service) begin(uid string, digest [sha256.Size]byte) (replayed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.accepted[uid]; ok {
		if s.replay && previous == digest {
			return true, nil
		}
		return false, &StatusError{Status: DIMSEStatusDuplicateSOPInstance, Err: ErrDuplicate}
	}
	if s.inflight[uid] {
		return false, &StatusError{Status: DIMSEStatusDuplicateSOPInstance, Err: ErrDuplicate}
	}
	s.inflight[uid] = true
	return false, nil
}

func (s *Service) finish(uid string, digest [sha256.Size]byte, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, uid)
	if !accepted {
		return
	}
	s.accepted[uid] = digest
	s.order = append(s.order, uid)
	for len(s.order) > s.capacity {
		delete(s.accepted, s.order[0])
		s.order = s.order[1:]
	}
}

func dataSetDigest(ds *object.Object) ([sha256.Size]byte, error) {
	var buf bytes.Buffer
	if err := object.WriteDataSet(&buf, ds, transfer.ExplicitVRLittleEndian); err != nil {
		return [sha256.Size]byte{}, &StatusError{Status: DIMSEStatusInvalidAttributeValue, Err: ErrInvalidDataSet}
	}
	return sha256.Sum256(buf.Bytes()), nil
}

// NormalizedOptions returns the N-CREATE handler for one association. Only
// the Instance Availability Notification SOP Class is accepted; the other
// normalized operations are not provided, so the runtime refuses them with
// 0211H Unrecognized Operation.
func (s *Service) NormalizedOptions(assoc *ul.Association) dimse.NormalizedSCPOptions {
	var calling, called string
	if assoc != nil {
		calling, called = assoc.CallingAETitle, assoc.CalledAETitle
	}
	return dimse.NormalizedSCPOptions{
		MaxDataSetBytes: s.limits.MaxDataSetBytes,
		PresentationContextPolicy: func(pc ul.AcceptedContext, commandSOPClassUID string) error {
			if pc.AbstractSyntaxUID != SOPClassUID || commandSOPClassUID != SOPClassUID {
				return &dimse.NormalizedPresentationContextError{PresentationContextID: pc.ID, AbstractSyntaxUID: pc.AbstractSyntaxUID, CommandSOPClassUID: commandSOPClassUID}
			}
			return nil
		},
		CreateHandler: func(ctx context.Context, request dimse.NormalizedCreateRequest, attributes *object.Object) (dimse.NormalizedCreateSCPResult, error) {
			received, err := s.receive(ctx, Received{SOPInstanceUID: request.AffectedSOPInstanceUID, CallingAETitle: calling, CalledAETitle: called, DataSet: attributes})
			response := dimse.NormalizedCreateResponse{Status: StatusOf(err), StatusFields: statusFields(err)}
			if err == nil {
				response.AffectedSOPInstanceUID = received.SOPInstanceUID
			}
			return dimse.NormalizedCreateSCPResult{Response: response}, err
		},
	}
}

func statusFields(err error) *dimse.NormalizedStatusFields {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		return nil
	}
	if statusErr.ErrorComment == "" && len(statusErr.Tags) == 0 {
		return nil
	}
	return &dimse.NormalizedStatusFields{ErrorComment: statusErr.ErrorComment, AttributeIdentifierList: append([]core.Tag(nil), statusErr.Tags...)}
}
