package render

import (
	"errors"
	"testing"
)

func TestVRCropRegionsIndependent27CellOracleAndBoundaries(t *testing.T) {
	lo, hi := Vec3{X: .3, Y: .3, Z: .3}, Vec3{X: .7, Y: .7, Z: .7}
	for _, flags := range []uint32{VRCropSubvolume, VRCropFence, VRCropCross, VRCropInvertedFence, VRCropInvertedCross} {
		clip := NewVRClip()
		clip.SetCropBox(lo, hi)
		if !clip.SetCropRegionFlags(flags) {
			t.Fatal("valid mask rejected")
		}
		for z := 0; z < 3; z++ {
			for y := 0; y < 3; y++ {
				for x := 0; x < 3; x++ {
					inside := 0
					for _, axis := range []int{x, y, z} {
						if axis == 1 {
							inside++
						}
					}
					want := inside == 3
					switch flags {
					case VRCropFence:
						want = inside >= 1
					case VRCropCross:
						want = inside >= 2
					case VRCropInvertedFence:
						want = inside == 0
					case VRCropInvertedCross:
						want = inside < 2
					}
					point := Vec3{X: .1 + .4*float64(x), Y: .1 + .4*float64(y), Z: .1 + .4*float64(z)}
					if got := clip.keep(point); got != want {
						t.Fatalf("mask%x cell%d%d%d got%v want%v", flags, x, y, z, got, want)
					}
				}
			}
		}
		clone := *clip
		clip.SetCropRegionFlags(VRCropSubvolume)
		if clone.CropRegionFlags() != flags {
			t.Fatal("clone aliases region selection")
		}
	}
	clip := NewVRClip()
	clip.SetCropBox(lo, hi)
	if !clip.keep(lo) || !clip.keep(hi) {
		t.Fatal("crop plane boundary not inside")
	}
	if clip.SetCropRegionFlags(1 << 27) {
		t.Fatal("invalid mask accepted")
	}
}

func TestViewStateCropRegionVersionAndMaskValidation(t *testing.T) {
	s := validVRViewState()
	s.ContractVersion, s.StructSize = ViewStateContractVersionV25, ViewStateHeaderSizeV25
	s.VR.OpacityUnitDistanceMM = 1
	s.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	s.VR.Crop = NormalizedCropBox{Enabled: 1, RegionFlags: VRCropFence, Min: [3]float64{.25, .25, .25}, Max: [3]float64{.75, .75, .75}}
	if err := ValidateViewState(s); err != nil {
		t.Fatal(err)
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV24, ViewStateHeaderSizeV24
	if err := ValidateViewState(s); !errors.Is(err, ErrInvalidViewState) {
		t.Fatal("old contract accepted region mask", err)
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV25, ViewStateHeaderSizeV25
	s.VR.Crop.RegionFlags = 1 << 27
	if err := ValidateViewState(s); !errors.Is(err, ErrInvalidViewState) {
		t.Fatal("invalid high mask bits accepted", err)
	}
	s.VR.Crop.RegionFlags = VRCropCross
	s.VR.Crop.Enabled = 0
	if err := ValidateViewState(s); !errors.Is(err, ErrInvalidViewState) {
		t.Fatal("disabled crop accepted regions", err)
	}
}

func TestVRCropFenceRayIntervalRetainsDisjointOuterSamples(t *testing.T) {
	c := NewVRClip()
	c.SetCropBox(Vec3{X: .25, Y: .25, Z: .25}, Vec3{X: .75, Y: .75, Z: .75})
	c.SetCropRegionFlags(VRCropFence)
	// X and Y lie inside their bands, so fence keeps the entire Z ray.
	lo, hi, hit := c.clipRay(Vec3{Z: -2}, Vec3{Z: 1}, Vec3{X: -.5, Y: -.5, Z: -.5}, Vec3{X: 1, Y: 1, Z: 1}, 1.5, 2.5, .1)
	if !hit || lo != 1.5 || hi != 2.5 {
		t.Fatal("central interval erased fence's outer regions", lo, hi, hit)
	}
}
