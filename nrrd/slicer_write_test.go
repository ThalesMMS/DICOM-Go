package nrrd_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
	"github.com/ThalesMMS/dicom-go/seg"
)

// labelVolume builds an unsigned 8-bit labelmap on obliqueAffine from
// label(i, j, k, layer).
func labelVolume(t *testing.T, dims [4]int, label func(i, j, k, layer int) uint8) *nifti.Volume {
	t.Helper()
	var payload []byte
	for layer := 0; layer < max(dims[3], 1); layer++ {
		for k := 0; k < dims[2]; k++ {
			for j := 0; j < dims[1]; j++ {
				for i := 0; i < dims[0]; i++ {
					payload = append(payload, label(i, j, k, layer))
				}
			}
		}
	}
	volume, err := nifti.NewVolume(context.Background(), bytes.NewReader(payload), nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint8, Dimensions: dims, IndexToLPS: obliqueAffine()})
	if err != nil {
		t.Fatal(err)
	}
	return volume
}

func liverSegment() seg.Segment {
	return seg.Segment{
		Number: 1, Label: "Liver", RecommendedDisplayCIELab: seg.RGBToDICOMCIELab([3]int{230, 177, 140}),
		SegmentedPropertyCategory: seg.CodedEntry{CodingSchemeDesignator: "SCT", CodeValue: "123037004", CodeMeaning: "Anatomical Structure"},
		SegmentedPropertyType:     seg.CodedEntry{CodingSchemeDesignator: "SCT", CodeValue: "10200004", CodeMeaning: "Liver"},
	}
}

