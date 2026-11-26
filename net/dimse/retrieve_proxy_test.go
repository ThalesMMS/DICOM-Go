package dimse

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Synthetic, non-PHI identifiers.
const (
	proxyCTClass = "1.2.840.10008.5.1.4.1.1.2"
	proxyMRClass = "1.2.840.10008.5.1.4.1.1.4"
	proxyStudy   = "1.2.826.0.1.3680043.10.1171.1"
	proxySeries1 = "1.2.826.0.1.3680043.10.1171.1.1"
	proxySeries2 = "1.2.826.0.1.3680043.10.1171.1.2"
	proxyPatient = "PROXYTEST"
)

type proxyTestInstance struct {
	series, class, uid string
}

// proxyTestArchive is the upstream: an in-memory C-FIND and C-GET SCP.
type proxyTestArchive struct {
	instances []proxyTestInstance
	missing   map[string]bool
	// omitClass leaves SOP Class UID out of C-FIND responses, as DCMTK's
	// dcmqrscp does.
	omitClass bool

	mu           sync.Mutex
	associations int
}

func proxyTestDataSet(instance proxyTestInstance) *object.Object {
	return object.FromElements([]core.Element{
		uiElement(tagSOPClassUID, instance.class),
		uiElement(tagSOPInstanceUID, instance.uid),
		loElement(tagPatientID, proxyPatient),
		uiElement(tagStudyInstanceUID, proxyStudy),
		uiElement(tagSeriesInstanceUID, instance.series),
	}, std.Dictionary)
}

