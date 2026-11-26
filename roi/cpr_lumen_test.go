package roi

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

func lumenStenosisPhantom(t *testing.T) (*render.Volume, *render.CPRPath) {
	t.Helper()
	const n, depth = 80, 41
	stack := &render.Stack{}
	for z := 0; z < depth; z++ {
		pixels := make([]byte, n*n*2)
		arc := float64(z) * .5
		radius := 6.0
		if math.Abs(arc-10) <= 2 {
			radius = 3
		}
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				// Partial-volume CT field whose 250 HU iso-line is the exact
				// analytic radius; a binary pixel-centre disk instead defines a
				// staircase extending half a voxel beyond that radius.
				distance := math.Hypot(float64(x)*.5-20, float64(y)*.5-20)
				value := math.Max(0, math.Min(500, 250+(radius-distance)*500))
				binary.LittleEndian.PutUint16(pixels[(y*n+x)*2:], uint16(math.Round(value)))
			}
		}
		stack.Frames = append(stack.Frames, &render.Frame{
			Metadata:   pixeldata.Metadata{Rows: n, Columns: n, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 16, HighBit: 15, PhotometricInterpretation: "MONOCHROME2"},
			PixelBytes: pixels, ByteOrder: binary.LittleEndian, Rescale: render.Rescale{Slope: 1}, PixelSpacing: []float64{.5, .5}, ImageOrientation: []float64{1, 0, 0, 0, 1, 0}, ImagePosition: []float64{0, 0, arc},
		})
	}
	volume, err := render.BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	path := render.NewCPRPath([]render.Vec3{{X: 20, Y: 20, Z: 1}, {X: 20, Y: 20, Z: 19}})
	return volume, path
}

func TestCPRLumenKnownCircularStenosisPhysicalAreaAndDiameters(t *testing.T) {
	volume, path := lumenStenosisPhantom(t)
	profile, err := AnalyzeCPRLumen(context.Background(), volume, path, CPRLumenOptions{ThresholdMin: 250, ThresholdMax: 750, SectionStepMM: 1, PixelSpacingMM: .125, RadiusMM: 9, ReferenceArcMM: 1.3})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Sections[profile.ReferenceIndex].ArcLengthMM != 1.3 {
		t.Fatal("reference was rounded to a section")
	}
	var normal, narrow CPRLumenSection
	for _, s := range profile.Sections {
		if !s.Valid {
			t.Fatal(s.ArcLengthMM, s.Reason)
		}
		if s.ArcLengthMM == 2 {
			normal = s
		}
		if s.ArcLengthMM == 9 {
			narrow = s
		}
	}
	for _, fixture := range []struct {
		s CPRLumenSection
		r float64
	}{{normal, 6}, {narrow, 3}} {
		if math.Abs(fixture.s.AreaMM2-math.Pi*fixture.r*fixture.r)/(math.Pi*fixture.r*fixture.r) > .04 || math.Abs(fixture.s.MinDiameterMM-2*fixture.r) > .4 || math.Abs(fixture.s.MaxDiameterMM-2*fixture.r) > .4 {
			t.Fatalf("radius%g area%g min%g max%g", fixture.r, fixture.s.AreaMM2, fixture.s.MinDiameterMM, fixture.s.MaxDiameterMM)
		}
	}
	if math.Abs(narrow.DiameterReductionPercent-50) > 3 || math.Abs(narrow.AreaReductionPercent-75) > 3 {
		t.Fatal(narrow.DiameterReductionPercent, narrow.AreaReductionPercent)
	}
}

func TestCPRLumenInvalidSectionsLimitsAndCancellation(t *testing.T) {
	volume, path := lumenStenosisPhantom(t)
	for _, opts := range []CPRLumenOptions{{ThresholdMin: 250, ThresholdMax: 750, RadiusMM: 2}, {ThresholdMin: 1000, ThresholdMax: 1500}} {
		if _, err := AnalyzeCPRLumen(context.Background(), volume, path, opts); !errors.Is(err, ErrCPRLumenReference) {
			t.Fatal("invalid reference silently measured", err)
		}
	}
	if _, err := AnalyzeCPRLumen(context.Background(), volume, path, CPRLumenOptions{ThresholdMin: 250, ThresholdMax: 750, RadiusMM: 1000, PixelSpacingMM: .001}); !errors.Is(err, ErrCPRLumenLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AnalyzeCPRLumen(ctx, volume, path, CPRLumenOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCPRLumenObliqueEllipseAndSourceBoundaryAreExplicit(t *testing.T) {
	frame := render.CPRPathSample{Position: render.Vec3{X: 100, Y: -20, Z: 30}, Normal: render.Vec3{X: 1 / math.Sqrt2, Y: 1 / math.Sqrt2}, Binormal: render.Vec3{Z: 1}, ArcLength: 7}
	options := CPRLumenOptions{ThresholdMin: 250, ThresholdMax: 750, PixelSpacingMM: .1, RadiusMM: 8}
	sample := func(patient render.Vec3) (float64, bool) {
		p := patient.Sub(frame.Position)
		u, v := p.Dot(frame.Normal), p.Dot(frame.Binormal)
		return 500 - 250*math.Sqrt(u*u/25+v*v/4), true
	}
	section, err := measureCPRLumenSection(context.Background(), sample, frame, options, 161)
	if err != nil || !section.Valid {
		t.Fatal(section.Reason, err)
	}
	if math.Abs(section.AreaMM2-math.Pi*10)/(math.Pi*10) > .01 || math.Abs(section.MinDiameterMM-4) > .05 || math.Abs(section.MaxDiameterMM-10) > .05 {
		t.Fatal(section.AreaMM2, section.MinDiameterMM, section.MaxDiameterMM)
	}
	outside := func(patient render.Vec3) (float64, bool) {
		value, valid := sample(patient)
		if patient.Sub(frame.Position).Dot(frame.Normal) > 3 {
			return 0, false
		}
		return value, valid
	}
	section, err = measureCPRLumenSection(context.Background(), outside, frame, options, 161)
	if err != nil || section.Valid || section.Reason != "lumen reaches source FOV boundary" {
		t.Fatal("partial source was quantified", section.Reason, err)
	}
}

func TestCPRLumenCancellationDuringSampling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	sample := func(render.Vec3) (float64, bool) {
		count++
		if count == 300 {
			cancel()
		}
		return 500, true
	}
	_, err := measureCPRLumenSection(ctx, sample, render.CPRPathSample{Normal: render.Vec3{X: 1}, Binormal: render.Vec3{Y: 1}}, CPRLumenOptions{ThresholdMin: 250, ThresholdMax: 750, PixelSpacingMM: .1}, 161)
	if !errors.Is(err, context.Canceled) || count >= 161*161 {
		t.Fatal("active sampling ignored cancellation", count, err)
	}
}
