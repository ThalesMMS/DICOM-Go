package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"io"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errStorageEventLimit = errors.New("storescp: storage event capacity exceeded")

const storageHookReservationBytes = int64(32768 + 2048)

type storageHookEvent struct {
	argv  []string
	study bool
}
type storageHookMetrics struct {
	Completed, Failed, Cancelled uint64
	Slots                        int
	Bytes                        int64
}
type storescpStorageRuntime struct {
	policy       *storageLayoutPolicy
	root         string
	tracker      *dimse.StudyInactivityTracker
	mu           sync.Mutex
	closed       bool
	slots        int
	bytes        int64
	queue        []storageHookEvent
	future       map[uint64]bool
	metrics      storageHookMetrics
	ctx          context.Context
	cancel       context.CancelFunc
	wake         chan struct{}
	done         chan struct{}
	pollStop     chan struct{}
	pollDone     chan struct{}
	closeOnce    sync.Once
	now          func() time.Time
	execute      func(context.Context, []string, string) error
	manualPoll   bool
	pollTicks    <-chan time.Time
	pollObserved func()
}
type storagePrepared struct {
	runtime  *storescpStorageRuntime
	relative string
	fields   storageEventFields
	study    *dimse.StudyActivityReservation
	receipt  bool
	done     bool
}
type storageSerialWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *storageSerialWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

type storageRuntimeDependencies struct {
	now          func() time.Time
	execute      func(context.Context, []string, string) error
	manualPoll   bool
	pollTicks    <-chan time.Time
	pollObserved func()
}

func newStorescpStorageRuntime(policy *storageLayoutPolicy, root string) (*storescpStorageRuntime, error) {
	return newStorescpStorageRuntimeWith(policy, root, storageRuntimeDependencies{})
}
func newStorescpStorageRuntimeWith(policy *storageLayoutPolicy, root string, deps storageRuntimeDependencies) (*storescpStorageRuntime, error) {
	if policy == nil {
		return nil, nil
	}
	root, err := storageAbsoluteRoot(root)
	if err != nil {
		return nil, err
	}
	dir, err := nofollow.OpenDirectory(root)
	if err != nil {
		return nil, errStorageFeatureUsage
	}
	if err := dir.Close(); err != nil {
		return nil, errStorageFeatureUsage
	}
	r := &storescpStorageRuntime{policy: policy, root: root, future: make(map[uint64]bool), wake: make(chan struct{}, 1), done: make(chan struct{}), pollStop: make(chan struct{}), pollDone: make(chan struct{}), now: time.Now, execute: runStorageHook}
	if deps.now != nil {
		r.now = deps.now
	}
	if deps.execute != nil {
		r.execute = deps.execute
	}
	r.manualPoll = deps.manualPoll
	r.pollTicks = deps.pollTicks
	r.pollObserved = deps.pollObserved
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for _, h := range []*storageHookSpec{policy.reception, policy.endStudy} {
		if err := h.validateExecutable(); err != nil {
			r.cancel()
			return nil, err
		}
	}
	if policy.reception != nil || policy.endStudy != nil {
		if err := storageHookPlatformReady(); err != nil {
			r.cancel()
			return nil, err
		}
	}
	if policy.endStudy != nil {
		r.tracker, err = dimse.NewStudyInactivityTracker(dimse.StudyInactivityOptions{Timeout: policy.flags.inactivity, MaxStudies: policy.flags.studies, MaxBytes: policy.flags.studyBytes})
		if err != nil {
			r.cancel()
			return nil, err
		}
	}
	if r.tracker != nil && !r.manualPoll {
		go r.poller()
	} else {
		close(r.pollDone)
	}
	go r.worker()
	return r, nil
}
func (r *storescpStorageRuntime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *storescpStorageRuntime) reserve(n int) error {
	if r.closed || r.ctx.Err() != nil || n > r.policy.flags.capacity-r.slots || int64(n)*storageHookReservationBytes > r.policy.flags.hookBytes-r.bytes {
		return errStorageEventLimit
	}
	r.slots += n
	r.bytes += int64(n) * storageHookReservationBytes
	return nil
}
func (r *storescpStorageRuntime) release(n int) {
	r.slots -= n
	r.bytes -= int64(n) * storageHookReservationBytes
}

