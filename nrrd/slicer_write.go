package nrrd

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/seg"
)

// Writing 3D Slicer segmentations (.seg.nrrd), from the segmentation file
// layout Slicer documents: a labelmap NRRD whose key/value pairs describe
// every segment (SegmentN_ID, _Name, _Color, _LabelValue, _Layer, _Extent,
// _Tags…) and the segmentation (Segmentation_MasterRepresentation…). Segments
// that overlap live in separate layers: a fourth, first ("list") axis whose
// samples are interleaved, as Slicer writes them.

// Slicer terminology context names written in SegmentN_Tags.
const (
	slicerCategoryTypeContext = "Segmentation category and type - DICOM master list"
	slicerAnatomicContext     = "Anatomic codes - DICOM master list"
)

// slicerFallbackColors colour segments without a recommended display colour.
var slicerFallbackColors = [][3]int{
	{128, 174, 128}, {241, 214, 145}, {177, 122, 101}, {111, 184, 210},
	{216, 101, 79}, {221, 130, 101}, {144, 238, 144}, {192, 104, 88},
}

// NewSlicerSegment describes a DICOM segment as segment labelValue of
// labelmap layer: its label (or "Segment N") as name, its recommended display
// colour (a fixed palette colour when it has none) and, when coded, its
// property category and type as a Slicer terminology entry.
func NewSlicerSegment(segment seg.Segment, labelValue, layer int) SlicerSegment {
	name := strings.TrimSpace(segment.Label)
	if name == "" {
		name = fmt.Sprintf("Segment %d", segment.Number)
	}
	rgb := slicerFallbackColors[(max(segment.Number, 1)-1)%len(slicerFallbackColors)]
	if segment.RecommendedDisplayCIELab != ([3]uint16{}) {
		rgb = seg.DICOMCIELabToRGB(segment.RecommendedDisplayCIELab)
	}
	out := SlicerSegment{
		Name: name, LabelValue: labelValue, Layer: layer,
		Category: segment.SegmentedPropertyCategory, Type: segment.SegmentedPropertyType,
	}
	for i, c := range rgb {
		out.Color[i] = math.Min(math.Max(float64(c)/255, 0), 1)
	}
	return out
}

// slicerTags is the SegmentN_Tags value of s: its own Tags when set,
// otherwise an in-progress status and the terminology entry of its codes.
func (s SlicerSegment) slicerTags() string {
	if strings.TrimSpace(s.Tags) != "" {
		return s.Tags
	}
	tags := "Segmentation.Status:inprogress|"
	if s.Category.CodeValue == "" || s.Type.CodeValue == "" {
		return tags
	}
	code := func(c seg.CodedEntry) string {
		clean := strings.NewReplacer("~", " ", "^", " ", "|", " ", "\n", " ", "\r", " ")
		return clean.Replace(c.CodingSchemeDesignator) + "^" + clean.Replace(c.CodeValue) + "^" + clean.Replace(c.CodeMeaning)
	}
	return tags + "TerminologyEntry:" + slicerCategoryTypeContext + "~" + code(s.Category) + "~" + code(s.Type) +
		"~^^~" + slicerAnatomicContext + "~^^~^^|"
}

// WriteSlicerSegmentation writes labelmap as a 3D Slicer segmentation
// (.seg.nrrd) in the left-posterior-superior space of its geometry. The
// labelmap holds unsigned 8- or 16-bit labels; a fourth dimension holds the
// layers of overlapping segments and is written as Slicer's first, list
// axis. Each segment names its layer and non-zero label value; every
// non-zero label of the labelmap must belong to a segment. Segment indices
// follow the order of segments; empty IDs become "Segment_N" and empty names
// the ID. SegmentN_Extent is computed from the voxels (inclusive i, j, k
// bounds; "0 -1 0 -1 0 -1" for an empty segment). options.KeyValues may add
// pairs that do not collide with the segmentation keys.
func WriteSlicerSegmentation(w io.Writer, labelmap *nifti.Volume, segments []SlicerSegment, options WriteOptions) error {
	return WriteSlicerSegmentationContext(context.Background(), w, labelmap, segments, options)
}

