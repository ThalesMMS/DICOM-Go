package dimse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagQueryRetrieveLevel = core.NewTag(0x0008, 0x0052)
	tagPatientID          = core.NewTag(0x0010, 0x0020)
	tagStudyInstanceUID   = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID  = core.NewTag(0x0020, 0x000E)
	tagSOPClassUID        = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID     = core.NewTag(0x0008, 0x0018)
)

// DIMSERetrieveSourceOptions configures NewDIMSERetrieveSource.
type DIMSERetrieveSourceOptions struct {
	// Address is the upstream archive's host:port.
	Address        string
	CalledAETitle  string
	CallingAETitle string
	// DialOptions is the template for upstream associations (timeouts,
	// MaxPDU, TLS, user identity); AE Titles, contexts and roles are set by
	// the source.
	DialOptions ul.DialOptions
	// TransferSyntaxUIDs are proposed for the upstream's C-STORE
	// sub-operations, each in its own context per SOP Class; empty is
	// Explicit then Implicit VR Little Endian, which asks the upstream to
	// decompress. Add compressed syntaxes the downstream peers accept to
	// pass compressed instances through unchanged.
	TransferSyntaxUIDs []string
	// MaxInstances bounds the matches Locate gathers (studies, series and
	// instances alike); zero is 65535.
	MaxInstances int
	// ResponseTimeout bounds each upstream C-FIND response; zero is none.
	ResponseTimeout time.Duration
	// RequireExactInventory rejects C-FIND results that do not identify their
	// level and match the requested ancestors. The instance results must also
	// identify a SOP Class. Enable this when using Locate to choose contexts
	// for one whole-study or whole-series C-GET.
	RequireExactInventory bool
}

// DIMSERetrieveSource is a RetrieveSource backed by another archive over
// DIMSE: Locate walks the hierarchy with C-FIND down to the IMAGE level
// (one query per study and series, as hierarchical matching requires), and
// Fetch retrieves each instance with an IMAGE-level C-GET on one upstream
// association kept for the retrieve. The Query/Retrieve model is the
// downstream request's (Patient or Study Root). SOP Class UID (0008,0016)
// is requested at the IMAGE level; an upstream that does not return it
// (DCMTK's dcmqrscp) leaves the class to the fetched instance, and the
// C-GET association then proposes DefaultCGetStorageSOPClassUIDs.
type DIMSERetrieveSource struct {
	options DIMSERetrieveSourceOptions
}

// NewDIMSERetrieveSource validates options and returns the source.
func NewDIMSERetrieveSource(options DIMSERetrieveSourceOptions) (*DIMSERetrieveSource, error) {
	if strings.TrimSpace(options.Address) == "" {
		return nil, errors.New("dicom dimse: DIMSE retrieve source address is required")
	}
	if options.MaxInstances <= 0 || options.MaxInstances > maxCMoveSubOperations {
		options.MaxInstances = maxCMoveSubOperations
	}
	if len(options.TransferSyntaxUIDs) == 0 {
		options.TransferSyntaxUIDs = []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}
	}
	options.TransferSyntaxUIDs = normalizedTransferSyntaxUIDs(options.TransferSyntaxUIDs)
	return &DIMSERetrieveSource{options: options}, nil
}

type dimseRetrieveModel struct {
	patientRoot bool
	find, get   string
}

func dimseRetrieveModelFor(sopClassUID string) (dimseRetrieveModel, error) {
	switch sopClassUID {
	case PatientRootMoveSOPClassUID, PatientRootGetSOPClassUID, PatientRootFindSOPClassUID:
		return dimseRetrieveModel{patientRoot: true, find: PatientRootFindSOPClassUID, get: PatientRootGetSOPClassUID}, nil
	case StudyRootMoveSOPClassUID, StudyRootGetSOPClassUID, StudyRootFindSOPClassUID:
		return dimseRetrieveModel{find: StudyRootFindSOPClassUID, get: StudyRootGetSOPClassUID}, nil
	default:
		return dimseRetrieveModel{}, fmt.Errorf("dicom dimse: unsupported Query/Retrieve model %q", sopClassUID)
	}
}

