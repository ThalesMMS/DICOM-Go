package render

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
)

// roundedOrientation is the Image Orientation (Patient) of a rotation about
// an oblique axis, written with the given number of decimals as scanners
// write DS values: slightly off orthonormal.
func roundedOrientation(decimals int) []float64 {
	// Rows along a unit vector, columns along another unit vector orthogonal
	// to it (exact before rounding).
	row := Vec3{X: 0.8, Y: 0.36, Z: 0.48}
	column := Vec3{X: -0.6, Y: 0.48, Z: 0.64}
	scale := math.Pow(10, float64(decimals))
	round := func(v float64) float64 { return math.Round(v*scale) / scale }
	return []float64{round(row.X), round(row.Y), round(row.Z), round(column.X), round(column.Y), round(column.Z)}
}

func obliqueStack(iop func(z int) []float64) *Stack {
	stack := gradientColumnStackWithSpacing(8, 8, 5, 0.7, 0.7, 2)
	for z, frame := range stack.Frames {
		frame.ImageOrientation = iop(z)
		frame.ImagePosition = []float64{0, 0, 0}
		normal := Vec3{X: 0.8, Y: 0.36, Z: 0.48}.Cross(Vec3{X: -0.6, Y: 0.48, Z: 0.64})
		position := normal.Scale(2 * float64(z))
		frame.ImagePosition = []float64{position.X, position.Y, position.Z}
	}
	return stack
}

// TestNonOrthonormalOrientationIsFlagged covers #1111. Direction cosines
// written with limited precision are accepted and yield an orthonormal,
// right-handed basis (the contract of ITK's itkGDCMImageIOOrthoDirTest, whose
// reader must return a valid rotation); cosines beyond
// object.ImageOrientationTolerance fail the volume with
// GeometryIssueNonOrthonormalOrientation instead of being orthogonalized.
func TestNonOrthonormalOrientationIsFlagged(t *testing.T) {
	for _, decimals := range []int{4, 6} {
		stack := obliqueStack(func(int) []float64 { return roundedOrientation(decimals) })
		assessment := stack.GeometryAssessment()
		if assessment.Disposition == GeometryUnsupported {
			t.Fatalf("%d decimals: unsupported (%q)", decimals, assessment.FallbackReason)
		}
		deviation, _ := object.ImageOrientationDeviation(roundedOrientation(decimals))
		geometry := assessment.Geometry
		if geometry.Metrics.MaximumOrientationDeviation != deviation || deviation == 0 {
			t.Fatalf("%d decimals: reported deviation %g, want %g", decimals, geometry.Metrics.MaximumOrientationDeviation, deviation)
		}
		row, column, normal := geometry.RowDir, geometry.ColDir, geometry.Normal
		if math.Abs(row.Length()-1) > 1e-12 || math.Abs(column.Length()-1) > 1e-12 || math.Abs(row.Dot(column)) > 1e-12 ||
			row.Cross(column).Sub(normal).Length() > 1e-12 {
			t.Fatalf("%d decimals: basis %v %v %v is not an orthonormal rotation", decimals, row, column, normal)
		}
		if _, err := BuildVolume(stack); err != nil {
			t.Fatalf("%d decimals: %v", decimals, err)
		}
	}

	// Frame 3 has a skewed column (dot 0.002) and frame 4 a stretched row.
	for _, tc := range []struct {
		name  string
		frame int
		skew  func([]float64) []float64
	}{
		{"skewed", 2, func(iop []float64) []float64 {
			iop[3] += 0.002 * 0.8
			iop[4] += 0.002 * 0.36
			iop[5] += 0.002 * 0.48
			return iop
		}},
		{"stretched", 3, func(iop []float64) []float64 { iop[0] *= 1.01; iop[1] *= 1.01; iop[2] *= 1.01; return iop }},
	} {
		stack := obliqueStack(func(z int) []float64 {
			iop := roundedOrientation(6)
			if z == tc.frame {
				return tc.skew(iop)
			}
			return iop
		})
		assessment := stack.GeometryAssessment()
		if assessment.Disposition != GeometryUnsupported || assessment.FallbackReason != GeometryIssueNonOrthonormalOrientation ||
			assessment.FailureFrame != tc.frame+1 || assessment.Geometry.Metrics.MaximumOrientationDeviation <= object.ImageOrientationTolerance {
			t.Fatalf("%s: %+v", tc.name, assessment)
		}
		var guardrail *GeometryGuardrailError
		if _, err := BuildVolume(stack); !errors.As(err, &guardrail) || guardrail.Reason != GeometryIssueNonOrthonormalOrientation || guardrail.FrameIndex != tc.frame+1 {
			t.Fatalf("%s: BuildVolume error %v", tc.name, err)
		}
	}
}
