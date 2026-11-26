package dimse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// BenchmarkLoopbackDataSetSendPDU streams a 32 MiB data set over a loopback
// association whose acceptor offers a 1 MiB Maximum Length, comparing the
// former 16 KiB outgoing P-DATA size with 64 KiB and the 128 KiB default.
// Both sides use progress timeouts, as storescu and storescp do.
func BenchmarkLoopbackDataSetSendPDU(b *testing.B) {
	payload := bytes.Repeat([]byte{0x5a}, 32<<20)
	for _, maxSend := range []uint32{16 << 10, 64 << 10, ul.DefaultMaxSendPDU} {
		b.Run(fmt.Sprintf("%dKiB", maxSend>>10), func(b *testing.B) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			serverDone := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "STORESCP", Context: ctx, MaxPDU: 1 << 20, ReadProgressTimeout: time.Minute,
					SupportedAbstractSyntaxes: []string{dicomtest.TestSOPClassUID},
					SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID},
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = assoc.Close() }()
				for {
					pdu, err := assoc.ReadPDU()
					if err != nil {
						serverDone <- err
						return
					}
					switch pdu := pdu.(type) {
					case *ul.PDataTF:
						_ = pdu
					case *ul.ReleaseRQ:
						serverDone <- assoc.WritePDU(&ul.ReleaseRP{})
						return
					default:
						serverDone <- fmt.Errorf("unexpected %T", pdu)
						return
					}
				}
			}()
			assoc, err := ul.Dial(listener.Addr().String(), ul.DialOptions{
				CalledAETitle: "STORESCP", CallingAETitle: "STORESCU", Context: ctx, MaxSendPDU: maxSend, WriteProgressTimeout: time.Minute,
				Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}},
			})
			if err != nil {
				b.Fatal(err)
			}
			accepted, err := AcceptedContextForSOPClass(assoc, dicomtest.TestSOPClassUID)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				writer := NewPDataWriterWithContext(ctx, assoc, accepted.ID, false, peerMaxPDUWithHeader(assoc))
				if _, err := io.Copy(writer, bytes.NewReader(payload)); err != nil {
					b.Fatal(err)
				}
				if err := writer.Finish(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := assoc.Release(ctx); err != nil {
				b.Fatal(err)
			}
			if err := <-serverDone; err != nil {
				b.Fatal(err)
			}
		})
	}
}
