package dimse

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The sender can make the command and dataset agree with an authentic
// Verification or Query/Retrieve presentation context. That does not turn
// the negotiated abstract syntax into a Storage service.
func TestCStoreRejectsNonStorageContextWithMatchingIdentity(t *testing.T) {
	const privateClass = "1.2.826.0.1.3680043.10.2300.77"
	cases := []struct {
		class    string
		instance string
		want     uint16
	}{
		{VerificationSOPClassUID, "1.2.826.0.1.3680043.10.2300.35", StatusCStoreDataSetDoesNotMatch},
		{StudyRootFindSOPClassUID, "1.2.826.0.1.3680043.10.2300.36", StatusCStoreDataSetDoesNotMatch},
		{privateClass, "1.2.826.0.1.3680043.10.2300.37", StatusSuccess},
	}
	for _, mode := range []string{"decoded", "unverified-stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dir := t.TempDir()
			options := AssociationSCPOptions{}
			if mode == "unverified-stream" {
				options.StreamSink = FileStreamSink{Dir: dir}
			} else {
				options.StoreHandler = CStoreHandlerFunc(func(ctx context.Context, req CStoreRequestContext) (uint16, error) {
					_, err := netstore.SavePart10WithContext(ctx, dir, req.DataSet, req.DataSetSyntax)
					return StatusSuccess, err
				})
			}
			client, done := startDCMTKReferenceSCP(t, ctx, []string{VerificationSOPClassUID, StudyRootFindSOPClassUID, privateClass}, options)
			store := NewStoreClient(client)
			for _, tc := range cases {
				dataSet := storeAdmissionDataSet(t, tc.class, tc.instance, 4096)
				result, err := store.StoreEncodedWithOptions(ctx, func(_ context.Context, dest io.Writer, _ transfer.Syntax) error {
					_, err := dest.Write(dataSet)
					return err
				}, CStoreOptions{AffectedSOPClassUID: tc.class, AffectedSOPInstanceUID: tc.instance})
				if result.Response == nil {
					t.Fatalf("class %s received no C-STORE response: %v", tc.class, err)
				}
				if got := result.Response.Status; got != tc.want {
					t.Errorf("class %s status = 0x%04X, want 0x%04X", tc.class, got, tc.want)
				}
			}
			dirEntries, err := os.ReadDir(dir)
			if err != nil || len(dirEntries) != 1 || dirEntries[0].Name() != cases[2].instance+".dcm" {
				t.Errorf("published entries %v, error %v; want only the private Storage instance", dirEntries, err)
			}
			verify, ok := AcceptedVerificationContext(client)
			if !ok {
				t.Fatal("Verification not negotiated")
			}
			if _, err := SendCEcho(client, verify.ID, 100); err != nil {
				t.Fatalf("valid C-ECHO after C-STORE requests: %v", err)
			}
			if err := client.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("SCP: %v", err)
			}
		})
	}
}

