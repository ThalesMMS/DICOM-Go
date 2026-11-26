package dimse

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const secondaryCaptureSOPClassUID = "1.2.840.10008.5.1.4.1.1.7"

var tagPixelData = core.NewTag(0x7FE0, 0x0010)

// largeSecondaryCapture writes a 16-frame 512x512 16-bit Secondary Capture
// (8 MiB of Pixel Data) and returns its path and pixel bytes.
func largeSecondaryCapture(t *testing.T) (string, []byte) {
	t.Helper()
	pixels := make([]byte, 16*512*512*2)
	for i := range pixels {
		pixels[i] = byte(i*7 + i>>11)
	}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), secondaryCaptureSOPClassUID),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.826.0.1.3680043.10.543.991.1"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "OT"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0064), core.VRCS, "WSD"),
		dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "TEST^PDU"),
		dicomtest.NewStringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "PDU991"),
		dicomtest.NewUIElement(core.NewTag(0x0020, 0x000D), "1.2.826.0.1.3680043.10.543.991.2"),
		dicomtest.NewUIElement(core.NewTag(0x0020, 0x000E), "1.2.826.0.1.3680043.10.543.991.3"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "16"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 512),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 512),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 16),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 16),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 15),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		dicomtest.BytesElement(tagPixelData, core.VROW, pixels),
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "large-sc.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pixels
}

func freePort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// TestLargeStoreSendPDUWithDCMTK is opt-in evidence for #991: an 8 MiB
// C-STORE reaches DCMTK storescp intact whether it accepts 128 KiB PDUs or
// only the 4 KiB minimum, and dicom-go reads the up to 128 KiB PDUs that
// DCMTK storescu sends to a 1 MiB acceptor. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestLargeStoreSendPDUWithDCMTK(t *testing.T) {
	storescpTool := dcmtkoracle.Tool(t, "storescp")
	storescu := dcmtkoracle.Tool(t, "storescu")
	path, pixels := largeSecondaryCapture(t)
	for _, maxPDU := range []int{131072, 16384, 4096} {
		t.Run("to storescp max "+strconv.Itoa(maxPDU), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			port := freePort(t)
			outDir := t.TempDir()
			storescp := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescpTool, "-aet", "PDUSCP", "--max-pdu", strconv.Itoa(maxPDU), "-od", outDir, strconv.Itoa(port))
			if err := storescp.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storescp.Process.Kill(); _ = storescp.Wait() })
			time.Sleep(300 * time.Millisecond)
			session, err := NewStoreSession(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), StoreSessionOptions{
				DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "PDUSCP", WriteProgressTimeout: 10 * time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			item, err := session.Store(ctx, NewPathStoreSource(path))
			_ = session.Close(ctx)
			if err != nil || item.Outcome != StoreOutcomeSuccess {
				t.Fatalf("store: %s %v", item.Outcome, err)
			}
			var received []byte
			for deadline := time.Now().Add(5 * time.Second); received == nil && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				entries, _ := os.ReadDir(outDir)
				for _, entry := range entries {
					file, err := object.OpenFile(filepath.Join(outDir, entry.Name()))
					if err != nil {
						continue
					}
					received, _ = file.Dataset.GetRaw(tagPixelData)
					_ = file.Close()
				}
			}
			if !bytes.Equal(received, pixels) {
				t.Fatalf("DCMTK persisted %d Pixel Data bytes that differ from the %d sent", len(received), len(pixels))
			}
		})
	}
	t.Run("from storescu", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		port, done := serveOneStore(t, ctx, ul.AcceptOptions{AETitle: "GOSCP", MaxPDU: 1 << 20, ReadProgressTimeout: 10 * time.Second})
		if out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescu, "-aec", "GOSCP", "-xe", "127.0.0.1", strconv.Itoa(port), path).CombinedOutput(); err != nil {
			t.Fatalf("storescu: %v\n%s", err, out)
		}
		got := <-done
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !bytes.Equal(got.pixels, pixels) {
			t.Fatalf("received %d Pixel Data bytes that differ from the %d sent", len(got.pixels), len(pixels))
		}
	})
}

type storedPixels struct {
	pixels         []byte
	transferSyntax string
	err            error
}

// serveOneStore accepts one association with opts (by default Secondary
// Capture in Explicit VR Little Endian), receives one C-STORE and its
// release, and reports the Pixel Data received.
func serveOneStore(t *testing.T, ctx context.Context, opts ul.AcceptOptions) (int, <-chan storedPixels) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	opts.Context = ctx
	if opts.SupportedAbstractSyntaxes == nil && opts.PresentationContextRules == nil {
		opts.SupportedAbstractSyntaxes = []string{secondaryCaptureSOPClassUID}
	}
	if opts.SupportedTransferSyntaxes == nil {
		opts.SupportedTransferSyntaxes = []string{transfer.ExplicitVRLittleEndian.UID}
	}
	done := make(chan storedPixels, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(opts)
		if err != nil {
			done <- storedPixels{err: err}
			return
		}
		defer func() { _ = assoc.Close() }()
		pcID, request, err := ReceiveCStoreRequestAny(assoc)
		if err != nil {
			done <- storedPixels{err: err}
			return
		}
		dataset, err := ReceiveDataSet(assoc, pcID, transfer.ExplicitVRLittleEndian)
		if err == nil {
			err = SendCStoreResponse(assoc, pcID, CStoreResponse{
				AffectedSOPClassUID: request.AffectedSOPClassUID, MessageIDBeingRespondedTo: request.MessageID,
				AffectedSOPInstanceUID: request.AffectedSOPInstanceUID, Status: StatusSuccess,
			})
		}
		if err == nil {
			err = respondToStoreRelease(assoc)
		}
		var received []byte
		if dataset != nil {
			received, _ = dataset.GetRaw(tagPixelData)
		}
		var syntax string
		for _, accepted := range assoc.AcceptedContexts {
			if accepted.ID == pcID {
				syntax = accepted.TransferSyntaxUID
			}
		}
		done <- storedPixels{pixels: received, transferSyntax: syntax, err: err}
	}()
	return listener.Addr().(*net.TCPAddr).Port, done
}
