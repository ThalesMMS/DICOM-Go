package dimse

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const ctImageStorageSOPClassUID = "1.2.840.10008.5.1.4.1.1.2"

// TestCRetrieveExtendedNegotiationBytes checks the PS3.4 C.5.2.1/C.5.3.1
// layout: byte 1 relational retrieval, optional byte 2 Enhanced Multi-Frame
// Image Conversion.
func TestCRetrieveExtendedNegotiationBytes(t *testing.T) {
	tests := []struct {
		name string
		in   CRetrieveExtendedNegotiation
		want []byte
	}{
		{"none", CRetrieveExtendedNegotiation{}, []byte{0}},
		{"relational", CRetrieveExtendedNegotiation{RelationalRetrieval: true}, []byte{1}},
		{"conversion", CRetrieveExtendedNegotiation{EnhancedMultiFrameImageConversion: true}, []byte{0, 1}},
		{"both", CRetrieveExtendedNegotiation{RelationalRetrieval: true, EnhancedMultiFrameImageConversion: true}, []byte{1, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Bytes(); !bytes.Equal(got, tc.want) {
				t.Fatalf("Bytes = %v, want %v", got, tc.want)
			}
			parsed, err := ParseCRetrieveExtendedNegotiation(tc.want)
			if err != nil || parsed != tc.in {
				t.Fatalf("Parse = %+v, %v", parsed, err)
			}
		})
	}
	if _, err := ParseCRetrieveExtendedNegotiation(nil); err == nil {
		t.Fatal("empty application information parsed")
	}
	if parsed, _ := ParseCRetrieveExtendedNegotiation([]byte{2, 1, 1}); parsed != (CRetrieveExtendedNegotiation{EnhancedMultiFrameImageConversion: true}) {
		t.Fatalf("lenient parse = %+v", parsed)
	}
}

// TestStorageExtendedNegotiationBytes checks the PS3.4 B.3.1 layout: byte 1
// level of support, byte 3 digital signature, byte 5 element coercion, with
// reserved bytes 2, 4 and 6 sent as 00H and not checked on receipt.
func TestStorageExtendedNegotiationBytes(t *testing.T) {
	scu := StorageSCUExtendedNegotiation()
	if got := scu.Bytes(); !bytes.Equal(got, []byte{3, 0, 0, 0, 2, 0}) {
		t.Fatalf("SCU defaults = %v", got)
	}
	level2 := StorageExtendedNegotiation{LevelOfSupport: StorageLevel2SCP, DigitalSignatureSupport: StorageSignatureLevel3, ElementCoercion: StorageDoesNotCoerce}
	if got := level2.Bytes(); !bytes.Equal(got, []byte{2, 0, 3, 0, 0, 0}) {
		t.Fatalf("level 2 = %v", got)
	}
	tests := []struct {
		name    string
		data    []byte
		want    StorageExtendedNegotiation
		wantErr bool
	}{
		{"full", []byte{2, 0xFF, 3, 0xFF, 0, 0xFF}, level2, false},
		{"one byte keeps defaults", []byte{1}, StorageExtendedNegotiation{LevelOfSupport: StorageLevel1SCP, ElementCoercion: StorageCoercionSCUOnly}, false},
		{"level out of range", []byte{4, 0, 0, 0, 0, 0}, StorageExtendedNegotiation{}, true},
		{"signature out of range", []byte{2, 0, 4, 0, 0, 0}, StorageExtendedNegotiation{}, true},
		{"coercion out of range", []byte{2, 0, 0, 0, 3, 0}, StorageExtendedNegotiation{}, true},
		{"empty", nil, StorageExtendedNegotiation{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStorageExtendedNegotiation(tc.data)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("Parse = %+v, %v; want %+v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestExtendedNegotiationPolicyHandler(t *testing.T) {
	storage := StorageExtendedNegotiation{LevelOfSupport: StorageLevel2SCP, ElementCoercion: StorageDoesNotCoerce}
	policy := ExtendedNegotiationPolicy{
		Find:                &CFindExtendedNegotiation{RelationalQueries: true, CombinedDateTimeMatching: true},
		Retrieve:            &CRetrieveExtendedNegotiation{RelationalRetrieval: true},
		Storage:             &storage,
		StorageSOPClassUIDs: []string{"1.2.826.0.1.3680043.10.543.77"},
	}
	tests := []struct {
		name       string
		policy     ExtendedNegotiationPolicy
		item       ul.SopClassExtendedNegotiationItem
		wantAction ul.ExtendedNegotiationAction
		wantData   []byte
	}{
		{"find subset at request length", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootFindSOPClassUID, Data: []byte{1, 1, 1, 1, 1, 1, 1}}, ul.ExtendedNegotiationActionAccept, []byte{1, 1, 0, 0, 0, 0, 0}},
		{"find single byte", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: PatientRootFindSOPClassUID, Data: []byte{1}}, ul.ExtendedNegotiationActionAccept, []byte{1}},
		{"move relational", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootMoveSOPClassUID, Data: []byte{1, 1}}, ul.ExtendedNegotiationActionAccept, []byte{1, 0}},
		{"get single byte", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: PatientRootGetSOPClassUID, Data: []byte{1}}, ul.ExtendedNegotiationActionAccept, []byte{1}},
		{"storage capability", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: ctImageStorageSOPClassUID, Data: StorageSCUExtendedNegotiation().Bytes()}, ul.ExtendedNegotiationActionAccept, []byte{2, 0, 0, 0, 0, 0}},
		{"private storage class", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: "1.2.826.0.1.3680043.10.543.77", Data: []byte{3}}, ul.ExtendedNegotiationActionAccept, []byte{2, 0, 0, 0, 0, 0}},
		{"malformed storage ignored", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: ctImageStorageSOPClassUID, Data: []byte{9}}, ul.ExtendedNegotiationActionIgnore, nil},
		{"unknown class ignored", policy, ul.SopClassExtendedNegotiationItem{SopClassUID: VerificationSOPClassUID, Data: []byte{1}}, ul.ExtendedNegotiationActionIgnore, nil},
		{"nil find ignored", ExtendedNegotiationPolicy{}, ul.SopClassExtendedNegotiationItem{SopClassUID: StudyRootFindSOPClassUID, Data: []byte{1}}, ul.ExtendedNegotiationActionIgnore, nil},
		{"nil storage ignored", ExtendedNegotiationPolicy{}, ul.SopClassExtendedNegotiationItem{SopClassUID: ctImageStorageSOPClassUID, Data: []byte{3}}, ul.ExtendedNegotiationActionIgnore, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.policy.Handler()(tc.item)
			if err != nil {
				t.Fatal(err)
			}
			if result.Action != tc.wantAction || !bytes.Equal(result.Data, tc.wantData) {
				t.Fatalf("result = %+v, want action %d data %v", result, tc.wantAction, tc.wantData)
			}
		})
	}
}

