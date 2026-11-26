package storetranscode

import (
	"context"
	"errors"
	"sync"
)

var ErrPreparedClosed = errors.New("dicom: prepared representation session is closed")

// PreparedSessionOptions bounds the entire session, including all concurrent
// preparations, incremental staging and retained representations. No global
// cache or implicit unbounded mode is provided.
type PreparedSessionOptions struct {
	MaxRetainedBytes int64
	MaxEntries       int
}

// PreparedSessionStats separates conservative preparation capacity reservations
// from published physical spool bytes. Shared readers do not duplicate charges.
type PreparedSessionStats struct {
	ReservedBytes, RetainedBytes, PeakBytes int64
	Entries                                 int
	Closing, Closed                         bool
}

type PreparedSession struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	options PreparedSessionOptions
	sources map[*PreparedSource]struct{}
	stats   PreparedSessionStats
	closing bool
	stop    func() bool
}

func NewPreparedSession(ctx context.Context, options PreparedSessionOptions) (*PreparedSession, error) {
	if options.MaxRetainedBytes < 0 || options.MaxEntries < 0 {
		return nil, ErrOptions
	}
	if options.MaxRetainedBytes == 0 {
		options.MaxRetainedBytes = 2 << 30
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = 1024
	}
	ctx, cancel := context.WithCancel(contextOrBackground(ctx))
	s := &PreparedSession{ctx: ctx, cancel: cancel, options: options, sources: map[*PreparedSource]struct{}{}}
	s.stop = context.AfterFunc(ctx, s.requestClose)
	return s, nil
}
func (s *PreparedSession) Stats() PreparedSessionStats {
	if s == nil {
		return PreparedSessionStats{Closed: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
func (s *PreparedSession) reserve(n int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		return 0, ErrPreparedClosed
	}
	available := s.options.MaxRetainedBytes - s.stats.ReservedBytes - s.stats.RetainedBytes
	if available <= 0 {
		return 0, ErrResourceLimit
	}
	if n > available {
		n = available
	}
	s.stats.ReservedBytes += n
	if peak := s.stats.ReservedBytes + s.stats.RetainedBytes; peak > s.stats.PeakBytes {
		s.stats.PeakBytes = peak
	}
	return n, nil
}
func (s *PreparedSession) publish(reserved, retained int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.ReservedBytes -= reserved
	s.stats.RetainedBytes += retained
}
func (s *PreparedSession) releaseReserved(n int64) {
	s.mu.Lock()
	s.stats.ReservedBytes -= n
	s.mu.Unlock()
}
func (s *PreparedSession) releaseRetained(n int64) {
	s.mu.Lock()
	s.stats.RetainedBytes -= n
	s.mu.Unlock()
}
func (s *PreparedSession) requestClose() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closing = true
	s.stats.Closing = true
	all := make([]*PreparedSource, 0, len(s.sources))
	for p := range s.sources {
		all = append(all, p)
	}
	s.mu.Unlock()
	s.cancel()
	for _, p := range all {
		p.requestClose()
	}
}

// Close revokes idle leases, requests cooperative cancellation and waits for
// actual I/O/preparation exit before deleting private files. A deadline leaves
// the session closing with truthful retained accounting; another Close may wait.
func (s *PreparedSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ctx = contextOrBackground(ctx)
	s.mu.Lock()
	s.closing = true
	s.stats.Closing = true
	all := make([]*PreparedSource, 0, len(s.sources))
	for p := range s.sources {
		all = append(all, p)
	}
	s.mu.Unlock()
	attempts := make([]*preparedCleanupAttempt, len(all))
	for i, p := range all {
		attempts[i] = p.closeAttempt()
	}
	// Capture each immutable result before cancellation can finish an attempt.
	s.requestClose()
	var err error
	for _, a := range attempts {
		err = errors.Join(err, awaitPreparedCleanup(ctx, a))
	}
	s.mu.Lock()
	if err == nil && s.stats.Entries == 0 && s.stats.ReservedBytes == 0 && s.stats.RetainedBytes == 0 {
		s.stats.Closed = true
	}
	s.mu.Unlock()
	if s.stop != nil {
		s.stop()
	}
	return err
}

func (s *PreparedSession) releaseEntry(p *PreparedSource) {
	s.mu.Lock()
	if _, ok := s.sources[p]; ok {
		delete(s.sources, p)
		s.stats.Entries--
		if s.closing && s.stats.Entries == 0 && s.stats.ReservedBytes == 0 && s.stats.RetainedBytes == 0 {
			s.stats.Closed = true
		}
	}
	s.mu.Unlock()
}