func newProxyTestArchive() *proxyTestArchive {
	return &proxyTestArchive{
		instances: []proxyTestInstance{
			{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.1.1.1"},
			{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.1.1.2"},
			{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.1.1.3"},
			{proxySeries2, proxyMRClass, "1.2.826.0.1.3680043.10.1171.1.2.1"},
			{proxySeries2, proxyMRClass, "1.2.826.0.1.3680043.10.1171.1.2.2"},
		},
		missing: map[string]bool{},
	}
}

func matchesKey(identifier *object.Object, tag core.Tag, value string) bool {
	wanted := identifierStrings(identifier, tag)
	if len(wanted) == 0 {
		return true
	}
	for _, candidate := range wanted {
		if candidate == value {
			return true
		}
	}
	return false
}

func (a *proxyTestArchive) Find(_ context.Context, request CFindRequestContext) ([]*object.Object, error) {
	id := request.Identifier
	if !matchesKey(id, tagPatientID, proxyPatient) || !matchesKey(id, tagStudyInstanceUID, proxyStudy) {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []*object.Object
	for _, instance := range a.instances {
		if !matchesKey(id, tagSeriesInstanceUID, instance.series) || !matchesKey(id, tagSOPInstanceUID, instance.uid) {
			continue
		}
		elements := []core.Element{csElement(tagQueryRetrieveLevel, request.QueryRetrieveLevel), uiElement(tagStudyInstanceUID, proxyStudy)}
		key := proxyStudy
		switch request.QueryRetrieveLevel {
		case QueryRetrieveLevelSeries:
			key = instance.series
			elements = append(elements, uiElement(tagSeriesInstanceUID, instance.series))
		case QueryRetrieveLevelImage:
			key = instance.uid
			elements = append(elements, uiElement(tagSeriesInstanceUID, instance.series), uiElement(tagSOPInstanceUID, instance.uid))
			if !a.omitClass {
				elements = append(elements, uiElement(tagSOPClassUID, instance.class))
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, object.FromElements(elements, std.Dictionary))
	}
	return out, nil
}

func (a *proxyTestArchive) Get(_ context.Context, request CGetRequestContext) ([]CGetSubOperation, error) {
	if request.QueryRetrieveLevel != QueryRetrieveLevelImage {
		return nil, errors.New("proxy test archive serves IMAGE level C-GET only")
	}
	var ops []CGetSubOperation
	for _, instance := range a.instances {
		instance := instance
		if !matchesKey(request.Identifier, tagSOPInstanceUID, instance.uid) {
			continue
		}
		ops = append(ops, CGetSubOperation{
			AffectedSOPClassUID: instance.class, AffectedSOPInstanceUID: instance.uid,
			LoadDataSet: func(context.Context) (*object.Object, error) {
				if a.missing[instance.uid] {
					return nil, errors.New("instance is offline")
				}
				return proxyTestDataSet(instance), nil
			},
		})
	}
	return ops, nil
}

func (a *proxyTestArchive) associationCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.associations
}

var proxyQRClasses = []string{
	StudyRootFindSOPClassUID, StudyRootMoveSOPClassUID, StudyRootGetSOPClassUID,
	PatientRootFindSOPClassUID, PatientRootMoveSOPClassUID, PatientRootGetSOPClassUID,
}

func proxyStorageRoles(classes ...string) []ul.RoleSelectionItem {
	roles := make([]ul.RoleSelectionItem, 0, len(classes))
	for _, class := range classes {
		roles = append(roles, ul.RoleSelectionItem{SopClassUID: class, SCURole: true, SCPRole: true})
	}
	return roles
}

func startProxyTestServer(t *testing.T, ctx context.Context, aeTitle string, abstract, syntaxes []string, roles []ul.RoleSelectionItem, handler ul.AssociationHandler) string {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if len(syntaxes) == 0 {
		syntaxes = []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian}
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: 8,
		Accept: ul.AcceptOptions{
			AETitle: aeTitle, SupportedAbstractSyntaxes: abstract, SupportedTransferSyntaxes: syntaxes,
			PreferProposedTransferSyntaxOrder: true, RoleSelections: roles, NegotiationTimeout: 5 * time.Second,
		},
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			return handler(ctx, assoc)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(serverCtx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
	})
	return listener.Addr().String()
}

func startProxyTestArchive(t *testing.T, ctx context.Context, archive *proxyTestArchive) string {
	t.Helper()
	return startProxyTestServer(t, ctx, "UPSTREAM", append(append([]string(nil), proxyQRClasses...), proxyCTClass, proxyMRClass), nil,
		proxyStorageRoles(proxyCTClass, proxyMRClass), func(ctx context.Context, assoc *ul.Association) error {
			archive.mu.Lock()
			archive.associations++
			archive.mu.Unlock()
			return ServeAssociation(ctx, assoc, AssociationSCPOptions{CFindHandler: archive, CGetHandler: archive})
		})
}

type proxyTestStore struct {
	SOPInstanceUID          string
	CallingAETitle          string
	CalledAETitle           string
	MoveOriginatorAETitle   string
	MoveOriginatorMessageID uint16
	TransferSyntaxUID       string
}

// proxyTestDestination is a Storage SCP answering per-instance statuses.
type proxyTestDestination struct {
	statuses map[string]uint16

	mu     sync.Mutex
	stores []proxyTestStore
}

func (d *proxyTestDestination) received() []proxyTestStore {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]proxyTestStore(nil), d.stores...)
}

func startProxyTestDestination(t *testing.T, ctx context.Context, destination *proxyTestDestination, syntaxes ...string) string {
	t.Helper()
	return startProxyTestServer(t, ctx, "DEST", []string{proxyCTClass, proxyMRClass}, syntaxes, nil, func(ctx context.Context, assoc *ul.Association) error {
		return ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{
			StoreHandler: CStoreHandlerFunc(func(_ context.Context, request CStoreRequestContext) (uint16, error) {
				store := proxyTestStore{
					SOPInstanceUID: request.Request.AffectedSOPInstanceUID,
					CallingAETitle: assoc.CallingAETitle, CalledAETitle: assoc.CalledAETitle,
					MoveOriginatorAETitle: request.Request.MoveOriginatorAETitle,
					TransferSyntaxUID:     request.DataSetSyntax.UID,
				}
				if request.Request.MoveOriginatorMessageIDOrNil != nil {
					store.MoveOriginatorMessageID = *request.Request.MoveOriginatorMessageIDOrNil
				}
				destination.mu.Lock()
				destination.stores = append(destination.stores, store)
				destination.mu.Unlock()
				return destination.statuses[request.Request.AffectedSOPInstanceUID], nil
			}),
		}})
	})
}

