package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"sync"
	"testing"
)

func TestSpaceLeapGradientBoundCoversContinuousAnisotropicStencil(t *testing.T) {
	s := &volumeSampler{cols: 33, rows: 25, depth: 17, values: make([]float32, 33*25*17), vol: &Volume{ColSpacing: .7, RowSpacing: 1.3, SliceSpacing: 2.1, AxisX: Vec3{X: 1}, AxisY: Vec3{X: .2, Y: 1}, Normal: Vec3{Z: 1}}}
	for z := 0; z < s.depth; z++ {
		for y := 0; y < s.rows; y++ {
			for x := 0; x < s.cols; x++ {
				s.values[(z*s.rows+y)*s.cols+x] = float32(100*math.Sin(float64(x)/3) + float64(y*z))
			}
		}
	}
	grid, err := buildVRBlockGrid(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	// Several render callers share one immutable generation's lazy metadata.
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := grid.physicalGradientBounds(context.Background(), s)
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	bounds, err := grid.physicalGradientBounds(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	leap := newVRSpaceLeap(grid, s)
	for z := 0.; z < float64(s.depth); z += .77 {
		for y := 0.; y < float64(s.rows); y += 1.13 {
			for x := 0.; x < float64(s.cols); x += .93 {
				tex := Vec3{X: x / float64(s.cols-1), Y: y / float64(s.rows-1), Z: z / float64(s.depth-1)}
				block := leap.block(tex)
				if block < 0 {
					continue
				}
				actual := s.vol.physicalGradientMagnitude(s.gradientIndexAt(tex))
				if actual > bounds[block] {
					t.Fatalf("physical stencil %g exceeds block %d bound %g at %+v", actual, block, bounds[block], tex)
				}
			}
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := grid.physicalGradientBounds(canceled, s); !errors.Is(err, context.Canceled) {
		t.Fatal("cached gradient ignored cancellation", err)
	}
}

func TestSpaceLeapGradientOpacityAnd2DKeepExactImages(t *testing.T) {
	volume, err := BuildVolume(ctPhantomStack(49, 41, 33))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := volume.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	sampler, err := prepared.newSampler()
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	preset := VRPreset{Mode: VRModeDVR, TF: NewNormalizedVRTransferFunction([]TFColorStop{{HU: 0, R: 1}, {HU: 1, R: 1}}, []TFAlphaStop{{HU: 0, A: .05}, {HU: 1, A: .05}}, 1), GradientOpacity: VRGradientOpacityRamp(5, 60)}
	table := make([]RGBA, 8*8)
	for y := 4; y < 8; y++ {
		for x := 0; x < 8; x++ {
			table[y*8+x] = RGBA{G: 1, A: .05}
		}
	}
	transfer, err := NewVRTransfer2D(8, 8, -1200, 3000, 140, table)
	if err != nil {
		t.Fatal(err)
	}
	twoD := preset
	twoD.Transfer2D = transfer
	twoD.GradientOpacity = nil
	for _, preset := range []VRPreset{preset, twoD} {
		lut, effective, dataMin, dataMax := vrTransferSetup(prepared, preset, WindowLevel{Center: 40, Width: 400})
		leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, effective, dataMin, dataMax)
		if err != nil {
			t.Fatal(err)
		}
		empty := 0
		for _, value := range leap.empty {
			if value {
				empty++
			}
		}
		if empty == 0 || empty == len(leap.empty) {
			t.Fatal("gradient did not classify both empty and contributing blocks", empty, len(leap.empty))
		}
		camera := NewVRCamera(volume.BoundingRadiusMM())
		camera.FitVolume(volume)
		camera.Rotate(23, -11)
		quality := DefaultVRQuality(47, 39)
		quality.Jitter = true
		for _, clip := range []*VRClip{nil, func() *VRClip { c := NewVRClip(); c.AddClipPlane(Vec3{X: 0, Y: 0, Z: 1}, .55); return c }()} {
			got := RenderVR(volume, camera, preset, effective, true, clip, quality).(*image.NRGBA)
			want := RenderVR(volume, camera, preset, effective, true, fullMarch(clip), quality).(*image.NRGBA)
			if !bytes.Equal(got.Pix, want.Pix) {
				t.Fatal("gradient skipping changed pixels")
			}
		}
	}
	curve := VRGradientOpacity{{Magnitude: 0, Opacity: 0}, {Magnitude: 10, Opacity: 1}, {Magnitude: 20, Opacity: 0}}
	if gradientOpacityZeroUntil(curve, 30) {
		t.Fatal("nonmonotone opacity peak was skipped")
	}
}

func TestSpaceLeapUnrepresentableScalarBoundsRemainUnknown(t *testing.T) {
	sampler := &volumeSampler{cols: 2, rows: 2, depth: 2, signed16: make([]int16, 8), slope: 1, intercept: 1e308, vol: &Volume{ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1, AxisX: Vec3{X: 1}, AxisY: Vec3{Y: 1}, Normal: Vec3{Z: 1}}}
	grid, err := buildVRBlockGrid(context.Background(), sampler)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(float64(grid.max[0]), 1) {
		t.Fatal("fixture did not exceed float32 block metadata")
	}
	transfer, err := NewVRTransfer2D(2, 2, 0, 1, 1, []RGBA{{}, {A: 1}, {}, {A: 1}})
	if err != nil {
		t.Fatal(err)
	}
	leap := newVRSpaceLeap(grid, sampler)
	leap.forDVR2D(transfer, []float64{0})
	if leap.empty[0] || transfer.Lookup(1e308, 0).A != 1 {
		t.Fatal("unrepresentable bound skipped a visible sample")
	}
}
