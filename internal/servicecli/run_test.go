package servicecli

import (
	"context"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type readyWriter struct {
	once  sync.Once
	ready chan string
}

func (w *readyWriter) Write(p []byte) (int, error) {
	parts := strings.Fields(string(p))
	if len(parts) > 0 {
		w.once.Do(func() { w.ready <- parts[len(parts)-1] })
	}
	return len(p), nil
}
func startService(t *testing.T, tool string, extra ...string) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ready := &readyWriter{ready: make(chan string, 1)}
	done := make(chan error, 1)
	args := append([]string{"-port", "0", "-duration", "4s", "-timeout", "1s"}, extra...)
	go func() { done <- Execute(ctx, tool, args, ready, io.Discard) }()
	var address string
	select {
	case address = <-ready.ready:
	case err := <-done:
		cancel()
		t.Fatalf("server startup: %v", err)
	case <-ctx.Done():
		cancel()
		t.Fatal("startup timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server end: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server handlers not joined")
		}
	})
	return ctx, address
}
func TestMPPSRealCreateAndRefusal(t *testing.T) {
	ctx, addr := startService(t, "mppsscp")
	a, err := ul.DialContext(ctx, addr, ul.DialOptions{CalledAETitle: "MPPSSCP", CallingAETitle: "MODALITY", Contexts: []ul.PresentationContext{mpps.PresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	client := mpps.NewClient(a)
	attributes, err := mpps.BuildCreate(mpps.CreateAttributes{ScheduledSteps: []mpps.ScheduledStep{{StudyInstanceUID: "1.2.3", AccessionNumber: "SYN", RequestedProcedureID: "RP", RequestedProcedureDescription: "Synthetic", ScheduledProcedureStepID: "SPS", ScheduledProcedureStepDescription: "Synthetic"}}, PatientName: "SYNTHETIC^PATIENT", PatientID: "SYN", PatientBirthDate: "19700101", PatientSex: "O", PerformedProcedureStepID: "PPS", PerformedStationAETitle: "MODALITY", PerformedStationName: "STATION", PerformedLocation: "ROOM", StartDate: "20261009", StartTime: "101500", Modality: "CT", Description: "Synthetic", StudyID: "SYN"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Create(ctx, "1.2.3.99", attributes)
	if err != nil || result.Status != 0 {
		t.Fatalf("N-CREATE status %v err %v", result, err)
	}
	result, err = client.Create(ctx, "1.2.3.99", attributes)
	if err == nil || result.Status == 0 {
		t.Fatal("duplicate N-CREATE accepted")
	}
	if err = dimsecli.Release(a); err != nil {
		t.Fatal(err)
	}
}
func TestIANRealReceiveAndPersistedIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx, addr := startService(t, "ianscp", "-output-dir", dir)
	a, err := ul.DialContext(ctx, addr, ul.DialOptions{CalledAETitle: "IANSCP", CallingAETitle: "ARCHIVE", Contexts: []ul.PresentationContext{ian.PresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	notification := ian.Notification{StudyInstanceUID: "1.2.3", Series: []ian.Series{{SeriesInstanceUID: "1.2.3.1", Instances: []ian.Instance{{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.2", Availability: ian.AvailabilityOnline, RetrieveAETitle: "ARCHIVE"}}}}}
	result, err := ian.NewClient(a).NotifyNotification(ctx, "1.2.3.88", notification)
	if err != nil || result.Status != 0 {
		t.Fatalf("IAN status %v err %v", result, err)
	}
	file, err := object.OpenFile(filepath.Join(dir, "1.2.3.88.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := ian.Parse(file.Dataset)
	if err != nil || got.Count() != 1 {
		t.Fatal("stored notification invalid", err)
	}
	uid, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if uid != "1.2.3.88" {
		t.Fatal("command identity lost")
	}
	if err = dimsecli.Release(a); err != nil {
		t.Fatal(err)
	}
}
func TestMWLRealFindLockAndRawRequest(t *testing.T) {
	root := t.TempDir()
	aeDir := filepath.Join(root, "WLMSCP")
	os.Mkdir(aeDir, 0700)
	os.WriteFile(filepath.Join(aeDir, "lockfile"), nil, 0600)
	text := func(tag core.Tag, vr core.VR, s string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{s}}
	}
	step := object.FromElements([]core.Element{text(core.NewTag(0x0040, 0x0001), core.VRAE, "MODALITY"), text(core.NewTag(0x0040, 0x0002), core.VRDA, "20261009"), text(core.NewTag(0x0040, 0x0003), core.VRTM, "123000"), text(core.NewTag(0x0008, 0x0060), core.VRCS, "CT"), text(core.NewTag(0x0040, 0x0009), core.VRSH, "STEP"), text(core.NewTag(0x0040, 0x0007), core.VRLO, "Synthetic")}, nil)
	record := object.FromElements([]core.Element{text(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^PATIENT"), text(core.NewTag(0x0010, 0x0020), core.VRLO, "SYN"), text(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.3"), text(core.NewTag(0x0040, 0x1001), core.VRSH, "REQUEST"), text(core.NewTag(0x0032, 0x1060), core.VRLO, "Synthetic"), object.SequenceElement(core.NewTag(0x0040, 0x0100), step)}, nil)
	f, err := os.Create(filepath.Join(aeDir, "synthetic.wl"))
	if err != nil {
		t.Fatal(err)
	}
	if err = object.WriteDataSet(f, record, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	f.Close()
	queries := t.TempDir()
	ctx, addr := startService(t, "wlmscp", "-data-file-path", root, "-request-file-path", queries)
	a, err := ul.DialContext(ctx, addr, ul.DialOptions{CalledAETitle: "WLMSCP", CallingAETitle: "MODALITY", Contexts: []ul.PresentationContext{dimse.ModalityWorklistFindPresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	client, err := dimse.NewModalityWorklistClient(a)
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	_, err = client.Find(ctx, dimse.ModalityWorklistQuery{PatientName: dimse.MWLMatch("SYN*"), PatientID: dimse.MWLReturnKey()}, func(ds *object.Object) error {
		id, _ := ds.GetString(core.NewTag(0x0010, 0x0020))
		if id != "SYN" {
			t.Error("wrong MWL record")
		}
		matches++
		return nil
	})
	if err != nil || matches != 1 {
		t.Fatalf("matches %d err %v", matches, err)
	}
	entries, _ := os.ReadDir(queries)
	if len(entries) != 1 {
		t.Fatal("query not persisted")
	}
	if err = dimsecli.Release(a); err != nil {
		t.Fatal(err)
	}
}
func TestServerBoundedLifetimeAndCallingFilter(t *testing.T) {
	ctx, addr := startService(t, "mppsscp", "-calling", "ALLOWED")
	a, err := ul.DialContext(ctx, addr, ul.DialOptions{CalledAETitle: "MPPSSCP", CallingAETitle: "DENIED", Contexts: []ul.PresentationContext{mpps.PresentationContext()}})
	if a != nil {
		a.Close()
	}
	if err == nil {
		t.Fatal("Calling AE filter bypassed")
	}
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || port == "" {
		t.Fatal("invalid endpoint")
	}
}