func startRetrieveProxy(t *testing.T, ctx context.Context, proxy *RetrieveProxy) string {
	t.Helper()
	return startProxyTestServer(t, ctx, "PROXY", append(append([]string(nil), proxyQRClasses...), proxyCTClass, proxyMRClass), nil,
		proxyStorageRoles(proxyCTClass, proxyMRClass), func(ctx context.Context, assoc *ul.Association) error {
			handler := proxy.Handler(assoc)
			return ServeAssociation(ctx, assoc, AssociationSCPOptions{CMoveHandler: handler, CGetHandler: handler})
		})
}

func newDIMSEProxy(t *testing.T, upstream string, destinations MoveDestinationResolver, maxInstances int) *RetrieveProxy {
	t.Helper()
	source, err := NewDIMSERetrieveSource(DIMSERetrieveSourceOptions{
		Address: upstream, CalledAETitle: "UPSTREAM", CallingAETitle: "PROXY",
		DialOptions: ul.DialOptions{NegotiationTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewRetrieveProxy(RetrieveProxyOptions{
		Source: source, Destinations: destinations, AETitle: "PROXY", MaxInstances: maxInstances,
		DestinationDialOptions: ul.DialOptions{NegotiationTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func proxyMove(t *testing.T, ctx context.Context, address, callingAE, sopClassUID, destination string, identifier *object.Object) []*CMoveResponse {
	t.Helper()
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CalledAETitle: "PROXY", CallingAETitle: callingAE,
		Contexts: []ul.PresentationContext{presentationContextFor(sopClassUID, DefaultMoveTransferSyntaxes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = assoc.Release(ctx)
		_ = assoc.Close()
	}()
	pc, err := AcceptedContextForSOPClass(assoc, sopClassUID)
	if err != nil {
		t.Fatal(err)
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := SendCMoveWithProgress(ctx, assoc, pc.ID, CMoveRequest{
		AffectedSOPClassUID: sopClassUID, MessageID: 7, Priority: PriorityMedium, MoveDestination: destination,
	}, identifier, syntax)
	if err != nil {
		t.Fatal(err)
	}
	var responses []*CMoveResponse
	for event := range progress {
		if event.Err != nil {
			t.Fatalf("C-MOVE: %v", event.Err)
		}
		responses = append(responses, event.Response)
	}
	return responses
}

func studyIdentifier(level string, keys ...core.Element) *object.Object {
	return object.FromElements(append([]core.Element{csElement(tagQueryRetrieveLevel, level)}, keys...), std.Dictionary)
}

func TestRetrieveProxyCMoveForwardsDestinationStatusesAndCounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	archive.missing[archive.instances[3].uid] = true
	destination := &proxyTestDestination{statuses: map[string]uint16{
		archive.instances[1].uid: 0xB007, // Data Set does not match SOP Class (warning)
		archive.instances[2].uid: 0x0107, // Attribute list error (warning)
		archive.instances[4].uid: 0xA700, // Out of resources (failure)
	}}
	upstream := startProxyTestArchive(t, ctx, archive)
	policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination)}}}
	address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 0))

	responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy)))
	if len(responses) != 5 {
		t.Fatalf("responses = %d, want four pending and one final", len(responses))
	}
	assertCMoveCounts(t, "pending 1", responses[0], StatusPending, 4, 1, 0, 0)
	assertCMoveCounts(t, "pending 2", responses[1], StatusPending, 3, 1, 0, 1)
	assertCMoveCounts(t, "pending 3", responses[2], StatusPending, 2, 1, 0, 2)
	assertCMoveCounts(t, "pending 4", responses[3], StatusPending, 1, 1, 1, 2)
	assertCMoveCounts(t, "final", responses[4], StatusCMoveSubOperationsCompleteOneOrMoreFailures, 0, 1, 2, 2)

	stores := destination.received()
	if len(stores) != 4 {
		t.Fatalf("destination stores = %d, want 4 (the offline instance is never sent)", len(stores))
	}
	for i, store := range stores {
		if store.MoveOriginatorAETitle != "VIEWER" || store.MoveOriginatorMessageID != 7 {
			t.Fatalf("store %d Move Originator = %q/%d, want VIEWER/7", i, store.MoveOriginatorAETitle, store.MoveOriginatorMessageID)
		}
		if store.CallingAETitle != "PROXY" || store.CalledAETitle != "DEST" {
			t.Fatalf("store %d association = %s -> %s, want PROXY -> DEST", i, store.CallingAETitle, store.CalledAETitle)
		}
		if store.SOPInstanceUID == archive.instances[3].uid {
			t.Fatal("offline instance reached the destination")
		}
	}
	// One association locates, one fetches every instance.
	if got := archive.associationCount(); got != 2 {
		t.Fatalf("upstream associations = %d, want 2", got)
	}
}

