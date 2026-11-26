package dimse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/telemetry"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestMessagePackingInterop uses real DCMTK and dcm4che executables, entirely
// on loopback with synthetic data. Default tests stay offline. Provision and
// pin tools separately; enable DICOMGO_PACKING_INTEROP=1 and set the two bin
// directories below. Executable/JAR versions and hashes belong in the run log.
func TestMessagePackingInterop(t *testing.T) {
	if os.Getenv("DICOMGO_PACKING_INTEROP") != "1" {
		t.Skip("set DICOMGO_PACKING_INTEROP=1 with pinned DCMTK/dcm4che tools")
	}
	for _, peer := range []struct{ name, env string }{{"DCMTK", "DICOMGO_PACKING_DCMTK_BIN"}, {"dcm4che", "DICOMGO_PACKING_DCM4CHE_BIN"}} {
		dir := os.Getenv(peer.env)
		if dir == "" {
			t.Fatalf("missing %s", peer.env)
		}
		for _, tool := range []string{"findscu", "storescp"} {
			if info, err := os.Stat(filepath.Join(dir, tool)); err != nil || info.Mode()&0111 == 0 {
				t.Fatalf("missing executable %s/%s", peer.env, tool)
			}
		}
		t.Run(peer.name+"/C-FIND", func(t *testing.T) { packingFindOracle(t, peer.name, filepath.Join(dir, "findscu")) })
		for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
			for _, async := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/C-STORE/%s/async-%v", peer.name, syntax.Name, async), func(t *testing.T) { packingStoreOracle(t, peer.name, filepath.Join(dir, "storescp"), syntax, async) })
			}
		}
		if peer.name == "dcm4che" {
			t.Run("dcm4che/MPPS", func(t *testing.T) { packingMPPSOracle(t, filepath.Join(dir, "mppsscp")) })
		}
	}
}

