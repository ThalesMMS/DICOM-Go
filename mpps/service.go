package mpps

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DIMSEStatusInvalidObjectInstance answers an N-CREATE whose Affected SOP
// Instance UID is not a valid UID (PS3.7 10.1.5.1.6).
const DIMSEStatusInvalidObjectInstance uint16 = 0x0117

// Limits bound one step. Zero fields select the defaults.
type Limits struct {
	// MaxDataSetBytes bounds a received Attribute or Modification List
	// (default 1 MiB).
	MaxDataSetBytes int64
	// MaxPerformedSeries bounds the Performed Series Sequence (default 256).
	MaxPerformedSeries int
	// MaxReferences bounds image plus non-image references over all series
	// (default 10000).
	MaxReferences int
}

// ServiceOptions configure a Service.
type ServiceOptions struct {
	Limits Limits
	Clock  func() time.Time
	// UIDGenerator assigns the SOP Instance UID when N-CREATE omits it;
	// nil uses a random 2.25 UID.
	UIDGenerator func() (string, error)
	// AcceptIdenticalCreateReplay answers an N-CREATE that repeats an
	// existing, unchanged step byte for byte with success instead of 0111H.
	// It lets an SCU retry an N-CREATE whose outcome it could not observe.
	AcceptIdenticalCreateReplay bool
}

// Service is the MPPS SCP state machine over a Repository. It is safe for
// concurrent use; lost updates are prevented by the Repository's
// compare-and-swap, and a failed operation publishes nothing.
type Service struct {
	repository Repository
	limits     Limits
	clock      func() time.Time
	uids       func() (string, error)
	replay     bool
}

// NewService returns an SCP over repository.
func NewService(repository Repository, options ServiceOptions) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidDataSet
	}
	limits := options.Limits
	if limits.MaxDataSetBytes < 0 || limits.MaxPerformedSeries < 0 || limits.MaxReferences < 0 {
		return nil, ErrResourceLimit
	}
	if limits.MaxDataSetBytes == 0 {
		limits.MaxDataSetBytes = 1 << 20
	}
	if limits.MaxPerformedSeries == 0 {
		limits.MaxPerformedSeries = 256
	}
	if limits.MaxReferences == 0 {
		limits.MaxReferences = 10000
	}
	s := &Service{repository: repository, limits: limits, clock: options.Clock, uids: options.UIDGenerator, replay: options.AcceptIdenticalCreateReplay}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.uids == nil {
		s.uids = randomUID
	}
	return s, nil
}

func randomUID() (string, error) {
	return core.NewUID()
}

// Create stores a new step in state IN PROGRESS. An empty sopInstanceUID is
// assigned by the SCP.
func (s *Service) Create(ctx context.Context, sopInstanceUID string, attributes *object.Object) (Instance, error) {
	uid := strings.TrimSpace(sopInstanceUID)
	if uid == "" {
		generated, err := s.uids()
		if err != nil {
			return Instance{}, err
		}
		uid = generated
	}
	if !core.IsValidUID(uid) {
		return Instance{}, &StatusError{Status: DIMSEStatusInvalidObjectInstance, Err: ErrInvalidDataSet}
	}
	if err := ValidateCreate(attributes); err != nil {
		return Instance{}, err
	}
	if err := s.checkLimits(attributes); err != nil {
		return Instance{}, err
	}
	digest, err := dataSetDigest(attributes)
	if err != nil {
		return Instance{}, err
	}
	now := s.clock()
	instance := Instance{SOPInstanceUID: uid, Status: StatusInProgress, Attributes: attributes.ToDataSet(), CreatedAt: now, UpdatedAt: now, CreateDigest: digest}
	created, err := s.repository.Create(ctx, instance)
	if err != nil {
		return Instance{}, err
	}
	if created {
		return s.repository.Get(ctx, uid)
	}
	existing, err := s.repository.Get(ctx, uid)
	if err != nil {
		return Instance{}, err
	}
	if s.replay && existing.Version == 1 && existing.CreateDigest == digest {
		return existing, nil
	}
	return Instance{}, &StatusError{Status: DIMSEStatusDuplicateSOPInstance, Err: ErrDuplicate}
}

