package render

import (
	"context"
	"image"
	"math"
	"sort"
	"testing"
)

// vrDVRSampleCount marches every pixel's DVR ray of a frame as
// renderVRPreparedContext does (empty-space skipping included, one worker)
// and returns the classified samples: the ray caster's cost measure,
// independent of the machine's load.
func vrDVRSampleCount(tb testing.TB, vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, quality VRQuality) int {
	tb.Helper()
	prepared, err := vol.PrepareVRVolume(preset.Prefilter)
	if err != nil {
		tb.Fatal(err)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		tb.Fatal(err)
	}
	defer sampler.Close()
	size := vol.PhysicalSize()
	boxMin, boxMax := size.Scale(-0.5), size.Scale(0.5)
	lut, window, dataMin, dataMax := vrTransferSetup(prepared, preset, window)
	leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, window, dataMin, dataMax)
	if err != nil {
		tb.Fatal(err)
	}
	plan, err := volumeVRRaySampling(prepared.Volume, quality)
	if err != nil {
		tb.Fatal(err)
	}
	stepLen := plan.SampleDistanceMM
	stepper := newVRAdaptiveStepper(quality.AdaptiveStep, stepLen)
	rayGen := newVRRayGenerator(cam, quality.Width, quality.Height)
	cell := new(volumeGradientCell)
	for py := 0; py < quality.Height; py++ {
		row := rayGen.rowBase(py)
		for px := 0; px < quality.Width; px++ {
			o, d := rayGen.ray(px, row)
			t0, t1, hit := intersectAABB(o, d, boxMin, boxMax)
			if !hit {
				continue
			}
			marchDVRContext(context.Background(), sampler, cell, lut, o, d, t0, t1, stepLen, boxMin, size, window, dataMin, dataMax, preset, shading, nil, leap, nil, nil, stepper, nil)
		}
	}
	return stepper.samples
}

// vrImageError is the mean and 99th percentile of the absolute RGB channel
// differences of two frames, in 8-bit levels.
func vrImageError(a, b *image.NRGBA) (mean, p99 float64) {
	var differences []float64
	for i := 0; i < len(a.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			differences = append(differences, math.Abs(float64(a.Pix[i+c])-float64(b.Pix[i+c])))
		}
	}
	sort.Float64s(differences)
	sum := 0.0
	for _, difference := range differences {
		sum += difference
	}
	return sum / float64(len(differences)), differences[len(differences)*99/100]
}

// adaptiveStepScenes are the CT phantom (air, fat, soft tissue, bone) under
// an opaque bone preset, the shaded bones-and-skin preset with gradient
// opacity, and a translucent soft-tissue function that keeps most rays
// marching through homogeneous tissue.
func adaptiveStepScenes(tb testing.TB) (*Volume, VRCamera, []adaptiveStepScene) {
	tb.Helper()
	vol, err := BuildVolume(ctPhantomStack(96, 96, 64))
	if err != nil {
		tb.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	soft := NewVRTransferFunction(
		[]TFColorStop{{HU: -200, R: 0.9, G: 0.6, B: 0.5}, {HU: 100, R: 1, G: 0.8, B: 0.7}, {HU: 1200, R: 1, G: 1, B: 1}},
		[]TFAlphaStop{{HU: -150, A: 0}, {HU: -80, A: 0.05}, {HU: 60, A: 0.08}, {HU: 300, A: 0.5}, {HU: 1200, A: 0.9}},
	)
	return vol, cam, []adaptiveStepScene{
		{"bonesBW", bonesBWPreset(), WindowLevel{Center: 300, Width: 1500}, true},
		{"bonesSkin", DefaultVRPreset(), WindowLevel{Center: 40, Width: 400}, true},
		{"softTissue", VRPreset{Name: "soft", TF: soft, Mode: VRModeDVR}, WindowLevel{Center: 40, Width: 2000}, false},
	}
}

type adaptiveStepScene struct {
	name    string
	preset  VRPreset
	window  WindowLevel
	shading bool
}

// TestAdaptiveStepStaysWithinToleranceOfFixedStep (#1231) renders the
// phantom scenes with the adaptive step and with the fixed step of the same
// physical distance: diagonal/192 at the interaction level
// and diagonal/512 at the settled level. The
// documented tolerance holds: the mean channel difference is at most 4 of 255
// levels at 192 steps and 1 at 512, the 99th percentile at most 32 and 8.
// Against a 1536-step reference the adaptive interaction frame is no further
// than the fixed preview it replaces (96 steps), and the adaptive settled
// frame within half a level of the fixed settled frame.
func TestAdaptiveStepStaysWithinToleranceOfFixedStep(t *testing.T) {
	vol, cam, scenes := adaptiveStepScenes(t)
	const side = 96
	// Preserve this existing tolerance fixture at its original physical
	// distances; MaxSteps now provides only a safety ceiling.
	unit := vol.PhysicalSize().Length() / math.Min(vol.ColSpacing, math.Min(vol.RowSpacing, vol.SliceSpacing))
	interaction := AdaptivePreviewVRQuality(side, side)
	interaction.SpacingFraction = unit / 192
	preview := PreviewVRQuality(side, side)
	preview.SpacingFraction = unit / 96
	for _, level := range []struct {
		name            string
		adaptive        VRQuality
		replaces        VRQuality
		maxMean, maxP99 float64
		referenceMargin float64
	}{
		{"interaction", interaction, preview, 4, 32, 0.1},
		{"settled", VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps, SpacingFraction: unit / 512, AdaptiveStep: DefaultVRAdaptiveStep()}, VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps, SpacingFraction: unit / 512}, 1, 8, 0.5},
	} {
		fixedQuality := level.adaptive
		fixedQuality.AdaptiveStep = VRAdaptiveStep{}
		for _, scene := range scenes {
			render := func(q VRQuality) *image.NRGBA {
				return RenderVR(vol, cam, scene.preset, scene.window, scene.shading, nil, q).(*image.NRGBA)
			}
			adaptive, fixed := render(level.adaptive), render(fixedQuality)
			reference := render(VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps, SpacingFraction: unit / 1536})
			mean, p99 := vrImageError(adaptive, fixed)
			if mean > level.maxMean || p99 > level.maxP99 {
				t.Errorf("%s/%s: adaptive vs fixed step mean %.2f p99 %.0f, tolerance %.2f/%.0f", level.name, scene.name, mean, p99, level.maxMean, level.maxP99)
			}
			adaptiveMean, _ := vrImageError(adaptive, reference)
			replacedMean, _ := vrImageError(render(level.replaces), reference)
			if adaptiveMean > replacedMean+level.referenceMargin {
				t.Errorf("%s/%s: adaptive frame %.2f from the reference, the frame it replaces %.2f", level.name, scene.name, adaptiveMean, replacedMean)
			}
			t.Logf("%s/%s: vs fixed mean %.2f p99 %.0f; vs reference adaptive %.2f replaced %.2f", level.name, scene.name, mean, p99, adaptiveMean, replacedMean)
		}
	}
}