func TestWriteSlicerSegmentationRoundTrip(t *testing.T) {
	// Layer 0: Liver (1) and Vessel (2); layer 1: an overlapping Tumor (1).
	// The Cyst segment of layer 1 (label 2) is empty.
	twoLayers := labelVolume(t, [4]int{4, 3, 2, 2}, func(i, j, k, layer int) uint8 {
		switch {
		case layer == 0 && i >= 1 && i <= 2 && j <= 1:
			return 1
		case layer == 0 && i == 3 && j == 2 && k == 1:
			return 2
		case layer == 1 && i == 2 && j == 1:
			return 1
		}
		return 0
	})
	segments := []nrrd.SlicerSegment{
		nrrd.NewSlicerSegment(liverSegment(), 1, 0),
		nrrd.NewSlicerSegment(seg.Segment{Number: 2, Label: "Vessel"}, 2, 0),
		nrrd.NewSlicerSegment(seg.Segment{Number: 3, Label: "Tumor", RecommendedDisplayCIELab: seg.RGBToDICOMCIELab([3]int{51, 204, 51})}, 1, 1),
		nrrd.NewSlicerSegment(seg.Segment{Number: 4}, 2, 1),
	}
	oneLayer := labelVolume(t, [4]int{4, 3, 2, 1}, func(i, j, k, _ int) uint8 {
		if i == 0 && k == 1 {
			return 1
		}
		return 0
	})
	for _, tc := range []struct {
		name     string
		volume   *nifti.Volume
		segments []nrrd.SlicerSegment
		raw      bool
		kinds    string
		extents  []string
	}{
		{"two layers gzip", twoLayers, segments, false, "list domain domain domain", []string{"1 2 0 1 0 1", "3 3 2 2 1 1", "2 2 1 1 0 1", "0 -1 0 -1 0 -1"}},
		{"two layers raw", twoLayers, segments, true, "list domain domain domain", []string{"1 2 0 1 0 1", "3 3 2 2 1 1", "2 2 1 1 0 1", "0 -1 0 -1 0 -1"}},
		{"one layer", oneLayer, segments[:1], false, "domain domain domain", []string{"0 0 0 2 1 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var encoded bytes.Buffer
			if err := nrrd.WriteSlicerSegmentation(&encoded, tc.volume, tc.segments, nrrd.WriteOptions{Raw: tc.raw}); err != nil {
				t.Fatal(err)
			}
			image, err := nrrd.Read(context.Background(), bytes.NewReader(encoded.Bytes()), nrrd.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			requireSameVolume(t, image.Volume, tc.volume)
			if got := image.Header.Fields["kinds"]; got != tc.kinds {
				t.Fatalf("kinds %q, want %q", got, tc.kinds)
			}
			values := image.Header.KeyValues
			if values["Segmentation_MasterRepresentation"] != "Binary labelmap" || values["Segmentation_ReferenceImageExtentOffset"] != "0 0 0" {
				t.Fatalf("segmentation keys %v", values)
			}
			read, err := image.Header.SlicerSegments()
			if err != nil || len(read) != len(tc.segments) {
				t.Fatalf("segments %+v %v", read, err)
			}
			for index, want := range tc.segments {
				got := read[index]
				if got.Index != index || got.Name != want.Name || got.LabelValue != want.LabelValue || got.Layer != want.Layer ||
					got.Category != want.Category || got.Type != want.Type || got.ID != "Segment_"+string(rune('1'+index)) {
					t.Fatalf("segment %d = %+v, want %+v", index, got, want)
				}
				for c := range want.Color {
					if math.Abs(got.Color[c]-want.Color[c]) > 1e-15 {
						t.Fatalf("segment %d colour %v, want %v", index, got.Color, want.Color)
					}
				}
				if extent := values["Segment"+string(rune('0'+index))+"_Extent"]; extent != tc.extents[index] {
					t.Fatalf("segment %d extent %q, want %q", index, extent, tc.extents[index])
				}
			}
		})
	}

	// The segment definitions come back as DICOM segments.
	liver := segments[0].Segment()
	if liver.Label != "Liver" || liver.SegmentedPropertyType.CodeValue != "10200004" {
		t.Fatalf("liver segment %+v", liver)
	}
	if rgb := seg.DICOMCIELabToRGB(liver.RecommendedDisplayCIELab); math.Abs(float64(rgb[0]-230)) > 1 || math.Abs(float64(rgb[2]-140)) > 1 {
		t.Fatalf("liver colour %v", rgb)
	}
	if segments[3].Name != "Segment 4" || segments[3].Color == ([3]float64{}) || strings.Contains(segments[1].Tags, "TerminologyEntry") {
		t.Fatalf("defaults %+v %+v", segments[3], segments[1])
	}
}

func TestWriteSlicerSegmentationRejectsInconsistentInput(t *testing.T) {
	labels := labelVolume(t, [4]int{2, 2, 1, 1}, func(i, _, _, _ int) uint8 { return uint8(i) })
	liver := nrrd.NewSlicerSegment(liverSegment(), 1, 0)
	int16Labels := int16Volume(t, [4]int{2, 2, 1, 1})
	for name, tc := range map[string]struct {
		volume   *nifti.Volume
		segments []nrrd.SlicerSegment
		options  nrrd.WriteOptions
		err      error
	}{
		"signed labels":   {int16Labels, []nrrd.SlicerSegment{liver}, nrrd.WriteOptions{}, nrrd.ErrUnsupported},
		"no segments":     {labels, nil, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"unowned label":   {labels, []nrrd.SlicerSegment{{Name: "Other", LabelValue: 2}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"shared label":    {labels, []nrrd.SlicerSegment{liver, liver}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"missing layer":   {labels, []nrrd.SlicerSegment{{LabelValue: 1, Layer: 1}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"zero label":      {labels, []nrrd.SlicerSegment{{LabelValue: 0}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"colour range":    {labels, []nrrd.SlicerSegment{{LabelValue: 1, Color: [3]float64{2, 0, 0}}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"duplicate ID":    {labels, []nrrd.SlicerSegment{{ID: "a", LabelValue: 1}, {ID: "a", LabelValue: 2}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
		"key collision":   {labels, []nrrd.SlicerSegment{liver}, nrrd.WriteOptions{KeyValues: map[string]string{"Segment0_Name": "x"}}, nrrd.ErrMalformedHeader},
		"name line break": {labels, []nrrd.SlicerSegment{{Name: "a\rb", LabelValue: 1}}, nrrd.WriteOptions{}, nrrd.ErrMalformedHeader},
	} {
		var sink bytes.Buffer
		if err := nrrd.WriteSlicerSegmentation(&sink, tc.volume, tc.segments, tc.options); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
}
