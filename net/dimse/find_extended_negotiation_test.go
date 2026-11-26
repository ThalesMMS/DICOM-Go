package dimse

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCFindExtendedNegotiationBytesRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   CFindExtendedNegotiation
		want []byte
	}{
		{"none", CFindExtendedNegotiation{}, []byte{0}},
		{"relational", CFindExtendedNegotiation{RelationalQueries: true}, []byte{1}},
		{"fuzzy", CFindExtendedNegotiation{FuzzySemanticMatching: true}, []byte{0, 0, 1}},
		{"first four", CFindExtendedNegotiation{RelationalQueries: true, CombinedDateTimeMatching: true, FuzzySemanticMatching: true, TimezoneQueryAdjustment: true}, []byte{1, 1, 1, 1}},
		{"enhanced multi-frame conversion", CFindExtendedNegotiation{EnhancedMultiFrameImageConversion: true}, []byte{0, 0, 0, 0, 1}},
		{"all seven", CFindExtendedNegotiation{true, true, true, true, true, true, true}, []byte{1, 1, 1, 1, 1, 1, 1}},
		{"multiple value matching", CFindExtendedNegotiation{MultipleValueMatching: true}, []byte{0, 0, 0, 0, 0, 0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Bytes()
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("Bytes = %v, want %v", got, tc.want)
			}
			parsed, err := ParseCFindExtendedNegotiation(got)
			if err != nil || parsed != tc.in {
				t.Fatalf("Parse = %+v, %v; want %+v", parsed, err, tc.in)
			}
		})
	}
	if _, err := ParseCFindExtendedNegotiation(nil); err == nil {
		t.Fatal("empty application information parsed")
	}
	parsed, err := ParseCFindExtendedNegotiation([]byte{2, 1, 1, 0, 1, 0, 2, 1})
	if err != nil || parsed != (CFindExtendedNegotiation{CombinedDateTimeMatching: true, FuzzySemanticMatching: true, EnhancedMultiFrameImageConversion: true}) {
		t.Fatalf("lenient parse = %+v, %v", parsed, err)
	}
}

func TestCFindExtendedNegotiationHandlerAnswersSupportedSubset(t *testing.T) {
	handler := CFindExtendedNegotiationHandler(CFindExtendedNegotiation{FuzzySemanticMatching: true})
	tests := []struct {
		name       string
		item       ul.SopClassExtendedNegotiationItem
		wantAction ul.ExtendedNegotiationAction
		wantData   []byte
	}{
		{"fuzzy accepted, relational refused", ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootFindSOPClassUID, Data: []byte{1, 1, 1, 1}}, ul.ExtendedNegotiationActionAccept, []byte{0, 0, 1, 0}},
		{"keeps request length", ul.SopClassExtendedNegotiationItem{SopClassUID: PatientRootFindSOPClassUID, Data: []byte{1}}, ul.ExtendedNegotiationActionAccept, []byte{0}},
		{"other SOP class ignored", ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootMoveSOPClassUID, Data: []byte{1, 0, 1}}, ul.ExtendedNegotiationActionIgnore, nil},
		{"empty data ignored", ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootFindSOPClassUID}, ul.ExtendedNegotiationActionIgnore, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := handler(tc.item)
			if err != nil {
				t.Fatal(err)
			}
			if result.Action != tc.wantAction || !bytes.Equal(result.Data, tc.wantData) {
				t.Fatalf("result = %+v, want action %d data %v", result, tc.wantAction, tc.wantData)
			}
		})
	}
}

func TestMemoryFindIndexAppliesFuzzyPersonNamesOnlyWhenNegotiated(t *testing.T) {
	candidate := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.QueryRetrieveLevel, VR: core.VRCS}, Value: core.StringValue{QueryRetrieveLevelStudy}},
		{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: core.StringValue{"SMITH^JOHN"}},
		{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.300"}},
	}, std.Dictionary)
	index := MemoryFindIndex{Candidates: []*object.Object{candidate}, FuzzyPersonNames: fuzzy.Soundex{}}
	identifier := mustBuild(t, func() ([]core.Element, error) {
		return BuildStudyRootStudyFindKeys(map[string]string{"PatientName": "SMYTH^JON"}, "StudyInstanceUID")
	})
	tests := []struct {
		name       string
		scpSupport *CFindExtendedNegotiation
		request    *CFindExtendedNegotiation
		wantFuzzy  bool
		wantHits   int
	}{
		{"not requested", &CFindExtendedNegotiation{FuzzySemanticMatching: true}, nil, false, 0},
		{"requested but not accepted", nil, &CFindExtendedNegotiation{FuzzySemanticMatching: true}, false, 0},
		{"refused by policy", &CFindExtendedNegotiation{}, &CFindExtendedNegotiation{FuzzySemanticMatching: true}, false, 0},
		{"negotiated", &CFindExtendedNegotiation{FuzzySemanticMatching: true}, &CFindExtendedNegotiation{FuzzySemanticMatching: true}, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hits, clientView := runNegotiatedStudyRootFind(t, ctx, index, identifier, tc.scpSupport, tc.request)
			if clientView.FuzzySemanticMatching != tc.wantFuzzy {
				t.Fatalf("client negotiated = %+v, want fuzzy %v", clientView, tc.wantFuzzy)
			}
			if len(hits) != tc.wantHits {
				t.Fatalf("hits = %d, want %d", len(hits), tc.wantHits)
			}
		})
	}
}

func runNegotiatedStudyRootFind(t *testing.T, ctx context.Context, index MemoryFindIndex, identifier []core.Element, scpSupport, request *CFindExtendedNegotiation) ([]*object.Object, CFindExtendedNegotiation) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "listener", listener)
	accept := ul.AcceptOptions{
		AETitle:                   "FINDSCP",
		Context:                   ctx,
		SupportedAbstractSyntaxes: []string{StudyRootFindSOPClassUID},
		SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
	}
	if scpSupport != nil {
		accept.ExtendedNegotiationHandler = CFindExtendedNegotiationHandler(*scpSupport)
	}
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(accept)
		if err != nil {
			serverDone <- err
			return
		}
		defer closeOrFail(t, "server association", assoc)
		pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- ServeStudyRootCFind(ctx, assoc, pc.ID, index)
	}()
	dial := ul.DialOptions{
		CallingAETitle: "FINDSCU",
		CalledAETitle:  "FINDSCP",
		Context:        ctx,
		Contexts:       []ul.PresentationContext{StudyRootFindPresentationContext()},
	}
	if request != nil {
		dial.ExtendedNegotiation = []ul.SopClassExtendedNegotiationItem{request.Item(StudyRootFindSOPClassUID)}
	}
	assoc, err := ul.Dial(listener.Addr().String(), dial)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "client association", assoc)
	negotiated := AcceptedCFindExtendedNegotiation(assoc, StudyRootFindSOPClassUID)
	pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	results, errs := Find(ctx, assoc, pc.ID, CFindRequest{
		AffectedSOPClassUID: StudyRootFindSOPClassUID,
		MessageID:           1,
	}, object.FromElements(identifier, std.Dictionary), transfer.ImplicitVRLittleEndian)
	var matches []*object.Object
	for result := range results {
		if CategorizeCFindStatus(result.Status()) == CFindStatusPending && result.Identifier != nil {
			matches = append(matches, result.Identifier)
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("Find SCU error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
	return matches, negotiated
}
