package dimse

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrStudyActivityLimit = errors.New("dicom dimse: study activity limit exceeded")
var ErrStudyActivityInput = errors.New("dicom dimse: invalid study activity metadata")

// StudyInactivityOptions controls observed successful storage, not study
// completeness. Zero selects finite defaults. The caller supplies a clock.
type StudyInactivityOptions struct {
	Timeout    time.Duration
	MaxStudies int
	MaxBytes   int64
}
type studyActivityKey struct{ called, study string }
type studyActivity struct {
	key               studyActivityKey
	generation, count uint64
	pending           int
	last              time.Time
	path              string
	bytes             int64
}
type StudyInactivityTracker struct {
	mu      sync.Mutex
	options StudyInactivityOptions
	states  map[studyActivityKey]*studyActivity
	next    uint64
	bytes   int64
}

// StudyActivityReservation owns one admitted store until Commit or Abort. Methods
// are concurrent-safe; a store never extends inactivity until Commit succeeds.
type StudyActivityReservation struct {
	tracker       *StudyInactivityTracker
	state         *studyActivity
	done          bool
	newGeneration bool
}

// StudyInactiveEvent describes one inactive generation. Strings are independently
// owned; it contains no Object, instance list, sender attribution or filesystem.
type StudyInactiveEvent struct {
	CalledAETitle, StudyInstanceUID, LastPath string
	Generation, Count                         uint64
	LastCommit                                time.Time
}

func NewStudyInactivityTracker(opts StudyInactivityOptions) (*StudyInactivityTracker, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxStudies == 0 {
		opts.MaxStudies = 128
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 1 << 20
	}
	if opts.Timeout <= 0 || opts.Timeout > 24*time.Hour || opts.MaxStudies <= 0 || opts.MaxStudies > 4096 || opts.MaxBytes <= 0 || opts.MaxBytes > 64<<20 {
		return nil, ErrStudyActivityInput
	}
	return &StudyInactivityTracker{options: opts, states: make(map[studyActivityKey]*studyActivity)}, nil
}

func (t *StudyInactivityTracker) Reserve(ctx context.Context, calledAE, studyUID string) (*StudyActivityReservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil || len(calledAE) > 16 || ul.ValidateAETitle(calledAE) != nil || calledAE != ul.NormalizeAETitle(calledAE) || len(studyUID) > 64 || !core.IsValidUID(studyUID) {
		return nil, ErrStudyActivityInput
	}
	key := studyActivityKey{calledAE, studyUID}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := t.states[key]
	isNew := state == nil
	if isNew {
		// Reserve maximum last-path bytes before any owned key/path copy.
		cost := int64(8192 + 128 + len(calledAE) + len(studyUID))
		if len(t.states) >= t.options.MaxStudies || cost > t.options.MaxBytes-t.bytes || t.next == math.MaxUint64 {
			return nil, ErrStudyActivityLimit
		}
		key = studyActivityKey{strings.Clone(calledAE), strings.Clone(studyUID)}
		t.next++
		state = &studyActivity{key: key, generation: t.next, bytes: cost}
		t.states[key] = state
		t.bytes += cost
	}
	if state.pending == math.MaxInt || state.count == math.MaxUint64 || uint64(state.pending) >= math.MaxUint64-state.count {
		return nil, ErrStudyActivityLimit
	}
	state.pending++
	return &StudyActivityReservation{tracker: t, state: state, newGeneration: isNew}, nil
}
func (r *StudyActivityReservation) NewGeneration() bool { return r != nil && r.newGeneration }
func (r *StudyActivityReservation) Generation() uint64 {
	if r == nil {
		return 0
	}
	return r.state.generation
}

// GenerationActive reports whether another in-flight or committed store still
// retains this generation after a reservation aborts.
func (r *StudyActivityReservation) GenerationActive() bool {
	if r == nil || r.tracker == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	return r.tracker.states[r.state.key] == r.state
}
func (r *StudyActivityReservation) Commit(publishedPath string, now time.Time) error {
	if r == nil || r.tracker == nil || len(publishedPath) > 8192 || publishedPath == "" || strings.ContainsRune(publishedPath, 0) || now.IsZero() {
		return ErrStudyActivityInput
	}
	t := r.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.done {
		return ErrStudyActivityInput
	}
	s := r.state
	if !s.last.IsZero() && now.Before(s.last) {
		return ErrStudyActivityInput
	}
	s.path = strings.Clone(publishedPath)
	s.last = now
	s.count++
	s.pending--
	r.done = true
	return nil
}
func (r *StudyActivityReservation) Abort() {
	if r == nil || r.tracker == nil {
		return
	}
	t := r.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	s := r.state
	s.pending--
	if s.pending == 0 && s.count == 0 {
		delete(t.states, s.key)
		t.bytes -= s.bytes
	}
}

// Poll atomically retires due generations. Known stores in flight defer expiry;
// association release, another study or failed storage is never an end signal.
func (t *StudyInactivityTracker) Poll(ctx context.Context, now time.Time) ([]StudyInactiveEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil || now.IsZero() {
		return nil, ErrStudyActivityInput
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	due := make([]*studyActivity, 0, len(t.states))
	for _, s := range t.states {
		if s.pending == 0 && s.count > 0 && now.Sub(s.last) >= t.options.Timeout {
			due = append(due, s)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].last.Equal(due[j].last) {
			return due[i].generation < due[j].generation
		}
		return due[i].last.Before(due[j].last)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events := make([]StudyInactiveEvent, 0, len(due))
	for _, s := range due {
		events = append(events, StudyInactiveEvent{s.key.called, s.key.study, s.path, s.generation, s.count, s.last})
		delete(t.states, s.key)
		t.bytes -= s.bytes
	}
	return events, nil
}
