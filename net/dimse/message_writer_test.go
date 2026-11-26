package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func packingCommand() []core.Element {
	return (CStoreRequest{AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: "2.25.1299", MessageID: 1}).CommandSet()
}

func packingWire(t *testing.T, cap uint32, syntax transfer.Syntax, produce func(io.Writer) error) ([]*ul.PDataTF, error) {
	t.Helper()
	var wire bytes.Buffer
	conn := &messagePackingConn{capture: &wire}
	assoc := &ul.Association{Conn: conn, PeerMaxPDU: cap, MaxPDU: ul.DefaultMaxPDU}
	conn.writeHook = func(p []byte) error {
		if len(p)-int(ul.PDUHeaderSize) > int(assoc.SendPDULimit()) {
			return fmt.Errorf("physical PDU body exceeds negotiated/local maximum")
		}
		return nil
	}
	err := sendMessageWithContext(context.Background(), assoc, 1, packingCommand(), syntax, produce)
	var pdus []*ul.PDataTF
	for wire.Len() > 0 {
		pdu, parseErr := ul.ReadPDU(&wire, max(assoc.SendPDULimit(), ul.DefaultMaxPDU))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		pdus = append(pdus, pdu.(*ul.PDataTF))
	}
	return pdus, err
}

func reassemblePacking(t *testing.T, pdus []*ul.PDataTF) ([]byte, []byte) {
	t.Helper()
	var command, dataset []byte
	commandLast, datasetLast := false, false
	for _, pdu := range pdus {
		for _, pdv := range pdu.Values {
			if pdv.PresentationContextID != 1 || len(pdv.Data) == 0 || len(pdv.Data)%2 != 0 {
				t.Fatalf("invalid PDV: id=%d bytes=%d", pdv.PresentationContextID, len(pdv.Data))
			}
			if pdv.IsCommand {
				if commandLast || len(dataset) > 0 {
					t.Fatal("command interleaved or follows its final PDV")
				}
				command = append(command, pdv.Data...)
				commandLast = pdv.IsLast
			} else {
				if !commandLast || datasetLast {
					t.Fatal("dataset order/final flag invalid")
				}
				dataset = append(dataset, pdv.Data...)
				datasetLast = pdv.IsLast
			}
		}
	}
	if !commandLast || !datasetLast {
		t.Fatal("missing command/dataset last flags")
	}
	want, err := EncodeCommandSet(packingCommand())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(command, want) {
		t.Fatal("command bytes changed")
	}
	return command, dataset
}

func TestMessagePackingBoundaryAndStreamingCapacity(t *testing.T) {
	encoded, _ := EncodeCommandSet(packingCommand())
	for _, tc := range []struct {
		name  string
		cap   uint32
		bytes int
		pdus  int
		mixed bool
	}{
		{"small", 16378, 512, 1, true},
		{"exact", uint32(len(encoded) + 12 + 512), 512, 1, true},
		{"above", uint32(len(encoded) + 12 + 510), 512, 2, true},
		{"below", uint32(len(encoded) + 12 + 514), 512, 1, true},
		{"no dataset room", uint32(len(encoded) + 12), 512, 6, false},
		{"fragmented command", 96, 512, 0, true},
		{"large single Write", 1024, 32768, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x5a}, tc.bytes)
			pdus, err := packingWire(t, tc.cap, transfer.ExplicitVRLittleEndian, func(w io.Writer) error { _, err := w.Write(payload); return err })
			if err != nil {
				t.Fatal(err)
			}
			if tc.pdus > 0 && len(pdus) != tc.pdus {
				t.Fatalf("PDUs=%d want %d", len(pdus), tc.pdus)
			}
			mixed := false
			for _, pdu := range pdus {
				if len(pdu.Values) == 2 {
					mixed = true
				}
			}
			if mixed != tc.mixed {
				t.Fatalf("mixed=%v want %v", mixed, tc.mixed)
			}
			_, got := reassemblePacking(t, pdus)
			if !bytes.Equal(got, payload) {
				t.Fatal("dataset bytes changed")
			}
			if tc.name == "large single Write" {
				for i, pdu := range pdus {
					if i > 0 && i < len(pdus)-1 {
						if len(pdu.Values) != 1 || len(pdu.Values[0].Data) != int(tc.cap)-6 {
							t.Fatal("prefix reduced later fragment capacity")
						}
					}
				}
			}
		})
	}
}

