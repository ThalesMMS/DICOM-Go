package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestWriteResamplesWithCubicBSplineWhenSelected (#1190): the cubic B-spline
// policy writes the same grid as the linear one, reports its kernel, keeps
// the source slices the grid passes through and fills the gap differently.
func TestWriteResamplesWithCubicBSplineWhenSelected(t *testing.T) {
	positions := []float64{0, 1, 2, 4, 5}
	pixels := make([][]byte, len(positions))
	var files []*object.File
	for index, z := range positions {
		pixels[index] = make([]byte, 6)
		for i := range pixels[index] {
			pixels[index][i] = byte(50 + 40*math.Sin(0.9*float64(i)+1.3*z))
		}
		files = append(files, testSlice(t, imageSpec{position: [3]float64{0, 0, z}, spacing: [2]float64{1, 1}, rows: 2, columns: 3, pixels: pixels[index]}))
	}
	export := func(policy nifti.GeometryPolicy) (nifti.Report, []float32) {
		t.Helper()
		options := nifti.DefaultOptions()
		options.Geometry = policy
		var output bytes.Buffer
		report, err := nifti.WriteFiles(context.Background(), &output, files, options)
		if err != nil {
			t.Fatalf("policy %d: %v", policy, err)
		}
		payload := output.Bytes()[352:]
		voxels := make([]float32, len(payload)/4)
		for i := range voxels {
			voxels[i] = math.Float32frombits(binary.LittleEndian.Uint32(payload[4*i:]))
		}
		return report, voxels
	}
	linearReport, linear := export(nifti.GeometryResampleLinear)
	cubicReport, cubic := export(nifti.GeometryResampleCubicBSpline)
	if !cubicReport.Resampled || cubicReport.Interpolation != "cubic-bspline" || cubicReport.Sidecar.Interpolation != "cubic-bspline" ||
		cubicReport.Datatype != nifti.DatatypeFloat32 {
		t.Fatalf("cubic report: resampled %v interpolation %q sidecar %q datatype %d",
			cubicReport.Resampled, cubicReport.Interpolation, cubicReport.Sidecar.Interpolation, cubicReport.Datatype)
	}
	if cubicReport.Dimensions != [4]int{3, 2, 6, 1} || cubicReport.Dimensions != linearReport.Dimensions ||
		cubicReport.IndexToRAS != linearReport.IndexToRAS || len(cubic) != len(linear) {
		t.Fatalf("cubic grid %v, linear %v", cubicReport.Dimensions, linearReport.Dimensions)
	}
	for gridZ, source := range map[int]int{0: 0, 1: 1, 2: 2, 4: 3, 5: 4} {
		for i, want := range pixels[source] {
			if got := cubic[gridZ*6+i]; math.Abs(float64(got)-float64(want)) > 1e-3 {
				t.Fatalf("cubic voxel %d of slice %d = %v, source %v", i, gridZ, got, want)
			}
		}
	}
	differs := false
	for i := 3 * 6; i < 4*6; i++ {
		differs = differs || cubic[i] != linear[i]
	}
	if !differs {
		t.Fatal("the cubic slice in the gap equals the linear one")
	}
	// Regular geometry is written unresampled under the cubic policy too.
	regular, err := nifti.WriteFiles(context.Background(), new(bytes.Buffer), files[:3], func() nifti.Options {
		options := nifti.DefaultOptions()
		options.Geometry = nifti.GeometryResampleCubicBSpline
		return options
	}())
	if err != nil || regular.Resampled || regular.Interpolation != "" {
		t.Fatalf("regular series: err %v, resampled %v with %q", err, regular.Resampled, regular.Interpolation)
	}
	invalid := nifti.DefaultOptions()
	invalid.Geometry = nifti.GeometryResampleCubicBSpline + 1
	if _, err := nifti.WriteFiles(context.Background(), new(bytes.Buffer), files, invalid); err == nil {
		t.Fatal("an unknown geometry policy was accepted")
	}
}
