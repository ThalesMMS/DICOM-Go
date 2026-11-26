package dimse

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	dcmqrscpPatientID = "DEFLATE1274"
	dcmqrscpStudyUID  = "1.2.826.0.1.3680043.10.1274.10"
)

// TestDeflatedQueryRetrieveIndependentDCMTKPeer is opt-in evidence for #1274:
// DCMTK dcmqrscp preferring Deflated (+xd) selects it for the Study Root
// C-FIND and C-GET contexts, and parses the identifiers dicom-go deflates.
// C-FIND runs through SendCFindRequest and Find; C-GET retrieves the stored
// instance, whose C-STORE sub-operation also arrives deflated. Without the
// gate (DICOMGO_DCMTK_INTEGRATION=1) the test skips.
func TestDeflatedQueryRetrieveIndependentDCMTKPeer(t *testing.T) {
	dcmqrscp := dcmtkoracle.Tool(t, "dcmqrscp")
	dcmqridx := dcmtkoracle.Tool(t, "dcmqridx")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	dir := t.TempDir()
	storage := filepath.Join(dir, "QRSCP")
	if err := os.Mkdir(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	source := deflatedStoreDataSet()
	for _, element := range []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0020), VR: core.VRDA}, Value: core.StringValue{"20261007"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0060), VR: core.VRCS}, Value: core.StringValue{"OT"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{dcmqrscpPatientID}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{dcmqrscpStudyUID}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000E), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1274.11"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0013), VR: core.VRIS}, Value: core.StringValue{"1"}},
	} {
		source.Put(element)
	}
	wantPixels, _ := source.GetRaw(core.TagPixelData)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	instance := filepath.Join(storage, "instance.dcm")
	if err := os.WriteFile(instance, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmqridx, storage, instance)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	config := filepath.Join(dir, "dcmqrscp.cfg")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`NetworkTCPPort = %d
MaxPDUSize = 16384
MaxAssociations = 4

HostTable BEGIN
HostTable END

VendorTable BEGIN
VendorTable END

AETable BEGIN
QRSCP %s RW (10, 64mb) ANY
AETable END
`, port, storage)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-c", config}, dcmtkoracle.QRSCPProcessOptions(ctx, t, dcmqrscp)...)
	server := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, dcmqrscp, append(args, "+xd", strconv.Itoa(port))...)
	var serverLog bytes.Buffer
	server.Stdout, server.Stderr = &serverLog, &serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		if t.Failed() {
			t.Logf("dcmqrscp output:\n%s", serverLog.String())
		}
	})
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var assoc *ul.Association
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		assoc, err = ul.DialContext(ctx, address, ul.DialOptions{
			CallingAETitle: "QRSCU",
			CalledAETitle:  "QRSCP",
			Contexts: []ul.PresentationContext{
				{AbstractSyntaxUID: StudyRootFindSOPClassUID, TransferSyntaxUIDs: []string{transfer.DeflatedExplicitVRLittleEndian.UID, ul.ExplicitVRLittleEndian}},
				{AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{transfer.DeflatedExplicitVRLittleEndian.UID, ul.ExplicitVRLittleEndian}},
				{AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.7", TransferSyntaxUIDs: []string{transfer.DeflatedExplicitVRLittleEndian.UID, ul.ExplicitVRLittleEndian}},
			},
			RoleSelections: []ul.RoleSelectionItem{{SopClassUID: "1.2.840.10008.5.1.4.1.1.7", SCPRole: true}},
		})
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("associate with dcmqrscp: %v", err)
	}
	defer func() { _ = assoc.Release(ctx) }()
	deflatedContext := func(sopClass string) byte {
		t.Helper()
		pc, err := AcceptedContextForSOPClass(assoc, sopClass)
		if err != nil {
			t.Fatal(err)
		}
		if pc.TransferSyntaxUID != transfer.DeflatedExplicitVRLittleEndian.UID {
			t.Fatalf("dcmqrscp accepted %s with %s, want Deflated", sopClass, pc.TransferSyntaxUID)
		}
		return pc.ID
	}
	findPC := deflatedContext(StudyRootFindSOPClassUID)
	getPC := deflatedContext(StudyRootGetSOPClassUID)
	deflatedContext("1.2.840.10008.5.1.4.1.1.7")

	identifier := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0052), VR: core.VRCS}, Value: core.StringValue{QueryRetrieveLevelStudy}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{""}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{dcmqrscpPatientID}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{""}},
	}, std.Dictionary)
	checkMatch := func(label string, match *object.Object) {
		t.Helper()
		if match == nil {
			t.Fatalf("%s: no match", label)
		}
		if got, _ := match.GetString(core.NewTag(0x0020, 0x000D)); got != dcmqrscpStudyUID {
			t.Fatalf("%s: Study Instance UID %q, want %q", label, got, dcmqrscpStudyUID)
		}
		if got, _ := match.GetString(core.NewTag(0x0010, 0x0010)); got != "SYNTHETIC^DEFLATE" {
			t.Fatalf("%s: Patient Name %q", label, got)
		}
	}

	// The low-level request API, which sent the identifier uncompressed
	// before #1274.
	if err := SendCFindRequest(assoc, findPC, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageID: 1}, identifier.Elements()); err != nil {
		t.Fatalf("SendCFindRequest: %v", err)
	}
	var matches []*object.Object
	for {
		rsp, match, err := ReceiveCFindResponse(assoc, findPC, transfer.DeflatedExplicitVRLittleEndian)
		if err != nil {
			t.Fatalf("C-FIND-RSP from dcmqrscp: %v", err)
		}
		if rsp.Status != StatusPending && rsp.Status != 0xFF01 {
			if rsp.Status != StatusSuccess {
				t.Fatalf("C-FIND final status 0x%04X (%s)", rsp.Status, rsp.ErrorComment)
			}
			break
		}
		matches = append(matches, match)
	}
	if len(matches) != 1 {
		t.Fatalf("SendCFindRequest: %d matches, want 1", len(matches))
	}
	checkMatch("SendCFindRequest", matches[0])

	results, errs := Find(ctx, assoc, findPC, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageID: 2}, identifier, transfer.DeflatedExplicitVRLittleEndian)
	matches = matches[:0]
	for result := range results {
		if result.Identifier != nil {
			matches = append(matches, result.Identifier)
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("Find: %d matches, want 1", len(matches))
	}
	checkMatch("Find", matches[0])

	var retrieved []CGetStoreRequestContext
	getIdentifier := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0052), VR: core.VRCS}, Value: core.StringValue{QueryRetrieveLevelStudy}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{dcmqrscpStudyUID}},
	}, std.Dictionary)
	getRsp, err := SendCGet(ctx, assoc, getPC, CGetRequest{AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 3}, getIdentifier, transfer.DeflatedExplicitVRLittleEndian,
		CGetStoreHandlerFunc(func(_ context.Context, req CGetStoreRequestContext) (uint16, error) {
			retrieved = append(retrieved, req)
			return StatusSuccess, nil
		}))
	if err != nil {
		t.Fatalf("SendCGet: %v", err)
	}
	if getRsp.Status != StatusSuccess || len(retrieved) != 1 {
		t.Fatalf("C-GET status 0x%04X with %d sub-operations, want success with 1", getRsp.Status, len(retrieved))
	}
	if got := retrieved[0].DataSetSyntax.UID; got != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("C-GET sub-operation syntax %s, want Deflated", got)
	}
	if pixels, _ := retrieved[0].DataSet.GetRaw(core.TagPixelData); !bytes.Equal(pixels, wantPixels) {
		t.Fatal("C-GET sub-operation samples differ")
	}
}
