package ul

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// allocatedBytes reports the bytes allocated while fn runs.
func allocatedBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func pduHeader(pduType PDUType, length uint32) []byte {
	header := make([]byte, PDUHeaderSize)
	header[0] = byte(pduType)
	binary.BigEndian.PutUint32(header[2:], length)
	return header
}

// TestReadPDURejects2MiBRequestWithoutAllocating: a 2 MiB A-ASSOCIATE-RQ is
// refused from its header, before its body is read or allocated.
func TestReadPDURejects2MiBRequestWithoutAllocating(t *testing.T) {
	var err error
	allocated := allocatedBytes(func() {
		r := io.MultiReader(bytes.NewReader(pduHeader(PDUAssociateRQ, 2<<20)), io.LimitReader(zeroReader{}, 2<<20))
		_, err = ReadPDU(r, DefaultMaxPDU)
	})
	if !errors.Is(err, ErrPDUTooLarge) {
		t.Fatalf("ReadPDU error %v, want ErrPDUTooLarge", err)
	}
	if allocated > 64<<10 {
		t.Fatalf("rejecting the request allocated %d bytes", allocated)
	}
}

// TestReadPDUBodyGrowsWithArrivedBytes: a body that stops short of its
// declared length only costs the bytes that arrived plus the growth step.
func TestReadPDUBodyGrowsWithArrivedBytes(t *testing.T) {
	for _, pduType := range []PDUType{PDUAssociateRQ, PDUDataTF} {
		var err error
		allocated := allocatedBytes(func() {
			r := io.MultiReader(bytes.NewReader(pduHeader(pduType, 1000<<10)), io.LimitReader(zeroReader{}, 100))
			_, err = ReadPDU(r, 1<<20)
		})
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("type %#x: error %v, want io.ErrUnexpectedEOF", pduType, err)
		}
		if allocated > 2*pduReadGrowth {
			t.Fatalf("type %#x: a 100-byte truncated body allocated %d bytes", pduType, allocated)
		}
	}
}

func TestReadPDUWithAssociationLimit(t *testing.T) {
	rq := buildAssociateRQ(DialOptions{CalledAETitle: "SCP", CallingAETitle: "SCU", Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}})
	var encoded bytes.Buffer
	if err := WritePDU(&encoded, &rq); err != nil {
		t.Fatal(err)
	}
	bodyLength := uint32(encoded.Len()) - PDUHeaderSize
	if _, err := ReadPDUWithAssociationLimit(bytes.NewReader(encoded.Bytes()), MinimumPDUSize, bodyLength); err != nil {
		t.Fatalf("request at the limit: %v", err)
	}
	if _, err := ReadPDUWithAssociationLimit(bytes.NewReader(encoded.Bytes()), MinimumPDUSize, bodyLength-1); !errors.Is(err, ErrPDUTooLarge) {
		t.Fatalf("request above the limit: %v, want ErrPDUTooLarge", err)
	}
	// A raised limit admits a 2 MiB declaration (the truncated body then fails).
	if _, err := ReadPDUWithAssociationLimit(bytes.NewReader(pduHeader(PDUAssociateRQ, 2<<20)), MinimumPDUSize, 4<<20); errors.Is(err, ErrPDUTooLarge) {
		t.Fatalf("raised limit still refused the request: %v", err)
	}
}

// TestAcceptMaxAssociationPDU: a request above AcceptOptions.MaxAssociationPDU
// is answered with an A-P-ABORT, invalid-PDU-parameter-value.
func TestAcceptMaxAssociationPDU(t *testing.T) {
	addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", MaxAssociationPDU: 64, ARTIMTimeout: 200 * time.Millisecond})
	conn := rawPeer(t, addr, DialOptions{CalledAETitle: "ARCHIVE"})
	defer func() { _ = conn.Close() }()
	pdu, err := ReadPDU(conn, DefaultMaxPDU)
	if err != nil {
		t.Fatal(err)
	}
	if abort, ok := pdu.(*AbortRQ); !ok || abort.Source != AbortSourceServiceProvider || abort.Reason != AbortReasonInvalidPDUParameter {
		t.Fatalf("response %#v, want provider abort reason 6", pdu)
	}
	if err := <-done; !errors.Is(err, ErrPDUTooLarge) {
		t.Fatalf("Accept error %v, want ErrPDUTooLarge", err)
	}
}

// TestDialMaxAssociationPDU: an A-ASSOCIATE-AC above
// DialOptions.MaxAssociationPDU fails the dial.
func TestDialMaxAssociationPDU(t *testing.T) {
	addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}})
	assoc, err := Dial(addr, DialOptions{
		CalledAETitle: "ARCHIVE", CallingAETitle: "SCU", MaxAssociationPDU: 32,
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	if assoc != nil {
		_ = assoc.Close()
	}
	if !errors.Is(err, ErrPDUTooLarge) {
		t.Fatalf("Dial error %v, want ErrPDUTooLarge", err)
	}
	<-done
}
