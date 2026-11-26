package imagefilter_test

import (
	"context"
	"image"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// itkHoughTransform2DLinesImageTest.cxx extracts several peaks with
// suppression. Each of these parallel lines has equivalent peaks at zero
// and in both neighboring angle bins. Signed rho flips across the seam.
// The candidate limit makes the equal-vote order observable.
func TestITKReferenceHoughParallelLines(t *testing.T) {
	edges := imagefilter.NewVolume([3]int{16, 8, 1}, [3]float64{.5, 1, 0})
	for y := 0; y < 8; y++ {
		edges.Data[edges.Index(4, y, 0)] = 1
		edges.Data[edges.Index(12, y, 0)] = 1
	}
	lines := []imagefilter.HoughLine{{RhoMM: 2, Votes: 8}, {RhoMM: 6, Votes: 8}}
	for _, tc := range []struct {
		name     string
		limit    int
		minVotes uint32
		want     []imagefilter.HoughLine
	}{
		{"suppress equivalent seam peaks", 4, 8, lines},
		{"tie and candidate cap", 1, 8, lines[:1]},
		{"support below the requested minimum", 4, 9, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := imagefilter.HoughLines2D(context.Background(), edges, imagefilter.HoughLineOptions{
				Hough2DOptions: imagefilter.Hough2DOptions{
					Region: image.Rect(0, 0, 16, 8), MinVotes: tc.minVotes, MaxCandidates: tc.limit,
				},
				RhoMinMM: -16, RhoMaxMM: 16, RhoStepMM: 1,
				ThetaMin: 0, ThetaMax: math.Pi, ThetaStep: math.Pi / 64,
				SuppressRhoMM: .5, SuppressTheta: .06,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Candidates, tc.want) || got.Report.EdgePixels != 16 {
				t.Fatalf("got peaks %v from %d edges, want %v from 16 edges", got.Candidates, got.Report.EdgePixels, tc.want)
			}
		})
	}
}

// itkHoughTransform2DCirclesImageTest.cxx checks multiple centers and radii
// after peak suppression. Concentric cardinal samples give each physical
// radius four exact votes on an anisotropic grid, while permitting
// suppression of close radii independently from center distance.
func TestITKReferenceHoughConcentricCircles(t *testing.T) {
	edges := imagefilter.NewVolume([3]int{25, 13, 1}, [3]float64{.5, 1, 0})
	for _, radius := range []int{2, 4} {
		for _, p := range [][2]int{{12 - 2*radius, 6}, {12 + 2*radius, 6}, {12, 6 - radius}, {12, 6 + radius}} {
			edges.Data[edges.Index(p[0], p[1], 0)] = 1
		}
	}
	center := [2]float64{6, 6}
	circles := []imagefilter.HoughCircle{
		{CenterMM: center, RadiusMM: 2, Votes: 4},
		{CenterMM: center, RadiusMM: 4, Votes: 4},
	}
	for _, tc := range []struct {
		name        string
		radiusLimit float64
		suppress    float64
		want        []imagefilter.HoughCircle
	}{
		{"preserve distinct physical radii", 4, 1, circles},
		{"suppress at exact radius distance", 4, 2, circles[:1]},
		{"exclude radius outside search interval", 3, 1, circles[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := imagefilter.HoughCircles2D(context.Background(), edges, imagefilter.HoughCircleOptions{
				Hough2DOptions: imagefilter.Hough2DOptions{
					Region: image.Rect(0, 0, 25, 13), MinVotes: 4, MaxCandidates: 4,
				},
				CenterMinMM: [2]float64{5, 5}, CenterMaxMM: [2]float64{7, 7}, CenterStepMM: 1,
				RadiusMinMM: 2, RadiusMaxMM: tc.radiusLimit, RadiusStepMM: 1, AngleBins: 8,
				SuppressCenterMM: 1, SuppressRadiusMM: tc.suppress,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Candidates, tc.want) || got.Report.EdgePixels != 8 {
				t.Fatalf("got peaks %v from %d edges, want %v from eight edges", got.Candidates, got.Report.EdgePixels, tc.want)
			}
		})
	}
}
