package dimse

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// A failed association keeps the class of its cause, so a caller can report a
// refused connection or an untrusted certificate, but none of the peer's
// address, host name or certificate.
func TestStoreAssociationErrorKeepsValueFreeDialCause(t *testing.T) {
	refused := fmt.Errorf("dial 192.0.2.10:104: %w", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	err := newStoreError("associate", 0, fmt.Errorf("%w: %w", ErrStoreAssociation, refused), false)
	var errno syscall.Errno
	if !errors.Is(err, ErrStoreAssociation) || !errors.As(err, &errno) || errno != syscall.ECONNREFUSED {
		t.Fatalf("refused connection lost its class: %v", err)
	}
	if strings.Contains(err.Error(), "192.0.2.10") {
		t.Fatalf("the peer address leaked: %v", err)
	}

	untrusted := fmt.Errorf("handshake with pacs.example.test: %w", x509.UnknownAuthorityError{})
	err = newStoreError("associate", 0, fmt.Errorf("%w: %w", ErrStoreAssociation, untrusted), false)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) || strings.Contains(err.Error(), "pacs.example.test") {
		t.Fatalf("untrusted certificate class lost or host leaked: %v", err)
	}

	timeout := fmt.Errorf("%w: %w", ErrStoreAssociation, fmt.Errorf("peer 192.0.2.10: %w", ul.ErrAssociationTimeout))
	if err = newStoreError("associate", 0, timeout, false); !errors.Is(err, ul.ErrAssociationTimeout) {
		t.Fatalf("association timeout lost its class: %v", err)
	}
}
