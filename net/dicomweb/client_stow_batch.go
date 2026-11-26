package dicomweb

import (
	"context"
	"errors"
	"net/http"
)

// Default limits of one STOW-RS request of StoreInstancesBatched.
const (
	DefaultStoreBatchInstances       = 100
	DefaultStoreBatchBytes     int64 = 64 << 20
)

// StoreBatchLimits bounds the requests of StoreInstancesBatched. A zero
// field selects its default. An instance larger than MaxBytes is sent alone.
type StoreBatchLimits struct {
	// MaxInstances is the most instances one request carries.
	MaxInstances int
	// MaxBytes bounds the sum of StoreInstance.Size of one request.
	MaxBytes int64
}

func (limits StoreBatchLimits) normalized() StoreBatchLimits {
	if limits.MaxInstances <= 0 {
		limits.MaxInstances = DefaultStoreBatchInstances
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = DefaultStoreBatchBytes
	}
	return limits
}

// StoreBatch is one finished request of StoreInstancesBatched.
type StoreBatch struct {
	// Start and End delimit the instances the request carried:
	// instances[Start:End].
	Start, End int
	Result     StoreResult
	Err        error
	// Reduced reports a request that carries part of a batch the server
	// refused with HTTP 413 Content Too Large.
	Reduced bool
}

// StoreInstancesBatched stores instances with as many STOW-RS requests as
// the limits require, in order. Each request is bounded by Options.Timeout on
// its own, so the time of a whole upload grows with its size.
//
// MaxBytes bounds the bytes sent only as far as StoreInstance.Size gives the
// length of the payload Open returns.
//
// A request the server refuses with HTTP 413 is split once into two halves by
// instance count, each sent once; later requests keep the limits of the larger
// half (its instance count and bytes). A half refused again, a single
// instance, or instances whose payload cannot be read twice (Reader) are not
// split: the 413 is reported. The status says the server processed none of
// the request, so the repeat cannot duplicate an instance.
//
// An instance whose payload cannot be produced (a *StorePayloadError: Open
// or its reader failed) cuts its request before the server answers. When
// every payload of that request can be produced again, the instance is
// reported alone with that error, in its own StoreBatch, and the other
// instances of the request are sent again without it, so one unreadable
// object fails alone. A server that stored parts of the cut request before it
// broke off receives those parts again.
//
// onBatch receives every finished request, the refused 413 request and a
// request cut by a payload excepted when they are sent again. A non-nil
// return stops the upload and is returned; the remaining instances are not
// sent. Errors of a request are reported only to onBatch.
func (c Client) StoreInstancesBatched(ctx context.Context, instances []StoreInstance, limits StoreBatchLimits, onBatch func(StoreBatch) error) error {
	upload := storeBatchUpload{client: c, instances: instances, limits: limits.normalized(), onBatch: onBatch}
	for start := 0; start < len(instances); {
		end := nextStoreBatchEnd(instances, start, upload.limits)
		if err := upload.send(ctx, start, end, false); err != nil {
			return err
		}
		start = end
	}
	return nil
}

type storeBatchUpload struct {
	client    Client
	instances []StoreInstance
	limits    StoreBatchLimits
	onBatch   func(StoreBatch) error
}

func (u *storeBatchUpload) report(batch StoreBatch) error {
	if u.onBatch == nil {
		return nil
	}
	return u.onBatch(batch)
}

// send stores instances[start:end] with one request, or more when the server
// refuses it with 413 or a payload cuts it.
func (u *storeBatchUpload) send(ctx context.Context, start, end int, reduced bool) error {
	for start < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		requested := u.instances[start:end]
		result, err := u.client.StoreInstances(ctx, requested)
		var payloadErr *StorePayloadError
		// A payload stopped by the canceled upload is not the object's fault.
		if ctx.Err() == nil && errors.As(err, &payloadErr) && payloadErr.Index >= 0 && payloadErr.Index < len(requested) &&
			storeInstancesReplayable(requested[:payloadErr.Index]) && storeInstancesReplayable(requested[payloadErr.Index+1:]) {
			failed := start + payloadErr.Index
			if failed > start {
				if stop := u.send(ctx, start, failed, reduced); stop != nil {
					return stop
				}
			}
			if stop := u.report(StoreBatch{Start: failed, End: failed + 1, Err: err, Reduced: reduced}); stop != nil {
				return stop
			}
			start = failed + 1
			continue
		}
		if reduced || !storeRefusedAsTooLarge(err) || end-start < 2 || !storeInstancesReplayable(requested) {
			return u.report(StoreBatch{Start: start, End: end, Result: result, Err: err, Reduced: reduced})
		}
		middle := start + (end-start+1)/2
		u.limits = reducedStoreBatchLimits(u.instances[start:middle], u.instances[middle:end], u.limits)
		if stop := u.send(ctx, start, middle, true); stop != nil {
			return stop
		}
		return u.send(ctx, middle, end, true)
	}
	return nil
}

// PlanStoreBatches returns the end index of every request StoreInstancesBatched
// would make without a 413, for progress reporting and tests.
func PlanStoreBatches(instances []StoreInstance, limits StoreBatchLimits) []int {
	limits = limits.normalized()
	var ends []int
	for start := 0; start < len(instances); {
		start = nextStoreBatchEnd(instances, start, limits)
		ends = append(ends, start)
	}
	return ends
}

func nextStoreBatchEnd(instances []StoreInstance, start int, limits StoreBatchLimits) int {
	end := start
	var bytes int64
	for end < len(instances) && end-start < limits.MaxInstances {
		size := storeInstanceSize(instances[end])
		if end > start && size > limits.MaxBytes-bytes {
			break
		}
		bytes += size
		end++
	}
	return end
}

func storeInstanceSize(instance StoreInstance) int64 {
	if instance.Size > 0 {
		return instance.Size
	}
	if instance.Open == nil && instance.Reader == nil {
		return int64(len(instance.Data))
	}
	return 0
}

// reducedStoreBatchLimits bounds later batches by the larger half of a
// refused batch: its instance count and its known bytes.
func reducedStoreBatchLimits(first, second []StoreInstance, limits StoreBatchLimits) StoreBatchLimits {
	limits.MaxInstances = max(1, min(limits.MaxInstances, max(len(first), len(second))))
	bytes := max(storeBatchBytes(first), storeBatchBytes(second))
	if bytes > 0 {
		limits.MaxBytes = min(limits.MaxBytes, bytes)
	}
	return limits
}

func storeBatchBytes(instances []StoreInstance) int64 {
	var bytes int64
	for _, instance := range instances {
		bytes += storeInstanceSize(instance)
	}
	return bytes
}

func storeRefusedAsTooLarge(err error) bool {
	var webErr *Error
	return errors.As(err, &webErr) && webErr.Kind == ErrorKindHTTPStatus && webErr.StatusCode == http.StatusRequestEntityTooLarge
}

// storeInstancesReplayable reports whether every payload can be produced a
// second time.
func storeInstancesReplayable(instances []StoreInstance) bool {
	for _, instance := range instances {
		if instance.Open == nil && (instance.Reader != nil || len(instance.Data) == 0) {
			return false
		}
	}
	return true
}