func packingFindOracle(t *testing.T, peer, tool string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	outDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "PACKFIND", Context: ctx, SupportedAbstractSyntaxes: []string{StudyRootFindSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID}, Observability: packingObservability()})
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
		if err != nil {
			done <- err
			return
		}
		before := assoc.TelemetrySnapshot()
		err = ServeStudyRootCFind(ctx, assoc, pc.ID, CFindHandlerFunc(func(_ context.Context, req CFindRequestContext) ([]*object.Object, error) {
			if req.QueryRetrieveLevel != QueryRetrieveLevelStudy {
				return nil, fmt.Errorf("unexpected query level")
			}
			return []*object.Object{object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0052), core.VRCS, "STUDY"), dicomtest.NewUIElement(core.NewTag(0x0020, 0x000D), "2.25.1299001")}, nil)}, nil
		}))
		after := assoc.TelemetrySnapshot()
		if err == nil && (after.PDUsOutbound-before.PDUsOutbound != 2 || after.CommandsOutbound-before.CommandsOutbound != 2) {
			err = fmt.Errorf("find expected one mixed pending plus one final command, PDUs/commands=%d/%d", after.PDUsOutbound-before.PDUsOutbound, after.CommandsOutbound-before.CommandsOutbound)
		}
		if err == nil {
			err = respondToStoreRelease(assoc)
		}
		done <- err
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	args := []string{"-S", "-aec", "PACKFIND", "-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID", "-X", "-od", outDir, "127.0.0.1", port}
	if peer == "dcm4che" {
		args = []string{"-c", "PACKFIND@127.0.0.1:" + port, "--implicit-vr", "-M", "StudyRoot", "-L", "STUDY", "-r", "StudyInstanceUID", "--out-dir", outDir}
	}
	if out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput(); err != nil {
		t.Fatalf("synthetic find oracle error=%v\n%s", err, out)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(outDir, "*.dcm"))
	if err != nil || len(files) != 1 {
		t.Fatalf("find output files=%d error=%v", len(files), err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var match *object.Object
	if len(raw) >= 132 && string(raw[128:132]) == "DICM" {
		file, openErr := object.OpenFile(files[0])
		if openErr != nil {
			t.Fatal(openErr)
		}
		match = file.Dataset
		_ = file.Close()
	} else {
		// Both tools persist the raw identifier in the negotiated syntax.
		match, err = object.ReadDataSet(bytes.NewReader(raw), transfer.ImplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
	}

	if uid, ok := match.GetString(core.NewTag(0x0020, 0x000D)); !ok || uid != "2.25.1299001" {
		t.Fatal("oracle did not interpret the pending identifier")
	}
}

func packingStartOracle(t *testing.T, ctx context.Context, tool string, args []string, port int) {
	t.Helper()
	log, err := os.Create(filepath.Join(t.TempDir(), "synthetic-oracle.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
		if t.Failed() {
			if p, err := os.ReadFile(log.Name()); err == nil {
				t.Logf("synthetic oracle diagnostic:\n%s", p)
			}
		}
	})
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("oracle loopback listener did not become ready")
}

func packingStoreOracle(t *testing.T, peer, tool string, syntax transfer.Syntax, async bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port := freePort(t)
	outDir := t.TempDir()
	args := []string{"-aet", "PACKSTORE", "+xa", "-od", outDir, strconv.Itoa(port)}
	if peer == "dcm4che" {
		args = []string{"-b", "PACKSTORE@127.0.0.1:" + strconv.Itoa(port), "--directory", outDir}
	}
	packingStartOracle(t, ctx, tool, args, port)
	assoc, err := ul.DialContext(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ul.DialOptions{CalledAETitle: "PACKSTORE", CallingAETitle: "PACKSCU", Observability: packingObservability(), Contexts: []ul.PresentationContext{{AbstractSyntaxUID: secondaryCaptureSOPClassUID, TransferSyntaxUIDs: []string{syntax.UID}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	pc, err := AcceptedContextForSOPClass(assoc, secondaryCaptureSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	elements := dicomtest.DatasetWithPixelData()
	for i := range elements {
		if elements[i].Tag() == core.NewTag(0x0008, 0x0016) {
			elements[i] = dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), secondaryCaptureSOPClassUID)
		}
	}
	dataset := object.FromElements(elements, nil)
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, dataset, syntax); err != nil {
		t.Fatal(err)
	}
	before := assoc.TelemetrySnapshot()
	writer := func(_ context.Context, w io.Writer, negotiated transfer.Syntax) error {
		if negotiated.UID != syntax.UID {
			return fmt.Errorf("unexpected negotiated syntax")
		}
		_, err := w.Write(encoded.Bytes())
		return err
	}
	if async {
		session, err := NewAsyncSession(assoc, AsyncSessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		operation, err := session.StartCStoreEncoded(ctx, pc.ID, CStoreRequest{AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID}, writer)
		if err != nil {
			_ = session.Close()
			t.Fatal(err)
		}
		response, err := operation.Wait(ctx)
		if err != nil {
			_ = session.Close()
			t.Fatal(err)
		}
		parsed, err := ParseCStoreResponse(response.Command)
		if err != nil || parsed.Status != StatusSuccess {
			_ = session.Close()
			t.Fatalf("C-STORE response error=%v", err)
		}
		after := assoc.TelemetrySnapshot()
		if after.PDUsOutbound-before.PDUsOutbound != 1 || after.CommandsOutbound-before.CommandsOutbound != 1 {
			t.Fatal("async store did not use exactly one mixed PDU/one command")
		}
		if err := session.Release(ctx, AsyncReleaseWait); err != nil {
			t.Fatal(err)
		}
	} else {
		result, err := NewStoreClient(assoc).StoreEncodedWithOptions(ctx, writer, CStoreOptions{AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID})
		if err != nil || result.Response == nil || result.Response.Status != StatusSuccess {
			t.Fatalf("store response error=%v", err)
		}
		after := assoc.TelemetrySnapshot()
		if after.PDUsOutbound-before.PDUsOutbound != 1 || after.CommandsOutbound-before.CommandsOutbound != 1 {
			t.Fatal("serial store did not use exactly one mixed PDU/one command")
		}
		if err := assoc.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("store files=%d error=%v", len(entries), err)
	}
	file, err := object.OpenFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if file.TransferSyntax.UID != syntax.UID {
		t.Fatal("persisted transfer syntax changed")
	}
	want, _ := dataset.GetRaw(core.TagPixelData)
	got, _ := file.Dataset.GetRaw(core.TagPixelData)
	if !bytes.Equal(want, got) {
		t.Fatal("persisted pixel bytes changed")
	}
	if uid, ok := file.Dataset.GetString(core.NewTag(0x0008, 0x0018)); !ok || uid != dicomtest.TestSOPInstanceUID {
		t.Fatal("persisted identity differs")
	}
}

func packingMPPSOracle(t *testing.T, tool string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port := freePort(t)
	outDir := t.TempDir()
	// This qualifies DIMSE packing, not the MPPS IOD validation profile.
	packingStartOracle(t, ctx, tool, []string{"-b", "PACKMPPS@127.0.0.1:" + strconv.Itoa(port), "--directory", outDir, "--no-validate"}, port)
	const class = "1.2.840.10008.3.1.2.3.3"
	assoc, err := ul.DialContext(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ul.DialOptions{CalledAETitle: "PACKMPPS", CallingAETitle: "PACKSCU", Observability: packingObservability(), Contexts: []ul.PresentationContext{{AbstractSyntaxUID: class, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	client := NewNormalizedClient(assoc)
	before := assoc.TelemetrySnapshot()
	createData := object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0040, 0x0252), core.VRCS, "IN PROGRESS")}, nil)
	created, err := client.Create(ctx, NormalizedCreateRequest{AffectedSOPClassUID: class, AffectedSOPInstanceUID: "2.25.1299002", CommandDataSetType: DataSetPresent}, createData)
	if err != nil || created.Response == nil || created.Response.Status != StatusSuccess {
		t.Fatalf("MPPS create error=%v", err)
	}
	setData := object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0040, 0x0252), core.VRCS, "COMPLETED")}, nil)
	set, err := client.Set(ctx, NormalizedSetRequest{RequestedSOPClassUID: class, RequestedSOPInstanceUID: "2.25.1299002"}, setData)
	if err != nil || set.Response == nil || set.Response.Status != StatusSuccess {
		t.Fatalf("MPPS set error=%v", err)
	}
	after := assoc.TelemetrySnapshot()
	if after.PDUsOutbound-before.PDUsOutbound != 2 || after.CommandsOutbound-before.CommandsOutbound != 2 {
		t.Fatal("N-CREATE/N-SET were not one mixed PDU each")
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("MPPS files=%d error=%v", len(entries), err)
	}
	file, err := object.OpenFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if status, ok := file.Dataset.GetString(core.NewTag(0x0040, 0x0252)); !ok || status != "COMPLETED" {
		t.Fatal("MPPS oracle did not interpret/retain the modification list")
	}
}

func packingObservability() *ul.ObservabilityOptions {
	return &ul.ObservabilityOptions{Sink: telemetry.SinkFunc(func(context.Context, telemetry.Event) {})}
}