func TestValidateCFindIdentifier(t *testing.T) {
	str := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
	}
	studyUID := str(tags.StudyInstanceUID, core.VRUI, "1.2.3")
	seriesUID := str(tags.SeriesInstanceUID, core.VRUI, "")
	tests := []struct {
		name       string
		model      QueryRetrieveModel
		level      string
		elements   []core.Element
		relational bool
		wantErr    bool
	}{
		{"study root study with patient keys", QueryRetrieveModelStudyRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.PatientName, core.VRPN, "DOE*"), str(tags.StudyDate, core.VRDA, "20240101")}, false, false},
		{"study root series hierarchical", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{studyUID, seriesUID, str(tags.Modality, core.VRCS, "CT")}, false, false},
		{"missing unique key above", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{seriesUID, str(tags.Modality, core.VRCS, "CT")}, false, true},
		{"missing unique key above, relational", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{seriesUID, str(tags.Modality, core.VRCS, "CT")}, true, false},
		{"non-unique key above", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{studyUID, str(tags.StudyDate, core.VRDA, "20240101")}, false, true},
		{"non-unique key above, relational", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{str(tags.StudyDate, core.VRDA, "20240101"), str(tags.PatientName, core.VRPN, "DOE")}, true, false},
		{"wild card unique key above", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{str(tags.StudyInstanceUID, core.VRUI, "1.2.*")}, false, true},
		{"empty unique key above", QueryRetrieveModelStudyRoot, QueryRetrieveLevelSeries, []core.Element{str(tags.StudyInstanceUID, core.VRUI, "")}, false, true},
		{"key below level", QueryRetrieveModelStudyRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.Modality, core.VRCS, "CT")}, false, true},
		{"key below level, relational", QueryRetrieveModelStudyRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.SOPInstanceUID, core.VRUI, "")}, true, true},
		{"patient root study needs patient ID", QueryRetrieveModelPatientRoot, QueryRetrieveLevelStudy, []core.Element{studyUID}, false, true},
		{"patient root study with patient ID", QueryRetrieveModelPatientRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.PatientID, core.VRLO, "P1"), str(tags.StudyDate, core.VRDA, "")}, false, false},
		{"patient root patient name above study", QueryRetrieveModelPatientRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.PatientID, core.VRLO, "P1"), str(tags.PatientName, core.VRPN, "")}, false, true},
		{"unclassified attribute passes", QueryRetrieveModelStudyRoot, QueryRetrieveLevelStudy, []core.Element{str(tags.RetrieveAETitle, core.VRAE, ""), str(tags.TimezoneOffsetFromUTC, core.VRSH, "+0100")}, false, false},
		{"level outside model", QueryRetrieveModelStudyRoot, QueryRetrieveLevelPatient, nil, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCFindIdentifier(tc.model, tc.level, object.FromElements(tc.elements, std.Dictionary), tc.relational)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrCFindIdentifierKeys) {
				t.Fatalf("err = %v, want ErrCFindIdentifierKeys", err)
			}
		})
	}
}

