package dimse

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// These opt-in tests check the typed SOP Class Extended Negotiation of #1156
// against pynetdicom, an independent implementation. Enable with:
//
//	DICOMGO_PYNETDICOM_INTEGRATION=1 go test ./net/dimse -run Pynetdicom -count=1 -v

// TestCFindExtendedNegotiationFromPynetdicomSCU: pynetdicom requests
// relational queries, combined date-time matching and timezone adjustment;
// the Go SCP accepts the first two and answers a relational SERIES query
// without the Study Instance UID while checking Identifiers.
func TestCFindExtendedNegotiationFromPynetdicomSCU(t *testing.T) {
	python := requirePynetdicomInterop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	candidate := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1156"}},
		{Header: core.ElementHeader{Tag: tags.SeriesInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1156.1"}},
		{Header: core.ElementHeader{Tag: tags.StudyDate, VR: core.VRDA}, Value: core.StringValue{"20060706"}},
		{Header: core.ElementHeader{Tag: tags.StudyTime, VR: core.VRTM}, Value: core.StringValue{"070000"}},
	}, std.Dictionary)
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "DICOMGO", Context: ctx,
			SupportedAbstractSyntaxes:  []string{StudyRootFindSOPClassUID},
			SupportedTransferSyntaxes:  []string{transfer.ImplicitVRLittleEndian.UID},
			ExtendedNegotiationHandler: ExtendedNegotiationPolicy{Find: &CFindExtendedNegotiation{RelationalQueries: true, CombinedDateTimeMatching: true}}.Handler(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer assoc.Close()
		serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{CFindHandler: MemoryFindIndex{Candidates: []*object.Object{candidate}}, CheckCFindIdentifier: true})
	}()
	host, port, err := splitInteropAddress(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, python, "-c", pynetdicomRelationalFindSCU, host, port).CombinedOutput()
	if err != nil {
		t.Fatalf("pynetdicom C-FIND SCU: %v\n%s", err, output)
	}
	// The answer keeps the request's four bytes: relational and date-time
	// accepted, fuzzy and timezone not.
	for _, want := range []string{"ACCEPTED 01010000", "MATCH 1.2.826.0.1.3680043.10.543.1156.1", "FINAL 0000"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("pynetdicom output lacks %q:\n%s", want, output)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// TestCFindExtendedNegotiationToPynetdicomSCP: the Go SCU requests every
// C-FIND option and reads pynetdicom's one-byte answer (relational only).
func TestCFindExtendedNegotiationToPynetdicomSCP(t *testing.T) {
	python := requirePynetdicomInterop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port, logs := startPynetdicomSCP(t, ctx, python, pynetdicomExtendedNegotiationFindSCP)
	requested := CFindExtendedNegotiation{
		RelationalQueries: true, CombinedDateTimeMatching: true, FuzzySemanticMatching: true, TimezoneQueryAdjustment: true,
		EnhancedMultiFrameImageConversion: true, EmptyValueMatching: true, MultipleValueMatching: true,
	}
	assoc, err := ul.DialContext(ctx, "127.0.0.1:"+port, ul.DialOptions{
		CalledAETitle: "PY_FIND", CallingAETitle: "DICOMGO",
		Contexts:            []ul.PresentationContext{StudyRootFindPresentationContext()},
		ExtendedNegotiation: []ul.SopClassExtendedNegotiationItem{requested.Item(StudyRootFindSOPClassUID)},
	})
	if err != nil {
		t.Fatalf("associate: %v\n%s", err, logs())
	}
	defer assoc.Close()
	if got := AcceptedCFindExtendedNegotiation(assoc, StudyRootFindSOPClassUID); got != (CFindExtendedNegotiation{RelationalQueries: true}) {
		t.Fatalf("accepted %+v, want relational only", got)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	waitForPynetdicomLine(t, logs, "REQUESTED 01010101010101")
}

// startPynetdicomSCP runs a pynetdicom acceptor script that prints
// "PORT <n>" when listening and stops when its stdin closes. logs returns
// everything the script printed so far.
func startPynetdicomSCP(t *testing.T, ctx context.Context, python, script string) (string, func() string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, python, "-u", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pynetdicom SCP: %v", err)
	}
	var lines lockedBuffer
	portLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		sent := false
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = lines.Write([]byte(line + "\n"))
			if !sent && strings.HasPrefix(line, "PORT ") {
				portLine <- strings.TrimSpace(strings.TrimPrefix(line, "PORT "))
				sent = true
			}
		}
		if !sent {
			close(portLine)
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	logs := func() string { return lines.String() + stderr.String() }
	select {
	case port, ok := <-portLine:
		if !ok || port == "" {
			t.Fatalf("pynetdicom SCP exited before listening:\n%s", logs())
		}
		return port, logs
	case <-ctx.Done():
		t.Fatalf("pynetdicom SCP did not start: %v\n%s", ctx.Err(), logs())
	}
	return "", logs
}

func waitForPynetdicomLine(t *testing.T, logs func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pynetdicom output lacks %q:\n%s", want, logs())
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const pynetdicomRelationalFindSCU = `
import sys
from pydicom.dataset import Dataset
from pynetdicom import AE
from pynetdicom.pdu_primitives import SOPClassExtendedNegotiation
from pynetdicom.sop_class import StudyRootQueryRetrieveInformationModelFind as FIND

ae = AE(ae_title="PY_FIND")
ae.add_requested_context(FIND)
item = SOPClassExtendedNegotiation()
item.sop_class_uid = FIND
item.service_class_application_information = bytes([1, 1, 0, 1])
assoc = ae.associate(sys.argv[1], int(sys.argv[2]), ae_title="DICOMGO", ext_neg=[item])
if not assoc.is_established:
    raise RuntimeError("association rejected")
print("ACCEPTED " + assoc.acceptor.sop_class_extended[FIND].hex())
query = Dataset()
query.QueryRetrieveLevel = "SERIES"
query.StudyDate = "20060705-20060707"
query.StudyTime = "1000-1800"
query.SeriesInstanceUID = ""
for status, identifier in assoc.send_c_find(query, FIND):
    if status.Status in (0xFF00, 0xFF01):
        print("MATCH " + identifier.SeriesInstanceUID)
    else:
        print("FINAL %04X" % status.Status)
assoc.release()
`

const pynetdicomExtendedNegotiationFindSCP = `
import sys
from pynetdicom import AE, evt
from pynetdicom.sop_class import StudyRootQueryRetrieveInformationModelFind as FIND

def on_extended(event):
    for uid, data in event.app_info.items():
        print("REQUESTED " + data.hex(), flush=True)
    return {uid: bytes([1]) for uid in event.app_info}

def on_find(event):
    yield 0x0000, None

ae = AE(ae_title="PY_FIND")
ae.add_supported_context(FIND)
server = ae.start_server(("127.0.0.1", 0), block=False,
                         evt_handlers=[(evt.EVT_SOP_EXTENDED, on_extended), (evt.EVT_C_FIND, on_find)])
print("PORT %d" % server.server_address[1], flush=True)
sys.stdin.readline()
server.shutdown()
`
