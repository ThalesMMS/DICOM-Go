package render

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestCPRGeometryMatchesScalarOracleAndNeedsNoVolume(t *testing.T) {
	volume, err := BuildVolume(gradientXZStack(12, 12, 6))
	if err != nil {
		t.Fatal(err)
	}
	path := NewCPRPath([]Vec3{volume.VoxelToPatient(Vec3{X: 5, Y: 2, Z: 1}), volume.VoxelToPatient(Vec3{X: 7, Y: 5, Z: 2}), volume.VoxelToPatient(Vec3{X: 5, Y: 9, Z: 4})})
	for _, mode := range []CPRMode{CPRStraightened, CPRStretched, CPRTransverse} {
		req := CPRRequest{Mode: mode, Volume: volume, Path: path, Width: 7, ArcSpacing: .7, CrossSpacing: .8, ArcLength: 3, RotationDegrees: 31}
		oracle, err := RenderCPRScalar(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		req.Volume = nil // Geometry preparation must not need samples or a lease.
		geometry, err := PrepareCPRGeometry(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if geometry.Width != oracle.Width || geometry.Height != oracle.Height {
			t.Fatal("geometry dimensions changed")
		}
		for x, column := range geometry.Columns {
			for y := range geometry.Height {
				p := Vec3{X: column.OriginLPS[0] + float64(y)*column.StepVLPS[0], Y: column.OriginLPS[1] + float64(y)*column.StepVLPS[1], Z: column.OriginLPS[2] + float64(y)*column.StepVLPS[2]}
				value, ok := volume.TrilinearAt(p)
				index := y*geometry.Width + x
				if ok != oracle.Valid[index] || ok && math.Abs(value-oracle.Values[index]) > 1e-9 {
					t.Fatalf("mode%d pixel%d,%d geometry=%g/%v oracle=%g/%v", mode, x, y, value, ok, oracle.Values[index], oracle.Valid[index])
				}
			}
		}
	}
}

func TestCPRGeometryCancellationAndLimits(t *testing.T) {
	path := NewCPRPath([]Vec3{{X: 1}, {X: 10}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareCPRGeometry(ctx, CPRRequest{Path: path}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := PrepareCPRGeometry(context.Background(), CPRRequest{Path: path, Width: MaxCPROutputDimension + 1}); !errors.Is(err, ErrCPRLimit) {
		t.Fatal(err)
	}
}

func TestCPRViewContractValidation(t *testing.T) {
	valid := ViewState{ContractVersion: ViewStateContractVersionV18, StructSize: ViewStateHeaderSizeV18, Kind: ViewKindMPR, VolumeGeneration: 1, ViewGeneration: 1, OutputWidth: 1, OutputHeight: 3, MPR: MPRViewState{PixelStepULPS: [3]float64{1, 0, 0}, PixelStepVLPS: [3]float64{0, 1, 0}, Interpolation: InterpolationLinear, CPRColumns: []CPRColumn{{StepVLPS: [3]float64{0, 1, 0}, SlabAxisLPS: [3]float64{0, 0, 1}}}}}
	if err := ValidateViewState(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ViewState){
		"old-contract":      func(v *ViewState) { v.ContractVersion = ViewStateContractVersionV17 },
		"truncated-columns": func(v *ViewState) { v.OutputWidth = 2 },
		"nan-origin":        func(v *ViewState) { v.MPR.CPRColumns[0].OriginLPS[0] = math.NaN() },
		"zero-step":         func(v *ViewState) { v.MPR.CPRColumns[0].StepVLPS = [3]float64{} },
		"bad-axis":          func(v *ViewState) { v.MPR.CPRColumns[0].SlabAxisLPS[2] = 2 },
		"averaged-level":    func(v *ViewState) { v.MPR.Antialias2x = true },
		"volume-filter":     func(v *ViewState) { v.VolumeFilter = VolumeFilterState{Mode: VolumeFilterGaussian, SigmaMM: 1} },
	} {
		t.Run(name, func(t *testing.T) {
			view := valid
			view.MPR.CPRColumns = append([]CPRColumn(nil), valid.MPR.CPRColumns...)
			change(&view)
			if !errors.Is(ValidateViewState(view), ErrInvalidViewState) {
				t.Fatal("invalid CPR request admitted")
			}
		})
	}
	filtered := valid
	filtered.ContractVersion, filtered.StructSize = ViewStateContractVersionV28, ViewStateHeaderSizeV28
	filtered.VolumeFilter = VolumeFilterState{Mode: VolumeFilterGaussian, SigmaMM: 1}
	if err := ValidateViewState(filtered); err != nil {
		t.Fatal(err)
	}
	fused := valid
	fused.ContractVersion, fused.StructSize = ViewStateContractVersionV28, ViewStateHeaderSizeV28
	fused.MPR.Presentation = MPRPresentationState{
		Mode:    MPRPresentationFusion,
		Base:    MPRLayerPresentation{WindowWidth: 400, Alpha: 1},
		Overlay: MPRLayerPresentation{WindowWidth: 200, Alpha: 0.5},
	}
	fused.MPR.Presentation.Fusion = NewMPRFusionComposite(FusionCompositeMode{}, fused.MPR.Presentation.Overlay)
	fused.MPR.Presentation.Fusion.OverlayGeneration = 2
	fused.MPR.Presentation.Fusion.OverlayDimensions = [3]uint32{10, 10, 10}
	fused.MPR.Presentation.Fusion.TargetToOverlayIndex = GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	if err := ValidateViewState(fused); err != nil {
		t.Fatal(err)
	}
	legacy := fused
	legacy.ContractVersion = ViewStateContractVersionV27
	if err := ValidateViewState(legacy); !errors.Is(err, ErrInvalidViewState) {
		t.Fatal("legacy fused CPR accepted", err)
	}
	fused.VolumeFilter = filtered.VolumeFilter
	if err := ValidateViewState(fused); !errors.Is(err, ErrInvalidViewState) {
		t.Fatal("filtered fusion accepted", err)
	}
}