func (s *DIMSERetrieveSource) dial(ctx context.Context, contexts []ul.PresentationContext, roles []ul.RoleSelectionItem) (*ul.Association, error) {
	options := s.options.DialOptions
	options.CalledAETitle = s.options.CalledAETitle
	options.CallingAETitle = s.options.CallingAETitle
	options.Contexts = contexts
	options.RoleSelections = roles
	options.ExtendedNegotiation = nil
	options.CommonExtendedNegotiation = nil
	options.AsynchronousOperationsWindow = nil
	assoc, err := ul.DialContext(ctx, strings.TrimSpace(s.options.Address), options)
	if err != nil {
		return nil, fmt.Errorf("dicom dimse: upstream association failed: %w", err)
	}
	return assoc, nil
}

// Locate finds the instances matching the request on the upstream.
func (s *DIMSERetrieveSource) Locate(ctx context.Context, request RetrieveSourceRequest) (RetrieveSet, error) {
	model, err := dimseRetrieveModelFor(request.SOPClassUID)
	if err != nil {
		return nil, err
	}
	if request.Identifier == nil {
		return nil, errors.New("dicom dimse: retrieve identifier is required")
	}
	assoc, err := s.dial(ctx, []ul.PresentationContext{{ID: 1, AbstractSyntaxUID: model.find, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}}}, nil)
	if err != nil {
		return nil, err
	}
	walker := &dimseLocator{source: s, model: model, assoc: assoc, priority: request.Priority}
	instances, err := walker.walk(ctx, strings.ToUpper(strings.TrimSpace(request.QueryRetrieveLevel)), request.Identifier)
	if err != nil {
		_ = assoc.Abort(ul.AbortReasonNotSpecified)
		_ = assoc.Close()
		return nil, err
	}
	releaseParent := context.Background()
	if s.options.RequireExactInventory {
		releaseParent = ctx
	}
	releaseCtx, cancel := context.WithTimeout(releaseParent, releaseTimeoutOrDefault(s.options.DialOptions))
	if releaseErr := assoc.Release(releaseCtx); releaseErr != nil {
		_ = assoc.Abort(ul.AbortReasonNotSpecified)
	}
	cancel()
	_ = assoc.Close()
	located := make([]RetrieveInstance, len(instances))
	patientIDs := make(map[string]string, len(instances))
	for i, instance := range instances {
		located[i] = instance.RetrieveInstance
		patientIDs[instance.SOPInstanceUID] = instance.patientID
	}
	return &dimseRetrieveSet{source: s, model: model, priority: request.Priority, instances: located, patientIDs: patientIDs}, nil
}

type dimseLocatedInstance struct {
	RetrieveInstance
	patientID string
}

type dimseLocator struct {
	source    *DIMSERetrieveSource
	model     dimseRetrieveModel
	assoc     *ul.Association
	priority  uint16
	messageID uint16
	matches   int
}

