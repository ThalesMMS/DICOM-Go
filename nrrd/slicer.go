package nrrd

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/seg"
)

// SlicerSegment is one segment of a 3D Slicer segmentation (.seg.nrrd), from
// its SegmentN_* key/value pairs.
type SlicerSegment struct {
	Index      int
	ID         string
	Name       string
	LabelValue int
	// Layer is the index of the labelmap layer holding the segment: the
	// fourth dimension of a multi-layer file (overlapping segments).
	Layer int
	// Color is the display colour in [0, 1].
	Color [3]float64
	Tags  string
	// Category and Type come from the terminology entry in Tags, when
	// present.
	Category, Type seg.CodedEntry
}

// SlicerSegments lists the segments a Slicer segmentation declares, in index
// order. A file without SegmentN_ keys returns none.
func (h Header) SlicerSegments() ([]SlicerSegment, error) {
	indices := map[int]bool{}
	for key := range h.KeyValues {
		if !strings.HasPrefix(key, "Segment") {
			continue
		}
		number, _, ok := strings.Cut(strings.TrimPrefix(key, "Segment"), "_")
		if !ok {
			continue
		}
		if index, err := strconv.Atoi(number); err == nil && index >= 0 && index < 1<<16 {
			indices[index] = true
		}
	}
	var out []SlicerSegment
	for index := range indices {
		prefix := "Segment" + strconv.Itoa(index) + "_"
		value := func(name string) string { return strings.TrimSpace(h.KeyValues[prefix+name]) }
		segment := SlicerSegment{Index: index, ID: value("ID"), Name: value("Name"), Tags: value("Tags"), LabelValue: 1}
		if text := value("LabelValue"); text != "" {
			label, err := strconv.Atoi(text)
			if err != nil || label < 1 || label > math.MaxUint16 {
				return nil, fmt.Errorf("%w: %sLabelValue %q", ErrMalformedHeader, prefix, truncate(text))
			}
			segment.LabelValue = label
		}
		if text := value("Layer"); text != "" {
			layer, err := strconv.Atoi(text)
			if err != nil || layer < 0 {
				return nil, fmt.Errorf("%w: %sLayer %q", ErrMalformedHeader, prefix, truncate(text))
			}
			segment.Layer = layer
		}
		if parts := strings.Fields(value("Color")); len(parts) == 3 {
			for i, part := range parts {
				c, err := strconv.ParseFloat(part, 64)
				if err != nil || c < 0 || c > 1 {
					return nil, fmt.Errorf("%w: %sColor", ErrMalformedHeader, prefix)
				}
				segment.Color[i] = c
			}
		}
		segment.Category, segment.Type = terminologyEntry(segment.Tags)
		out = append(out, segment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// terminologyEntry reads the category and type codes of a Slicer
// "TerminologyEntry:context~scheme^value^meaning~scheme^value^meaning~…" tag.
func terminologyEntry(tags string) (category, typ seg.CodedEntry) {
	for _, tag := range strings.Split(tags, "|") {
		value, ok := strings.CutPrefix(tag, "TerminologyEntry:")
		if !ok {
			continue
		}
		parts := strings.Split(value, "~")
		code := func(text string) seg.CodedEntry {
			fields := strings.SplitN(text, "^", 3)
			if len(fields) != 3 || fields[0] == "" || fields[1] == "" {
				return seg.CodedEntry{}
			}
			return seg.CodedEntry{CodingSchemeDesignator: fields[0], CodeValue: fields[1], CodeMeaning: fields[2]}
		}
		if len(parts) > 2 {
			return code(parts[1]), code(parts[2])
		}
	}
	return seg.CodedEntry{}, seg.CodedEntry{}
}

// Segment is the DICOM segment definition of s: its name, recommended
// display colour and, when the terminology entry has them, its category and
// type codes.
func (s SlicerSegment) Segment() seg.Segment {
	rgb := [3]int{}
	for i, c := range s.Color {
		rgb[i] = int(math.Round(c * 255))
	}
	return seg.Segment{
		Label:                     s.Name,
		AlgorithmType:             seg.AlgorithmManual,
		RecommendedDisplayCIELab:  seg.RGBToDICOMCIELab(rgb),
		SegmentedPropertyCategory: s.Category,
		SegmentedPropertyType:     s.Type,
	}
}

// SEGDocument converts one labelmap layer of a Slicer segmentation to a
// binary DICOM Segmentation referencing the source series, with the
// segments' names, colours and codes: the layer is the time index of the
// volume (0 for a single-layer file). options.Segments and TimeIndex are
// filled from the file.
func (img *Image) SEGDocument(layer int, options nifti.SEGOptions) (*seg.Document, nifti.SEGReport, error) {
	if img == nil || img.Volume == nil {
		return nil, nifti.SEGReport{}, fmt.Errorf("%w: nil image", ErrMalformedHeader)
	}
	segments, err := img.Header.SlicerSegments()
	if err != nil {
		return nil, nifti.SEGReport{}, err
	}
	if layer < 0 || layer >= img.Volume.Dimensions[3] {
		return nil, nifti.SEGReport{}, fmt.Errorf("%w: layer %d of %d", ErrMalformedHeader, layer, img.Volume.Dimensions[3])
	}
	if len(segments) > 0 {
		options.Segments = map[int]seg.Segment{}
		for _, segment := range segments {
			if segment.Layer == layer {
				options.Segments[segment.LabelValue] = segment.Segment()
			}
		}
	}
	options.TimeIndex = layer
	return img.Volume.SEGDocument(options)
}