// TestRetrieveProxyLearnsUnreportedSOPClasses: an upstream that does not
// return SOP Class UID in C-FIND still serves C-MOVE and C-GET, the class
// read from each fetched instance.
func TestRetrieveProxyLearnsUnreportedSOPClasses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	archive.omitClass = true
	destination := &proxyTestDestination{}
	upstream := startProxyTestArchive(t, ctx, archive)
	policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination)}}}
	address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 0))
	identifier := studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy))

	responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", identifier)
	assertCMoveCounts(t, "C-MOVE final", responses[len(responses)-1], StatusSuccess, 0, 5, 0, 0)
	if got := len(destination.received()); got != 5 {
		t.Fatalf("destination stores = %d, want 5", got)
	}

	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CalledAETitle: "PROXY", CallingAETitle: "VIEWER",
		Contexts: []ul.PresentationContext{
			{ID: 1, AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUIDs: DefaultGetTransferSyntaxes},
			{ID: 3, AbstractSyntaxUID: proxyCTClass, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}},
		},
		RoleSelections: []ul.RoleSelectionItem{{SopClassUID: proxyCTClass, SCPRole: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = assoc.Release(ctx)
		_ = assoc.Close()
	}()
	var classes []string
	response, err := SendCGet(ctx, assoc, 1, CGetRequest{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 1}, identifier, transfer.ImplicitVRLittleEndian,
		CGetStoreHandlerFunc(func(_ context.Context, store CGetStoreRequestContext) (uint16, error) {
			classes = append(classes, store.Request.AffectedSOPClassUID)
			return StatusSuccess, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(classes, []string{proxyCTClass, proxyCTClass, proxyCTClass}) {
		t.Fatalf("C-GET stored classes %v, want the three CT instances", classes)
	}
	if cMoveCountValue(response.NumberOfCompletedSuboperationsOrNil) != 3 || cMoveCountValue(response.NumberOfFailedSuboperationsOrNil) != 2 {
		t.Fatalf("C-GET final 0x%04X completed %d failed %d, want 3/2 (MR not accepted)", response.Status,
			cMoveCountValue(response.NumberOfCompletedSuboperationsOrNil), cMoveCountValue(response.NumberOfFailedSuboperationsOrNil))
	}
}

func TestRetrieveProxyPatientRootWalksTheHierarchy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	destination := &proxyTestDestination{}
	upstream := startProxyTestArchive(t, ctx, archive)
	policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination)}}}
	address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 0))

	cases := []struct {
		level      string
		identifier *object.Object
		want       int
	}{
		{QueryRetrieveLevelPatient, studyIdentifier(QueryRetrieveLevelPatient, loElement(tagPatientID, proxyPatient)), 5},
		{QueryRetrieveLevelSeries, studyIdentifier(QueryRetrieveLevelSeries, loElement(tagPatientID, proxyPatient), uiElement(tagStudyInstanceUID, proxyStudy), uiElement(tagSeriesInstanceUID, proxySeries2)), 2},
		{QueryRetrieveLevelImage, studyIdentifier(QueryRetrieveLevelImage, loElement(tagPatientID, proxyPatient), uiElement(tagStudyInstanceUID, proxyStudy), uiElement(tagSeriesInstanceUID, proxySeries1), uiElement(tagSOPInstanceUID, archive.instances[0].uid, archive.instances[2].uid)), 2},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			responses := proxyMove(t, ctx, address, "VIEWER", PatientRootMoveSOPClassUID, "DEST", tc.identifier)
			assertCMoveCounts(t, "final", responses[len(responses)-1], StatusSuccess, 0, uint16(tc.want), 0, 0)
		})
	}
}

