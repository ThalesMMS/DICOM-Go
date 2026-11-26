package ul

import (
	"bytes"
	"context"
	"math"
	"net"
	"testing"
	"time"
)

func TestSendPDULimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		assoc *Association
		want  uint32
	}{
		{"nil", nil, DefaultMaxPDU},
		{"peer smaller than the cap", &Association{PeerMaxPDU: 32768}, 32768},
		{"peer larger than the default cap", &Association{PeerMaxPDU: 1 << 20}, DefaultMaxSendPDU},
		{"unlimited peer", &Association{PeerMaxPDU: math.MaxUint32}, DefaultMaxSendPDU},
		{"local cap", &Association{PeerMaxPDU: 1 << 20, maxSendPDU: 65536}, 65536},
		{"local cap above the peer", &Association{PeerMaxPDU: 16384, maxSendPDU: 1 << 20}, 16384},
		{"tiny peer", &Association{PeerMaxPDU: 1}, PDVHeaderSize + 1},
	} {
		if got := tc.assoc.SendPDULimit(); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if resolveMaxSendPDU(0) != DefaultMaxSendPDU || resolveMaxSendPDU(4096) != 4096 {
		t.Fatal("resolveMaxSendPDU")
	}
}

// TestReadPDUGrowsWithArrivedBytes: a declared P-DATA length alone does not
// make the reader allocate it.
func TestReadPDUGrowsWithArrivedBytes(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	assoc := &Association{Conn: server, MaxPDU: 1 << 20}
	go func() {
		header := []byte{byte(PDUDataTF), 0, 0, 0x0F, 0, 0}
		_, _ = client.Write(append(header, 0, 0, 0x10))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := assoc.Receive(ctx); err == nil {
		t.Fatal("truncated PDU was accepted")
	}
	if got := cap(assoc.pduReadBuffer); got > pduReadGrowth+int(PDUHeaderSize) {
		t.Fatalf("read buffer grew to %d bytes for a partial PDU", got)
	}
}

// TestReadPDUReusesBufferAcrossPData: consecutive P-DATA-TF PDUs share the
// read buffer while each value keeps its own bytes.
func TestReadPDUReusesBufferAcrossPData(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	assoc := &Association{Conn: server, MaxPDU: DefaultMaxPDU}
	payloads := [][]byte{bytes.Repeat([]byte{0xA1}, 4000), bytes.Repeat([]byte{0xB2}, 4000)}
	go func() {
		for _, payload := range payloads {
			_ = WritePDU(client, &PDataTF{Values: []PDataValue{{PresentationContextID: 1, Data: payload}}})
		}
	}()
	var got []*PDataTF
	var buffers []*byte
	for range payloads {
		pdu, err := assoc.ReadPDU()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, pdu.(*PDataTF))
		buffers = append(buffers, &assoc.pduReadBuffer[:1][0])
	}
	if buffers[0] != buffers[1] {
		t.Fatal("read buffer was not reused between P-DATA PDUs")
	}
	for i, pdu := range got {
		if !bytes.Equal(pdu.Values[0].Data, payloads[i]) {
			t.Fatalf("P-DATA %d was overwritten by a later PDU", i)
		}
	}
}

type recordingWriter struct{ writes [][]byte }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestWritePDUSingleWrite(t *testing.T) {
	var w recordingWriter
	payload := bytes.Repeat([]byte{0x5A}, 1000)
	if err := WritePDU(&w, &PDataTF{Values: []PDataValue{{PresentationContextID: 3, IsLast: true, Data: payload}}}); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("%d writes, want header and body in one", len(w.writes))
	}
	pdu, err := ReadPDU(bytes.NewReader(w.writes[0]), DefaultMaxPDU)
	if err != nil {
		t.Fatal(err)
	}
	if value := pdu.(*PDataTF).Values[0]; value.PresentationContextID != 3 || !value.IsLast || !bytes.Equal(value.Data, payload) {
		t.Fatalf("round trip %#v", value)
	}
}
