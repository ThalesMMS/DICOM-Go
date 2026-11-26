package ian_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

type ianPeer struct {
	input  io.WriteCloser
	output *bufio.Scanner
	stderr *strings.Builder
}

// startIANPeer runs testdata/ian_peer.py. Opt-in with
// DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (#903 peer profile:
// pydicom 3.0.2, pynetdicom 3.0.4).
func startIANPeer(t *testing.T, ctx context.Context, mode string) ianPeer {
	t.Helper()
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON; uses the #903 pinned peer profile")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	command := exec.CommandContext(ctx, python, "-u", "testdata/ian_peer.py", mode)
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
	return ianPeer{input: input, output: bufio.NewScanner(output), stderr: stderr}
}

func (p ianPeer) line(t *testing.T) string {
	t.Helper()
	if !p.output.Scan() {
		t.Fatalf("peer ended: %s", p.stderr.String())
	}
	return p.output.Text()
}

// pynetdicom acts as the IAN SCU against the dicom-go SCP, including the
// refusals of a duplicate, an invalid and an incomplete notification.
func TestPynetdicomSCUAgainstServiceSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startIANPeer(t, ctx, "scu")
	rec := &recorder{}
	service, err := ian.NewService(rec.handle, ian.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	address, stop := serve(t, ctx, service)
	defer stop()
	if _, err := fmt.Fprintln(peer.input, address[strings.LastIndex(address, ":")+1:]); err != nil {
		t.Fatal(err)
	}
	if got := peer.line(t); got != "OK" {
		t.Fatalf("peer: %q\n%s", got, peer.stderr.String())
	}
	if rec.count() != 1 {
		t.Fatalf("delivered %d", rec.count())
	}
	got := rec.received[0]
	if got.SOPInstanceUID != "1.2.826.0.1.3680043.10.1168.5.8.1" || got.CallingAETitle != "PEERIAN" || got.Notification.Count() != 4 ||
		len(got.Notification.Series) != 2 || got.Notification.Series[1].Instances[1].RetrieveAETitle != "PEERARCH" ||
		len(got.Notification.PerformedProcedureSteps) != 1 {
		t.Fatalf("received %+v", got)
	}
}

// The dicom-go client notifies a pynetdicom SCP, which records what it
// decoded.
func TestClientAgainstPynetdicomSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startIANPeer(t, ctx, "scp")
	fields := strings.Fields(peer.line(t))
	if len(fields) != 2 || fields[0] != "PORT" {
		t.Fatal("invalid peer readiness")
	}
	assoc, err := ul.DialContext(ctx, "127.0.0.1:"+fields[1], ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PEERIANSCP", Contexts: []ul.PresentationContext{ian.PresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	client := ian.NewClient(assoc)
	n := notification(2, 3)
	n.Series[1].Instances[2].Availability = ian.AvailabilityNearline
	if result, err := client.NotifyNotification(ctx, "1.2.826.0.1.3680043.10.1168.6.1", n); err != nil || result.Status != 0 {
		t.Fatalf("notify %+v %v", result, err)
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
	if len(log) != 1 {
		t.Fatalf("peer saw %v", log)
	}
	entry := log[0]
	if entry["class"] != ian.SOPClassUID || entry["uid"] != "1.2.826.0.1.3680043.10.1168.6.1" || entry["study"] != testStudy ||
		entry["series"] != float64(2) || entry["instances"] != float64(6) ||
		fmt.Sprint(entry["availability"]) != "[NEARLINE ONLINE]" || fmt.Sprint(entry["retrieve"]) != "[ARCHIVE]" || fmt.Sprint(entry["pps"]) != "["+testPPS+"]" {
		t.Fatalf("peer decoded %v", entry)
	}
}