func TestRetrieveProxyEnforcesMoveDestinationPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	destination := &proxyTestDestination{}
	upstream := startProxyTestArchive(t, ctx, archive)
	destAddress := startProxyTestDestination(t, ctx, destination)
	policy := MoveDestinationPolicy{
		Destinations: map[string]MoveDestinationAddress{"DEST": {Address: destAddress}, "VIEWER": {Address: destAddress}},
		Callers:      map[string][]string{"DEST": {"WORKSTATION"}},
	}
	address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 0))
	identifier := studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy))

	for _, tc := range []struct{ caller, destination string }{
		{"VIEWER", "UNKNOWN"}, // not configured
		{"VIEWER", "DEST"},    // configured, caller not allowed
	} {
		responses := proxyMove(t, ctx, address, tc.caller, StudyRootMoveSOPClassUID, tc.destination, identifier)
		if len(responses) != 1 || responses[0].Status != StatusCMoveMoveDestinationUnknown {
			t.Fatalf("%s -> %s: responses %d, final 0x%04X, want one A801H", tc.caller, tc.destination, len(responses), responses[len(responses)-1].Status)
		}
		if comment := responses[0].ErrorComment; comment != "Move Destination unknown" {
			t.Fatalf("%s -> %s: Error Comment %q reveals the policy", tc.caller, tc.destination, comment)
		}
	}
	if got := archive.associationCount(); got != 0 {
		t.Fatalf("refused moves reached the upstream %d times", got)
	}
	responses := proxyMove(t, ctx, address, "WORKSTATION", StudyRootMoveSOPClassUID, "DEST", identifier)
	assertCMoveCounts(t, "allowed caller", responses[len(responses)-1], StatusSuccess, 0, 5, 0, 0)
	responses = proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "VIEWER", identifier)
	assertCMoveCounts(t, "open destination", responses[len(responses)-1], StatusSuccess, 0, 5, 0, 0)
}

func TestRetrieveProxyRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	upstream := startProxyTestArchive(t, ctx, archive)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	_ = closed.Close()
	destination := &proxyTestDestination{}
	reachable := startProxyTestDestination(t, ctx, destination)
	identifier := studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy))

	t.Run("unreachable destination", func(t *testing.T) {
		policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: unreachable}}}
		address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 0))
		responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", identifier)
		if final := responses[len(responses)-1]; len(responses) != 1 || final.Status != StatusRetrieveOutOfResourcesSubOperations {
			t.Fatalf("responses %d, final 0x%04X, want one A702H", len(responses), final.Status)
		}
	})
	t.Run("too many instances", func(t *testing.T) {
		policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: reachable}}}
		address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, policy, 2))
		responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", identifier)
		if final := responses[len(responses)-1]; final.Status != StatusRetrieveOutOfResourcesSubOperations {
			t.Fatalf("final 0x%04X, want A702H", final.Status)
		}
	})
	t.Run("upstream unreachable", func(t *testing.T) {
		policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: reachable}}}
		address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, unreachable, policy, 0))
		responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", identifier)
		final := responses[len(responses)-1]
		if final.Status != StatusRetrieveOutOfResourcesMatches || final.ErrorComment != "Unable to calculate number of matches" {
			t.Fatalf("final 0x%04X %q, want A701H without upstream details", final.Status, final.ErrorComment)
		}
	})
	if got := len(destination.received()); got != 0 {
		t.Fatalf("refused moves stored %d instances", got)
	}
}