// TestCFindSCPHonoursNegotiatedQueryOptions runs a Study Root C-FIND over a
// loopback association: the SCP checks the Identifier hierarchically unless
// relational queries were negotiated, and MemoryFindIndex combines Study Date
// and Study Time only when combined date-time matching was.
func TestCFindSCPHonoursNegotiatedQueryOptions(t *testing.T) {
	series := func(seriesUID, date, tm string) *object.Object {
		return object.FromElements([]core.Element{
			{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1156"}},
			{Header: core.ElementHeader{Tag: tags.SeriesInstanceUID, VR: core.VRUI}, Value: core.StringValue{seriesUID}},
			{Header: core.ElementHeader{Tag: tags.StudyDate, VR: core.VRDA}, Value: core.StringValue{date}},
			{Header: core.ElementHeader{Tag: tags.StudyTime, VR: core.VRTM}, Value: core.StringValue{tm}},
			{Header: core.ElementHeader{Tag: tags.Modality, VR: core.VRCS}, Value: core.StringValue{"CT"}},
		}, std.Dictionary)
	}
	index := MemoryFindIndex{Candidates: []*object.Object{
		series("1.2.826.0.1.3680043.10.543.1156.1", "20060705", "230000"),
		series("1.2.826.0.1.3680043.10.543.1156.2", "20060706", "070000"),
	}}
	element := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
	}
	level := element(tags.QueryRetrieveLevel, core.VRCS, QueryRetrieveLevelSeries)
	relationalIdentifier := []core.Element{level,
		element(tags.StudyDate, core.VRDA, "20060705-20060707"),
		element(tags.StudyTime, core.VRTM, "1000-1800"),
		element(tags.SeriesInstanceUID, core.VRUI, ""),
	}
	hierarchicalIdentifier := []core.Element{level,
		element(tags.StudyInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.543.1156"),
		element(tags.SeriesInstanceUID, core.VRUI, ""),
		element(tags.Modality, core.VRCS, "CT"),
	}
	supported := CFindExtendedNegotiation{RelationalQueries: true, CombinedDateTimeMatching: true}
	tests := []struct {
		name       string
		request    *CFindExtendedNegotiation
		identifier []core.Element
		wantStatus uint16
		wantHits   int
	}{
		{"hierarchical query accepted", nil, hierarchicalIdentifier, StatusSuccess, 2},
		{"relational keys refused without negotiation", nil, relationalIdentifier, CFindStatusIdentifierDoesNotMatchSOPClass, 0},
		{"relational without combined date-time", &CFindExtendedNegotiation{RelationalQueries: true}, relationalIdentifier, StatusSuccess, 0},
		{"relational with combined date-time", &CFindExtendedNegotiation{RelationalQueries: true, CombinedDateTimeMatching: true}, relationalIdentifier, StatusSuccess, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			status, hits, negotiated := runCheckedStudyRootFind(t, ctx, index, supported, tc.request, tc.identifier)
			if tc.request != nil && negotiated != *tc.request {
				t.Fatalf("negotiated %+v, want %+v", negotiated, *tc.request)
			}
			if status != tc.wantStatus || hits != tc.wantHits {
				t.Fatalf("status 0x%04X hits %d, want 0x%04X and %d", status, hits, tc.wantStatus, tc.wantHits)
			}
		})
	}
}

func runCheckedStudyRootFind(t *testing.T, ctx context.Context, index MemoryFindIndex, supported CFindExtendedNegotiation, request *CFindExtendedNegotiation, identifier []core.Element) (uint16, int, CFindExtendedNegotiation) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "listener", listener)
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                    "FINDSCP",
			Context:                    ctx,
			SupportedAbstractSyntaxes:  []string{StudyRootFindSOPClassUID},
			SupportedTransferSyntaxes:  []string{ul.ImplicitVRLittleEndian},
			ExtendedNegotiationHandler: ExtendedNegotiationPolicy{Find: &supported}.Handler(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{CFindHandler: index, CheckCFindIdentifier: true})
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
	defer func() { _ = assoc.Close() }()
	negotiated := AcceptedCFindExtendedNegotiation(assoc, StudyRootFindSOPClassUID)
	pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	results, errs := Find(ctx, assoc, pc.ID, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageID: 1},
		object.FromElements(identifier, std.Dictionary), transfer.ImplicitVRLittleEndian)
	hits := 0
	var status uint16
	for result := range results {
		status = result.Status()
		if CategorizeCFindStatus(status) == CFindStatusPending {
			hits++
		}
	}
	if err := <-errs; err != nil && CategorizeCFindStatus(status) != CFindStatusFailure {
		t.Fatalf("Find SCU error = %v (last status 0x%04X)", err, status)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
	return status, hits, negotiated
}
