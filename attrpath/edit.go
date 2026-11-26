package attrpath

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// DefaultMaxCreatedItems bounds the sequence items one edit may create.
const DefaultMaxCreatedItems = 1024

// Options control Create, Insert and Modify.
type Options struct {
	// Dictionary gives the VR of attributes that do not exist yet (the
	// standard dictionary when nil). Chain a private catalog to create
	// private attributes by creator.
	Dictionary dictionary.DataDictionary
	// VR, when set, is used for the addressed attribute instead of its
	// existing or dictionary VR.
	VR core.VR
	// MaxCreatedItems bounds the sequence items one edit may create
	// (DefaultMaxCreatedItems when zero).
	MaxCreatedItems int
	// SkipValueValidation permits matching syntax such as C-FIND date ranges.
	// VR conversion still applies, including numeric parsing.
	SkipValueValidation bool
}

// Match is one attribute or item selected by Find, with the concrete path
// (wildcards resolved) that reaches it.
type Match struct {
	Path Path
	// Element is the selected attribute; it is zero when the path selects an
	// item.
	Element core.Element
	// Item is a detached copy of the selected item, when the path selects
	// one.
	Item *object.Object
}

type mode int

const (
	modeFind mode = iota
	modeCreate
	modeInsert
	modeModify
	modeRemove
)

func (m mode) creates() bool { return m == modeCreate || m == modeInsert }

// Find returns the attributes or items p selects. A path without wildcards
// that does not resolve fails with ErrNotFound; a wildcard path fails only
// when it selects nothing.
func Find(dataset *object.Object, p Path) ([]Match, error) {
	e := &editor{mode: modeFind}
	if _, err := e.run(dataset, p); err != nil {
		return nil, err
	}
	return e.matches, nil
}

// Create makes sure p exists, adding missing sequences, items (up to the
// requested index) and an empty leaf attribute, like DCMTK's
// findOrCreatePath. A wildcard only reaches existing items. It returns the
// number of leaf attributes or items the path reaches.
func Create(dataset *object.Object, p Path, opts Options) (int, error) {
	return (&editor{mode: modeCreate, opts: opts}).run(dataset, p)
}

// Insert sets the attribute p selects to value, creating it and any missing
// sequences and items, and replacing an existing value (dcmodify -i).
// Multiple values are separated by a backslash, except for LT, ST, UT and
// UR.
func Insert(dataset *object.Object, p Path, value string, opts Options) (int, error) {
	return (&editor{mode: modeInsert, opts: opts, value: value}).run(dataset, p)
}

// Modify replaces the value of attributes that already exist (dcmodify -m);
// nothing is created.
func Modify(dataset *object.Object, p Path, value string, opts Options) (int, error) {
	return (&editor{mode: modeModify, opts: opts, value: value}).run(dataset, p)
}

// Remove deletes the attributes or items p selects (dcmodify -e).
func Remove(dataset *object.Object, p Path) (int, error) {
	return (&editor{mode: modeRemove}).run(dataset, p)
}

type editor struct {
	mode       mode
	opts       Options
	value      string
	created    int
	count      int
	matches    []Match
	session    *EditSession
	buildValue func(*object.Object, core.Tag, Segment, *core.Element) (core.Element, error)
}

