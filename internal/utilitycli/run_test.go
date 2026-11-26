package utilitycli

import (
	"bytes"
	"context"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDcm2strUsesQualifiedTemplateAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.dcm")
	file, _ := encapdoc.NewPDF(encapdoc.PDFOptions{SOPInstanceUID: "1.2.3", StudyInstanceUID: "1.2.4", SeriesInstanceUID: "1.2.5", PatientID: "../unsafe/segment", Data: []byte("%PDF-1.4\n%%EOF\n")})
	if err := wrappercli.WriteFile(path, file); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Execute(context.Background(), "dcm2str", []string{"-p", "{PatientID}/{SOPInstanceUID}", path}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "../") || !strings.HasSuffix(out.String(), "/1.2.3\n") {
		t.Fatalf("unsafe or wrong path %q", out.String())
	}
	if err = Execute(context.Background(), "dcm2str", []string{"-p", "../{PatientID}", path}, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted unsafe template")
	}
}
func TestDcmftestMarkerOnlyAndBatchRefusal(t *testing.T) {
	dir := t.TempDir()
	yes, no := filepath.Join(dir, "yes"), filepath.Join(dir, "no")
	b := make([]byte, 132)
	copy(b[128:], "DICM")
	os.WriteFile(yes, b, 0600)
	os.WriteFile(no, []byte("DICM"), 0600)
	if !hasMarker(yes) || hasMarker(no) || hasMarker(dir) {
		t.Fatal("marker offsets wrong")
	}
	var out bytes.Buffer
	if err := Execute(context.Background(), "dcmftest", []string{yes, no}, &out, io.Discard); err == nil {
		t.Fatal("batch nonpart10 accepted")
	}
	if !strings.Contains(out.String(), "yes: ") || !strings.Contains(out.String(), "no: ") {
		t.Fatal("missing each result")
	}
}
func TestTermscuPrivateContextAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		a, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "TERM", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{PrivateShutdownUID}, SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian}, NegotiationTimeout: time.Second})
		if err != nil {
			done <- err
			return
		}
		defer a.Close()
		p, err := a.ReadPDU()
		if err == nil {
			if _, ok := p.(*ul.ReleaseRQ); !ok {
				err = io.ErrUnexpectedEOF
			} else {
				err = a.WritePDU(ul.ReleaseRP{})
			}
		}
		done <- err
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	var out bytes.Buffer
	if err := Execute(ctx, "termscu", []string{"-host", host, "-port", port, "-called", "TERM", "-timeout", "2s"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not confirmed") {
		t.Fatal("reported shutdown proof")
	}
}

func TestMarkerFIFORefusedWithoutWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO qualification")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- hasMarker(fifo) }()
	select {
	case marker := <-done:
		if marker {
			t.Fatal("FIFO has marker")
		}
	case <-time.After(200 * time.Millisecond):
		writer, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
		if err == nil {
			writer.Close()
		}
		<-done
		t.Fatal("marker FIFO open blocked")
	}
}
