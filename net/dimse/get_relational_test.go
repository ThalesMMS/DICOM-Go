package dimse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCGetIdentifierExactKeys(t *testing.T) {
	const (
		study  = "1.2.3.4"
		series = "1.2.3.4.5"
		image  = "1.2.3.4.5.6"
		other  = "1.2.3.7.8.9"
	)
	studyKey := getIdentifierElement(tags.StudyInstanceUID, core.VRUI, study)
	seriesKey := getIdentifierElement(tags.SeriesInstanceUID, core.VRUI, series)
	imageKey := getIdentifierElement(tags.SOPInstanceUID, core.VRUI, image)
	patientKey := getIdentifierElement(tags.PatientID, core.VRLO, "TEST")
	baseline := CRetrieveExtendedNegotiation{}
	relational := CRetrieveExtendedNegotiation{RelationalRetrieval: true}
	for _, tc := range []struct {
		name       string
		model      QueryRetrieveModel
		level      string
		negotiated CRetrieveExtendedNegotiation
		elems      []core.Element
		want       CGetIdentifierKeys
	}{
		{name: "baseline UID list", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: baseline,
			elems: []core.Element{studyKey, seriesKey, getIdentifierElement(tags.SOPInstanceUID, core.VRUI, image, other, image)},
			want:  CGetIdentifierKeys{Level: QueryRetrieveLevelImage, StudyInstanceUIDs: []string{study}, SeriesInstanceUIDs: []string{series}, SOPInstanceUIDs: []string{image, other}}},
		{name: "baseline requires all ancestors", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: baseline,
			elems: []core.Element{seriesKey, imageKey}},
		{name: "relational preserves supplied ancestor", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{studyKey, imageKey}, want: CGetIdentifierKeys{Level: QueryRetrieveLevelImage, StudyInstanceUIDs: []string{study}, SOPInstanceUIDs: []string{image}}},
		{name: "relational ancestor must be exact", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{getIdentifierElement(tags.StudyInstanceUID, core.VRUI, study, other), imageKey}},
		{name: "relational present empty ancestor refused", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{getIdentifierElement(tags.StudyInstanceUID, core.VRUI, ""), imageKey}},
		{name: "relational requires target", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{seriesKey}},
		{name: "empty list member", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{getIdentifierElement(tags.SOPInstanceUID, core.VRUI, image, "")}},
		{name: "wildcard", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{getIdentifierElement(tags.SOPInstanceUID, core.VRUI, image+".*")}},
		{name: "bad UID VR", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelImage, negotiated: relational,
			elems: []core.Element{getIdentifierElement(tags.SOPInstanceUID, core.VRLO, image)}},
		{name: "lower level key", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelSeries, negotiated: relational,
			elems: []core.Element{seriesKey, imageKey}},
		{name: "study root patient key refused", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelStudy, negotiated: relational,
			elems: []core.Element{studyKey, patientKey}},
		{name: "patient root baseline", model: QueryRetrieveModelPatientRoot, level: QueryRetrieveLevelStudy, negotiated: baseline,
			elems: []core.Element{studyKey, patientKey}, want: CGetIdentifierKeys{Level: QueryRetrieveLevelStudy, PatientID: "TEST", StudyInstanceUIDs: []string{study}}},
		{name: "patient root relational study", model: QueryRetrieveModelPatientRoot, level: QueryRetrieveLevelStudy, negotiated: relational,
			elems: []core.Element{studyKey}, want: CGetIdentifierKeys{Level: QueryRetrieveLevelStudy, StudyInstanceUIDs: []string{study}}},
		{name: "view requires its own agreement", model: QueryRetrieveModelStudyRoot, level: QueryRetrieveLevelStudy, negotiated: relational,
			elems: []core.Element{studyKey, getIdentifierElement(getTagQueryRetrieveView, core.VRCS, "ENHANCED")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identifier := object.FromElements(append([]core.Element{
				getIdentifierElement(tags.QueryRetrieveLevel, core.VRCS, tc.level),
			}, tc.elems...), std.Dictionary)
			got, err := ParseCGetIdentifierKeys(tc.model, identifier, tc.negotiated)
			if tc.want.Level == "" {
				if !errors.Is(err, ErrCGetIdentifierKeys) {
					t.Fatalf("invalid keys accepted: %+v, %v", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keys = %+v, %v, want %+v", got, err, tc.want)
			}
		})
	}
}

// The Error Comment of the A900 answer to a refused C-GET Identifier names
// the reason without the Go error prefix and fits the 64 characters of LO.
func TestCGetIdentifierErrorComment(t *testing.T) {
	studyKey := getIdentifierElement(tags.StudyInstanceUID, core.VRUI, "1.2.3.4")
	for _, tc := range []struct {
		name  string
		level string
		elems []core.Element
		want  string
	}{
		{"missing level", "", []core.Element{studyKey}, "C-GET Identifier: missing or unsupported Query/Retrieve Level"},
		{"series without its study", QueryRetrieveLevelSeries, []core.Element{getIdentifierElement(tags.SeriesInstanceUID, core.VRUI, "1.2.3.4.5")},
			"C-GET Identifier: missing STUDY Unique Key (0020,000D)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elems := tc.elems
			if tc.level != "" {
				elems = append([]core.Element{getIdentifierElement(tags.QueryRetrieveLevel, core.VRCS, tc.level)}, elems...)
			}
			_, err := ParseCGetIdentifierKeys(QueryRetrieveModelStudyRoot, object.FromElements(elems, std.Dictionary), CRetrieveExtendedNegotiation{})
			if got := CGetIdentifierErrorComment(err); got != tc.want {
				t.Fatalf("CGetIdentifierErrorComment(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
	long := errors.New(strings.Repeat("x", 80))
	if got := CGetIdentifierErrorComment(long); len(got) != 64 {
		t.Fatalf("comment of an 80-character error has %d characters, want 64", len(got))
	}
	if got := CGetIdentifierErrorComment(nil); got != "" {
		t.Fatalf("CGetIdentifierErrorComment(nil) = %q, want empty", got)
	}
}

func TestCGetRelationalNegotiationIsScopedToAcceptedSOPClass(t *testing.T) {
	relational := CRetrieveExtendedNegotiation{RelationalRetrieval: true}
	for _, tc := range []struct {
		name        string
		support     bool
		proposeFor  string
		wantEnabled bool
	}{
		{name: "unoffered", support: true},
		{name: "offered but refused", proposeFor: StudyRootGetSOPClassUID},
		{name: "only C-MOVE accepted", support: true, proposeFor: StudyRootMoveSOPClassUID},
		{name: "agreed for C-GET", support: true, proposeFor: StudyRootGetSOPClassUID, wantEnabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer closeOrFail(t, "listener", listener)
			role := ul.RoleSelectionItem{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}
			serverDone := make(chan error, 1)
			handled := make(chan CGetRequestContext, 2)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "GETSCP", Context: ctx, RoleSelections: []ul.RoleSelectionItem{role},
					SupportedAbstractSyntaxes:  []string{StudyRootGetSOPClassUID, StudyRootMoveSOPClassUID, cGetTestStorageSOPClassUID},
					SupportedTransferSyntaxes:  []string{ul.ImplicitVRLittleEndian},
					ExtendedNegotiationHandler: ExtendedNegotiationPolicy{Retrieve: &CRetrieveExtendedNegotiation{RelationalRetrieval: tc.support}}.Handler(),
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer closeOrFail(t, "server association", assoc)
				serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{
					CheckCGetIdentifier: true,
					CGetHandler: CGetHandlerFunc(func(_ context.Context, req CGetRequestContext) ([]CGetSubOperation, error) {
						handled <- req
						return []CGetSubOperation{cGetSubOperation("1.2.3.4.5")}, nil
					}),
				})
			}()
			dial := ul.DialOptions{
				CalledAETitle: "GETSCP", CallingAETitle: "GETSCU", RoleSelections: []ul.RoleSelectionItem{role},
				Contexts: []ul.PresentationContext{
					StudyRootGetPresentationContext(), StudyRootMovePresentationContext(),
					{AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
				},
			}
			if tc.proposeFor != "" {
				dial.ExtendedNegotiation = []ul.SopClassExtendedNegotiationItem{relational.Item(tc.proposeFor)}
			}
			assoc, err := ul.DialContext(ctx, listener.Addr().String(), dial)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOrFail(t, "client association", assoc)
			if negotiated := AcceptedCRetrieveExtendedNegotiation(assoc, StudyRootGetSOPClassUID); negotiated.RelationalRetrieval != tc.wantEnabled {
				t.Fatalf("negotiated = %+v, want relational %t", negotiated, tc.wantEnabled)
			}
			if tc.proposeFor == StudyRootMoveSOPClassUID && !AcceptedCRetrieveExtendedNegotiation(assoc, StudyRootMoveSOPClassUID).RelationalRetrieval {
				t.Fatal("test must negotiate C-MOVE independently")
			}
			pc, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			received := 0
			for _, completePath := range []bool{false, true} {
				elems := []core.Element{
					getIdentifierElement(tags.QueryRetrieveLevel, core.VRCS, QueryRetrieveLevelImage),
					getIdentifierElement(tags.SOPInstanceUID, core.VRUI, "1.2.3.4.5"),
				}
				id := uint16(1)
				if completePath {
					id++
					elems = append(elems, getIdentifierElement(tags.StudyInstanceUID, core.VRUI, "1.2.3"),
						getIdentifierElement(tags.SeriesInstanceUID, core.VRUI, "1.2.3.4"))
				}
				wantStatus := uint16(StatusSuccess)
				wantCompleted := uint16(1)
				if !completePath && !tc.wantEnabled {
					wantStatus = StatusCGetIdentifierDoesNotMatchSOPClass
					wantCompleted = 0
				}
				before := received
				rsp, err := SendCGet(ctx, assoc, pc.ID, CGetRequest{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: id},
					object.FromElements(elems, std.Dictionary), transfer.ImplicitVRLittleEndian,
					CGetStoreHandlerFunc(func(_ context.Context, req CGetStoreRequestContext) (uint16, error) {
						received++
						return StatusSuccess, nil
					}))
				if err != nil {
					t.Fatal(err)
				}
				assertCGetCounts(t, "final", rsp, wantStatus, 0, wantCompleted, 0, 0)
				if received-before != int(wantCompleted) || len(handled) != int(wantCompleted) {
					t.Fatalf("store count %d, handler count %d for status %04x", received-before, len(handled), wantStatus)
				}
				if wantCompleted != 0 {
					req := <-handled
					if req.ExtendedNegotiation.RelationalRetrieval != tc.wantEnabled || req.QueryRetrieveLevel != QueryRetrieveLevelImage {
						t.Fatalf("handler got %+v", req)
					}
				}
			}
			if err := assoc.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func getIdentifierElement(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}