// Set merges an N-SET Modification List into an IN PROGRESS step. A step
// already COMPLETED or DISCONTINUED is refused with 0110H and Error ID A710
// (Table F.7.2-2). A transition to a final state requires the Final State
// attributes. Sequences replace the stored ones whole.
func (s *Service) Set(ctx context.Context, sopInstanceUID string, modifications *object.Object) (Instance, error) {
	if err := validateModificationShape(modifications); err != nil {
		return Instance{}, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		current, err := s.repository.Get(ctx, sopInstanceUID)
		if err != nil {
			return Instance{}, err
		}
		if current.Status != StatusInProgress {
			return Instance{}, &StatusError{Status: DIMSEStatusProcessingFailure, ErrorComment: ErrorCommentNoLongerUpdatable, ErrorID: ErrorIDNoLongerUpdatable, Err: ErrNotUpdatable}
		}
		merged, err := mergeModifications(current.Attributes, modifications)
		if err != nil {
			return Instance{}, err
		}
		mergedObject := object.FromDataSet(merged, std.Dictionary)
		next := current
		next.Attributes = merged
		next.UpdatedAt = s.clock()
		if status, err := modifications.LookupString(TagPerformedProcedureStepStatus); err == nil {
			next.Status = strings.TrimSpace(status)
		}
		if next.Status == StatusCompleted || next.Status == StatusDiscontinued {
			if err := ValidateFinal(mergedObject); err != nil {
				return Instance{}, err
			}
		}
		if err := s.checkLimits(mergedObject); err != nil {
			return Instance{}, err
		}
		updated, err := s.repository.CompareAndSwap(ctx, sopInstanceUID, current.Version, next)
		if errors.Is(err, ErrConcurrentUpdate) {
			// Another N-SET won; re-read and re-validate after a short,
			// jittered pause, within a bounded number of attempts.
			if err := sleepContext(ctx, time.Duration(attempt+1)*time.Duration(1+jitter(500))*time.Microsecond); err != nil {
				return Instance{}, err
			}
			continue
		}
		return updated, err
	}
	return Instance{}, &StatusError{Status: DIMSEStatusProcessingFailure, Err: ErrConcurrentUpdate}
}

// Get returns a stored step.
func (s *Service) Get(ctx context.Context, sopInstanceUID string) (Instance, error) {
	return s.repository.Get(ctx, sopInstanceUID)
}

func (s *Service) checkLimits(ds *object.Object) error {
	series, _ := ds.GetSequence(TagPerformedSeriesSequence)
	if len(series) > s.limits.MaxPerformedSeries {
		return &StatusError{Status: DIMSEStatusResourceLimitation, Tags: []core.Tag{TagPerformedSeriesSequence}, Err: ErrResourceLimit}
	}
	references := 0
	for _, item := range series {
		images, _ := item.GetSequence(TagReferencedImageSequence)
		others, _ := item.GetSequence(TagReferencedNonImageSequence)
		references += len(images) + len(others)
	}
	if references > s.limits.MaxReferences {
		return &StatusError{Status: DIMSEStatusResourceLimitation, Tags: []core.Tag{TagPerformedSeriesSequence}, Err: ErrResourceLimit}
	}
	return nil
}

