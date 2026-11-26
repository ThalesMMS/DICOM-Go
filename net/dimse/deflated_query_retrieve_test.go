package dimse

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var tagFailedSOPInstanceUIDList = core.NewTag(0x0008, 0x0058)

// oddDeflatedObject returns the first build(n) whose deflate bitstream has an
// odd length, so PS3.5 A.5 requires the trailing NULL pad. Builders vary
// repeated string lengths as well as numbers: changing only decimal digits
// can keep the compressed parity constant on supported older Go versions.
func oddDeflatedObject(t *testing.T, build func(n int) *object.Object) *object.Object {
	t.Helper()
	for n := 0; n < 256; n++ {
		candidate := build(n)
		var stream bytes.Buffer
		if err := object.WriteDataSet(&stream, candidate, transfer.DeflatedExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		source := bytes.NewReader(stream.Bytes())
		inflater := flate.NewReader(source)
		_, readErr := io.Copy(io.Discard, inflater)
		closeErr := inflater.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("inflate fixture: read=%v close=%v", readErr, closeErr)
		}
		if source.Len() == 1 {
			return candidate
		}
	}
	t.Fatal("no candidate produced an odd deflate bitstream")
	return nil
}

func failedSOPInstancesIdentifier(n int) *object.Object {
	return object.FromElements([]core.Element{{
		Header: core.ElementHeader{Tag: tagFailedSOPInstanceUIDList, VR: core.VRUI},
		Value:  core.StringValue{fmt.Sprintf("1.2.826.0.1.3680043.10.1274.%d%s", n+1, strings.Repeat("7", n%24)), "1.2.826.0.1.3680043.10.1274.99"},
	}}, std.Dictionary)
}

// checkDeflatedOnTheWire asserts that one recorded data set was sent as a raw
// deflate bitstream padded to even length, in even PDVs, and returns the
// data set it inflates to.
func checkDeflatedOnTheWire(t *testing.T, label string, recorded recordedDataSet) *object.Object {
	t.Helper()
	for i, n := range recorded.lengths {
		if n%2 != 0 {
			t.Fatalf("%s: data set PDV %d has odd length %d (all: %v)", label, i, n, recorded.lengths)
		}
	}
	source := bytes.NewReader(recorded.encoded)
	inflater := flate.NewReader(source)
	inflated, readErr := io.ReadAll(inflater)
	closeErr := inflater.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("%s: inflate data set: read=%v close=%v", label, readErr, closeErr)
	}
	if rest := recorded.encoded[len(recorded.encoded)-source.Len():]; !bytes.Equal(rest, []byte{0}) {
		t.Fatalf("%s: bytes after the deflate bitstream = %x, want the PS3.5 A.5 NULL pad", label, rest)
	}
	dataset, err := object.ReadDataSet(bytes.NewReader(inflated), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("%s: inflated data set: %v", label, err)
	}
	return dataset
}

