package dimse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestMessagePackingTypedEmptyPreservesLegacySeparateStreams(t *testing.T) {
	var wire bytes.Buffer
	conn := &messagePackingConn{capture: &wire}
	err := sendMessageDataSet(context.Background(), &ul.Association{Conn: conn}, 1, packingCommand(), object.FromElements(nil, nil), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if conn.pdus != 2 || conn.pdvs != 2 {
		t.Fatal("typed-empty legacy envelope contract changed")
	}
	first, err := ul.ReadPDU(&wire, ul.DefaultMaxPDU)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ul.ReadPDU(&wire, ul.DefaultMaxPDU)
	if err != nil {
		t.Fatal(err)
	}
	command := first.(*ul.PDataTF).Values
	data := second.(*ul.PDataTF).Values
	if len(command) != 1 || !command[0].IsCommand || !command[0].IsLast || len(data) != 1 || data[0].IsCommand || !data[0].IsLast || len(data[0].Data) != 0 {
		t.Fatal("legacy empty stream was changed or packed")
	}
}

func TestMessagePackingNormalizedResponseKeepsHandlerError(t *testing.T) {
	conn := &messagePackingConn{}
	failure := errors.New("synthetic handler failure")
	command := (NormalizedGetResponse{AffectedSOPClassUID: secondaryCaptureSOPClassUID, MessageIDBeingRespondedTo: 1, CommandDataSetType: DataSetPresent, Status: StatusProcessingFailure}).CommandSet()
	err := sendNormalizedSCPResponse(context.Background(), &ul.Association{Conn: conn}, 1, transfer.ExplicitVRLittleEndian, command, messagePackingData(512), failure)
	if !errors.Is(err, failure) || conn.pdus != 1 || conn.pdvs != 2 {
		t.Fatalf("handler error=%v PDUs/PDVs=%d/%d", err, conn.pdus, conn.pdvs)
	}
}

func TestMessagePackingStoreFailureKeepsUncertainState(t *testing.T) {
	failure := errors.New("synthetic writer failure")
	for _, size := range []int{0, 2, 32768} {
		t.Run(fmt.Sprintf("producer-%d", size), func(t *testing.T) {
			conn := &messagePackingConn{}
			assoc := &ul.Association{Conn: conn, PeerMaxPDU: 1024, AcceptedContexts: []ul.AcceptedContext{{ID: 1, AbstractSyntaxUID: secondaryCaptureSOPClassUID, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID}}}
			result, err := NewStoreClient(assoc).StoreEncodedWithOptions(context.Background(), func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
				if _, err := w.Write(bytes.Repeat([]byte{0x5a}, size)); err != nil {
					return err
				}
				return failure
			}, CStoreOptions{AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: "2.25.1299"})
			if !errors.Is(err, failure) || !errors.Is(err, ErrAssociationStateUncertain) || result.Response != nil {
				t.Fatalf("failure lost uncertainty/correlation: %v", err)
			}
			if size < 512 && conn.pdus != 0 {
				t.Fatal("staging claimed a published command")
			}
			if size > 1024 && conn.pdus == 0 {
				t.Fatal("late failure did not exercise real writes")
			}
		})
	}
}

func TestMessagePackingFinishFailureAndCanceledProducerReleaseBuffers(t *testing.T) {
	for _, cancelProducer := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-%v", cancelProducer), func(t *testing.T) {
			failure := errors.New("synthetic socket failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &messagePackingConn{writeHook: func([]byte) error { return failure }}
			var writer *PDataWriter
			err := sendMessageWithContext(ctx, &ul.Association{Conn: conn}, 1, packingCommand(), transfer.ExplicitVRLittleEndian, func(w io.Writer) error {
				writer = w.(*PDataWriter)
				_, err := w.Write([]byte{1, 2})
				if cancelProducer {
					cancel()
				}
				return err
			})
			if err == nil || writer == nil || writer.buf != nil || writer.commandPrefix != nil || !writer.finished || conn.pdus != 0 {
				t.Fatalf("finish failure released/published incorrectly: %v", err)
			}
			if !cancelProducer && !errors.Is(err, failure) {
				t.Fatal(err)
			}
		})
	}
}

