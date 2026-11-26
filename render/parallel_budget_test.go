package render

import (
	"context"
	"image"
	"runtime"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// TestN4AndVRShareTheConcurrencyBudget runs an N4 bias correction and a run
// of volume renders at the same time under a small budget (#1188): the
// helpers they run together never exceed it, and both produce exactly what
// they produce alone.
func TestN4AndVRShareTheConcurrencyBudget(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs at least two CPUs")
	}
	original := runtime.GOMAXPROCS(max(4, runtime.GOMAXPROCS(0)))
	defer runtime.GOMAXPROCS(original)
	const budget = 2
	previous := parallel.SetBudget(budget)
	defer parallel.SetBudget(previous)

	n4Input := imagefilter.NewVolume([3]int{48, 48, 32}, [3]float64{1, 1, 1.5})
	for z := 0; z < 32; z++ {
		for y := 0; y < 48; y++ {
			for x := 0; x < 48; x++ {
				dx, dy, dz := float64(x-24), float64(y-24), float64(z-16)
				if dx*dx+dy*dy+2*dz*dz < 20*20 {
					n4Input.Data[n4Input.Index(x, y, z)] = float32(200 * (1 + 0.01*float64(x)))
				}
			}
		}
	}
	n4Options := imagefilter.N4Options{ShrinkFactor: 2, Iterations: []int{10, 10}}
	vol, cam, window := unbalancedVRScene(t)
	quality := DefaultVRQuality(96, 96)
	preset := translucentAbovePreset()
	render := func() image.Image { return RenderVR(vol, cam, preset, window, true, nil, quality) }

	// Alone, with the budget to themselves.
	wantN4, err := imagefilter.N4BiasFieldCorrection(context.Background(), n4Input, n4Options)
	if err != nil {
		t.Fatal(err)
	}
	wantVR := render()

	parallel.ResetPeak()
	var wg sync.WaitGroup
	var gotN4 *imagefilter.N4Result
	var n4Err error
	wg.Add(2)
	go func() {
		defer wg.Done()
		gotN4, n4Err = imagefilter.N4BiasFieldCorrection(context.Background(), n4Input, n4Options)
	}()
	var gotVR []image.Image
	go func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			gotVR = append(gotVR, render())
		}
	}()
	wg.Wait()
	if n4Err != nil {
		t.Fatal(n4Err)
	}
	if peak := parallel.Peak(); peak > budget || peak == 0 {
		t.Fatalf("N4 and VR together reserved %d helpers at once, budget %d", peak, budget)
	}
	for i, v := range gotN4.Corrected.Data {
		if v != wantN4.Corrected.Data[i] {
			t.Fatalf("concurrent N4 voxel %d = %v, alone %v", i, v, wantN4.Corrected.Data[i])
		}
	}
	for i, img := range gotVR {
		if !sameImage(img, wantVR) {
			t.Fatalf("concurrent render %d differs from the render alone", i)
		}
	}
	if parallel.InUse() != 0 {
		t.Fatalf("%d helper slots still reserved", parallel.InUse())
	}
}