// WriteSlicerSegmentationContext is WriteSlicerSegmentation with cancellation
// during metadata preparation, extent calculation and payload writing.
func WriteSlicerSegmentationContext(ctx context.Context, w io.Writer, labelmap *nifti.Volume, segments []SlicerSegment, options WriteOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if labelmap == nil || w == nil {
		return fmt.Errorf("%w: nil labelmap or destination", ErrMalformedHeader)
	}
	maxLabel := 0
	switch labelmap.Header.Datatype {
	case nifti.DatatypeUint8:
		maxLabel = math.MaxUint8
	case nifti.DatatypeUint16:
		maxLabel = math.MaxUint16
	default:
		return fmt.Errorf("%w: segmentation datatype %d (want unsigned 8- or 16-bit labels)", ErrUnsupported, labelmap.Header.Datatype)
	}
	if labelmap.Scaling.Applied {
		return fmt.Errorf("%w: value scaling", ErrUnsupported)
	}
	if len(segments) == 0 {
		return fmt.Errorf("%w: no segments", ErrMalformedHeader)
	}
	d := labelmap.Dimensions
	layers := max(d[3], 1)
	type slot struct{ layer, label int }
	bySlot := map[slot]int{}
	ids := map[string]bool{}
	written := make([]SlicerSegment, len(segments))
	for index, segment := range segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		segment.Index = index
		segment.ID = strings.TrimSpace(segment.ID)
		if segment.ID == "" {
			segment.ID = "Segment_" + strconv.Itoa(index+1)
		}
		if strings.TrimSpace(segment.Name) == "" {
			segment.Name = segment.ID
		}
		switch {
		case ids[segment.ID]:
			return fmt.Errorf("%w: duplicate segment ID %q", ErrMalformedHeader, truncate(segment.ID))
		case segment.Layer < 0 || segment.Layer >= layers:
			return fmt.Errorf("%w: segment %d layer %d of %d", ErrMalformedHeader, index, segment.Layer, layers)
		case segment.LabelValue < 1 || segment.LabelValue > maxLabel:
			return fmt.Errorf("%w: segment %d label value %d", ErrMalformedHeader, index, segment.LabelValue)
		}
		for _, c := range segment.Color {
			if !(c >= 0 && c <= 1) {
				return fmt.Errorf("%w: segment %d colour %v", ErrMalformedHeader, index, segment.Color)
			}
		}
		key := slot{segment.Layer, segment.LabelValue}
		if _, taken := bySlot[key]; taken {
			return fmt.Errorf("%w: two segments use label %d of layer %d", ErrMalformedHeader, segment.LabelValue, segment.Layer)
		}
		bySlot[key] = index
		ids[segment.ID] = true
		written[index] = segment
	}

	// Extents, and a check that every label belongs to a segment.
	extents := make([][6]int, len(written))
	for index := range extents {
		extents[index] = [6]int{math.MaxInt, -1, math.MaxInt, -1, math.MaxInt, -1}
	}
	for layer := 0; layer < layers; layer++ {
		voxel := labelmap.Index(0, 0, 0, layer)
		for k := 0; k < d[2]; k++ {
			for j := 0; j < d[1]; j++ {
				for i := 0; i < d[0]; i++ {
					if voxel%8192 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					label := int(labelmap.Stored(voxel))
					voxel++
					if label == 0 {
						continue
					}
					index, ok := bySlot[slot{layer, label}]
					if !ok {
						return fmt.Errorf("%w: label %d of layer %d has no segment", ErrMalformedHeader, label, layer)
					}
					e := &extents[index]
					e[0], e[1] = min(e[0], i), max(e[1], i)
					e[2], e[3] = min(e[2], j), max(e[3], j)
					e[4], e[5] = min(e[4], k), max(e[5], k)
				}
			}
		}
	}

	keyValues := map[string]string{
		"Segmentation_MasterRepresentation":         "Binary labelmap",
		"Segmentation_ContainedRepresentationNames": "Binary labelmap|",
		"Segmentation_ReferenceImageExtentOffset":   "0 0 0",
	}
	for index, segment := range written {
		if err := ctx.Err(); err != nil {
			return err
		}
		prefix := "Segment" + strconv.Itoa(index) + "_"
		extent := "0 -1 0 -1 0 -1"
		if e := extents[index]; e[1] >= 0 {
			extent = fmt.Sprintf("%d %d %d %d %d %d", e[0], e[1], e[2], e[3], e[4], e[5])
		}
		color := make([]string, 3)
		for i, c := range segment.Color {
			color[i] = strconv.FormatFloat(c, 'g', -1, 64)
		}
		for key, value := range map[string]string{
			"ID": segment.ID, "Name": segment.Name, "Color": strings.Join(color, " "),
			"ColorAutoGenerated": "0", "NameAutoGenerated": "0",
			"LabelValue": strconv.Itoa(segment.LabelValue), "Layer": strconv.Itoa(segment.Layer),
			"Extent": extent, "Tags": segment.slicerTags(),
		} {
			keyValues[prefix+key] = value
		}
	}
	for key, value := range options.KeyValues {
		if _, taken := keyValues[key]; taken || strings.HasPrefix(key, "Segment") {
			return fmt.Errorf("%w: key %q collides with the segmentation keys", ErrMalformedHeader, truncate(key))
		}
		keyValues[key] = value
	}
	options.KeyValues = keyValues
	options.FourthAxisKind = "list"
	header, err := headerSpec{
		datatype: labelmap.Header.Datatype, dimensions: d, indexToLPS: labelmap.Geometry.IndexToLPS,
		options: options, listFirst: true,
	}.header()
	if err != nil {
		return err
	}
	payload := labelmap.WritePayload
	if layers > 1 {
		payload = func(out io.Writer) error { return writeInterleavedLayers(out, labelmap, layers) }
	}
	_, err = writeEncoded(slicerContextWriter{ctx, w}, header, options.Raw, func(out io.Writer) error {
		return payload(slicerContextWriter{ctx, out})
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

type slicerContextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w slicerContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

// writeInterleavedLayers writes the labels with the layer varying fastest.
func writeInterleavedLayers(out io.Writer, labelmap *nifti.Volume, layers int) error {
	width := labelmap.BytesPerVoxel()
	spatial := labelmap.Len() / layers
	buffer := make([]byte, 0, 64<<10)
	for n := 0; n < spatial; n++ {
		for layer := 0; layer < layers; layer++ {
			value := uint16(labelmap.Stored(n + layer*spatial))
			if width == 1 {
				buffer = append(buffer, byte(value))
			} else {
				buffer = append(buffer, byte(value), byte(value>>8))
			}
		}
		if len(buffer) >= 63<<10 {
			if _, err := out.Write(buffer); err != nil {
				return err
			}
			buffer = buffer[:0]
		}
	}
	_, err := out.Write(buffer)
	return err
}