// mergeModifications replaces or inserts each modified attribute. The
// Specific Character Set of an N-SET must match the stored one: the SCP does
// not convert text between character sets (PS3.4 F.7.2.2.3 allows 0106H).
func mergeModifications(stored core.DataSet, modifications *object.Object) (core.DataSet, error) {
	merged := cloneDataSet(stored)
	for _, element := range modifications.Elements() {
		if element.Tag() == TagSpecificCharacterSet {
			if charsetOf(stored) != strings.TrimSpace(element.StringValue()) {
				return core.DataSet{}, &StatusError{Status: DIMSEStatusInvalidAttributeValue, Tags: []core.Tag{TagSpecificCharacterSet}, Err: ErrInvalidDataSet}
			}
			continue
		}
		replaced := false
		for i := range merged.Elements {
			if merged.Elements[i].Tag() == element.Tag() {
				merged.Elements[i] = element
				replaced = true
				break
			}
		}
		if !replaced {
			merged.Elements = append(merged.Elements, element)
		}
	}
	sort.Slice(merged.Elements, func(i, j int) bool { return lessTag(merged.Elements[i].Tag(), merged.Elements[j].Tag()) })
	return merged, nil
}

func charsetOf(ds core.DataSet) string {
	for _, element := range ds.Elements {
		if element.Tag() == TagSpecificCharacterSet {
			return strings.TrimSpace(element.StringValue())
		}
	}
	return ""
}

func dataSetDigest(ds *object.Object) ([sha256.Size]byte, error) {
	var buf bytes.Buffer
	if err := object.WriteDataSet(&buf, ds, transfer.ExplicitVRLittleEndian); err != nil {
		return [sha256.Size]byte{}, &StatusError{Status: DIMSEStatusInvalidAttributeValue, Err: ErrInvalidDataSet}
	}
	return sha256.Sum256(buf.Bytes()), nil
}

// NormalizedOptions returns the N-CREATE and N-SET handlers for one
// association. Only the MPPS SOP Class is accepted; other normalized
// operations are not provided, so the runtime refuses them.
func (s *Service) NormalizedOptions(_ *ul.Association) dimse.NormalizedSCPOptions {
	return dimse.NormalizedSCPOptions{
		MaxDataSetBytes: s.limits.MaxDataSetBytes,
		PresentationContextPolicy: func(pc ul.AcceptedContext, commandSOPClassUID string) error {
			if pc.AbstractSyntaxUID != SOPClassUID || commandSOPClassUID != SOPClassUID {
				return &dimse.NormalizedPresentationContextError{PresentationContextID: pc.ID, AbstractSyntaxUID: pc.AbstractSyntaxUID, CommandSOPClassUID: commandSOPClassUID}
			}
			return nil
		},
		CreateHandler: func(ctx context.Context, request dimse.NormalizedCreateRequest, attributes *object.Object) (dimse.NormalizedCreateSCPResult, error) {
			instance, err := s.Create(ctx, request.AffectedSOPInstanceUID, attributes)
			response := dimse.NormalizedCreateResponse{Status: StatusOf(err), StatusFields: statusFields(err)}
			if err == nil {
				response.AffectedSOPInstanceUID = instance.SOPInstanceUID
			}
			return dimse.NormalizedCreateSCPResult{Response: response}, err
		},
		SetHandler: func(ctx context.Context, request dimse.NormalizedSetRequest, modifications *object.Object) (dimse.NormalizedSetSCPResult, error) {
			_, err := s.Set(ctx, request.RequestedSOPInstanceUID, modifications)
			return dimse.NormalizedSetSCPResult{Response: dimse.NormalizedSetResponse{Status: StatusOf(err), StatusFields: statusFields(err)}}, err
		},
	}
}

func statusFields(err error) *dimse.NormalizedStatusFields {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		return nil
	}
	fields := &dimse.NormalizedStatusFields{ErrorComment: statusErr.ErrorComment, AttributeIdentifierList: append([]core.Tag(nil), statusErr.Tags...)}
	if statusErr.ErrorID != 0 {
		id := statusErr.ErrorID
		fields.ErrorIDOrNil = &id
	}
	if fields.ErrorComment == "" && fields.ErrorIDOrNil == nil && len(fields.AttributeIdentifierList) == 0 {
		return nil
	}
	return fields
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitter(max int64) int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return 0
	}
	return n.Int64()
}
