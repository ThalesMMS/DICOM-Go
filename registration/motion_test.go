package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// motionPhases builds a synthetic dynamic series: the phantom on a
// 40 × 40 × 32 grid of 2.5 mm moved by truths[k] (reference → phase k) and
// enhanced by 1 + 0.25k, as contrast uptake would.
func motionPhases(t *testing.T, truths []*Euler3D) []*Image {
	t.Helper()
	size := [3]int{40, 40, 32}
	spacing := render.Vec3{X: 2.5, Y: 2.5, Z: 2.5}
	phases := make([]*Image, len(truths))
	for k, truth := range truths {
		inverse, err := truth.Inverse()
		if err != nil {
			t.Fatal(err)
		}
		gain := 1 + 0.25*float64(k)
		data := make([]float32, size[0]*size[1]*size[2])
		for i := range data {
			p := render.Vec3{X: float64(i%size[0]) * spacing.X, Y: float64(i/size[0]%size[1]) * spacing.Y, Z: float64(i/(size[0]*size[1])) * spacing.Z}
			// phase(truth(x)) = reference(x) · gain.
			data[i] = float32(gain * phantom(inverse.TransformPoint(p)))
		}
		if phases[k], err = NewImage(size, data, render.Vec3{}, spacing, Identity3()); err != nil {
			t.Fatal(err)
		}
	}
	return phases
}

func motionTruth(t *testing.T, degrees [3]float64, translation [3]float64) *Euler3D {
	t.Helper()
	truth := NewEuler3D()
	// The centre of rotation does not change the motion; any centre works.
	if err := truth.SetCenter(render.Vec3{X: 48.75, Y: 48.75, Z: 38.75}); err != nil {
		t.Fatal(err)
	}
	r := math.Pi / 180
	if err := truth.SetParameters([]float64{degrees[0] * r, degrees[1] * r, degrees[2] * r, translation[0], translation[1], translation[2]}); err != nil {
		t.Fatal(err)
	}
	return truth
}

// TestCorrectMotionRecoversKnownPhaseMotions (#1204): four phases with known
// rigid motions (up to 4° and 4 mm) and growing enhancement are registered
// to phase 1; every motion is recovered within 0.5° and 0.5 mm at the
// corners, and the corrected phases match the reference (times their
// enhancement) far better than the uncorrected ones.
func TestCorrectMotionRecoversKnownPhaseMotions(t *testing.T) {
	truths := []*Euler3D{
		motionTruth(t, [3]float64{2, 0, -1}, [3]float64{-2, 1.5, 1}),
		motionTruth(t, [3]float64{}, [3]float64{}),
		motionTruth(t, [3]float64{0, 0, 3}, [3]float64{2, -1.5, 1}),
		motionTruth(t, [3]float64{-2, 1.5, 4}, [3]float64{3.5, 1, -2}),
	}
	// Motions relative to the reference phase (1), which here is the
	// identity.
	phases := motionPhases(t, truths)
	var order []int
	var stages = map[MotionStage]bool{}
	corrected := make([]*Resampled, len(phases))
	motions, err := CorrectMotion(context.Background(), len(phases), func(_ context.Context, index int) (*Image, error) {
		return phases[index], nil
	}, MotionOptions{Reference: 1, Seed: 1204, MaxPoints: 20000, PadWithMinimum: true, Progress: func(p MotionProgress) {
		stages[p.Stage] = true
		if p.Phases != len(phases) || p.Completed > p.Phases {
			t.Errorf("progress %+v", p)
		}
	}}, func(c PhaseCorrection) error {
		order = append(order, c.Index)
		corrected[c.Index] = c.Corrected
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 2, 3, 0}; len(order) != 4 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] || order[3] != want[3] {
		t.Fatalf("sink order %v, want %v", order, want)
	}
	if !stages[MotionLoad] || !stages[MotionRegister] || !stages[MotionResample] {
		t.Fatalf("stages reported %v", stages)
	}
	reference := phases[1]
	for k, motion := range motions {
		if motion.Index != k || motion.Reference != (k == 1) || motion.Transform == nil {
			t.Fatalf("motion %d: %+v", k, motion)
		}
		degrees, millimetres := transformErrors(motion.Transform, truths[k], reference)
		for _, level := range motion.Levels {
			t.Logf("phase %d: level %+v: %d points, %d iterations, %s", k, level.Level, level.Points, level.Iterations, level.Stop)
		}
		t.Logf("phase %d: rotation %.2f°, translation %.2f mm, largest displacement %.2f mm; error %.3f°, %.3f mm",
			k, motion.RotationDegrees, motion.Translation, motion.MaxDisplacementMM, degrees, millimetres)
		if degrees > 0.5 || millimetres > 0.5 {
			t.Fatalf("phase %d: rotation error %.3f°, point error %.3f mm", k, degrees, millimetres)
		}
		if k == 1 && motion.MaxDisplacementMM != 0 {
			t.Fatalf("reference phase moved %.3f mm", motion.MaxDisplacementMM)
		}
		// The reference phase is itself enhanced by 1.25.
		gain := (1 + 0.25*float64(k)) / 1.25
		var before, after float64
		var count int
		size := reference.Size()
		for z := 6; z < size[2]-6; z++ {
			for y := 6; y < size[1]-6; y++ {
				for x := 6; x < size[0]-6; x++ {
					want := gain * float64(reference.At(x, y, z))
					before += math.Pow(float64(phases[k].At(x, y, z))-want, 2)
					after += math.Pow(float64(corrected[k].Image.At(x, y, z))-want, 2)
					count++
				}
			}
		}
		before, after = math.Sqrt(before/float64(count)), math.Sqrt(after/float64(count))
		t.Logf("phase %d: RMS difference to the reference %.2f before, %.2f after", k, before, after)
		if k != 1 && (after > before/4 || after > 8) {
			t.Fatalf("phase %d: RMS difference %.2f after correction (%.2f before)", k, after, before)
		}
		if k == 1 && after > 1e-3 {
			t.Fatalf("reference phase changed by %.4f RMS", after)
		}
		if !corrected[k].Image.Grid().SameAs(reference.Grid(), 1e-9) {
			t.Fatalf("phase %d is not on the reference grid", k)
		}
	}
}