// TestAdaptivePreviewCostsAboutTheFixedPreview (#1231): the adaptive
// interaction quality classifies fewer samples than the same fixed physical
// step and at most 1.25 times the two-voxel preview's (fewer
// where homogeneous tissue dominates).
func TestAdaptivePreviewCostsAboutTheFixedPreview(t *testing.T) {
	vol, cam, scenes := adaptiveStepScenes(t)
	const side = 64
	for _, scene := range scenes {
		preview := vrDVRSampleCount(t, vol, cam, scene.preset, scene.window, scene.shading, PreviewVRQuality(side, side))
		adaptive := vrDVRSampleCount(t, vol, cam, scene.preset, scene.window, scene.shading, AdaptivePreviewVRQuality(side, side))
		fixedQuality := AdaptivePreviewVRQuality(side, side)
		fixedQuality.AdaptiveStep = VRAdaptiveStep{}
		fixed := vrDVRSampleCount(t, vol, cam, scene.preset, scene.window, scene.shading, fixedQuality)
		if preview == 0 || float64(adaptive) > 1.25*float64(preview) || adaptive >= fixed {
			t.Errorf("%s: samples preview %d, adaptive %d, fixed one-voxel step %d", scene.name, preview, adaptive, fixed)
		}
		t.Logf("%s: samples preview %d, adaptive %d (%.2fx), fixed one-voxel step %d", scene.name, preview, adaptive, float64(adaptive)/float64(preview), fixed)
	}
}

// TestAdaptiveStepSettledFrameSamplesFewer (#1231): at the settled half-voxel step
// DefaultVRAdaptiveStep classifies at most half of the fixed step's samples
// on every scene.
func TestAdaptiveStepSettledFrameSamplesFewer(t *testing.T) {
	vol, cam, scenes := adaptiveStepScenes(t)
	const side = 64
	for _, scene := range scenes {
		q := VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps}
		fixed := vrDVRSampleCount(t, vol, cam, scene.preset, scene.window, scene.shading, q)
		q.AdaptiveStep = DefaultVRAdaptiveStep()
		adaptive := vrDVRSampleCount(t, vol, cam, scene.preset, scene.window, scene.shading, q)
		if fixed == 0 || float64(adaptive) > 0.5*float64(fixed) {
			t.Errorf("%s: adaptive %d samples, fixed %d", scene.name, adaptive, fixed)
		}
		t.Logf("%s: adaptive %d samples, fixed %d (%.2fx)", scene.name, adaptive, fixed, float64(adaptive)/float64(fixed))
	}
}