func TestRetrieveProxyCGetDeliversOnTheRequesterAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	archive := newProxyTestArchive()
	archive.missing[archive.instances[1].uid] = true
	upstream := startProxyTestArchive(t, ctx, archive)
	address := startRetrieveProxy(t, ctx, newDIMSEProxy(t, upstream, nil, 0))

	// The requester takes CT only: the MR series fails on its side.
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CalledAETitle: "PROXY", CallingAETitle: "VIEWER",
		Contexts: []ul.PresentationContext{
			{ID: 1, AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUIDs: DefaultGetTransferSyntaxes},
			{ID: 3, AbstractSyntaxUID: proxyCTClass, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
		},
		RoleSelections: []ul.RoleSelectionItem{{SopClassUID: proxyCTClass, SCPRole: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = assoc.Release(ctx)
		_ = assoc.Close()
	}()
	var received []string
	var syntaxes []string
	response, err := SendCGet(ctx, assoc, 1, CGetRequest{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 3},
		studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy)), transfer.ImplicitVRLittleEndian,
		CGetStoreHandlerFunc(func(_ context.Context, store CGetStoreRequestContext) (uint16, error) {
			received = append(received, store.Request.AffectedSOPInstanceUID)
			syntaxes = append(syntaxes, store.DataSetSyntax.UID)
			return StatusSuccess, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{archive.instances[0].uid, archive.instances[2].uid}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	for _, syntax := range syntaxes {
		if syntax != transfer.ImplicitVRLittleEndian.UID {
			t.Fatalf("received in %s, want the accepted Implicit VR Little Endian", syntax)
		}
	}
	if response.Status != StatusCGetSubOperationsCompleteOneOrMoreFailures ||
		cMoveCountValue(response.NumberOfCompletedSuboperationsOrNil) != 2 || cMoveCountValue(response.NumberOfFailedSuboperationsOrNil) != 3 {
		t.Fatalf("final 0x%04X completed %d failed %d, want B000H 2/3", response.Status,
			cMoveCountValue(response.NumberOfCompletedSuboperationsOrNil), cMoveCountValue(response.NumberOfFailedSuboperationsOrNil))
	}
	wantFailed := []string{archive.instances[1].uid, archive.instances[3].uid, archive.instances[4].uid}
	if !reflect.DeepEqual(response.FailedSOPInstanceUIDListOrNil, wantFailed) {
		t.Fatalf("failed list %v, want %v", response.FailedSOPInstanceUIDListOrNil, wantFailed)
	}
	if strings.Contains(response.ErrorComment, "offline") {
		t.Fatalf("Error Comment %q carries the upstream's error text", response.ErrorComment)
	}
}

// TestRetrieveProxyTransferSyntaxes checks that a fetched native data set is
// converted to the syntax the destination accepted and that compressed data
// passes through only as itself.
func TestRetrieveProxyTransferSyntaxes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	native := proxyTestInstance{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.9.1"}
	compressed := proxyTestInstance{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.9.2"}
	source := RetrieveSourceFuncs{
		LocateFunc: func(context.Context, RetrieveSourceRequest) ([]RetrieveInstance, error) {
			return []RetrieveInstance{
				{SOPClassUID: native.class, SOPInstanceUID: native.uid},
				{SOPClassUID: compressed.class, SOPInstanceUID: compressed.uid, TransferSyntaxUID: transfer.JPEGBaseline.UID},
			}, nil
		},
		FetchFunc: func(_ context.Context, instance RetrieveInstance, _ []string) (*object.File, error) {
			if instance.SOPInstanceUID == native.uid {
				return &object.File{Dataset: proxyTestDataSet(native), TransferSyntax: transfer.ImplicitVRLittleEndian}, nil
			}
			return &object.File{Dataset: proxyTestDataSet(compressed), TransferSyntax: transfer.JPEGBaseline}, nil
		},
	}
	for _, tc := range []struct {
		name          string
		accepted      []string
		wantStored    map[string]string
		wantCompleted uint16
	}{
		{"explicit and JPEG accepted", []string{ul.ExplicitVRLittleEndian, transfer.JPEGBaseline.UID},
			map[string]string{native.uid: ul.ExplicitVRLittleEndian, compressed.uid: transfer.JPEGBaseline.UID}, 2},
		{"native only", []string{ul.ExplicitVRLittleEndian},
			map[string]string{native.uid: ul.ExplicitVRLittleEndian}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := &proxyTestDestination{}
			policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination, tc.accepted...)}}}
			proxy, err := NewRetrieveProxy(RetrieveProxyOptions{Source: source, Destinations: policy, DestinationTransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}})
			if err != nil {
				t.Fatal(err)
			}
			address := startRetrieveProxy(t, ctx, proxy)
			responses := proxyMove(t, ctx, address, "VIEWER", StudyRootMoveSOPClassUID, "DEST", studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy)))
			final := responses[len(responses)-1]
			if cMoveCountValue(final.NumberOfCompletedSuboperationsOrNil) != tc.wantCompleted || cMoveCountValue(final.NumberOfFailedSuboperationsOrNil) != 2-tc.wantCompleted {
				t.Fatalf("final 0x%04X completed %d failed %d", final.Status, cMoveCountValue(final.NumberOfCompletedSuboperationsOrNil), cMoveCountValue(final.NumberOfFailedSuboperationsOrNil))
			}
			got := map[string]string{}
			for _, store := range destination.received() {
				got[store.SOPInstanceUID] = store.TransferSyntaxUID
			}
			if !reflect.DeepEqual(got, tc.wantStored) {
				t.Fatalf("stored %v, want %v", got, tc.wantStored)
			}
		})
	}
}

