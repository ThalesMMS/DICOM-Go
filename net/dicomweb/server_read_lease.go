package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrReadLeaseDenied reports that a Studies Service read lease could not be
// acquired or is no longer valid. It also matches ErrForbidden.
var ErrReadLeaseDenied = fmt.Errorf("%w: read lease denied", ErrForbidden)

// ReadLease is an application-issued grant for one Studies Service read.
// It is valid strictly before ExpiresAt. Validate can check live session
// revocation, policy version and audit availability. It may run concurrently
// if the backend invokes CheckReadLease from multiple goroutines.
type ReadLease struct {
	ExpiresAt time.Time
	Validate  func(context.Context) error
}

// ReadLeaseAuthorizer acquires a grant after the configured request
// Authorizer has succeeded. The callback owns policy and resource scope.
type ReadLeaseAuthorizer func(context.Context, *http.Request, Operation) (ReadLease, error)

type readLeaseKey struct{}

type requestReadLease struct {
	ReadLease
	cancel context.CancelCauseFunc
	denied atomic.Bool
}

func isStudiesServiceRead(op Operation) bool {
	switch op {
	case OperationSearchStudies, OperationSearchSeries, OperationSearchInstances,
		OperationRetrieveStudy, OperationRetrieveSeries, OperationRetrieveInstance,
		OperationRetrieveMetadata, OperationRetrieveFrames, OperationRetrieveRendered,
		OperationRetrieveThumbnail, OperationRetrieveBulkData, OperationRetrieveWADOURI:
		return true
	default:
		return false
	}
}

func acquireReadLease(acquire ReadLeaseAuthorizer, r *http.Request, op Operation) (lease ReadLease, err error) {
	defer func() {
		if recover() != nil {
			lease, err = ReadLease{}, ErrReadLeaseDenied
		}
	}()
	lease, err = acquire(r.Context(), r, op)
	if err != nil || lease.ExpiresAt.IsZero() {
		return ReadLease{}, ErrReadLeaseDenied
	}
	return lease, nil
}

// CheckReadLease revalidates a configured Studies Service lease. Backends
// should call it immediately before opening objects, advancing a cursor, or
// starting other protected work. With no lease it returns only ctx.Err().
// The server also checks its own effects, including reading yielded streams
// and writing response bytes. Denial terminates the request.
func CheckReadLease(ctx context.Context) error {
	lease, _ := ctx.Value(readLeaseKey{}).(*requestReadLease)
	return lease.check(ctx)
}

func (lease *requestReadLease) check(ctx context.Context) error {
	if lease == nil {
		return ctx.Err()
	}
	if err := lease.contextError(ctx); err != nil {
		return err
	}
	if !lease.validAt(time.Now()) {
		return lease.deny()
	}
	valid := validateReadLease(ctx, lease.Validate)
	if err := lease.contextError(ctx); err != nil {
		return err
	}
	if !valid || !lease.validAt(time.Now()) {
		return lease.deny()
	}
	return nil
}

func (lease *requestReadLease) deny() error {
	lease.denied.Store(true)
	lease.cancel(ErrReadLeaseDenied)
	return ErrReadLeaseDenied
}

func (lease *requestReadLease) contextError(ctx context.Context) error {
	if lease.denied.Load() || errors.Is(context.Cause(ctx), ErrReadLeaseDenied) {
		return ErrReadLeaseDenied
	}
	return ctx.Err()
}

func (lease ReadLease) validAt(now time.Time) bool {
	return now.Before(lease.ExpiresAt)
}

func validateReadLease(ctx context.Context, validate func(context.Context) error) (valid bool) {
	defer func() { _ = recover() }()
	return validate == nil || validate(ctx) == nil
}