func (r *storescpStorageRuntime) prepare(ctx context.Context, dataset *object.Object, assoc *ul.Association, req dimse.CStoreRequest) (*storagePrepared, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f := storageEventFields{called: assoc.CalledAETitle, calling: assoc.CallingAETitle, class: req.AffectedSOPClassUID, instance: req.AffectedSOPInstanceUID}
	if r.policy.sortStudy || r.policy.reception.needsStudy() || r.policy.endStudy != nil {
		var err error
		f.study, err = storageStudyUID(dataset)
		if err != nil {
			return nil, err
		}
	}
	receipt := r.policy.reception != nil
	n := 0
	if receipt {
		n = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.reserve(n); err != nil {
		return nil, err
	}
	p := &storagePrepared{runtime: r, fields: f, receipt: receipt}
	rollback := func(err error) (*storagePrepared, error) {
		if p.study != nil {
			p.study.Abort()
		}
		r.release(n)
		return nil, err
	}
	if r.tracker != nil {
		var err error
		p.study, err = r.tracker.Reserve(ctx, f.called, f.study)
		if err != nil {
			return rollback(err)
		}
		if p.study.NewGeneration() {
			if err := r.reserve(1); err != nil {
				return rollback(err)
			}
			n++
			r.future[p.study.Generation()] = true
		}
	}
	// The only work under this lock is bounded state admission. Formatting and
	// random generation are performed after returning from this helper's lock.
	return p, nil
}

func (p *storagePrepared) format(ctx context.Context, dataset *object.Object) error {
	r := p.runtime
	relative, err := r.policy.format(ctx, dataset, p.fields.instance, p.fields.study, r.now())
	if err != nil {
		p.abort()
		return err
	}
	if len(relative) > 4092 || len(filepath.Base(relative)) > 251 {
		p.abort()
		return errStorageLayout
	}
	p.relative = relative
	// Preflight both templates for every possible collision before publication.
	directory, base := filepath.Split(relative)
	extension := filepath.Ext(base)
	worst := directory + strings.TrimSuffix(base, extension) + ".999" + extension
	fields := p.fields
	fields.path = filepath.Join(r.root, worst)
	fields.count = math.MaxUint64
	fields.generation = math.MaxUint64
	if len(fields.path) > 8192 {
		p.abort()
		return errStorageLayout
	}
	for _, h := range []*storageHookSpec{r.policy.reception, r.policy.endStudy} {
		if _, err := h.expand(fields, false); err != nil {
			p.abort()
			return errStorageLayout
		}
	}
	return nil
}
func (p *storagePrepared) abort() {
	if p == nil {
		return
	}
	r := p.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	if p.receipt {
		r.release(1)
	}
	if p.study != nil {
		p.study.Abort()
		if !p.study.GenerationActive() && r.future[p.study.Generation()] {
			delete(r.future, p.study.Generation())
			r.release(1)
		}
	}
	r.signal()
}
func (p *storagePrepared) stored(path string) {
	if p == nil {
		return
	}
	r := p.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	if p.study != nil {
		if err := p.study.Commit(path, r.now()); err != nil {
			p.study.Abort()
			if !p.study.GenerationActive() && r.future[p.study.Generation()] {
				delete(r.future, p.study.Generation())
				r.release(1)
			}
		}
	}
	if p.receipt {
		if r.closed {
			r.release(1)
			r.metrics.Cancelled++
		} else {
			fields := p.fields
			fields.path = path
			argv, err := r.policy.reception.expand(fields, true)
			if err != nil {
				r.release(1)
				r.metrics.Failed++
			} else {
				r.queue = append(r.queue, storageHookEvent{argv: argv})
			}
		}
	}
	r.signal()
}
func (r *storescpStorageRuntime) poll(now time.Time) {
	if r.tracker == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	events, err := r.tracker.Poll(r.ctx, now)
	if err != nil {
		return
	}
	for _, e := range events {
		if !r.future[e.Generation] {
			continue
		}
		delete(r.future, e.Generation)
		f := storageEventFields{path: e.LastPath, called: e.CalledAETitle, study: e.StudyInstanceUID, generation: e.Generation, count: e.Count}
		argv, err := r.policy.endStudy.expand(f, true)
		if err != nil {
			r.release(1)
			r.metrics.Failed++
		} else {
			r.queue = append(r.queue, storageHookEvent{argv: argv, study: true})
		}
	}
	r.signal()
}
func (r *storescpStorageRuntime) poller() {
	defer close(r.pollDone)
	interval := 250 * time.Millisecond
	if r.policy.flags.inactivity/10 < interval {
		interval = r.policy.flags.inactivity / 10
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	poll := r.pollTicks
	if poll == nil {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		poll = ticker.C
	}
	for {
		select {
		case <-r.pollStop:
			return
		case <-r.ctx.Done():
			return
		case now, ok := <-poll:
			if !ok {
				return
			}
			r.poll(now)
			if r.pollObserved != nil {
				r.pollObserved()
			}
		}
	}
}
func (r *storescpStorageRuntime) worker() {
	// done closes after both goroutines finish. Successful Close waits for it;
	// deadline errors retain the bounded fallback below.
	defer func() { <-r.pollDone; close(r.done) }()
	for {
		r.mu.Lock()
		if len(r.queue) > 0 {
			event := r.queue[0]
			r.queue[0] = storageHookEvent{}
			r.queue = r.queue[1:]
			r.mu.Unlock()
			err := r.ctx.Err()
			if err == nil {
				ctx, cancel := context.WithTimeout(r.ctx, r.policy.flags.hookTimeout)
				err = r.execute(ctx, event.argv, r.root)
				cancel()
			}
			r.mu.Lock()
			r.release(1)
			if errors.Is(err, context.Canceled) {
				r.metrics.Cancelled++
			} else if err != nil {
				r.metrics.Failed++
			} else {
				r.metrics.Completed++
			}
			r.mu.Unlock()
			continue
		}
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-r.wake:
		case <-r.ctx.Done():
		}
	}
}
func (r *storescpStorageRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.metrics.Cancelled += uint64(len(r.future))
		r.release(len(r.future))
		r.future = make(map[uint64]bool)
		r.mu.Unlock()
		close(r.pollStop)
		r.signal()
	})
	select {
	case <-r.done:
		r.cancel()
		return nil
	case <-ctx.Done():
		r.cancel()
		r.signal()
		select {
		case <-r.done:
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return ctx.Err()
		}
	}
}
func (r *storescpStorageRuntime) Metrics() storageHookMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.metrics
	m.Slots = r.slots
	m.Bytes = r.bytes
	return m
}
func storageFeatureErrorStatus(err error) uint16 {
	if errors.Is(err, errStorageLayout) {
		return statusCannotUnderstand
	}
	return statusOutOfResources
}
func storageRuntimeSummary(w io.Writer, r *storescpStorageRuntime) {
	if r == nil {
		return
	}
	m := r.Metrics()
	if r.policy.reception != nil || r.policy.endStudy != nil {
		_, _ = fmt.Fprintf(w, "storescp hooks completed=%d failed=%d cancelled=%d\n", m.Completed, m.Failed, m.Cancelled)
	}
}