func (e *editor) run(dataset *object.Object, p Path) (int, error) {
	if e.session != nil {
		if err := validateSessionPath(p); err != nil {
			return 0, err
		}
	}
	if dataset == nil || len(p) == 0 {
		return 0, fmt.Errorf("%w: empty path or data set", ErrNotFound)
	}
	if e.opts.Dictionary == nil {
		e.opts.Dictionary = std.Dictionary
	}
	if e.opts.MaxCreatedItems <= 0 {
		e.opts.MaxCreatedItems = DefaultMaxCreatedItems
	}
	if p.SelectsItem() && (e.mode == modeModify || (e.mode == modeInsert && e.value != "")) {
		return 0, fmt.Errorf("%w: an item takes no value", ErrValue)
	}
	if _, err := e.apply(dataset, p, 0, nil); err != nil {
		return 0, err
	}
	if e.count == 0 {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	return e.count, nil
}

// resolve returns the tag a segment names in dataset, reserving a private
// creator block when the edit creates attributes.
func (e *editor) resolve(dataset *object.Object, s Segment) (core.Tag, bool, error) {
	if !s.Private() {
		return s.Tag, false, nil
	}
	offset := uint8(s.Tag.Element)
	tag, err := dataset.PrivateTag(s.Tag.Group, s.Creator, offset)
	if err == nil {
		return tag, false, nil
	}
	if !e.mode.creates() {
		return core.Tag{}, false, fmt.Errorf("%w: private creator %q in group %04X", ErrNotFound, s.Creator, s.Tag.Group)
	}
	if e.session != nil {
		if err := e.session.reserveModel(1, 0); err != nil {
			return core.Tag{}, false, err
		}
		if err := e.session.reserveBytes(int64(len(s.Creator))); err != nil {
			return core.Tag{}, false, err
		}
	}
	block, err := dataset.ReservePrivateCreator(s.Tag.Group, s.Creator)
	if err != nil {
		return core.Tag{}, false, err
	}
	return core.NewTag(s.Tag.Group, uint16(block)<<8|uint16(offset)), true, nil
}

// apply edits dataset along p[i:] and reports whether it changed.
func (e *editor) apply(dataset *object.Object, p Path, i int, prefix Path) (bool, error) {
	if e.session != nil {
		if err := e.session.visit(); err != nil {
			return false, err
		}
	}
	s := p[i]
	tag, changed, err := e.resolve(dataset, s)
	if err != nil {
		return false, err
	}
	concrete := append(append(Path(nil), prefix...), s)
	last := i == len(p)-1
	if !s.HasItem {
		leafChanged, err := e.leaf(dataset, tag, s, concrete)
		return changed || leafChanged, err
	}

	element, exists := dataset.Get(tag)
	if exists {
		if _, ok := element.Value.(core.SequenceValue); !ok {
			return changed, fmt.Errorf("%w: %s", ErrNotSequence, concrete)
		}
	} else if !e.mode.creates() || s.AnyItem {
		return changed, fmt.Errorf("%w: %s", ErrNotFound, concrete)
	} else {
		if e.session != nil {
			if err := e.session.reserveModel(1, 0); err != nil {
				return changed, err
			}
		}
		element = core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{}}
		changed = true
	}
	var items []*object.Object
	if exists {
		items, _ = dataset.GetSequence(tag)
	}
	var firstTarget, lastTarget int
	switch {
	case s.AnyItem:
		if len(items) == 0 {
			return changed, fmt.Errorf("%w: %s has no items", ErrNotFound, concrete)
		}
		firstTarget, lastTarget = 0, len(items)
	case s.Item >= len(items):
		if !e.mode.creates() {
			return changed, fmt.Errorf("%w: %s", ErrNotFound, concrete)
		}
		add := s.Item + 1 - len(items)
		if e.created+add > e.opts.MaxCreatedItems {
			return changed, fmt.Errorf("%w: more than %d new items", ErrLimit, e.opts.MaxCreatedItems)
		}
		if e.session != nil {
			if err := e.session.reserveModel(0, add); err != nil {
				return changed, err
			}
		}
		e.created += add
		for range add {
			item := object.New(e.opts.Dictionary)
			item.SetValueByteOrder(dataset.ValueByteOrder())
			items = append(items, item)
		}
		changed = true
		firstTarget, lastTarget = s.Item, s.Item+1
	default:
		firstTarget, lastTarget = s.Item, s.Item+1
	}

	if e.session != nil && changed && e.mode != modeFind {
		seq := core.SequenceValue{Items: make([]core.DataSet, len(items))}
		for i, item := range items {
			seq.Items[i] = item.ToDataSet()
		}
		header := element.Header
		header.Length = 0
		header.LengthSet = false
		dataset.Put(core.Element{Header: header, Value: seq})
		items, _ = dataset.GetSequence(tag)
	}
	removed := map[int]bool{}
	found := false
	var firstErr error
	for index := firstTarget; index < lastTarget; index++ {
		if e.session != nil {
			if err := e.session.visit(); err != nil {
				return changed, err
			}
		}
		itemPath := append(append(Path(nil), prefix...), s)
		itemPath[len(itemPath)-1].AnyItem = false
		itemPath[len(itemPath)-1].Item = index
		if last {
			found = true
			e.count++
			switch e.mode {
			case modeFind:
				if e.session != nil {
					if err := e.session.addMatch(selectedMatch{path: itemPath, item: items[index], parent: dataset}); err != nil {
						return changed, err
					}
				} else {
					e.matches = append(e.matches, Match{Path: itemPath, Item: object.FromElements(items[index].Elements(), e.opts.Dictionary)})
				}
			case modeRemove:
				if e.session != nil {
					if err := e.session.touch(); err != nil {
						return changed, err
					}
				}
				removed[index] = true
				changed = true
			}
			continue
		}
		itemChanged, err := e.apply(items[index], p, i+1, itemPath)
		if err != nil {
			// A wildcard skips items where the rest of the path is missing.
			if s.AnyItem && errors.Is(err, ErrNotFound) {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			return changed, err
		}
		found = true
		changed = changed || itemChanged
	}
	if !found && firstErr != nil {
		return changed, firstErr
	}
	if changed && e.mode != modeFind {
		sequence := core.SequenceValue{Items: make([]core.DataSet, 0, len(items))}
		for index, item := range items {
			if !removed[index] {
				sequence.Items = append(sequence.Items, item.ToDataSet())
			}
		}
		header := element.Header
		header.Length, header.LengthSet = 0, false
		dataset.Put(core.Element{Header: header, Value: sequence})
	}
	return changed, nil
}

// leaf applies the edit to one attribute.
func (e *editor) leaf(dataset *object.Object, tag core.Tag, s Segment, path Path) (bool, error) {
	existing, exists := dataset.Get(tag)
	switch e.mode {
	case modeFind:
		if !exists {
			return false, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		e.count++
		if e.session != nil {
			if err := e.session.addMatch(selectedMatch{path: path, element: existing, parent: dataset}); err != nil {
				return false, err
			}
		} else {
			e.matches = append(e.matches, Match{Path: path, Element: existing})
		}
		return false, nil
	case modeRemove:
		if !exists {
			return false, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		if e.session != nil {
			if err := e.session.touch(); err != nil {
				return false, err
			}
		}
		dataset.Remove(tag)
		e.count++
		return true, nil
	case modeCreate:
		e.count++
		if exists {
			return false, nil
		}
		if e.session != nil {
			if err := e.session.reserveModel(1, 0); err != nil {
				return false, err
			}
			if err := e.session.touch(); err != nil {
				return false, err
			}
		}
		element, err := e.element(dataset, tag, s, nil, "")
		if err != nil {
			return false, err
		}
		dataset.Put(element)
		return true, nil
	case modeModify:
		if !exists {
			return false, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
	}
	var current *core.Element
	if exists {
		current = &existing
	}
	if e.session != nil {
		if !exists {
			if err := e.session.reserveModel(1, 0); err != nil {
				return false, err
			}
		}
		if err := e.session.touch(); err != nil {
			return false, err
		}
	}
	var element core.Element
	var err error
	if e.buildValue != nil {
		element, err = e.buildValue(dataset, tag, s, current)
	} else {
		element, err = e.element(dataset, tag, s, current, e.value)
	}
	if err != nil {
		return false, err
	}
	dataset.Put(element)
	e.count++
	return true, nil
}

// element builds the attribute with value, choosing the VR from the
// options, the existing attribute or the dictionary.
func (e *editor) element(dataset *object.Object, tag core.Tag, s Segment, existing *core.Element, value string) (core.Element, error) {
	vr := e.opts.VR
	if vr == "" && existing != nil && existing.Header.VR != core.VRUN && existing.Header.VR != "" {
		vr = existing.Header.VR
	}
	if vr == "" {
		var entry dictionary.Entry
		var ok bool
		if s.Private() {
			entry, ok = dictionary.LookupScopedEntry(e.opts.Dictionary, tag, s.Creator)
		} else {
			entry, ok = e.opts.Dictionary.ByTag(tag)
		}
		if ok {
			vr = entry.VR
		}
	}
	if _, err := core.ParseVR(string(vr)); err != nil || vr == "" || vr == core.VRUN {
		return core.Element{}, fmt.Errorf("%w: %s (set Options.VR)", ErrUnknownVR, tag)
	}
	if e.session != nil {
		if err := e.session.reserveText(vr, value); err != nil {
			return core.Element{}, err
		}
		if tag == core.NewTag(0x0008, 0x0005) {
			if err := e.session.reserveConditionTextValue(core.VRCS, core.StringValue{value}); err != nil {
				return core.Element{}, err
			}
		}
	}
	element, err := ElementFromString(tag, vr, value, dataset.ValueByteOrder())
	if err != nil {
		return core.Element{}, err
	}
	if !e.opts.SkipValueValidation && vr != core.VRSQ && value != "" {
		// Values are checked against their VR; the value multiplicity is
		// left to the caller, as an edit may pass through other counts.
		report, _ := validation.ValidateElement(context.Background(), element, validation.Options{Mode: validation.ModeStrict, Dictionary: e.opts.Dictionary, ByteOrder: dataset.ValueByteOrder()})
		if code, ok := firstError(report); ok {
			return core.Element{}, fmt.Errorf("%w: %s: %s", ErrValue, tag, code)
		}
	}
	return element, nil
}

// firstError returns the first error finding other than a value
// multiplicity mismatch.
func firstError(report validation.Report) (validation.Code, bool) {
	for _, finding := range report.Findings {
		if finding.Severity == validation.SeverityError && finding.Code != validation.CodeValueMultiplicity {
			return finding.Code, true
		}
	}
	return "", false
}

// ElementFromString builds an element of the given VR from text. Multiple
// values are separated by a backslash, except for LT, ST, UT and UR; numbers
// are decimal, tags (AT) are written "(gggg,eeee)" or "ggggeeee". Binary
// VRs (OB, OW, OF, OD, OL, OV, UN) and SQ accept only an empty value.
func ElementFromString(tag core.Tag, vr core.VR, text string, order binary.ByteOrder) (core.Element, error) {
	header := core.ElementHeader{Tag: tag, VR: vr}
	if vr.IsStringLike() {
		values := []string{text}
		if vr.UsesTextValueDelimiter() {
			values = strings.Split(text, `\`)
		}
		if text == "" {
			values = nil
		}
		return core.Element{Header: header, Value: core.StringValue(values)}, nil
	}
	parts := strings.Split(text, `\`)
	if text == "" {
		parts = nil
	}
	fail := func(i int) (core.Element, error) {
		return core.Element{}, fmt.Errorf("%w: %s value %d for VR %s", ErrValue, tag, i+1, vr)
	}
	switch vr {
	case core.VRSQ:
		if text != "" {
			return core.Element{}, fmt.Errorf("%w: a sequence takes no value", ErrValue)
		}
		return core.Element{Header: header, Value: core.SequenceValue{}}, nil
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
		bits := map[core.VR]int{core.VRUS: 16, core.VRSS: 16, core.VRUL: 32, core.VRSL: 32, core.VRUV: 64, core.VRSV: 64}[vr]
		signed := vr == core.VRSS || vr == core.VRSL || vr == core.VRSV
		unsigned := make([]uint64, len(parts))
		signedValues := make([]int64, len(parts))
		for i, part := range parts {
			part = strings.TrimSpace(part)
			var err error
			if signed {
				signedValues[i], err = strconv.ParseInt(part, 10, bits)
			} else {
				unsigned[i], err = strconv.ParseUint(part, 10, bits)
			}
			if err != nil {
				return fail(i)
			}
		}
		var value core.Value
		switch vr {
		case core.VRUS:
			v := make(core.Uint16Value, len(parts))
			for i := range v {
				v[i] = uint16(unsigned[i])
			}
			value = v
		case core.VRSS:
			v := make(core.Int16Value, len(parts))
			for i := range v {
				v[i] = int16(signedValues[i])
			}
			value = v
		case core.VRUL:
			v := make(core.Uint32Value, len(parts))
			for i := range v {
				v[i] = uint32(unsigned[i])
			}
			value = v
		case core.VRSL:
			v := make(core.Int32Value, len(parts))
			for i := range v {
				v[i] = int32(signedValues[i])
			}
			value = v
		case core.VRUV:
			value = core.Uint64Value(unsigned)
		default:
			value = core.Int64Value(signedValues)
		}
		return core.Element{Header: header, Value: value}, nil
	case core.VRFL, core.VRFD:
		size := 64
		if vr == core.VRFL {
			size = 32
		}
		values := make([]float64, len(parts))
		for i, part := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(part), size)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return fail(i)
			}
			values[i] = v
		}
		if vr == core.VRFD {
			return core.Element{Header: header, Value: core.Float64Value(values)}, nil
		}
		v := make(core.Float32Value, len(values))
		for i := range v {
			v[i] = float32(values[i])
		}
		return core.Element{Header: header, Value: v}, nil
	case core.VRAT:
		v := make(core.TagValue, len(parts))
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "(") && strings.HasSuffix(part, ")") {
				part = strings.ReplaceAll(part[1:len(part)-1], ",", "")
			}
			n, err := strconv.ParseUint(part, 16, 32)
			if err != nil || len(part) != 8 {
				return fail(i)
			}
			v[i] = core.NewTag(uint16(n>>16), uint16(n))
		}
		return core.Element{Header: header, Value: v}, nil
	default:
		if text != "" {
			return core.Element{}, fmt.Errorf("%w: VR %s takes binary data, not text", ErrValue, vr)
		}
		return core.Element{Header: header, Value: core.RawValue{}}, nil
	}
}