func identifierStrings(identifier *object.Object, tag core.Tag) []string {
	values, _ := identifier.GetStrings(tag)
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(strings.TrimRight(value, "\x00")); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func singleIdentifierValue(identifier *object.Object, tag core.Tag, name string) (string, error) {
	values := identifierStrings(identifier, tag)
	if len(values) != 1 {
		return "", fmt.Errorf("dicom dimse: retrieve identifier needs one %s", name)
	}
	return values[0], nil
}

// walk descends from the request level to the instances.
func (l *dimseLocator) walk(ctx context.Context, level string, identifier *object.Object) ([]dimseLocatedInstance, error) {
	patientID := ""
	if l.model.patientRoot {
		var err error
		if patientID, err = singleIdentifierValue(identifier, tagPatientID, "Patient ID"); err != nil {
			return nil, err
		}
	}
	switch level {
	case QueryRetrieveLevelPatient:
		if !l.model.patientRoot {
			return nil, errors.New("dicom dimse: PATIENT level needs the Patient Root model")
		}
		studies, err := l.find(ctx, QueryRetrieveLevelStudy, patientID, []core.Element{uiElement(tagStudyInstanceUID)})
		if err != nil {
			return nil, err
		}
		return l.studies(ctx, patientID, uidsOf(studies, tagStudyInstanceUID))
	case QueryRetrieveLevelStudy:
		studies := identifierStrings(identifier, tagStudyInstanceUID)
		if len(studies) == 0 {
			return nil, errors.New("dicom dimse: retrieve identifier needs Study Instance UID")
		}
		return l.studies(ctx, patientID, studies)
	case QueryRetrieveLevelSeries:
		study, err := singleIdentifierValue(identifier, tagStudyInstanceUID, "Study Instance UID")
		if err != nil {
			return nil, err
		}
		series := identifierStrings(identifier, tagSeriesInstanceUID)
		if len(series) == 0 {
			return nil, errors.New("dicom dimse: retrieve identifier needs Series Instance UID")
		}
		return l.series(ctx, patientID, study, series, nil)
	case QueryRetrieveLevelImage:
		study, err := singleIdentifierValue(identifier, tagStudyInstanceUID, "Study Instance UID")
		if err != nil {
			return nil, err
		}
		series, err := singleIdentifierValue(identifier, tagSeriesInstanceUID, "Series Instance UID")
		if err != nil {
			return nil, err
		}
		instances := identifierStrings(identifier, tagSOPInstanceUID)
		if len(instances) == 0 {
			return nil, errors.New("dicom dimse: retrieve identifier needs SOP Instance UID")
		}
		return l.series(ctx, patientID, study, []string{series}, instances)
	default:
		return nil, fmt.Errorf("dicom dimse: unsupported retrieve level %q", level)
	}
}

func (l *dimseLocator) studies(ctx context.Context, patientID string, studies []string) ([]dimseLocatedInstance, error) {
	var out []dimseLocatedInstance
	for _, study := range studies {
		series, err := l.find(ctx, QueryRetrieveLevelSeries, patientID, []core.Element{uiElement(tagStudyInstanceUID, study), uiElement(tagSeriesInstanceUID)})
		if err != nil {
			return nil, err
		}
		instances, err := l.series(ctx, patientID, study, uidsOf(series, tagSeriesInstanceUID), nil)
		if err != nil {
			return nil, err
		}
		out = append(out, instances...)
	}
	return out, nil
}

// series lists the instances of each series; sopInstanceUIDs, when set,
// restricts them by list matching.
func (l *dimseLocator) series(ctx context.Context, patientID, study string, series []string, sopInstanceUIDs []string) ([]dimseLocatedInstance, error) {
	var out []dimseLocatedInstance
	for _, uid := range series {
		results, err := l.find(ctx, QueryRetrieveLevelImage, patientID, []core.Element{
			uiElement(tagStudyInstanceUID, study),
			uiElement(tagSeriesInstanceUID, uid),
			uiElement(tagSOPInstanceUID, sopInstanceUIDs...),
			uiElement(tagSOPClassUID),
		})
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			classUID, _ := result.GetUID(tagSOPClassUID)
			instanceUID, _ := result.GetUID(tagSOPInstanceUID)
			if !core.IsValidUID(instanceUID) || (classUID != "" && !core.IsValidUID(classUID)) {
				return nil, errors.New("dicom dimse: upstream C-FIND response lacks a valid SOP Instance UID")
			}
			out = append(out, dimseLocatedInstance{
				RetrieveInstance: RetrieveInstance{SOPClassUID: classUID, SOPInstanceUID: instanceUID, StudyInstanceUID: study, SeriesInstanceUID: uid},
				patientID:        patientID,
			})
		}
	}
	return out, nil
}

func uidsOf(results []*object.Object, tag core.Tag) []string {
	var out []string
	seen := map[string]bool{}
	for _, result := range results {
		uid, _ := result.GetUID(tag)
		if uid = strings.TrimSpace(uid); uid != "" && !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out
}

func uiElement(tag core.Tag, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue(values)}
}

