package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// An uncertain C-STORE names a timeout of the wait for the peer with a
// value-free sentinel, and nothing else about the transport error.
func TestUncertainStoreErrorNamesATimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		timeout bool
	}{
		{"read deadline", fmt.Errorf("read 10.0.0.7:104: %w", os.ErrDeadlineExceeded), true},
		{"context deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), true},
		{"association timeout", fmt.Errorf("idle: %w", ul.ErrAssociationTimeout), true},
		{"connection closed", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), false},
	} {
		err := newStoreError("transfer", 3, uncertainStoreCause(tc.err), true)
		if !errors.Is(err, ErrStoreUncertain) {
			t.Errorf("%s: the error is not ErrStoreUncertain: %v", tc.name, err)
		}
		if got := errors.Is(err, ErrStoreTimeout); got != tc.timeout {
			t.Errorf("%s: timeout = %t, want %t", tc.name, got, tc.timeout)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s: the error carries the transport error", tc.name)
		}
		if err.Error() != "dicom dimse: C-STORE batch transfer failed" {
			t.Errorf("%s: text = %q", tc.name, err.Error())
		}
	}
}