func TestRetrieveProxyRejectsAMismatchedFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wanted := proxyTestInstance{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.8.1"}
	other := proxyTestInstance{proxySeries1, proxyCTClass, "1.2.826.0.1.3680043.10.1171.8.2"}
	source := RetrieveSourceFuncs{
		LocateFunc: func(context.Context, RetrieveSourceRequest) ([]RetrieveInstance, error) {
			return []RetrieveInstance{{SOPClassUID: wanted.class, SOPInstanceUID: wanted.uid}}, nil
		},
		FetchFunc: func(context.Context, RetrieveInstance, []string) (*object.File, error) {
			return &object.File{Dataset: proxyTestDataSet(other), TransferSyntax: transfer.ExplicitVRLittleEndian}, nil
		},
	}
	destination := &proxyTestDestination{}
	policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination)}}}
	proxy, err := NewRetrieveProxy(RetrieveProxyOptions{Source: source, Destinations: policy})
	if err != nil {
		t.Fatal(err)
	}
	responses := proxyMove(t, ctx, startRetrieveProxy(t, ctx, proxy), "VIEWER", StudyRootMoveSOPClassUID, "DEST", studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy)))
	assertCMoveCounts(t, "final", responses[len(responses)-1], StatusCMoveSubOperationsCompleteOneOrMoreFailures, 0, 0, 1, 0)
	if got := len(destination.received()); got != 0 {
		t.Fatalf("a different instance reached the destination (%d stores)", got)
	}
}

// blockingRetrieveSet serves its first instance and blocks fetching the
// next until the operation is canceled.
type blockingRetrieveSet struct {
	instances []RetrieveInstance
	closed    chan struct{}
	once      sync.Once
}

func (s *blockingRetrieveSet) Instances() []RetrieveInstance { return s.instances }