func csElement(tag core.Tag, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRCS}, Value: core.StringValue{value}}
}

func loElement(tag core.Tag, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRLO}, Value: core.StringValue{value}}
}

// retrieveKeys builds a query or retrieve identifier: the level, the
// Patient ID for the Patient Root model, then keys.
func retrieveKeys(level string, patientRoot bool, patientID string, keys []core.Element) *object.Object {
	elements := []core.Element{csElement(tagQueryRetrieveLevel, level)}
	if patientRoot {
		elements = append(elements, loElement(tagPatientID, patientID))
	}
	elements = append(elements, keys...)
	return object.FromElements(elements, std.Dictionary)
}

// find runs one C-FIND and returns the pending identifiers.
func (l *dimseLocator) find(ctx context.Context, level, patientID string, keys []core.Element) ([]*object.Object, error) {
	client, err := NewFindClientForSOPClass(l.assoc, l.model.find)
	if err != nil {
		return nil, err
	}
	l.messageID++
	findCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, errs := client.FindWithOptions(OperationOptions{Context: findCtx, ResponseTimeout: l.source.options.ResponseTimeout},
		CFindRequest{AffectedSOPClassUID: l.model.find, MessageID: l.messageID, Priority: l.priority},
		retrieveKeys(level, l.model.patientRoot, patientID, keys))
	var out []*object.Object
	for result := range results {
		if result.Response == nil || CategorizeCFindStatus(result.Response.Status) != CFindStatusPending || result.Identifier == nil {
			continue
		}
		if l.source.options.RequireExactInventory && !validRetrieveInventoryMatch(result.Identifier, level, keys) {
			cancel()
			for range results {
			}
			<-errs
			return nil, errors.New("dicom dimse: inconsistent C-FIND retrieve inventory")
		}
		l.matches++
		if l.matches > l.source.options.MaxInstances {
			cancel()
			for range results {
			}
			<-errs
			return nil, fmt.Errorf("dicom dimse: upstream matches exceed %d", l.source.options.MaxInstances)
		}
		out = append(out, result.Identifier)
	}
	if err := <-errs; err != nil {
		return nil, fmt.Errorf("dicom dimse: upstream C-FIND failed: %w", err)
	}
	return out, nil
}

func validRetrieveInventoryMatch(identifier *object.Object, level string, requested []core.Element) bool {
	returnedLevel, ok := readCGetSingleValue(identifier, tagQueryRetrieveLevel, core.VRCS)
	if !ok || returnedLevel != level {
		return false
	}
	for _, key := range requested {
		// All scoped inventory keys are UIDs. Empty request keys ask the
		// peer to provide a single identifying UID at the current level.
		value, err := readCGetUIDList(identifier, key.Tag(), false)
		if err != nil {
			return false
		}
		if values := key.StringValues(); len(values) > 0 && !slices.Contains(values, value[0]) {
			return false
		}
	}
	return true
}

// dimseRetrieveSet fetches located instances with IMAGE-level C-GETs.
type dimseRetrieveSet struct {
	source     *DIMSERetrieveSource
	model      dimseRetrieveModel
	priority   uint16
	instances  []RetrieveInstance
	patientIDs map[string]string

	mu        sync.Mutex
	assoc     *ul.Association
	messageID uint16
	closed    bool
}

func (s *dimseRetrieveSet) Instances() []RetrieveInstance { return s.instances }