func TestCGetRejectsStoreSuboperationOnQueryContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syntax := transfer.ExplicitVRLittleEndian
	peer, local := testPipeAssociations(t, []ul.AcceptedContext{
		{ID: 1, AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUID: syntax.UID},
		{ID: 3, AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUID: syntax.UID},
	})
	local.AcceptedRoleSelections = []ul.RoleSelectionItem{{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}}

	runRemote := func() error {
		req, _, err := ReceiveCGetRequest(peer, 1, syntax)
		if err != nil {
			return err
		}
		bad := cGetStoredObject()
		if err := bad.SetStrings(tagSOPClassUID, core.VRUI, StudyRootGetSOPClassUID); err != nil {
			return err
		}
		good := cGetStoredObject()
		for _, tc := range []struct {
			contextID byte
			class     string
			dataset   *object.Object
			status    uint16
		}{
			{1, StudyRootGetSOPClassUID, bad, StatusCStoreDataSetDoesNotMatch},
			{3, cGetTestStorageSOPClassUID, good, StatusSuccess},
		} {
			instance, _ := tc.dataset.GetUID(tagSOPInstanceUID)
			if err := SendCStoreRequestWithContext(ctx, peer, tc.contextID, CStoreRequest{
				AffectedSOPClassUID: tc.class, AffectedSOPInstanceUID: instance, MessageID: uint16(tc.contextID) + 10,
			}); err != nil {
				return err
			}
			if err := SendDataSetWithContext(ctx, peer, tc.contextID, tc.dataset, syntax); err != nil {
				return err
			}
			rsp, err := ReceiveCStoreResponseWithContext(ctx, peer, tc.contextID)
			if err != nil {
				return err
			}
			if rsp.Status != tc.status {
				return fmt.Errorf("C-STORE response on context %d = 0x%04X, want 0x%04X", tc.contextID, rsp.Status, tc.status)
			}
		}
		one := uint16(1)
		return SendCGetResponseWithContext(ctx, peer, 1, CGetResponse{
			AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageIDBeingRespondedTo: req.MessageID,
			Status:                              StatusCGetSubOperationsCompleteOneOrMoreFailures,
			NumberOfCompletedSuboperationsOrNil: &one, NumberOfFailedSuboperationsOrNil: &one,
		})
	}
	done := make(chan error, 1)
	go func() {
		defer peer.Close()
		done <- runRemote()
	}()
	var calls atomic.Int32
	handler := CGetStoreHandlerFunc(func(_ context.Context, request CGetStoreRequestContext) (uint16, error) {
		calls.Add(1)
		if request.PresentationContextID != 3 {
			t.Errorf("C-GET call from context %d, want Storage context 3", request.PresentationContextID)
		}
		return StatusSuccess, nil
	})
	rsp, err := SendCGet(ctx, local, 1, CGetRequest{
		AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 1,
	}, studyIdentifier(QueryRetrieveLevelStudy, uiElement(tagStudyInstanceUID, "1.2.3.4")), syntax, handler)
	if peerErr := <-done; peerErr != nil {
		t.Fatal(peerErr)
	}
	if err != nil || rsp == nil || rsp.Status != StatusCGetSubOperationsCompleteOneOrMoreFailures || calls.Load() != 1 {
		t.Fatalf("C-GET returned %+v, error %v, calls %d; want final mixed status and one Storage call", rsp, err, calls.Load())
	}
}

func TestAsyncCStoreRejectsVerificationContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syntax := transfer.ExplicitVRLittleEndian
	peer, local := testPipeAssociations(t, []ul.AcceptedContext{
		{ID: 1, AbstractSyntaxUID: VerificationSOPClassUID, TransferSyntaxUID: syntax.UID},
		{ID: 3, AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUID: syntax.UID},
	})
	session, err := NewAsyncSession(local, AsyncSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var calls atomic.Int32
	session.Handle(CStoreRQ, func(ctx context.Context, session *AsyncSession, message AsyncMessage) error {
		calls.Add(1)
		req, err := ParseCStoreRequest(message.Command)
		if err != nil {
			return err
		}
		return session.Respond(ctx, message, (CStoreResponse{
			AffectedSOPClassUID: req.AffectedSOPClassUID, AffectedSOPInstanceUID: req.AffectedSOPInstanceUID, Status: StatusSuccess,
		}).CommandSet(), nil)
	})
	for _, tc := range []struct {
		contextID byte
		class     string
		status    uint16
	}{
		{1, VerificationSOPClassUID, StatusCStoreDataSetDoesNotMatch},
		{3, cGetTestStorageSOPClassUID, StatusSuccess},
	} {
		dataset := cGetStoredObject()
		if err := dataset.SetStrings(tagSOPClassUID, core.VRUI, tc.class); err != nil {
			t.Fatal(err)
		}
		instance, _ := dataset.GetUID(tagSOPInstanceUID)
		if err := SendCStoreRequestWithContext(ctx, peer, tc.contextID, CStoreRequest{
			AffectedSOPClassUID: tc.class, AffectedSOPInstanceUID: instance, MessageID: uint16(tc.contextID),
		}); err != nil {
			t.Fatal(err)
		}
		if err := SendDataSetWithContext(ctx, peer, tc.contextID, dataset, syntax); err != nil {
			t.Fatal(err)
		}
		rsp, err := ReceiveCStoreResponseWithContext(ctx, peer, tc.contextID)
		if err != nil || rsp.Status != tc.status {
			t.Errorf("C-STORE context %d returned %+v, error %v; want 0x%04X", tc.contextID, rsp, err, tc.status)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("async handler calls = %d, want one valid Storage call", got)
	}
	if err := peer.Release(ctx); err != nil {
		t.Fatalf("async association release: %v", err)
	}
}