func (s *blockingRetrieveSet) Fetch(ctx context.Context, instance RetrieveInstance, _ []string) (*object.File, error) {
	if instance.SOPInstanceUID != s.instances[0].SOPInstanceUID {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &object.File{Dataset: proxyTestDataSet(proxyTestInstance{proxySeries1, instance.SOPClassUID, instance.SOPInstanceUID}), TransferSyntax: transfer.ExplicitVRLittleEndian}, nil
}

func (s *blockingRetrieveSet) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type blockingRetrieveSource struct{ set *blockingRetrieveSet }

func (s blockingRetrieveSource) Locate(context.Context, RetrieveSourceRequest) (RetrieveSet, error) {
	return s.set, nil
}

func TestRetrieveProxyCMoveHonorsCCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	set := &blockingRetrieveSet{closed: make(chan struct{})}
	for _, suffix := range []string{"1", "2", "3"} {
		set.instances = append(set.instances, RetrieveInstance{SOPClassUID: proxyCTClass, SOPInstanceUID: "1.2.826.0.1.3680043.10.1171.6." + suffix})
	}
	destination := &proxyTestDestination{}
	policy := MoveDestinationPolicy{Destinations: map[string]MoveDestinationAddress{"DEST": {Address: startProxyTestDestination(t, ctx, destination)}}}
	proxy, err := NewRetrieveProxy(RetrieveProxyOptions{Source: blockingRetrieveSource{set}, Destinations: policy})
	if err != nil {
		t.Fatal(err)
	}
	address := startRetrieveProxy(t, ctx, proxy)
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CalledAETitle: "PROXY", CallingAETitle: "VIEWER",
		Contexts: []ul.PresentationContext{{ID: 1, AbstractSyntaxUID: StudyRootMoveSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = assoc.Release(ctx)
		_ = assoc.Close()
	}()
	if err := SendCMoveRequest(assoc, 1, CMoveRequest{AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 9, MoveDestination: "DEST"}); err != nil {
		t.Fatal(err)
	}
	if err := SendDataSet(assoc, 1, studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, proxyStudy)), transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	first, err := ReceiveCMoveResponse(assoc, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertCMoveCounts(t, "pending", first, StatusPending, 2, 1, 0, 0)
	if err := SendCCancelRequest(assoc, 1, CCancelRequest{MessageIDBeingRespondedTo: 9}); err != nil {
		t.Fatal(err)
	}
	final, err := ReceiveCMoveResponse(assoc, 1)
	if err != nil {
		t.Fatal(err)
	}
	if final.NumberOfRemainingSuboperationsOrNil != nil {
		t.Fatalf("cancel Remaining = %v, want absent", *final.NumberOfRemainingSuboperationsOrNil)
	}
	assertCMoveCounts(t, "final", final, StatusCMoveCancel, 0, 1, 0, 0)
	select {
	case <-set.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("retrieve set was not closed after the cancel")
	}
	if got := len(destination.received()); got != 1 {
		t.Fatalf("destination stores = %d, want 1", got)
	}
}

func TestMoveDestinationPolicy(t *testing.T) {
	policy := MoveDestinationPolicy{
		Destinations: map[string]MoveDestinationAddress{"DEST": {Address: "dest:104"}, "WS1": {Address: "ws1:104"}},
		Callers:      map[string][]string{"DEST": {"WS1", "WS2"}},
	}
	callerOnly := policy
	callerOnly.CallerOnly = true
	for _, tc := range []struct {
		name               string
		policy             MoveDestinationPolicy
		caller, destinaton string
		want               error
	}{
		{"unknown", policy, "WS1", "OTHER", ErrMoveDestinationUnknown},
		{"allowed caller", policy, "WS2", "DEST", nil},
		{"refused caller", policy, "WS3", "DEST", ErrMoveDestinationRefused},
		{"open destination", policy, "WS3", "WS1", nil},
		{"caller only to itself", callerOnly, "WS1", "WS1", nil},
		{"caller only elsewhere", callerOnly, "WS1", "DEST", ErrMoveDestinationRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.policy.ResolveMoveDestination(context.Background(), MoveDestinationRequest{CallingAETitle: tc.caller, MoveDestination: tc.destinaton})
			if !errors.Is(err, tc.want) || (err == nil && got.Address == "") {
				t.Fatalf("ResolveMoveDestination = %+v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestStoreTransferSyntaxUIDs(t *testing.T) {
	ile, ele, jpeg := ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian, transfer.JPEGBaseline.UID
	for _, tc := range []struct {
		source  string
		allowed []string
		want    []string
	}{
		{ile, nil, []string{ile, ele}},
		{ile, []string{ele}, []string{ele}},
		{jpeg, []string{ele, jpeg}, []string{jpeg}},
		{jpeg, []string{ele, ile}, []string{}},
	} {
		if got := StoreTransferSyntaxUIDs(tc.source, tc.allowed); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("StoreTransferSyntaxUIDs(%s, %v) = %v, want %v", tc.source, tc.allowed, got, tc.want)
		}
	}
}

func TestDestinationContextsFallBackToOnePerSOPClass(t *testing.T) {
	var instances []RetrieveInstance
	for i := 0; i < 70; i++ {
		instances = append(instances, RetrieveInstance{SOPClassUID: "1.2.826.0.1.3680043.10.1171.7." + string(rune('0'+i/10)) + string(rune('0'+i%10)), SOPInstanceUID: "1.2.3"})
	}
	contexts, err := destinationContexts(instances[:10], []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian})
	if err != nil || len(contexts) != 20 {
		t.Fatalf("10 classes: %d contexts, %v; want 20", len(contexts), err)
	}
	contexts, err = destinationContexts(instances, []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian})
	if err != nil || len(contexts) != 70 || len(contexts[0].TransferSyntaxUIDs) != 2 {
		t.Fatalf("70 classes: %d contexts, %v; want 70 with two syntaxes each", len(contexts), err)
	}
}
