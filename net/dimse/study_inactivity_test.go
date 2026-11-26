package dimse

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func newActivityTest(t *testing.T, options StudyInactivityOptions) *StudyInactivityTracker {
	t.Helper()
	tracker, err := NewStudyInactivityTracker(options)
	if err != nil {
		t.Fatal(err)
	}
	return tracker
}
func reserveActivity(t *testing.T, tracker *StudyInactivityTracker, ae, study string) *StudyActivityReservation {
	t.Helper()
	r, err := tracker.Reserve(context.Background(), ae, study)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func activityPoll(t *testing.T, tracker *StudyInactivityTracker, now time.Time) []StudyInactiveEvent {
	t.Helper()
	events, err := tracker.Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
func TestStudyInactivityCommitAbortInFlightAndLateGeneration(t *testing.T) {
	tracker := newActivityTest(t, StudyInactivityOptions{Timeout: time.Second})
	now := time.Unix(1700000000, 0)
	first := reserveActivity(t, tracker, "MixedAE", "1.2.3")
	if !first.NewGeneration() {
		t.Fatal("first generation")
	}
	if events := activityPoll(t, tracker, now.Add(time.Hour)); len(events) != 0 {
		t.Fatal("pending store expired")
	}
	if err := first.Commit("/private/one.dcm", now); err != nil {
		t.Fatal(err)
	}
	pending := reserveActivity(t, tracker, "MixedAE", "1.2.3")
	if pending.NewGeneration() || pending.Generation() != first.Generation() {
		t.Fatal("split active generation")
	}
	if events := activityPoll(t, tracker, now.Add(2*time.Second)); len(events) != 0 {
		t.Fatal("in-flight expiry")
	}
	pending.Abort()
	pending.Abort()
	events := activityPoll(t, tracker, now.Add(2*time.Second))
	if len(events) != 1 || events[0].Count != 1 || events[0].LastCommit != now || events[0].LastPath != "/private/one.dcm" {
		t.Fatalf("abort reset timer: %+v", events)
	}
	if events = activityPoll(t, tracker, now.Add(time.Hour)); len(events) != 0 {
		t.Fatal("duplicate expiry")
	}
	late := reserveActivity(t, tracker, "MixedAE", "1.2.3")
	if !late.NewGeneration() || late.Generation() <= first.Generation() {
		t.Fatal("late generation identity")
	}
	late.Abort()
	if late.GenerationActive() {
		t.Fatal("aborted empty state retained")
	}
}
func TestStudyInactivityActualCalledAEAndRemainingBudget(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tracker := newActivityTest(t, StudyInactivityOptions{Timeout: time.Second, MaxStudies: 2, MaxBytes: 20000})
	for _, ae := range []string{"CT_SCP", "ct_scp"} {
		r := reserveActivity(t, tracker, ae, "1.2.3")
		if err := r.Commit("/private/file", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracker.Reserve(context.Background(), "MR_SCP", "1.2.3"); !errors.Is(err, ErrStudyActivityLimit) {
		t.Fatalf("state bound %v", err)
	}
	events := activityPoll(t, tracker, now.Add(time.Second))
	if len(events) != 2 || events[0].CalledAETitle == events[1].CalledAETitle {
		t.Fatal("AE keys merged")
	}
	tracker = newActivityTest(t, StudyInactivityOptions{MaxBytes: int64(8320 + len("AE") + len("1.2.3") - 1)})
	if _, err := tracker.Reserve(context.Background(), "AE", "1.2.3"); !errors.Is(err, ErrStudyActivityLimit) || len(tracker.states) != 0 || tracker.bytes != 0 {
		t.Fatalf("copied over-budget state: %v", err)
	}
	for _, bad := range []struct{ ae, uid string }{{" AE", "1.2.3"}, {"AE", "01.2"}, {"AE", strings.Repeat("1", 65)}, {"A\\E", "1.2.3"}} {
		if _, err := tracker.Reserve(context.Background(), bad.ae, bad.uid); !errors.Is(err, ErrStudyActivityInput) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
}
func TestStudyInactivityFirstAbortDoesNotRetireOtherReservations(t *testing.T) {
	tracker := newActivityTest(t, StudyInactivityOptions{})
	one := reserveActivity(t, tracker, "AE", "1.2.3")
	two := reserveActivity(t, tracker, "AE", "1.2.3")
	one.Abort()
	if !one.GenerationActive() {
		t.Fatal("retired other reservation")
	}
	if err := two.Commit("path", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := two.Commit("path", time.Unix(101, 0)); !errors.Is(err, ErrStudyActivityInput) {
		t.Fatal("double commit accepted")
	}
}
func TestStudyInactivityConcurrentReservationsAndCancellation(t *testing.T) {
	tracker := newActivityTest(t, StudyInactivityOptions{Timeout: time.Second})
	now := time.Unix(1700000000, 0)
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := tracker.Reserve(context.Background(), "AE", "1.2.3")
			if err != nil {
				t.Error(err)
				return
			}
			if err := r.Commit("owned-path", now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tracker.Reserve(ctx, "AE", "1.2.3"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := tracker.Poll(ctx, now.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	events := activityPoll(t, tracker, now.Add(time.Second))
	if len(events) != 1 || events[0].Count != n {
		t.Fatalf("lost commits %+v", events)
	}
}
func TestStudyInactivityCommitInputAndClockDoNotChangeActivity(t *testing.T) {
	tracker := newActivityTest(t, StudyInactivityOptions{Timeout: time.Second})
	now := time.Unix(100, 0)
	r := reserveActivity(t, tracker, "AE", "1.2.3")
	if err := r.Commit("first", now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		when time.Time
	}{{"", now}, {strings.Repeat("x", 8193), now}, {"path", time.Time{}}, {"path", now.Add(-time.Second)}} {
		r = reserveActivity(t, tracker, "AE", "1.2.3")
		if err := r.Commit(tc.path, tc.when); !errors.Is(err, ErrStudyActivityInput) {
			t.Fatalf("invalid commit %v", err)
		}
		r.Abort()
	}
	events := activityPoll(t, tracker, now.Add(time.Second))
	if len(events) != 1 || events[0].Count != 1 || events[0].LastPath != "first" {
		t.Fatalf("failed commits changed state %+v", events)
	}
}
func TestStudyInactivityInterleavedStudiesAtExactBoundary(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tracker := newActivityTest(t, StudyInactivityOptions{Timeout: time.Second})
	a := reserveActivity(t, tracker, "AE", "1.2.3")
	b := reserveActivity(t, tracker, "AE", "1.2.4")
	if err := a.Commit("A", now); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit("B", now); err != nil {
		t.Fatal(err)
	}
	pendingA := reserveActivity(t, tracker, "AE", "1.2.3")
	failedC := reserveActivity(t, tracker, "AE", "1.2.5")
	failedC.Abort()
	if events := activityPoll(t, tracker, now.Add(time.Second-time.Nanosecond)); len(events) != 0 {
		t.Fatal("early expiry")
	}
	events := activityPoll(t, tracker, now.Add(time.Second))
	if len(events) != 1 || events[0].StudyInstanceUID != "1.2.4" {
		t.Fatalf("interleaved/in-flight expiry %+v", events)
	}
	pendingA.Abort()
	events = activityPoll(t, tracker, now.Add(time.Second))
	if len(events) != 1 || events[0].StudyInstanceUID != "1.2.3" || events[0].Count != 1 {
		t.Fatalf("abort changed deadline/count %+v", events)
	}
}
func TestStudyInactivityCountAndGenerationOverflowBeforeAdmission(t *testing.T) {
	for _, kind := range []string{"count", "pending", "generation"} {
		t.Run(kind, func(t *testing.T) {
			tracker := newActivityTest(t, StudyInactivityOptions{})
			if kind == "generation" {
				tracker.next = math.MaxUint64
			} else {
				r := reserveActivity(t, tracker, "AE", "1.2.3")
				if kind == "count" {
					r.state.count = math.MaxUint64
					r.state.pending = 0
				} else {
					r.state.pending = math.MaxInt
				}
			}
			beforeBytes, beforeStates := tracker.bytes, len(tracker.states)
			if r, err := tracker.Reserve(context.Background(), "AE", "1.2.3"); r != nil || !errors.Is(err, ErrStudyActivityLimit) {
				t.Fatalf("overflow admitted %v/%v", r, err)
			}
			if tracker.bytes != beforeBytes || len(tracker.states) != beforeStates {
				t.Fatal("overflow allocated state")
			}
		})
	}
}