// TestCorrectMotionRefusesAndCancels: invalid options are refused, a
// source or sink error stops the run, and a cancelled context ends it.
func TestCorrectMotionRefusesAndCancels(t *testing.T) {
	phases := motionPhases(t, []*Euler3D{motionTruth(t, [3]float64{}, [3]float64{}), motionTruth(t, [3]float64{}, [3]float64{1, 0, 0})})
	source := func(_ context.Context, index int) (*Image, error) { return phases[index], nil }
	sink := func(PhaseCorrection) error { return nil }
	for name, run := range map[string]func() error{
		"one phase": func() error {
			_, err := CorrectMotion(context.Background(), 1, source, MotionOptions{}, sink)
			return err
		},
		"reference": func() error {
			_, err := CorrectMotion(context.Background(), 2, source, MotionOptions{Reference: 2}, sink)
			return err
		},
		"metric": func() error {
			_, err := CorrectMotion(context.Background(), 2, source, MotionOptions{Metric: 9}, sink)
			return err
		},
		"padding": func() error {
			_, err := CorrectMotion(context.Background(), 2, source, MotionOptions{Padding: float32(math.NaN())}, sink)
			return err
		},
		"no sink": func() error {
			_, err := CorrectMotion(context.Background(), 2, source, MotionOptions{}, nil)
			return err
		},
	} {
		if err := run(); !errors.Is(err, ErrMotion) {
			t.Fatalf("%s: %v, want ErrMotion", name, err)
		}
	}
	failure := errors.New("unreadable phase")
	if _, err := CorrectMotion(context.Background(), 2, func(_ context.Context, index int) (*Image, error) {
		if index == 1 {
			return nil, failure
		}
		return phases[index], nil
	}, MotionOptions{}, sink); !errors.Is(err, failure) {
		t.Fatalf("source error: %v", err)
	}
	stop := errors.New("stop")
	if _, err := CorrectMotion(context.Background(), 2, source, MotionOptions{}, func(PhaseCorrection) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("sink error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CorrectMotion(ctx, 2, source, MotionOptions{}, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// Cancelled during the registration of the second phase.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, err := CorrectMotion(ctx, 2, source, MotionOptions{Progress: func(p MotionProgress) {
		if p.Stage == MotionRegister && p.Evaluation > 3 {
			cancel()
		}
	}}, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled during registration: %v", err)
	}
}
