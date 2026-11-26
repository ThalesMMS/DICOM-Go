package storetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PreparedSource retains one verified representation only in its owning
// PreparedSession. Unlike ordinary Source, it must be closed with the session.
// Byte reuse grants no delivery/retry authority; StoreSession retains that role.
type PreparedSource struct {
	mu                                       sync.Mutex
	session                                  *PreparedSession
	source                                   *Source
	binding                                  *PreparedBinding
	token                                    [32]byte
	policy                                   [32]byte
	ctx                                      context.Context
	cancel                                   context.CancelFunc
	gate                                     chan struct{}
	cache                                    *prepared
	report                                   Report
	seals                                    map[string]preparedSpoolSeal
	retained                                 int64
	handles                                  map[*preparedHandle]struct{}
	operations                               int
	closing                                  bool
	cleanupAttempt                           *preparedCleanupAttempt
	removePrepared                           func(*prepared) error
	revalidationNanos, spoolNanos, copyNanos atomic.Int64
	failures                                 []preparedFailedSpool
}

func (s *PreparedSession) NewSource(base dimse.StoreSource, opts Options, binding *PreparedBinding) (*PreparedSource, error) {
	if s == nil || binding == nil || opts.Target.UID == "" {
		return nil, ErrOptions
	}
	// Opaque mutable registries, dictionaries and transformation callbacks cannot
	// truthfully be fingerprinted. Supply a privately constructed built-in binding.
	if opts.Transcode.EncoderRegistry != nil || opts.Transcode.DecoderRegistry != nil || opts.Dictionary != nil || opts.Transcode.CodestreamTranscoder != nil || opts.Transcode.Rewrite != nil || opts.Transcode.RecoverUN != nil {
		return nil, ErrPreparedBinding
	}
	token, err := binding.acquire(s.ctx)
	if err != nil {
		return nil, err
	}
	opts.Transcode.DecoderRegistry = binding.decoders
	opts.Transcode.EncoderRegistry = binding.encoders
	source, err := NewSource(base, opts)
	if err != nil {
		return nil, err
	}
	policy, err := preparedPolicyDigest(source.opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	p := &PreparedSource{session: s, source: source, binding: binding, token: token, policy: policy, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), handles: map[*preparedHandle]struct{}{}, cleanupAttempt: &preparedCleanupAttempt{done: make(chan struct{})}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		cancel()
		return nil, ErrPreparedClosed
	}
	if len(s.sources) >= s.options.MaxEntries {
		cancel()
		return nil, ErrResourceLimit
	}
	s.sources[p] = struct{}{}
	s.stats.Entries++
	return p, nil
}
func (p *PreparedSource) begin(ctx context.Context) (context.Context, func(), error) {
	if p == nil {
		return nil, nil, ErrOptions
	}
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if p.closing || p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil, nil, ErrPreparedClosed
	}
	p.operations++
	p.mu.Unlock()
	operation, cancel := context.WithCancel(p.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return operation, func() {
		stop()
		cancel()
		p.mu.Lock()
		p.operations--
		p.mu.Unlock()
		p.cleanup()
	}, nil
}

// A result belongs to one cleanup attempt and is immutable after done closes.
// This lets a later Close retry without changing results observed by old waiters.
type preparedCleanupAttempt struct {
	done              chan struct{}
	err               error
	started, finished bool // guarded by owner.mu
}