// TestVRAdaptiveStepperFollowsTheClassification walks the stepper along a
// homogeneous run, an edge and a second homogeneous run: steps lengthen to
// Max across the runs, a long step landing on the edge is rejected and the
// edge is resampled at Min, and each composited sample stands for the
// distance from the previous one.
func TestVRAdaptiveStepperFollowsTheClassification(t *testing.T) {
	const base = 1.0
	stepper := newVRAdaptiveStepper(VRAdaptiveStep{Max: 4, Min: 0.5, Threshold: 0.02}, base)
	classify := func(t float64) RGBA {
		if t < 20 {
			return RGBA{R: 1, G: 1, B: 1, A: 0.1}
		}
		return RGBA{R: 1, G: 1, B: 1, A: 0.6}
	}
	stepper.beginRay()
	step, covered, rejected := base, 0.0, 0
	maxStep, minStep := 0.0, math.Inf(1)
	for position := 0.0; position <= 40; position += step {
		accept, length, next, resume := stepper.observe(position, classify(position))
		step = next
		if !accept {
			rejected++
			if position < 20 || resume >= 20 || next != 0.5*base {
				t.Fatalf("rejected %v: resume %v next %v", position, resume, next)
			}
			position = resume
			continue
		}
		covered += length
		maxStep, minStep = math.Max(maxStep, next), math.Min(minStep, next)
	}
	if maxStep != 4*base || minStep != 0.5*base || rejected != 1 {
		t.Fatalf("steps in [%v, %v], %d rejected", minStep, maxStep, rejected)
	}
	if stepper.samples >= 41 {
		t.Fatalf("%d samples, the fixed step takes 41", stepper.samples)
	}
	// The fixed step's 41 samples stand for 41 mm; the adaptive ray may end
	// up to one long step before the exit, as the fixed one ends up to a step
	// before it.
	if covered < 40-4*base || covered > 41 {
		t.Fatalf("composited samples cover %v of the 40 mm ray", covered)
	}
	disabled := newVRAdaptiveStepper(VRAdaptiveStep{}, base)
	if accept, length, next, _ := disabled.observe(3, RGBA{A: 1}); !accept || length != base || next != base {
		t.Fatal("a disabled stepper does not march the base step")
	}
}

func TestVRAdaptiveStepNormalized(t *testing.T) {
	if (VRAdaptiveStep{}).Enabled() || (VRAdaptiveStep{Max: 1}).Enabled() || !DefaultVRAdaptiveStep().Enabled() {
		t.Fatal("Enabled")
	}
	got := VRAdaptiveStep{Max: 100, Min: -1, Threshold: math.NaN()}.Normalized()
	if got.Max != 16 || got.Min != 1 || got.Threshold != DefaultVRAdaptiveStepThreshold {
		t.Fatalf("Normalized = %+v", got)
	}
}

// TestAdaptiveStepLeavesDefaultQualitiesFixed: the settled and preview
// qualities (and so every golden rendered with them) keep the fixed step.
func TestAdaptiveStepLeavesDefaultQualitiesFixed(t *testing.T) {
	if DefaultVRQuality(8, 8).AdaptiveStep.Enabled() || PreviewVRQuality(8, 8).AdaptiveStep.Enabled() {
		t.Fatal("a default quality enables the adaptive step")
	}
	if q := AdaptivePreviewVRQuality(8, 8); !q.AdaptiveStep.Enabled() || q.SpacingFraction != PreviewVRQuality(8, 8).SpacingFraction/2 {
		t.Fatalf("AdaptivePreviewVRQuality = %+v", q)
	}
}

// BenchmarkRenderVRAdaptiveStep compares the fixed and adaptive steps on the
// 192x192x128 CT phantom at the interaction (fixed 96 steps, adaptive 192)
// and settled (512 steps) levels.
func BenchmarkRenderVRAdaptiveStep(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	const side = 160
	settledAdaptive := VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps, AdaptiveStep: DefaultVRAdaptiveStep()}
	for _, scene := range []struct {
		name    string
		preset  VRPreset
		window  WindowLevel
		shading bool
	}{
		{"bonesSkin", DefaultVRPreset(), WindowLevel{Center: 40, Width: 400}, true},
		{"bonesBW", bonesBWPreset(), WindowLevel{Center: 300, Width: 1500}, true},
	} {
		for _, variant := range []struct {
			name    string
			quality VRQuality
		}{
			{"preview-fixed-two-voxels", PreviewVRQuality(side, side)},
			{"preview-fixed-one-voxel", VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps, SpacingFraction: 1}},
			{"preview-adaptive-one-voxel", AdaptivePreviewVRQuality(side, side)},
			{"settled-fixed-half-voxel", VRQuality{Width: side, Height: side, MaxSteps: DefaultVRMaxSteps}},
			{"settled-adaptive-half-voxel", settledAdaptive},
		} {
			b.Run(scene.name+"/"+variant.name, func(b *testing.B) {
				benchVRImage = RenderVR(vol, cam, scene.preset, scene.window, scene.shading, nil, variant.quality)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchVRImage = RenderVR(vol, cam, scene.preset, scene.window, scene.shading, nil, variant.quality)
				}
			})
		}
	}
}