// getContexts proposes the GET model and, per located SOP Class, one
// context per transfer syntax with the SCP role.
func (s *dimseRetrieveSet) getContexts() ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	contexts := []ul.PresentationContext{{ID: 1, AbstractSyntaxUID: s.model.get, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}}}
	var classes []string
	seen := map[string]bool{}
	unknown := false
	for _, instance := range s.instances {
		if instance.SOPClassUID == "" {
			unknown = true
		} else if !seen[instance.SOPClassUID] {
			seen[instance.SOPClassUID] = true
			classes = append(classes, instance.SOPClassUID)
		}
	}
	if len(classes) > 127 {
		return nil, nil, fmt.Errorf("dicom dimse: %d SOP Classes exceed one C-GET association", len(classes))
	}
	if unknown {
		for _, class := range DefaultCGetStorageSOPClassUIDs() {
			if len(classes) >= 127 {
				break
			}
			if !seen[class] {
				seen[class] = true
				classes = append(classes, class)
			}
		}
	}
	syntaxes := s.source.options.TransferSyntaxUIDs
	perSyntax := len(classes)*len(syntaxes) <= 127
	id := byte(3)
	roles := make([]ul.RoleSelectionItem, 0, len(classes))
	for _, class := range classes {
		roles = append(roles, ul.RoleSelectionItem{SopClassUID: class, SCPRole: true})
		if !perSyntax {
			contexts = append(contexts, ul.PresentationContext{ID: id, AbstractSyntaxUID: class, TransferSyntaxUIDs: append([]string(nil), syntaxes...)})
			id += 2
			continue
		}
		for _, syntax := range syntaxes {
			contexts = append(contexts, ul.PresentationContext{ID: id, AbstractSyntaxUID: class, TransferSyntaxUIDs: []string{syntax}})
			id += 2
		}
	}
	return contexts, roles, nil
}

// Fetch retrieves one instance with an IMAGE-level C-GET. A transport
// failure drops the association; the next Fetch opens a new one.
func (s *dimseRetrieveSet) Fetch(ctx context.Context, instance RetrieveInstance, _ []string) (*object.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("dicom dimse: retrieve set is closed")
	}
	if s.assoc == nil {
		contexts, roles, err := s.getContexts()
		if err != nil {
			return nil, err
		}
		assoc, err := s.source.dial(ctx, contexts, roles)
		if err != nil {
			return nil, err
		}
		s.assoc = assoc
	}
	pc, err := AcceptedContextForSOPClass(s.assoc, s.model.get)
	if err != nil {
		return nil, err
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		return nil, err
	}
	s.messageID++
	if s.messageID == 0 {
		s.messageID = 1
	}
	var received *object.File
	handler := CGetStoreHandlerFunc(func(_ context.Context, store CGetStoreRequestContext) (uint16, error) {
		if received != nil || store.Request.AffectedSOPInstanceUID != instance.SOPInstanceUID {
			return StatusCStoreCannotUnderstand, errors.New("dicom dimse: unexpected instance from upstream")
		}
		received = &object.File{Dataset: store.DataSet, TransferSyntax: store.DataSetSyntax}
		return StatusSuccess, nil
	})
	identifier := retrieveKeys(QueryRetrieveLevelImage, s.model.patientRoot, s.patientIDs[instance.SOPInstanceUID], []core.Element{
		uiElement(tagStudyInstanceUID, instance.StudyInstanceUID),
		uiElement(tagSeriesInstanceUID, instance.SeriesInstanceUID),
		uiElement(tagSOPInstanceUID, instance.SOPInstanceUID),
	})
	response, err := SendCGet(ctx, s.assoc, pc.ID, CGetRequest{AffectedSOPClassUID: s.model.get, MessageID: s.messageID, Priority: s.priority}, identifier, syntax, handler)
	if err != nil {
		_ = s.assoc.Abort(ul.AbortReasonNotSpecified)
		_ = s.assoc.Close()
		s.assoc = nil
		return nil, fmt.Errorf("dicom dimse: upstream C-GET failed: %w", err)
	}
	if received == nil {
		return nil, fmt.Errorf("dicom dimse: upstream C-GET ended with status 0x%04X without the instance", response.Status)
	}
	return received, nil
}

// Close releases the upstream association.
func (s *dimseRetrieveSet) Close() error {
	s.mu.Lock()
	assoc := s.assoc
	s.assoc = nil
	s.closed = true
	s.mu.Unlock()
	if assoc == nil {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeoutOrDefault(s.source.options.DialOptions))
	defer cancel()
	err := assoc.Release(releaseCtx)
	if err != nil {
		_ = assoc.Abort(ul.AbortReasonNotSpecified)
	}
	_ = assoc.Close()
	return err
}
