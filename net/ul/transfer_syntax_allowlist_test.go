package ul

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDialTransferSyntaxAllowlistRejectsEmptyIntersectionBeforeConnecting(t *testing.T) {
	dialed := false
	opts := DialOptions{
		Contexts: []PresentationContext{{
			AbstractSyntaxUID:  "1.2.840.10008.1.1",
			TransferSyntaxUIDs: []string{ExplicitVRLittleEndian},
		}},
		TransferSyntaxAllowlist: []string{ImplicitVRLittleEndian},
		Transport: TransportOptions{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		}},
	}
	assoc, err := DialContext(context.Background(), "unused", opts)
	if assoc != nil || dialed || !errors.Is(err, transfer.ErrUnsupportedTransferSyntax) {
		t.Fatalf("assoc=%v, dialed=%t, error=%v, want local syntax conflict", assoc, dialed, err)
	}
}
