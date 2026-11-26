package headers

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Kind identifies one of the metadata-only formats implemented here.
type Kind string

const (
	PhilipsSDS    Kind = "philips-sds"
	ToshibaCT3    Kind = "toshiba-ct3"
	ToshibaMECMR3 Kind = "toshiba-mec-mr3"
	PMTF          Kind = "pmtf"
	CanonPMTF     Kind = "canon-pmtf"
	ToshibaPMTF   Kind = "toshiba-pmtf"
	CanonPAS      Kind = "canon-pas"
)

type headerSpec struct {
	kind           Kind
	title, creator string
	group          uint16
	offset         uint8
}

var specs = []headerSpec{
	{PhilipsSDS, "Philips SDS", "Philips MR Imaging DD 002", 0x2005, 0x32},
	{ToshibaCT3, "Toshiba CT Private Data 2", "TOSHIBA_MEC_CT3", 0x7005, 0x10},
	{ToshibaMECMR3, "Toshiba MEC MR3 Original Data", "TOSHIBA_MEC_MR3", 0x700d, 0x08},
	{PMTF, "PMTF Information Data", "PMTF INFORMATION DATA", 0x0029, 1},
	{CanonPMTF, "Canon MEC MR3 PMTF", "CANON_MEC_MR3", 0x0029, 1},
	{ToshibaPMTF, "Toshiba MEC MR3 PMTF", "TOSHIBA_MEC_MR3", 0x0029, 1},
	{ToshibaPMTF, "Toshiba MEC MR3 PMTF", "TOSHIBA_MEC_MR3", 0x0029, 2},
	{CanonPAS, "Canon PAS Reproduct Information (XML)", "CANON_MEC_MR3^10", 0x700d, 0x19},
}

// Header is a decoded location. All these private formats may contain personal
// data; callers must obtain the same explicit opt-in as for legacy headers.
type Header struct {
	Kind      Kind
	Title     string
	Tag       core.Tag
	Lines     []string
	Truncated bool
	Err       error
}

// Options selects kinds and bounds decoding/display; MaxLines defaults to 20000.
type Options struct {
	Kinds    []Kind
	Limits   Limits
	MaxLines int
}

// DecodePrivateHeaders decodes only the new metadata formats, in stable order.
// Legacy Siemens/GE/ELSCINT/VEPRO headers remain available through vendors.
func DecodePrivateHeaders(o *object.Object, opts Options) []Header {
	if o == nil {
		return nil
	}
	max := opts.MaxLines
	if max <= 0 {
		max = 20000
	}
	var out []Header
	for _, spec := range specs {
		if len(opts.Kinds) > 0 {
			found := false
			for _, k := range opts.Kinds {
				found = found || k == spec.kind
			}
			if !found {
				continue
			}
		}
		tag, err := privateTagRequired(o, spec.group, spec.creator, spec.offset)
		if err != nil {
			continue
		}
		h := Header{Kind: spec.kind, Title: spec.title, Tag: tag}
		h.Lines, h.Err = render(o, spec, opts.Limits.normalized())
		if h.Err != nil {
			h.Lines = nil
		}
		if len(h.Lines) > max {
			h.Lines = h.Lines[:max]
			h.Truncated = true
		}
		out = append(out, h)
	}
	return out
}

// RenderPrivateHeader renders one creator-validated location. It is used by the
// combined vendors facade to avoid parsing the other locations a second time.
func RenderPrivateHeader(o *object.Object, kind Kind, tag core.Tag, l Limits) ([]string, error) {
	for _, spec := range specs {
		if spec.kind != kind {
			continue
		}
		actual, err := privateTagRequired(o, spec.group, spec.creator, spec.offset)
		if err == nil && actual == tag {
			return render(o, spec, l.normalized())
		}
	}
	return nil, ErrNotFound
}
func render(o *object.Object, spec headerSpec, l Limits) ([]string, error) {
	switch spec.kind {
	case PhilipsSDS:
		items, err := PhilipsSDSOf(o, l)
		if err != nil {
			return nil, err
		}
		return philipsSDSLines(items, l)
	case ToshibaCT3:
		obj, err := ToshibaCT3Of(o, l)
		if err != nil {
			return nil, err
		}
		var lines []string
		err = headerObjectLines(obj, newHeaderBudget(l), "", 0, &lines)
		return lines, err
	case ToshibaMECMR3:
		records, err := ToshibaMECMR3Of(o, l)
		if err != nil {
			return nil, err
		}
		return mecMR3Lines(records, l)
	case PMTF, CanonPMTF, ToshibaPMTF:
		items, err := PMTFOf(o, spec.creator, spec.offset, l)
		if err != nil {
			return nil, err
		}
		b := newHeaderBudget(l)
		var lines []string
		for i, item := range items {
			if err := b.line(&lines, fmt.Sprintf("Item %d", i)); err != nil {
				return nil, err
			}
			if err := headerObjectLines(item, b, "", 0, &lines); err != nil {
				return nil, err
			}
		}
		return lines, nil
	case CanonPAS:
		text, err := CanonPASOf(o, l)
		if err != nil {
			return nil, err
		}
		text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n\x00 ")
		if text == "" {
			return nil, ErrInvalid
		}
		return strings.Split(text, "\n"), nil
	}
	return nil, ErrUnsupportedHeader
}

// Definition describes a private creator-bound location without decoding it.
// Definitions returns a fresh ordered slice, including Toshiba's two offsets.
type Definition struct {
	Kind           Kind
	Title, Creator string
	Group          uint16
	Offset         uint8
}

func Definitions() []Definition {
	out := make([]Definition, len(specs))
	for i, s := range specs {
		out[i] = Definition{Kind: s.kind, Title: s.title, Creator: s.creator, Group: s.group, Offset: s.offset}
	}
	return out
}
