package roi

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestCompareMasksCancelDiscardsMetrics(t *testing.T) {
	mask := NewVolumeMask(2, 2, 2, [3]float64{1, 1, 1})
	mask.Voxels[0] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	empty := NewVolumeMask(2, 2, 2, [3]float64{1, 1, 1})
	for _, other := range []*VolumeMask{mask, empty} {
		metrics, err := CompareMasks(ctx, mask, other)
		if metrics != (OverlapMetrics{}) || !errors.Is(err, context.Canceled) {
			t.Errorf("CompareMasks = (%+v, %v), want zero metrics and context.Canceled", metrics, err)
		}
	}
}

// TestCompareMasksOnShiftedSpheres checks the metrics of two spheres of
// radius r shifted by d against their analytic values: Dice from the lens
// volume π(4r+d)(2r−d)²/12, Hausdorff d, and, for small d, surface
// distances d·|cos θ| uniform on [0,d] over the area (MSD d/2, HD95 0.95d).
func TestCompareMasksOnShiftedSpheres(t *testing.T) {
	const r, d = 12.0, 2.0
	spacing := [3]float64{0.5, 0.5, 0.6}
	a := NewVolumeMask(64, 64, 52, spacing)
	b := a.Clone()
	centre := [3]float64{15.7, 15.9, 15.4}
	sphereMask(a, centre, r)
	sphereMask(b, [3]float64{centre[0] + d*0.6, centre[1] + d*0.8, centre[2]}, r)
	m, err := CompareMasks(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	sphere := 4 * math.Pi * r * r * r / 3
	lens := math.Pi * (4*r + d) * (2*r - d) * (2*r - d) / 12
	wantDice := lens / sphere
	t.Logf("metrics %+v; analytic Dice %.4f", m, wantDice)
	if math.Abs(m.Dice-wantDice) > 0.01 || math.Abs(m.Jaccard-m.Dice/(2-m.Dice)) > 1e-12 {
		t.Errorf("Dice %.4f (analytic %.4f), Jaccard %.4f", m.Dice, wantDice, m.Jaccard)
	}
	if math.Abs(m.VolumeA-sphere)/sphere > 0.01 || math.Abs(m.VolumeB-sphere)/sphere > 0.01 {
		t.Errorf("volumes %.1f %.1f mm³, analytic %.1f", m.VolumeA, m.VolumeB, sphere)
	}
	voxel := 0.6
	if math.Abs(m.HausdorffMM-d) > voxel || math.Abs(m.HD95MM-0.95*d) > voxel || math.Abs(m.MeanSurfaceDistanceMM-d/2) > voxel/2 {
		t.Errorf("Hausdorff %.3f (d %v), HD95 %.3f (%.2f), MSD %.3f (%.2f)", m.HausdorffMM, d, m.HD95MM, 0.95*d, m.MeanSurfaceDistanceMM, d/2)
	}
	same, err := CompareMasks(context.Background(), a, a.Clone())
	if err != nil || same.Dice != 1 || same.Jaccard != 1 || same.HausdorffMM != 0 || same.MeanSurfaceDistanceMM != 0 {
		t.Errorf("identical masks: %+v %v", same, err)
	}
	if _, err := CompareMasks(context.Background(), a, NewVolumeMask(64, 64, 52, spacing)); !errors.Is(err, ErrEmptyMask) {
		t.Errorf("empty mask: %v", err)
	}
	if _, err := CompareMasks(context.Background(), a, NewVolumeMask(64, 64, 51, spacing)); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Errorf("different grids: %v", err)
	}
}