func (p *PreparedSource) closeAttempt() *preparedCleanupAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.cleanupAttempt
	if a.finished && a.err != nil && p.operations == 0 && len(p.handles) == 0 {
		a = &preparedCleanupAttempt{done: make(chan struct{})}
		p.cleanupAttempt = a
	}
	return a
}
func (p *PreparedSource) removeSpool(spool *prepared) error {
	if p.removePrepared != nil {
		return p.removePrepared(spool)
	}
	return spool.close()
}
func (p *PreparedSource) cleanup() {
	p.mu.Lock()
	a := p.cleanupAttempt
	if !p.closing || p.operations != 0 || len(p.handles) != 0 || a.started {
		p.mu.Unlock()
		return
	}
	a.started = true
	cache, retained := p.cache, p.retained
	failures := append([]preparedFailedSpool(nil), p.failures...)
	p.mu.Unlock()
	var err error
	cacheRemoved := cache == nil
	if cache != nil {
		cleanupErr := p.removeSpool(cache)
		err = errors.Join(err, cleanupErr)
		if cleanupErr == nil {
			p.session.releaseRetained(retained)
			cacheRemoved = true
		}
	}
	remaining := make([]preparedFailedSpool, 0, len(failures))
	for _, failure := range failures {
		cleanupErr := p.removeSpool(failure.spool)
		if cleanupErr == nil {
			p.session.releaseReserved(failure.reserved)
		} else {
			remaining = append(remaining, failure)
		}
		err = errors.Join(err, cleanupErr)
	}
	p.mu.Lock()
	if cacheRemoved {
		p.cache = nil
		p.retained = 0
	}
	p.failures = remaining
	p.mu.Unlock()
	if cacheRemoved && len(remaining) == 0 {
		p.session.releaseEntry(p)
	}
	p.mu.Lock()
	a.err = err
	a.finished = true
	close(a.done)
	p.mu.Unlock()
}
func awaitPreparedCleanup(ctx context.Context, a *preparedCleanupAttempt) error {
	select {
	case <-a.done:
		if a.err != nil {
			return &Error{Cause: a.err}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *PreparedSource) requestClose() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closing = true
	handles := make([]*preparedHandle, 0, len(p.handles))
	for h := range p.handles {
		handles = append(handles, h)
	}
	p.mu.Unlock()
	p.cancel()
	for _, h := range handles {
		h.requestClose()
	}
	p.cleanup()
}
func (p *PreparedSource) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	ctx = contextOrBackground(ctx)
	a := p.closeAttempt()
	p.requestClose()
	return awaitPreparedCleanup(ctx, a)
}
func preparedDescriptor(d dimse.StoreDescriptor) dimse.StoreDescriptor {
	d = cloneDescriptor(d)
	if d.TransferSyntaxSizes != nil {
		m := make(map[string]int64, len(d.TransferSyntaxSizes))
		for k, v := range d.TransferSyntaxSizes {
			m[k] = v
		}
		d.TransferSyntaxSizes = m
	}
	return d
}
func preparedSameDescriptor(a, b dimse.StoreDescriptor) bool {
	if !sameDescriptor(a, b) || a.Origin != b.Origin || len(a.TransferSyntaxSizes) != len(b.TransferSyntaxSizes) {
		return false
	}
	for k, v := range a.TransferSyntaxSizes {
		if b.TransferSyntaxSizes[k] != v {
			return false
		}
	}
	return true
}
func (p *PreparedSource) checkBinding(ctx context.Context) error {
	policy, err := preparedPolicyDigest(p.source.opts)
	if err != nil || policy != p.policy {
		return ErrPreparedBinding
	}
	token, err := p.binding.acquire(ctx)
	if err != nil {
		return err
	}
	if token != p.token {
		return ErrPreparedBinding
	}
	syntax, ok := transfer.DefaultRegistry.Get(p.source.opts.Target.UID)
	if !ok || syntax != p.source.opts.Target {
		return ErrPreparedBinding
	}
	return nil
}
func (p *PreparedSource) ensure(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := p.checkBinding(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	cache := p.cache
	p.mu.Unlock()
	if cache != nil {
		return nil
	}
	reserved, err := p.session.reserve(p.source.opts.MaxSpoolBytes)
	if err != nil {
		return err
	}
	opts := p.source.opts
	opts.MaxSpoolBytes = reserved
	var partial *prepared
	staging := &Source{base: p.source.base, opts: opts, preparedTracker: func(q *prepared) { q.plan.input = preparedDescriptor(q.plan.input); partial = q }}
	cache, report, err := staging.prepare(ctx, nil)
	if err != nil {
		return p.failPreparation(partial, reserved, err)
	}
	retained, err := preparedDirectoryBytes(cache.dir)
	if err != nil || retained > reserved {
		return p.failPreparation(cache, reserved, errors.Join(err, ErrResourceLimit))
	}
	seals, sealErr := sealPreparedSpools(ctx, cache)
	if sealErr != nil {
		return p.failPreparation(cache, reserved, sealErr)
	}
	p.session.publish(reserved, retained)
	p.mu.Lock()
	p.cache = cache
	p.report = report
	p.seals = seals
	p.retained = retained
	p.mu.Unlock()
	return nil
}
func preparedDirectoryBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return dimse.ErrStoreInvalidSource
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > int64(^uint64(0)>>1)-total {
			return ErrResourceLimit
		}
		total += info.Size()
		return nil
	})
	return total, err
}
func (p *PreparedSource) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	ctx, end, err := p.begin(ctx)
	if err != nil {
		return dimse.StoreDescriptor{}, err
	}
	defer end()
	if err := p.ensure(ctx); err != nil {
		return dimse.StoreDescriptor{}, err
	}
	// Even cached planning validates current origin and complete bytes. An old
	// descriptor is never newly advertised after policy or input mutation.
	if err := p.validateOriginal(ctx); err != nil {
		return dimse.StoreDescriptor{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return preparedDescriptor(p.cache.plan.output), nil
}
func (p *PreparedSource) validateOriginal(ctx context.Context) (err error) {
	started := time.Now()
	defer func() { p.revalidationNanos.Add(int64(time.Since(started))) }()
	if err = p.checkBinding(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	plan := p.cache.plan
	p.mu.Unlock()
	input, err := p.source.base.Inspect(ctx)
	if err != nil {
		return &Error{Cause: err}
	}
	if !preparedSameDescriptor(plan.input, input) {
		return dimse.ErrStoreSourceChanged
	}
	opened, err := p.source.base.Open(ctx)
	if err != nil {
		return &Error{Cause: err}
	}
	if opened.Close == nil || opened.WriteDataSet == nil {
		if opened.Close != nil {
			_ = opened.Close()
		}
		return dimse.ErrStoreInvalidSource
	}
	defer func() {
		closeErr := opened.Close()
		if closeErr != nil {
			err = errors.Join(err, &Error{Cause: closeErr})
		}
	}()
	if !preparedSameDescriptor(plan.input, opened.Descriptor) {
		return dimse.ErrStoreSourceChanged
	}
	hash := sha256.New()
	writer := &spoolWriter{ctx: ctx, w: hash, budget: &spoolBudget{remaining: p.source.opts.Transcode.Limits.MaxInputBytes}, remaining: p.source.opts.Transcode.Limits.MaxInputBytes}
	original, ok := transfer.DefaultRegistry.Get(plan.input.TransferSyntaxUID)
	if !ok {
		return dimse.ErrStoreTransferSyntax
	}
	if err := errors.Join(opened.WriteDataSet(ctx, writer, original), writer.err); err != nil {
		return &Error{Cause: err}
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if digest != plan.digest {
		return dimse.ErrStoreSourceChanged
	}
	return p.checkBinding(ctx)
}
func (p *PreparedSource) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	ctx, end, err := p.begin(ctx)
	if err != nil {
		return dimse.OpenedStoreSource{}, err
	}
	defer end()
	if err = p.ensure(ctx); err != nil {
		return dimse.OpenedStoreSource{}, err
	}
	if err = p.validateOriginal(ctx); err != nil {
		return dimse.OpenedStoreSource{}, err
	}
	p.mu.Lock()
	cache := p.cache
	p.mu.Unlock()
	h := &preparedHandle{owner: p, files: map[string]*os.File{}, offsets: map[string]int64{}, done: make(chan struct{}), gate: make(chan struct{}, 1)}
	for uid, path := range cache.paths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			h.closeFiles()
			return dimse.OpenedStoreSource{}, dimse.ErrStoreSourceChanged
		}
		f, err := os.Open(path)
		if err != nil {
			h.closeFiles()
			return dimse.OpenedStoreSource{}, &Error{Cause: err}
		}
		actual, err := f.Stat()
		if err != nil || !os.SameFile(info, actual) {
			_ = f.Close()
			h.closeFiles()
			return dimse.OpenedStoreSource{}, dimse.ErrStoreSourceChanged
		}
		// The prepare-time output digest is bound separately below; each attempt
		// reads the exact opened inode rather than reopening a path during sending.
		h.files[uid] = f
		h.offsets[uid] = cache.offsets[uid]
		if err := p.validateSpool(ctx, uid, f); err != nil {
			h.closeFiles()
			return dimse.OpenedStoreSource{}, err
		}
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		h.closeFiles()
		return dimse.OpenedStoreSource{}, ErrPreparedClosed
	}
	p.handles[h] = struct{}{}
	descriptor := preparedDescriptor(cache.plan.output)
	p.mu.Unlock()
	return dimse.OpenedStoreSource{Descriptor: descriptor, WriteDataSet: h.write, Close: h.close}, nil
}

