package nifti

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/seg"
)

// Label tables name and colour the labels of an integer labelmap (#1140).
// Two plain-text layouts are read:
//
//   - 3D Slicer colour tables (.ctbl, .txt) and FreeSurfer lookup tables:
//     "value name R G B [A]" with 0..255 components;
//   - ITK-SNAP label descriptions: "value R G B A VIS MSH \"name\"" with an
//     alpha in 0..1.
//
// Lines starting with "#" are comments. Value 0 (background) is ignored.

// ErrLabelTable reports a malformed label table.
var ErrLabelTable = errors.New("dicom/nifti: malformed label table")

// Label table limits.
const (
	MaxLabelTableBytes = 4 << 20
	maxLabelValue      = 65535
)

// LabelEntry is one named, coloured label.
type LabelEntry struct {
	Name string
	RGB  [3]uint8
}

// LabelTable maps label values to their entries.
type LabelTable map[int]LabelEntry

// ReadLabelTable parses a label table of at most MaxLabelTableBytes.
func ReadLabelTable(r io.Reader) (LabelTable, error) {
	table := LabelTable{}
	scanner := bufio.NewScanner(io.LimitReader(r, MaxLabelTableBytes+1))
	scanner.Buffer(make([]byte, 0, 4096), 64<<10)
	read := 0
	for line := 1; scanner.Scan(); line++ {
		read += len(scanner.Bytes()) + 1
		if read > MaxLabelTableBytes+1 {
			return nil, fmt.Errorf("%w: larger than %d bytes", ErrLabelTable, MaxLabelTableBytes)
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		value, entry, err := parseLabelLine(text)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrLabelTable, line, err)
		}
		if value != 0 {
			table[value] = entry
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLabelTable, err)
	}
	if read > MaxLabelTableBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrLabelTable, MaxLabelTableBytes)
	}
	return table, nil
}

func parseLabelLine(text string) (int, LabelEntry, error) {
	if quote := strings.IndexByte(text, '"'); quote >= 0 {
		// ITK-SNAP: value R G B A VIS MSH "name"
		end := strings.LastIndexByte(text, '"')
		if end <= quote {
			return 0, LabelEntry{}, errors.New("unterminated label name")
		}
		fields := strings.Fields(text[:quote])
		if len(fields) < 4 {
			return 0, LabelEntry{}, errors.New("expected value and R G B before the name")
		}
		value, rgb, err := parseValueRGB(fields[:4])
		if err != nil {
			return 0, LabelEntry{}, err
		}
		return value, LabelEntry{Name: strings.TrimSpace(text[quote+1 : end]), RGB: rgb}, nil
	}
	// Slicer / FreeSurfer: value name R G B [A]
	fields := strings.Fields(text)
	if len(fields) < 5 {
		return 0, LabelEntry{}, errors.New("expected value, name and R G B")
	}
	value, rgb, err := parseValueRGB([]string{fields[0], fields[2], fields[3], fields[4]})
	if err != nil {
		return 0, LabelEntry{}, err
	}
	return value, LabelEntry{Name: strings.ReplaceAll(fields[1], "_", " "), RGB: rgb}, nil
}

func parseValueRGB(fields []string) (int, [3]uint8, error) {
	value, err := strconv.Atoi(fields[0])
	if err != nil || value < 0 || value > maxLabelValue {
		return 0, [3]uint8{}, fmt.Errorf("label value %q is not in 0..%d", fields[0], maxLabelValue)
	}
	var rgb [3]uint8
	for i, field := range fields[1:4] {
		component, err := strconv.ParseFloat(field, 64)
		if err != nil || math.IsNaN(component) || component < 0 || component > 255 {
			return 0, [3]uint8{}, fmt.Errorf("colour component %q is not in 0..255", field)
		}
		rgb[i] = uint8(math.Round(component))
	}
	return value, rgb, nil
}

// Segments returns the SEG segment definitions of the table's labels, for
// SEGOptions.Segments: each named label keeps its name and colour, with the
// generic ROI category and type codes; numbers are assigned by SEGDocument.
func (t LabelTable) Segments() map[int]seg.Segment {
	if len(t) == 0 {
		return nil
	}
	values := make([]int, 0, len(t))
	for value := range t {
		values = append(values, value)
	}
	sort.Ints(values)
	segments := make(map[int]seg.Segment, len(values))
	for _, value := range values {
		entry := t[value]
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = fmt.Sprintf("Label %d", value)
		}
		segments[value] = seg.Segment{
			Label:                     name,
			AlgorithmType:             seg.AlgorithmManual,
			RecommendedDisplayCIELab:  seg.RGBToDICOMCIELab([3]int{int(entry.RGB[0]), int(entry.RGB[1]), int(entry.RGB[2])}),
			SegmentedPropertyCategory: seg.GenericROICategoryCode(),
			SegmentedPropertyType:     seg.GenericROITypeCode(),
		}
	}
	return segments
}
