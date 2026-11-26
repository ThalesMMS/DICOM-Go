package audit

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

type durableWorker struct {
	id        string
	logger    *DurableLogger
	transport SyslogLogger
	gate      chan struct{}
	wake      chan struct{}
	mu        sync.Mutex
	conn      net.Conn
}

func newDurableWorker(d AuditDestination, l *DurableLogger) *durableWorker {
	return &durableWorker{id: d.ID, logger: l, transport: SyslogLogger{Network: d.Network, Address: d.Address, TLS: d.TLS, Dial: d.Dial}, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
}
func (w *durableWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *durableWorker) closeConn() {
	w.mu.Lock()
	conn := w.conn
	w.conn = nil
	w.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}
func (w *durableWorker) connection(ctx context.Context) (net.Conn, error) {
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	conn, err := w.transport.dial(ctx)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	// Close uses this same publication mutex. Dials completing after the
	// shared lifecycle epoch is canceled can never install a live socket.
	if ctx.Err() != nil || w.logger.lifecycleCtx.Err() != nil {
		w.mu.Unlock()
		conn.Close()
		return nil, ErrSpoolClosed
	}
	w.conn = conn
	w.mu.Unlock()
	return conn, nil
}
func targets(r recordMeta, id string) bool {
	for _, target := range r.Targets {
		if target == id {
			return true
		}
	}
	return false
}
func (w *durableWorker) flush(ctx context.Context) error {
	l := w.logger
	l.mu.Lock()
	if err := l.usable(); err != nil {
		l.mu.Unlock()
		return err
	}
	l.active.Add(1)
	l.mu.Unlock()
	defer l.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.lifecycleCtx, cancel)
	defer stop()
	defer cancel()
	if l.lifecycleCtx.Err() != nil {
		cancel()
	}
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.gate }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		if err := l.usable(); err != nil {
			l.mu.Unlock()
			return err
		}
		if err := l.expireLocked(); err != nil {
			l.mu.Unlock()
			return err
		}
		var meta recordMeta
		found := false
		for _, r := range l.state.Records {
			if targets(r, w.id) && l.state.Destinations[w.id].Cursor < r.Sequence {
				meta = r
				found = true
				break
			}
		}
		if !found {
			l.mu.Unlock()
			return nil
		}
		record, err := l.readRecord(meta)
		if err != nil {
			l.corruptions++
			l.poisoned = true
			l.mu.Unlock()
			return spoolError("record_integrity", ErrSpoolCorrupt)
		}
		l.mu.Unlock()
		sendCtx, cancel := context.WithTimeout(ctx, l.options.SendTimeout)
		conn, err := w.connection(sendCtx)
		if err == nil {
			if err = l.fault("send"); err == nil {
				err = w.transport.send(sendCtx, conn, record)
			}
		}
		cancel()
		if err != nil {
			w.closeConn()
			l.mu.Lock()
			next := l.cloneState()
			next.Failed++
			if l.usable() == nil {
				if persistErr := l.commit(next); persistErr != nil {
					l.mu.Unlock()
					return persistErr
				}
			} else {
				l.state.Failed++
			}
			l.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return spoolError("transport", ErrDurableSpool)
		}
		l.mu.Lock()
		if err = l.usable(); err != nil {
			l.mu.Unlock()
			return err
		}
		if err = l.fault("checkpoint"); err != nil {
			err = l.diskFailure(err)
			l.mu.Unlock()
			return err
		}
		next := l.cloneState()
		d := next.Destinations[w.id]
		d.Cursor = max(d.Cursor, meta.Sequence)
		next.Destinations[w.id] = d
		next.Sent++
		err = l.commit(next)
		if err == nil {
			err = l.expireLocked()
		}
		l.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// Flush attempts all ACTIVE destinations independently. A failed repository
// never blocks another repository's checkpoint. It respects ctx cancellation.
func (l *DurableLogger) Flush(ctx context.Context) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	var wg sync.WaitGroup
	results := make(chan error, len(l.workers))
	for _, w := range l.workers {
		wg.Add(1)
		go func(w *durableWorker) { defer wg.Done(); results <- w.flush(ctx) }(w)
	}
	wg.Wait()
	close(results)
	var joined error
	for err := range results {
		joined = errors.Join(joined, err)
	}
	return joined
}

// Run has one sender/backoff per active destination. New admissions do not
// bypass a failed destination's backoff. Inactive backlog remains on disk.
func (l *DurableLogger) Run(ctx context.Context) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	l.runMu.Lock()
	if l.runDone != nil {
		l.runMu.Unlock()
		return spoolError("already_running", ErrDurableSpool)
	}
	l.mu.Lock()
	err := l.usable()
	l.mu.Unlock()
	if err != nil {
		l.runMu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	l.runCancel = cancel
	l.runDone = done
	l.runMu.Unlock()
	defer close(done)
	defer cancel()
	var wg sync.WaitGroup
	for _, worker := range l.workers {
		wg.Add(1)
		go func(w *durableWorker) {
			defer wg.Done()
			defer w.closeConn()
			wait := l.options.RetryInterval
			for {
				err := w.flush(ctx)
				if ctx.Err() != nil {
					return
				}
				delay := l.options.RetryInterval
				if err != nil {
					delay = wait
					wait = min(wait*2, l.options.MaxRetryInterval)
				} else {
					wait = l.options.RetryInterval
				}
				timer := time.NewTimer(delay)
				if err != nil {
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				} else {
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-w.wake:
						timer.Stop()
					case <-timer.C:
					}
				}
			}
		}(worker)
	}
	// Also expire paused backlog, including a spool with no active repository.
	ticker := time.NewTicker(l.options.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, w := range l.workers {
				w.closeConn()
			}
			wg.Wait()
			return ctx.Err()
		case <-ticker.C:
			l.mu.Lock()
			if l.usable() == nil {
				_ = l.expireLocked()
			}
			l.mu.Unlock()
		}
	}
}

// Close cancels senders and closes sockets. It NEVER flushes the network or
// deletes pending records. Pending durable records are available after reopen.
func (l *DurableLogger) Close() error {
	l.closeOnce.Do(func() { l.closeErr = l.closeInternal() })
	return l.closeErr
}
func (l *DurableLogger) closeInternal() error {
	l.lifecycleCancel()
	l.runMu.Lock()
	cancel, done := l.runCancel, l.runDone
	l.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, w := range l.workers {
		w.closeConn()
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	l.active.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock != nil {
		err := unlockSpool(l.lock)
		l.lock = nil
		if err != nil {
			return spoolError("unlock", ErrDurableSpool)
		}
	}
	return nil
}