func TestMessagePackingTypedSyntaxAndIrregularChunks(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			dataset := messagePackingData(1024)
			var encoded bytes.Buffer
			if err := object.WriteDataSet(&encoded, dataset, syntax); err != nil {
				t.Fatal(err)
			}
			for _, chunk := range []int{1, 3, 17, 65536} {
				pdus, err := packingWire(t, 256, syntax, func(w io.Writer) error {
					p := encoded.Bytes()
					for len(p) > 0 {
						n := min(chunk, len(p))
						if _, err := w.Write(p[:n]); err != nil {
							return err
						}
						p = p[n:]
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				_, got := reassemblePacking(t, pdus)
				if !bytes.Equal(got, encoded.Bytes()) {
					t.Fatal("encoded dataset changed")
				}
				if _, err := object.ReadDataSet(bytes.NewReader(got), syntax); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMessagePackingEmptyOddAndDeflatedPad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		syntax  transfer.Syntax
		wantErr bool
		want    []byte
	}{
		{"empty", nil, transfer.ExplicitVRLittleEndian, true, nil},
		{"odd native", []byte{1, 2, 3}, transfer.ExplicitVRLittleEndian, true, nil},
		{"deflated pad", []byte{1, 2, 3}, transfer.DeflatedExplicitVRLittleEndian, false, []byte{1, 2, 3, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pdus, err := packingWire(t, 16378, tc.syntax, func(w io.Writer) error { _, err := w.Write(tc.data); return err })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if tc.wantErr && len(pdus) != 0 {
				t.Fatal("invalid staged dataset published")
			}
			if !tc.wantErr {
				_, got := reassemblePacking(t, pdus)
				if !bytes.Equal(got, tc.want) {
					t.Fatal("deflated pad mismatch")
				}
			}
		})
	}
}

func TestMessagePackingProducerErrorsDiscardStaging(t *testing.T) {
	failure := errors.New("synthetic producer failure")
	for _, size := range []int{0, 2, 32768} {
		t.Run(string(rune('A'+size/1024)), func(t *testing.T) {
			var retained *PDataWriter
			pdus, err := packingWire(t, 1024, transfer.ExplicitVRLittleEndian, func(w io.Writer) error {
				retained = w.(*PDataWriter)
				if _, err := w.Write(bytes.Repeat([]byte{0x5a}, size)); err != nil {
					return err
				}
				return failure
			})
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if retained.buf != nil || retained.commandPrefix != nil || !retained.finished {
				t.Fatal("staging retained after failure")
			}
			if size < 512 && len(pdus) != 0 {
				t.Fatal("producer failure published staged command")
			}
			if size > 1024 && len(pdus) == 0 {
				t.Fatal("late failure did not exercise flushed prefix")
			}
		})
	}
}

func TestMessagePackingCancellationAndMixedProgressTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := sendMessageWithContext(ctx, &ul.Association{Conn: &messagePackingConn{}}, 1, packingCommand(), transfer.ExplicitVRLittleEndian, func(io.Writer) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("pre-canceled err=%v called=%v", err, called)
	}
	for _, controls := range []SCPControls{{CommandProgressTimeout: 20 * time.Millisecond, DataSetProgressTimeout: time.Second}, {CommandProgressTimeout: time.Second, DataSetProgressTimeout: 20 * time.Millisecond}} {
		server, client := net.Pipe()
		assoc := &ul.Association{Conn: client, PeerMaxPDU: 16378}
		started := time.Now()
		err := sendMessageWithContext(withSCPControls(context.Background(), controls), assoc, 1, packingCommand(), transfer.ExplicitVRLittleEndian, func(w io.Writer) error { _, err := w.Write([]byte{1, 2}); return err })
		_ = server.Close()
		_ = client.Close()
		if !errors.Is(err, ul.ErrAssociationTimeout) || time.Since(started) > time.Second {
			t.Fatalf("blocked mixed write error=%v elapsed=%v", err, time.Since(started))
		}
	}
}

func TestMessagePackingCommandOnlyRemainsOnePDU(t *testing.T) {
	conn := &messagePackingConn{}
	if err := sendMessageDataSet(context.Background(), &ul.Association{Conn: conn}, 1, (CEchoRequest{MessageID: 1}).CommandSet(), nil, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if conn.pdus != 1 || conn.pdvs != 1 {
		t.Fatalf("command-only PDUs/PDVs=%d/%d", conn.pdus, conn.pdvs)
	}
}

func TestMessagePackingUsesExistingSendLimitNormalization(t *testing.T) {
	for _, peerMaximum := range []uint32{0, 16378, 128 << 10, 4 << 20} {
		t.Run(fmt.Sprintf("peer-%d", peerMaximum), func(t *testing.T) {
			pdus, err := packingWire(t, peerMaximum, transfer.ExplicitVRLittleEndian, func(w io.Writer) error {
				_, err := w.Write(bytes.Repeat([]byte{0x5a}, 2<<20))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			_, dataset := reassemblePacking(t, pdus)
			if len(dataset) != 2<<20 {
				t.Fatal("normalized limit truncated the dataset")
			}
		})
	}
}
