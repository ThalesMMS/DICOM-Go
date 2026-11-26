package dimse

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TestAdmissionRejectionReachesDCMTK checks that DCMTK echoscu, rejected by
// calling AE title and by host, reports the A-ASSOCIATE-RJ reason instead
// of an abort. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestAdmissionRejectionReachesDCMTK(t *testing.T) {
	echoscu := dcmtkoracle.Tool(t, "echoscu")
	cases := []struct {
		name      string
		admission ul.AdmissionHandler
		calling   string
		want      []string
	}{
		{"calling AE", func(_ context.Context, req ul.AdmissionRequest) *ul.AssociationRJ {
			if req.CallingAETitle != "ALLOWED" {
				return ul.RejectCallingAETitle()
			}
			return nil
		}, "INTRUDER", []string{"Rejected Permanent", "Service User", "Calling AE Title Not Recognized"}},
		{"host", func(_ context.Context, req ul.AdmissionRequest) *ul.AssociationRJ {
			if host, _, _ := net.SplitHostPort(req.RemoteAddr.String()); host == "127.0.0.1" {
				return ul.RejectPermanent()
			}
			return nil
		}, "ALLOWED", []string{"Rejected Permanent", "Service User", "Reason: No Reason"}},
		{"admitted", func(context.Context, ul.AdmissionRequest) *ul.AssociationRJ { return nil }, "ALLOWED", []string{"Received Echo Response (Success)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				assoc, err := ul.Accept(conn, ul.AcceptOptions{
					AETitle: "ARCHIVE", SupportedAbstractSyntaxes: []string{"1.2.840.10008.1.1"},
					SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian},
					Admission:                 tc.admission, NegotiationTimeout: 5 * time.Second,
				})
				if err != nil {
					_ = conn.Close()
					return
				}
				_ = ServeAssociation(context.Background(), assoc, AssociationSCPOptions{})
			}()
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			out, _ := dcmtkoracle.Command(context.Background(), t, dcmtkoracle.Protocol, echoscu, "-v", "-aet", tc.calling, "-aec", "ARCHIVE", "127.0.0.1", port).CombinedOutput()
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Fatalf("echoscu output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(string(out), "Abort") {
				t.Fatalf("echoscu saw an abort:\n%s", out)
			}
		})
	}
}

// TestProviderAbortReachesDCMTK: an SCP that aborts while echoscu's
// request is still unread lingers, so echoscu reads the A-ABORT instead of
// a connection reset. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestProviderAbortReachesDCMTK(t *testing.T) {
	echoscu := dcmtkoracle.Tool(t, "echoscu")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		assoc, err := ul.Accept(conn, ul.AcceptOptions{
			AETitle: "ARCHIVE", SupportedAbstractSyntaxes: []string{"1.2.840.10008.1.1"},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian},
		})
		if err != nil {
			_ = conn.Close()
			return
		}
		// Give the C-ECHO-RQ time to arrive unread, then abort.
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = assoc.AbortForError(ctx, ul.UnexpectedPDUError(&ul.UnknownPDU{Type: 0x7F}, "testing"))
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	out, _ := dcmtkoracle.Command(context.Background(), t, dcmtkoracle.Protocol, echoscu, "-v", "-aec", "ARCHIVE", "127.0.0.1", port).CombinedOutput()
	text := string(out)
	if !strings.Contains(text, "Abort") || strings.Contains(strings.ToLower(text), "reset") {
		t.Fatalf("echoscu did not report the abort:\n%s", text)
	}
	t.Logf("echoscu:\n%s", text)
}
