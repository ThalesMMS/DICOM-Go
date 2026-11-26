package mpps_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

type mppsPeer struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Scanner
	stderr  *strings.Builder
}

// startMPPSPeer runs testdata/mpps_peer.py. Opt-in with
// DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (#903 peer profile:
// pydicom 3.0.2, pynetdicom 3.0.4).
func startMPPSPeer(t *testing.T, ctx context.Context, mode string) mppsPeer {
	t.Helper()
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON; uses the #903 pinned peer profile")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	command := exec.CommandContext(ctx, python, "-u", "testdata/mpps_peer.py", mode)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &strings.Builder{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	return mppsPeer{command: command, input: input, output: bufio.NewScanner(output), stderr: stderr}
}

func (p mppsPeer) line(t *testing.T) string {
	t.Helper()
	if !p.output.Scan() {
		t.Fatalf("peer ended: %s", p.stderr.String())
	}
	return p.output.Text()
}

// pynetdicom acts as the MPPS SCU against the dicom-go SCP.
func TestPynetdicomSCUAgainstServiceSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startMPPSPeer(t, ctx, "scu")
	service, _ := newService(t, mpps.ServiceOptions{})
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: 2, SaturationPolicy: ul.SaturationReject,
		Accept: ul.AcceptOptions{AETitle: "RIS", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{mpps.SOPClassUID}, SupportedTransferSyntaxes: mpps.PresentationContext().TransferSyntaxUIDs},
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			options := service.NormalizedOptions(assoc)
			return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	defer func() { stop(); <-done }()
	address := listener.Addr().String()
	if _, err := fmt.Fprintln(peer.input, address[strings.LastIndex(address, ":")+1:]); err != nil {
		t.Fatal(err)
	}
	if got := peer.line(t); got != "OK" {
		t.Fatalf("peer: %q\n%s", got, peer.stderr.String())
	}
	step, err := service.Get(ctx, "1.2.826.0.1.3680043.10.947.5.9.1")
	if err != nil || step.Status != mpps.StatusCompleted {
		t.Fatalf("stored %+v %v", step, err)
	}
	ds := step.DataSet()
	if items, _ := ds.GetSequence(mpps.TagPerformedSeriesSequence); len(items) != 2 {
		t.Fatalf("series %d", len(items))
	}
	if name, _ := ds.LookupString(mpps.TagPatientName); name != "SYNTHETIC^PEER" {
		t.Fatalf("patient %q", name)
	}
	if _, err := service.Get(ctx, "1.2.826.0.1.3680043.10.947.5.9.2"); !errors.Is(err, mpps.ErrNoSuchInstance) {
		t.Fatalf("refused N-CREATE stored: %v", err)
	}
}

// The dicom-go client drives a pynetdicom SCP whose handlers enforce the
// lifecycle and record the commands and datasets they received.
func TestClientAgainstPynetdicomSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startMPPSPeer(t, ctx, "scp")
	fields := strings.Fields(peer.line(t))
	if len(fields) != 2 || fields[0] != "PORT" {
		t.Fatal("invalid peer readiness")
	}
	assoc, err := ul.DialContext(ctx, "127.0.0.1:"+fields[1], ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PEERRIS", Contexts: []ul.PresentationContext{mpps.PresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	client := mpps.NewClient(assoc)
	uid := "1.2.826.0.1.3680043.10.947.6.1"
	if result, err := client.Create(ctx, uid, mustCreate(t)); err != nil || result.Status != 0 {
		t.Fatalf("create %+v %v", result, err)
	}
	progress, _ := mpps.BuildSet(mpps.Update{PerformedSeries: series(1)})
	if result, err := client.Set(ctx, uid, progress); err != nil || result.Status != 0 {
		t.Fatalf("progress %+v %v", result, err)
	}
	final, _ := mpps.Complete("20260925", "123000", series(2))
	if result, err := client.Set(ctx, uid, final); err != nil || result.Status != 0 {
		t.Fatalf("complete %+v %v", result, err)
	}
	result, err := client.Set(ctx, uid, progress)
	if err == nil || result.Status != mpps.DIMSEStatusProcessingFailure || result.StatusFields == nil || result.StatusFields.ErrorIDOrNil == nil || *result.StatusFields.ErrorIDOrNil != mpps.ErrorIDNoLongerUpdatable {
		t.Fatalf("set after COMPLETED: %+v %v", result, err)
	}
	_ = assoc.Release(ctx)
	_ = assoc.Close()
	if _, err := fmt.Fprintln(peer.input, "DONE"); err != nil {
		t.Fatal(err)
	}
	var log []map[string]any
	if err := json.Unmarshal([]byte(peer.line(t)), &log); err != nil {
		t.Fatal(err)
	}
	if len(log) != 4 {
		t.Fatalf("peer saw %d commands: %v", len(log), log)
	}
	for _, entry := range log {
		if entry["class"] != mpps.SOPClassUID || entry["uid"] != uid {
			t.Fatalf("command identity %v", entry)
		}
	}
	create, progressEntry, complete := log[0], log[1], log[2]
	if create["op"] != "N-CREATE" || create["status"] != mpps.StatusInProgress || create["patient"] != "SYNTHETIC^PATIENT" || create["modality"] != "CT" || create["steps"] != float64(1) {
		t.Fatalf("N-CREATE seen as %v", create)
	}
	if progressEntry["op"] != "N-SET" || progressEntry["series"] != float64(1) || progressEntry["status"] != nil {
		t.Fatalf("progress seen as %v", progressEntry)
	}
	tags := fmt.Sprint(complete["tags"])
	if complete["status"] != mpps.StatusCompleted || complete["series"] != float64(2) || complete["images"] != float64(2) ||
		!strings.Contains(tags, "(0040,0250)") || !strings.Contains(tags, "(0040,0251)") {
		t.Fatalf("completion seen as %v", complete)
	}
}