type preparedSpoolSeal struct {
	digest        [32]byte
	bytes, offset int64
}

func sealPreparedSpools(ctx context.Context, cache *prepared) (map[string]preparedSpoolSeal, error) {
	seals := make(map[string]preparedSpoolSeal, len(cache.paths))
	for uid, path := range cache.paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		n, copyErr := copyContext(ctx, hash, f)
		err = errors.Join(copyErr, f.Close())
		if err != nil {
			return nil, err
		}
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		seals[uid] = preparedSpoolSeal{digest, n, cache.offsets[uid]}
	}
	return seals, nil
}
func (p *PreparedSource) validateSpool(ctx context.Context, uid string, f *os.File) error {
	started := time.Now()
	defer func() { p.spoolNanos.Add(int64(time.Since(started))) }()
	p.mu.Lock()
	seal, ok := p.seals[uid]
	p.mu.Unlock()
	if !ok {
		return dimse.ErrStoreSourceChanged
	}
	hash := sha256.New()
	n, err := copyContext(ctx, hash, io.LimitReader(f, seal.bytes+1))
	if err != nil {
		return &Error{Cause: err}
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if n != seal.bytes || digest != seal.digest {
		return dimse.ErrStoreSourceChanged
	}
	_, err = f.Seek(seal.offset, io.SeekStart)
	if err != nil {
		return &Error{Cause: err}
	}
	return nil
}

type preparedHandle struct {
	mu                       sync.Mutex
	owner                    *PreparedSource
	files                    map[string]*os.File
	offsets                  map[string]int64
	gate                     chan struct{}
	closed, active, released bool
	cancel                   context.CancelFunc
	closeErr                 error
	done                     chan struct{}
}

func (h *preparedHandle) closeFiles() error {
	var err error
	for _, f := range h.files {
		err = errors.Join(err, f.Close())
	}
	return err
}
func (h *preparedHandle) release() {
	h.mu.Lock()
	if h.released || h.active {
		h.mu.Unlock()
		return
	}
	h.released = true
	h.mu.Unlock()
	err := h.closeFiles()
	h.mu.Lock()
	h.closeErr = err
	close(h.done)
	h.mu.Unlock()
	h.owner.mu.Lock()
	delete(h.owner.handles, h)
	h.owner.mu.Unlock()
	h.owner.cleanup()
}
func (h *preparedHandle) requestClose() {
	h.mu.Lock()
	h.closed = true
	active := h.active
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !active {
		h.release()
	}
}
func (h *preparedHandle) close() error {
	h.requestClose()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closeErr != nil {
		return &Error{Cause: h.closeErr}
	}
	return nil
}
func (h *preparedHandle) write(ctx context.Context, dst io.Writer, syntax transfer.Syntax) error {
	ctx, end, err := h.owner.begin(ctx)
	if err != nil {
		return err
	}
	defer end()
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrPreparedClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.active = true
	h.mu.Unlock()
	defer cancel()
	defer func() {
		h.mu.Lock()
		h.active = false
		h.cancel = nil
		closed := h.closed
		h.mu.Unlock()
		if closed {
			h.release()
		}
	}()
	uid := transfer.NormalizeUID(syntax.UID)
	f, ok := h.files[uid]
	if !ok {
		return dimse.ErrStoreTransferSyntax
	}
	if _, err := f.Seek(h.offsets[uid], io.SeekStart); err != nil {
		return &Error{Cause: err}
	}
	h.owner.mu.Lock()
	h.owner.report.SelectedTransferSyntaxUID = uid
	h.owner.report.Negotiation = "selected"
	h.owner.mu.Unlock()
	started := time.Now()
	_, err = copyContext(ctx, dst, f)
	h.owner.copyNanos.Add(int64(time.Since(started)))
	if err != nil {
		return &Error{Cause: err}
	}
	return nil
}

// Report returns detached observation; it never authorizes replay or delivery.
func (p *PreparedSource) Report() Report {
	if p == nil {
		return Report{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.report
	r.Candidates = append([]Candidate(nil), r.Candidates...)
	if r.LossyVerification != nil {
		v := *r.LossyVerification
		r.LossyVerification = &v
	}
	if r.VerificationFailure != nil {
		v := *r.VerificationFailure
		r.VerificationFailure = &v
	}
	return r
}
func (p *PreparedSource) RecordResult(result dimse.StoreItemResult) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.report.SelectedTransferSyntaxUID = result.NegotiatedTransferSyntaxUID
	p.report.Negotiation = string(result.Outcome)
	p.mu.Unlock()
}

type preparedFailedSpool struct {
	spool    *prepared
	reserved int64
}

func (p *PreparedSource) failPreparation(spool *prepared, reserved int64, cause error) error {
	var cleanupErr error
	if spool != nil {
		cleanupErr = spool.close()
	}
	if cleanupErr == nil {
		p.session.releaseReserved(reserved)
	} else {
		p.mu.Lock()
		p.failures = append(p.failures, preparedFailedSpool{spool, reserved})
		p.mu.Unlock()
	}
	return &Error{Cause: errors.Join(cause, cleanupErr)}
}

func preparedPolicyDigest(opts Options) ([32]byte, error) {
	opts.Transcode.DecoderRegistry = nil
	opts.Transcode.EncoderRegistry = nil
	value, err := preparedPolicyValue(reflect.ValueOf(opts))
	if err != nil {
		return [32]byte{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, ErrPreparedBinding
	}
	return sha256.Sum256(raw), nil
}

// Only detached value configuration is accepted. An arbitrary callback is not
// fingerprinted by its address and cannot be a prepared representation policy.
func preparedPolicyValue(v reflect.Value) (any, error) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil, nil
		}
		return preparedPolicyValue(v.Elem())
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if v.IsNil() {
			return nil, nil
		}
		return nil, ErrPreparedBinding
	case reflect.Struct:
		m := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			value, err := preparedPolicyValue(v.Field(i))
			if err != nil {
				return nil, err
			}
			m[field.Name] = value
		}
		return m, nil
	case reflect.Slice, reflect.Array:
		a := make([]any, v.Len())
		for i := range a {
			value, err := preparedPolicyValue(v.Index(i))
			if err != nil {
				return nil, err
			}
			a[i] = value
		}
		return a, nil
	case reflect.Map:
		if v.IsNil() {
			return nil, nil
		}
		return nil, ErrPreparedBinding
	default:
		if !v.CanInterface() {
			return nil, ErrPreparedBinding
		}
		return v.Interface(), nil
	}
}

// PreparedSourceTiming records actual work phases, not delivery confirmation.
// Copy includes destination blocking; durations across concurrent calls sum.
type PreparedSourceTiming struct{ OriginalValidation, SpoolIntegrity, Copy time.Duration }

func (p *PreparedSource) Timing() PreparedSourceTiming {
	if p == nil {
		return PreparedSourceTiming{}
	}
	return PreparedSourceTiming{time.Duration(p.revalidationNanos.Load()), time.Duration(p.spoolNanos.Load()), time.Duration(p.copyNanos.Load())}
}
