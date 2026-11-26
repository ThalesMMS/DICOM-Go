package dicomtest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// ReadDCMTKDump reads the text dump format that DCMTK's dump2dcm accepts, as
// used by the worklist examples under testdata/upstream/dcmtk: one
// "(gggg,eeee) VR value" line per element, a bracketed or bare value, "#"
// comments, SQ elements whose items open with (fffe,e000) and close with
// (fffe,e00d), and sequences closed by (fffe,e0dd) (or by the next element
// at the enclosing level when the dump omits the delimiters). Backslashes
// separate values. A tag repeated at the same level keeps its first value,
// as dump2dcm does (DCMTK's wklist2.dump lists Accession Number twice). Only
// the value representations of those examples are supported; this is a
// test-fixture reader, not a dump2dcm replacement.
func ReadDCMTKDump(r io.Reader) ([]core.Element, error) {
	type frame struct {
		tag     core.Tag
		items   []core.DataSet
		current *core.DataSet
	}
	var top []core.Element
	var stack []*frame
	appendElement := func(element core.Element) error {
		target := &top
		if len(stack) > 0 {
			f := stack[len(stack)-1]
			if f.current == nil {
				return fmt.Errorf("element %s outside a sequence item", element.Tag())
			}
			target = &f.current.Elements
		}
		for _, existing := range *target {
			if existing.Tag() == element.Tag() {
				return nil
			}
		}
		*target = append(*target, element)
		return nil
	}
	closeSequence := func() error {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.current != nil {
			f.items = append(f.items, *f.current)
		}
		return appendElement(NewSequenceElement(f.tag, f.items...))
	}
	scanner := bufio.NewScanner(r)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tag, vr, value, err := parseDCMTKDumpLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		switch tag {
		case core.NewTag(0xFFFE, 0xE000):
			if len(stack) == 0 {
				return nil, fmt.Errorf("line %d: item outside a sequence", lineNumber)
			}
			f := stack[len(stack)-1]
			if f.current != nil {
				f.items = append(f.items, *f.current)
			}
			f.current = &core.DataSet{}
			continue
		case core.NewTag(0xFFFE, 0xE00D):
			if len(stack) == 0 || stack[len(stack)-1].current == nil {
				return nil, fmt.Errorf("line %d: item delimiter outside an item", lineNumber)
			}
			f := stack[len(stack)-1]
			f.items = append(f.items, *f.current)
			f.current = nil
			continue
		case core.NewTag(0xFFFE, 0xE0DD):
			if len(stack) == 0 {
				return nil, fmt.Errorf("line %d: sequence delimiter outside a sequence", lineNumber)
			}
			if err := closeSequence(); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			continue
		}
		if vr == core.VRSQ {
			stack = append(stack, &frame{tag: tag})
			continue
		}
		element, err := dcmtkDumpElement(tag, vr, value)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		if err := appendElement(element); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for len(stack) > 0 {
		if err := closeSequence(); err != nil {
			return nil, err
		}
	}
	return top, nil
}

func parseDCMTKDumpLine(line string) (core.Tag, core.VR, string, error) {
	if len(line) < 11 || line[0] != '(' || line[5] != ',' || line[10] != ')' {
		return core.Tag{}, "", "", fmt.Errorf("malformed tag in %q", line)
	}
	group, err := strconv.ParseUint(line[1:5], 16, 16)
	if err != nil {
		return core.Tag{}, "", "", err
	}
	element, err := strconv.ParseUint(line[6:10], 16, 16)
	if err != nil {
		return core.Tag{}, "", "", err
	}
	tag := core.NewTag(uint16(group), uint16(element))
	rest := strings.TrimSpace(line[11:])
	if tag.Group == 0xFFFE {
		return tag, "", "", nil
	}
	fields := strings.SplitN(rest, " ", 2)
	vr := core.VR(fields[0])
	value := ""
	if len(fields) == 2 {
		value = strings.TrimSpace(fields[1])
	}
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 {
			return core.Tag{}, "", "", fmt.Errorf("unterminated value in %q", line)
		}
		value = value[1:end]
	} else if i := strings.Index(value, "#"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return tag, vr, value, nil
}

func dcmtkDumpElement(tag core.Tag, vr core.VR, value string) (core.Element, error) {
	switch vr {
	case core.VRAE, core.VRCS, core.VRDA, core.VRDS, core.VRIS, core.VRLO, core.VRLT, core.VRPN,
		core.VRSH, core.VRST, core.VRTM, core.VRUI, core.VRDT, core.VRAS, core.VRUT, core.VRUC:
		values := []string{}
		if value != "" {
			values = strings.Split(value, `\`)
		}
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}, nil
	case core.VRUS:
		if value == "" {
			return core.NewRawElement(tag, vr, nil), nil
		}
		n, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return core.Element{}, err
		}
		return NewUShortElement(tag, uint16(n)), nil
	default:
		return core.Element{}, fmt.Errorf("unsupported VR %s for %s", vr, tag)
	}
}
