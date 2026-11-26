package dimse

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

// A rejected data set that never sends its last fragment must not be drained
// forever.
func TestDrainOfARejectedDataSetStopsAtItsBudget(t *testing.T) {
	server, client := net.Pipe()
	defer closeOrFail(t, "server pipe", server)
	defer closeOrFail(t, "client pipe", client)

	assoc := &ul.Association{Conn: client, MaxPDU: ul.DefaultMaxPDU}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		fragment := &ul.PDataTF{Values: []ul.PDataValue{{PresentationContextID: 1, Data: make([]byte, 4096)}}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := ul.WritePDU(server, fragment); err != nil {
				return
			}
		}
	}()

	reader := newTypedPDataReader(assoc, 1, false)
	done := make(chan error, 1)
	go func() { done <- drainPDataReaderWithBudget(reader, 64<<10) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRejectedDataSetDrainLimit) {
			t.Fatalf("drain error = %v, want ErrRejectedDataSetDrainLimit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain of an endless data set did not stop")
	}
	if reader.receivedBytes > 128<<10 {
		t.Fatalf("the drain read %d bytes with a 64 KiB budget", reader.receivedBytes)
	}
}

// A data set that ends inside the budget is drained to its last fragment.
func TestDrainOfARejectedDataSetReachesItsEnd(t *testing.T) {
	server, client := net.Pipe()
	defer closeOrFail(t, "server pipe", server)
	defer closeOrFail(t, "client pipe", client)

	assoc := &ul.Association{Conn: client, MaxPDU: ul.DefaultMaxPDU}
	written := make(chan error, 1)
	go func() {
		for i := range 4 {
			value := ul.PDataValue{PresentationContextID: 1, IsLast: i == 3, Data: make([]byte, 1024)}
			if err := ul.WritePDU(server, &ul.PDataTF{Values: []ul.PDataValue{value}}); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	reader := newTypedPDataReader(assoc, 1, false)
	if err := drainPDataReader(reader); err != nil {
		t.Fatalf("drain error = %v", err)
	}
	if err := waitDone(t, written, time.Second); err != nil {
		t.Fatalf("writer error = %v", err)
	}
	if reader.receivedBytes != 4096 {
		t.Fatalf("the drain read %d bytes, want 4096", reader.receivedBytes)
	}
}

func TestResponseValidationErrorsAreProtocolViolations(t *testing.T) {
	command := CEchoResponse{MessageIDBeingRespondedTo: 1, Status: StatusSuccess}.CommandSet()
	for i := range command {
		if command[i].Header.Tag == CommandField {
			command[i] = newUSCommandElement(CommandField, CStoreRSP)
		}
	}
	if _, err := ParseCEchoResponse(object.FromElements(command, nil)); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("a C-STORE-RSP parsed as a C-ECHO response: error = %v, want ErrProtocolViolation", err)
	}
	if err := protocolViolationf("dicom dimse: response message ID %d, want %d", 8, 1); err.Error() != "dicom dimse: response message ID 8, want 1" {
		t.Fatalf("a protocol violation changed its message: %q", err.Error())
	}
}