func TestMessagePackingBoundedGeneratedStream(t *testing.T) {
	sizes := []int{32 << 20}
	if os.Getenv("DICOMGO_PACKING_LARGE") == "1" {
		sizes = append(sizes, 256<<20)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dMiB", size>>20), func(t *testing.T) {
			chunk := bytes.Repeat([]byte{0x5a}, 32768)
			actual, expected := sha256.New(), sha256.New()
			var received int64
			conn := &messagePackingConn{writeHook: func(p []byte) error {
				if len(p) < 6 || len(p)-6 > 16378 {
					return fmt.Errorf("PDU exceeds negotiated body")
				}
				for offset := 6; offset < len(p); {
					if offset+6 > len(p) {
						return io.ErrUnexpectedEOF
					}
					n := int(binary.BigEndian.Uint32(p[offset : offset+4]))
					if n < 2 || offset+4+n > len(p) {
						return io.ErrUnexpectedEOF
					}
					if p[offset+5]&1 == 0 {
						fragment := p[offset+6 : offset+4+n]
						received += int64(len(fragment))
						_, _ = actual.Write(fragment)
					}
					offset += 4 + n
				}
				return nil
			}}
			var peakStaging int
			err := sendMessageWithContext(context.Background(), &ul.Association{Conn: conn, PeerMaxPDU: 16378}, 1, packingCommand(), transfer.ExplicitVRLittleEndian, func(w io.Writer) error {
				fragmenter := w.(*PDataWriter)
				for remaining := size; remaining > 0; {
					n := min(remaining, len(chunk))
					_, _ = expected.Write(chunk[:n])
					if _, err := w.Write(chunk[:n]); err != nil {
						return err
					}
					peakStaging = max(peakStaging, cap(fragmenter.buf)+cap(fragmenter.commandPrefix))
					remaining -= n
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if received != int64(size) || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
				t.Fatal("generated streaming dataset changed")
			}
			// Each fragmenter buffer is bounded by the configured PDU or the
			// existing <=1 MiB pool retention cap, never by the dataset size.
			if peakStaging > 2<<20 {
				t.Fatalf("staging grew with dataset: %d", peakStaging)
			}
			t.Logf("dataset=%d P-DATA=%d peak-fragmenter-capacity=%d", size, conn.pdus, peakStaging)
		})
	}
}

func TestMessagePackingCommandTelemetryRequiresSuccessfulSend(t *testing.T) {
	for _, tc := range []struct {
		name         string
		size         int
		failSend     bool
		failProducer bool
		commands     int64
	}{
		{"staging failure", 2, false, true, 0},
		{"first send failure", 2, true, false, 0},
		{"successful mixed send", 2, false, false, 1},
		{"producer failure after flush", 32768, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			accepted := make(chan *ul.Association, 1)
			acceptErrors := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "PACKTELEMETRY", Context: ctx, SupportedAbstractSyntaxes: []string{secondaryCaptureSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
				if err != nil {
					acceptErrors <- err
					return
				}
				accepted <- assoc
			}()
			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "PACKTELEMETRY", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: secondaryCaptureSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}, Observability: packingObservability()})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case peer := <-accepted:
				defer func() { _ = peer.Close() }()
			case err := <-acceptErrors:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// A real negotiated association initializes enabled exact counters.
			// Replace only its synchronous transport to inject deterministic
			// success/failure without depending on asynchronous socket shutdown.
			original := assoc.Conn
			defer func() { _ = original.Close(); _ = assoc.Close() }()
			conn := &messagePackingConn{}
			if tc.failSend {
				conn.writeHook = func([]byte) error { return io.ErrClosedPipe }
			}
			assoc.Conn = conn
			before := assoc.TelemetrySnapshot()
			failure := errors.New("synthetic producer failure")
			err = sendMessageWithContext(ctx, assoc, 1, packingCommand(), transfer.ExplicitVRLittleEndian, func(w io.Writer) error {
				if _, err := w.Write(bytes.Repeat([]byte{0x5a}, tc.size)); err != nil {
					return err
				}
				if tc.failProducer {
					return failure
				}
				return nil
			})
			if (err != nil) != (tc.failSend || tc.failProducer) {
				t.Fatalf("unexpected error: %v", err)
			}
			after := assoc.TelemetrySnapshot()
			if got := after.CommandsOutbound - before.CommandsOutbound; got != uint64(tc.commands) {
				t.Fatalf("observed commands=%d want %d", got, tc.commands)
			}
			if got := after.PDUsOutbound - before.PDUsOutbound; got != uint64(conn.pdus) {
				t.Fatalf("observed PDUs=%d actual successful sends=%d", got, conn.pdus)
			}
		})
	}
}
