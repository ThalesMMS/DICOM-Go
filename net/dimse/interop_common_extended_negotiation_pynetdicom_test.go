package dimse

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// These opt-in tests check SOP Class Common Extended Negotiation (#1158)
// against pynetdicom. Enable with:
//
//	DICOMGO_PYNETDICOM_INTEGRATION=1 go test ./net/dimse -run Pynetdicom -count=1 -v

// TestCommonExtendedNegotiationFromPynetdicomSCU: pynetdicom proposes a
// private SOP Class with CT Image Storage as its Related General SOP Class;
// the Go SCP, which supports only CT Image Storage, accepts the context as
// CT and stores the instance.
func TestCommonExtendedNegotiationFromPynetdicomSCU(t *testing.T) {
	python := requirePynetdicomInterop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stored := make(chan CStoreRequestContext, 1)
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "DICOMGO", Context: ctx,
			SupportedAbstractSyntaxes:        []string{ctImageStorageSOPClassUID},
			SupportedTransferSyntaxes:        []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID},
			CommonExtendedNegotiationHandler: ul.AcceptRelatedGeneralSOPClasses,
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer assoc.Close()
		serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{
			StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
				stored <- req
				return StatusSuccess, nil
			}),
		}})
	}()
	host, port, err := splitInteropAddress(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, python, "-c", pynetdicomCommonExtendedStoreSCU, host, port, privateStorageSOPClassUID, ctImageStorageSOPClassUID).CombinedOutput()
	if err != nil {
		t.Fatalf("pynetdicom C-STORE SCU: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "STORE 0000") {
		t.Fatalf("pynetdicom output:\n%s", output)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	req := <-stored
	if req.Request.AffectedSOPClassUID != privateStorageSOPClassUID || req.PresentationContext.RelatedGeneralSOPClassUID != ctImageStorageSOPClassUID {
		t.Fatalf("stored %+v under %+v", req.Request, req.PresentationContext)
	}
}

// TestCommonExtendedNegotiationToPynetdicomSCP: a StoreSession proposes the
// private SOP Class with its related class and pynetdicom decodes the 57H
// sub-item before storing the instance.
func TestCommonExtendedNegotiationToPynetdicomSCP(t *testing.T) {
	python := requirePynetdicomInterop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port, logs := startPynetdicomSCP(t, ctx, python, strings.ReplaceAll(pynetdicomCommonExtendedStoreSCP, "PRIVATE_UID", privateStorageSOPClassUID))
	session, err := NewStoreSession("127.0.0.1:"+port, StoreSessionOptions{DialOptions: ul.DialOptions{
		CalledAETitle: "PY_STORE", CallingAETitle: "DICOMGO",
		CommonExtendedNegotiation: []ul.SopClassCommonExtendedNegotiationItem{
			RelatedGeneralStorageSOPClasses(privateStorageSOPClassUID, ctImageStorageSOPClassUID, "1.2.840.10008.5.1.4.1.1.7"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close(ctx) }()
	result, err := session.Store(ctx, syntheticStoreSource(privateStorageSOPClassUID, "1.2.826.0.1.3680043.10.543.1158.3"))
	if err != nil || result.Outcome != StoreOutcomeSuccess {
		t.Fatalf("store %+v, %v\n%s", result, err, logs())
	}
	waitForPynetdicomLine(t, logs, "COMMON "+privateStorageSOPClassUID+" "+StorageServiceClassUID+" "+ctImageStorageSOPClassUID+",1.2.840.10008.5.1.4.1.1.7")
}

const pynetdicomCommonExtendedStoreSCU = `
import sys
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.uid import ExplicitVRLittleEndian
from pynetdicom import AE
from pynetdicom.pdu_primitives import SOPClassCommonExtendedNegotiation

private_uid, related_uid = sys.argv[3], sys.argv[4]
ae = AE(ae_title="PY_STORE")
ae.add_requested_context(private_uid, ExplicitVRLittleEndian)
item = SOPClassCommonExtendedNegotiation()
item.sop_class_uid = private_uid
item.service_class_uid = "1.2.840.10008.4.2"
item.related_general_sop_class_identification = [related_uid]
assoc = ae.associate(sys.argv[1], int(sys.argv[2]), ae_title="DICOMGO", ext_neg=[item])
if not assoc.is_established:
    raise RuntimeError("association rejected")
ds = Dataset()
ds.SOPClassUID = private_uid
ds.SOPInstanceUID = "1.2.826.0.1.3680043.10.543.1158.4"
ds.PatientID = "SYNTHETIC-1158"
ds.file_meta = FileMetaDataset()
ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
status = assoc.send_c_store(ds)
print("STORE %04X" % status.Status)
assoc.release()
`

const pynetdicomCommonExtendedStoreSCP = `
import sys
from pydicom.uid import ExplicitVRLittleEndian, ImplicitVRLittleEndian
from pynetdicom import AE, evt

def on_common(event):
    for uid, item in event.items.items():
        related = ",".join(str(u) for u in item.related_general_sop_class_identification)
        print("COMMON %s %s %s" % (item.sop_class_uid, item.service_class_uid, related), flush=True)
    return event.items

def on_store(event):
    return 0x0000

ae = AE(ae_title="PY_STORE")
ae.add_supported_context("PRIVATE_UID", [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
server = ae.start_server(("127.0.0.1", 0), block=False,
                         evt_handlers=[(evt.EVT_SOP_COMMON, on_common), (evt.EVT_C_STORE, on_store)])
print("PORT %d" % server.server_address[1], flush=True)
sys.stdin.readline()
server.shutdown()
`
