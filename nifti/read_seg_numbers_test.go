package nifti

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/seg"
)

func TestSegmentDefinitionsReservesExplicitNumbers(t *testing.T) {
	labels := []int{1, 2, 5}
	for _, test := range []struct {
		name    string
		numbers []int
		want    []int
	}{
		{name: "defaults", want: []int{1, 2, 3}},
		{name: "explicit numbers", numbers: []int{1, 7, 3}, want: []int{1, 7, 3}},
		{name: "later explicit number", numbers: []int{0, 1, 0}, want: []int{2, 1, 3}},
		{name: "multiple reserved numbers", numbers: []int{0, 1, 2}, want: []int{3, 1, 2}},
		{name: "skip assigned numbers", numbers: []int{2, 0, 0}, want: []int{2, 3, 4}},
		{name: "duplicate explicit numbers", numbers: []int{1, 1, 0}},
		{name: "negative explicit number", numbers: []int{0, -1, 0}},
		{name: "explicit number overflow", numbers: []int{0, math.MaxUint16 + 1, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			definitions := map[int]seg.Segment{}
			for index, number := range test.numbers {
				definitions[labels[index]] = seg.Segment{Number: number}
			}
			segments, numbers, err := segmentDefinitions(labels, definitions)
			if test.want == nil {
				if !errors.Is(err, ErrSEGInvalidLabel) || segments != nil || numbers != nil {
					t.Fatalf("segmentDefinitions() = %v, %v, %v; want invalid label and no result", segments, numbers, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for index, label := range labels {
				if numbers[label] != test.want[index] || segments[label].Number != test.want[index] {
					t.Errorf("label %d has number %d (segment %d), want %d", label, numbers[label], segments[label].Number, test.want[index])
				}
			}
		})
	}
}

func TestSegmentDefinitionsAutomaticNumberLimit(t *testing.T) {
	labels := make([]int, math.MaxUint16)
	for index := range labels {
		labels[index] = index + 1
	}
	t.Run("last number available", func(t *testing.T) {
		_, numbers, err := segmentDefinitions(labels, nil)
		if err != nil {
			t.Fatal(err)
		}
		if numbers[math.MaxUint16] != math.MaxUint16 {
			t.Fatalf("last segment number = %d, want %d", numbers[math.MaxUint16], math.MaxUint16)
		}
	})
	t.Run("last number reserved", func(t *testing.T) {
		definitions := map[int]seg.Segment{1: {Number: math.MaxUint16}}
		segments, numbers, err := segmentDefinitions(labels, definitions)
		if !errors.Is(err, ErrSEGInvalidLabel) || segments != nil || numbers != nil {
			t.Fatalf("segmentDefinitions() returned error %v and partial results %t; want invalid label and no result", err, segments != nil || numbers != nil)
		}
	})
}