// #1274: C-FIND, C-MOVE and C-GET identifiers and their response identifiers
// are deflated on a Deflated Explicit VR Little Endian presentation context,
// padded to even length (PS3.5 A.5) and sent in even PDVs (PS3.8 E.1); the
// receivers on both sides read them back.
func TestQueryRetrieveIdentifiersAreDeflatedOnDeflatedContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	deflated := transfer.DeflatedExplicitVRLittleEndian
	findIdentifier := oddDeflatedObject(t, func(n int) *object.Object {
		return studyRootFindMatch(fmt.Sprintf("FIND%d%s", n, strings.Repeat("X", n%40)), "")
	})
	findMatch := oddDeflatedObject(t, func(n int) *object.Object {
		return studyRootFindMatch(fmt.Sprintf("MATCH%d%s", n, strings.Repeat("X", n%40)), "1.2.826.0.1.3680043.10.1274.1")
	})
	moveIdentifier := oddDeflatedObject(t, func(n int) *object.Object {
		return studyRootFindMatch(fmt.Sprintf("MOVE%d%s", n, strings.Repeat("X", n%40)), "1.2.826.0.1.3680043.10.1274.2")
	})
	getIdentifier := oddDeflatedObject(t, func(n int) *object.Object {
		return studyRootFindMatch(fmt.Sprintf("GET%d%s", n, strings.Repeat("X", n%40)), "1.2.826.0.1.3680043.10.1274.3")
	})
	moveFailures := oddDeflatedObject(t, failedSOPInstancesIdentifier)
	getFailures := oddDeflatedObject(t, func(n int) *object.Object { return failedSOPInstancesIdentifier(n + 1000) })

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "listener", listener)
	type received struct {
		find, move, get *object.Object
		err             error
	}
	serverDone := make(chan received, 1)
	go func() {
		var got received
		defer func() { serverDone <- got }()
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "QRSCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: []string{StudyRootFindSOPClassUID, StudyRootMoveSOPClassUID, StudyRootGetSOPClassUID, cGetTestStorageSOPClassUID},
			SupportedTransferSyntaxes: []string{deflated.UID},
			// C-GET needs the SCU to hold the storage SCP role, even though
			// this retrieval runs no sub-operation.
			RoleSelections: []ul.RoleSelectionItem{{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}},
		})
		if err != nil {
			got.err = err
			return
		}
		defer func() { _ = assoc.Close() }()
		pcFor := func(sopClass string) byte {
			pc, err := AcceptedContextForSOPClass(assoc, sopClass)
			if err != nil && got.err == nil {
				got.err = err
			}
			return pc.ID
		}
		findPC, movePC, getPC := pcFor(StudyRootFindSOPClassUID), pcFor(StudyRootMoveSOPClassUID), pcFor(StudyRootGetSOPClassUID)
		if got.err != nil {
			return
		}
		step := func(name string, err error) bool {
			if err != nil {
				got.err = fmt.Errorf("SCP %s: %w", name, err)
			}
			return err == nil
		}
		findReq, identifier, err := ReceiveCFindRequest(assoc, findPC, deflated)
		if !step("receive C-FIND-RQ", err) {
			return
		}
		got.find = identifier
		if !step("send pending C-FIND-RSP", SendCFindResponse(assoc, findPC, CFindResponse{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageIDBeingRespondedTo: findReq.MessageID, Status: StatusPending}, findMatch, deflated)) ||
			!step("send final C-FIND-RSP", SendCFindResponse(assoc, findPC, CFindResponse{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageIDBeingRespondedTo: findReq.MessageID, Status: StatusSuccess}, nil, deflated)) {
			return
		}
		moveReq, identifier, err := ReceiveCMoveRequestIdentifier(assoc, movePC, deflated)
		if !step("receive C-MOVE-RQ", err) {
			return
		}
		got.move = identifier
		if !step("send C-MOVE-RSP", SendCMoveResponse(assoc, movePC, CMoveResponse{AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageIDBeingRespondedTo: moveReq.MessageID, Status: StatusCMoveSubOperationsCompleteOneOrMoreFailures, Identifier: moveFailures})) {
			return
		}
		getReq, identifier, err := ReceiveCGetRequest(assoc, getPC, deflated)
		if !step("receive C-GET-RQ", err) {
			return
		}
		got.get = identifier
		step("send C-GET-RSP", SendCGetResponse(assoc, getPC, CGetResponse{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageIDBeingRespondedTo: getReq.MessageID, Status: StatusCGetSubOperationsCompleteOneOrMoreFailures, Identifier: getFailures}))
	}()

	relay, recorder := startPDVRecorder(t, listener.Addr().String())
	assoc, err := ul.DialContext(ctx, relay, ul.DialOptions{
		CalledAETitle:  "QRSCP",
		CallingAETitle: "QRSCU",
		Contexts: []ul.PresentationContext{
			{AbstractSyntaxUID: StudyRootFindSOPClassUID, TransferSyntaxUIDs: []string{deflated.UID}},
			{AbstractSyntaxUID: StudyRootMoveSOPClassUID, TransferSyntaxUIDs: []string{deflated.UID}},
			{AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{deflated.UID}},
			{AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUIDs: []string{deflated.UID}},
		},
		RoleSelections: []ul.RoleSelectionItem{{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}},
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer func() { _ = assoc.Close() }()
	pcFor := func(sopClass string) byte {
		t.Helper()
		pc, err := AcceptedContextForSOPClass(assoc, sopClass)
		if err != nil {
			t.Fatal(err)
		}
		if pc.TransferSyntaxUID != deflated.UID {
			t.Fatalf("%s accepted with %s, want Deflated", sopClass, pc.TransferSyntaxUID)
		}
		return pc.ID
	}
	findPC, movePC, getPC := pcFor(StudyRootFindSOPClassUID), pcFor(StudyRootMoveSOPClassUID), pcFor(StudyRootGetSOPClassUID)

	// C-FIND through the low-level request API that wrote the identifier
	// uncompressed before #1274.
	if err := SendCFindRequest(assoc, findPC, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageID: 1}, findIdentifier.Elements()); err != nil {
		t.Fatalf("SendCFindRequest() error = %v", err)
	}
	pending, match, err := ReceiveCFindResponse(assoc, findPC, deflated)
	if err != nil || pending.Status != StatusPending {
		t.Fatalf("pending C-FIND-RSP = %+v, %v", pending, err)
	}
	if got, _ := match.GetString(core.NewTag(0x0010, 0x0020)); got != mustString(t, findMatch, 0x0010, 0x0020) {
		t.Fatalf("C-FIND match Patient ID %q", got)
	}
	final, _, err := ReceiveCFindResponse(assoc, findPC, deflated)
	if err != nil || final.Status != StatusSuccess {
		t.Fatalf("final C-FIND-RSP = %+v, %v", final, err)
	}

	moveRsp, err := SendCMove(ctx, assoc, movePC, CMoveRequest{AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 2, MoveDestination: "DEST"}, moveIdentifier, deflated)
	if err != nil {
		t.Fatalf("SendCMove() error = %v", err)
	}
	if moveRsp.Status != StatusCMoveSubOperationsCompleteOneOrMoreFailures || moveRsp.Identifier == nil {
		t.Fatalf("C-MOVE-RSP = %+v", moveRsp)
	}
	if got, want := mustStrings(t, moveRsp.Identifier), mustStrings(t, moveFailures); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("C-MOVE-RSP Failed SOP Instance UID List = %v, want %v", got, want)
	}

	getRsp, err := SendCGet(ctx, assoc, getPC, CGetRequest{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 3}, getIdentifier, deflated,
		CGetStoreHandlerFunc(func(context.Context, CGetStoreRequestContext) (uint16, error) {
			t.Error("unexpected C-GET sub-operation")
			return StatusSuccess, nil
		}))
	if err != nil {
		t.Fatalf("SendCGet() error = %v", err)
	}
	if getRsp.Status != StatusCGetSubOperationsCompleteOneOrMoreFailures || getRsp.Identifier == nil {
		t.Fatalf("C-GET-RSP = %+v", getRsp)
	}
	if got, want := mustStrings(t, getRsp.Identifier), mustStrings(t, getFailures); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("C-GET-RSP Failed SOP Instance UID List = %v, want %v", got, want)
	}

	var server received
	select {
	case server = <-serverDone:
	case <-ctx.Done():
		t.Fatal("SCP did not finish")
	}
	if server.err != nil {
		t.Fatal(server.err)
	}
	for _, tc := range []struct {
		label     string
		got, want *object.Object
	}{
		{"C-FIND-RQ identifier", server.find, findIdentifier},
		{"C-MOVE-RQ identifier", server.move, moveIdentifier},
		{"C-GET-RQ identifier", server.get, getIdentifier},
	} {
		if tc.got == nil {
			t.Fatalf("%s not received", tc.label)
		}
		if got, want := mustString(t, tc.got, 0x0010, 0x0020), mustString(t, tc.want, 0x0010, 0x0020); got != want {
			t.Fatalf("%s Patient ID = %q, want %q", tc.label, got, want)
		}
	}

	requests := recorder.dataSets(true)
	responses := recorder.dataSets(false)
	if len(requests) != 3 || len(responses) != 3 {
		t.Fatalf("recorded %d request and %d response data sets, want 3 and 3", len(requests), len(responses))
	}
	for i, tc := range []struct {
		label    string
		recorded recordedDataSet
		want     *object.Object
		tag      core.Tag
	}{
		{"C-FIND-RQ identifier", requests[0], findIdentifier, core.NewTag(0x0010, 0x0020)},
		{"C-MOVE-RQ identifier", requests[1], moveIdentifier, core.NewTag(0x0010, 0x0020)},
		{"C-GET-RQ identifier", requests[2], getIdentifier, core.NewTag(0x0010, 0x0020)},
		{"C-FIND-RSP identifier", responses[0], findMatch, core.NewTag(0x0010, 0x0020)},
		{"C-MOVE-RSP identifier", responses[1], moveFailures, tagFailedSOPInstanceUIDList},
		{"C-GET-RSP identifier", responses[2], getFailures, tagFailedSOPInstanceUIDList},
	} {
		onWire := checkDeflatedOnTheWire(t, tc.label, tc.recorded)
		got, _ := onWire.GetStrings(tc.tag)
		want, _ := tc.want.GetStrings(tc.tag)
		if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("data set %d (%s) %s = %v, want %v", i, tc.label, tc.tag, got, want)
		}
	}
}

func mustString(t *testing.T, dataset *object.Object, group, element uint16) string {
	t.Helper()
	value, ok := dataset.GetString(core.NewTag(group, element))
	if !ok {
		t.Fatalf("missing (%04X,%04X)", group, element)
	}
	return value
}

func mustStrings(t *testing.T, dataset *object.Object) []string {
	t.Helper()
	values, ok := dataset.GetStrings(tagFailedSOPInstanceUIDList)
	if !ok {
		t.Fatal("missing Failed SOP Instance UID List")
	}
	return values
}
