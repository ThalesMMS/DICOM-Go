package datasettime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
)

var (
	// ErrInvalidOffset reports a malformed or out-of-range fixed UTC offset.
	ErrInvalidOffset = errors.New("dicom datasettime: invalid UTC offset")
	// ErrInvalidValue reports an invalid DA, TM, DT or context representation.
	ErrInvalidValue = errors.New("dicom datasettime: invalid temporal value")
	// ErrAmbiguous reports conversion that would invent precision/relationships.
	ErrAmbiguous = errors.New("dicom datasettime: ambiguous or unrepresentable conversion")
	// ErrMissingOffset reports implicit temporal values with no source context.
	ErrMissingOffset = errors.New("dicom datasettime: source UTC offset is undefined")
	// ErrUnsupported reports deferred/unavailable temporal or sequence payloads.
	ErrUnsupported = errors.New("dicom datasettime: unavailable temporal or sequence value")
	// ErrLimitExceeded reports invalid limits or exhausted structural budgets.
	ErrLimitExceeded = errors.New("dicom datasettime: structural copy limit exceeded")
)

var (
	timezoneTag           = core.NewTag(0x0008, 0x0201)
	contextVersionTag     = core.NewTag(0x0008, 0x0106)
	mappingResourceTag    = core.NewTag(0x0008, 0x0105)
	mappingResourceUIDTag = core.NewTag(0x0008, 0x0118)
)

// Options supplies source context and additional explicit DA/TM relationships.
type Options struct {
	// SourceOffset is a fallback only when neither this dataset nor its enclosing
	// item has a nonempty Timezone Offset From UTC. Empty means undefined, never
	// the machine's local timezone or UTC. Existing offsets take precedence.
	SourceOffset string
	// UnpairedDates defaults to RejectAmbiguous. PreserveCalendarDate keeps a
	// DA without a usable TM as a civil date, explicitly abandoning equivalence
	// of the unknown dated instant. ConvertWithReport records those locations.
	UnpairedDates UnpairedDatePolicy
	// Pairs adds private/site-specific pairs to StandardPairs. Tags may occur in
	// only one pair; both present elements must have exactly DA and TM VRs.
	Pairs []Pair
	// MaxSequenceDepth bounds recursion, including cyclic authored sequences.
	// Zero selects 64; a negative value is invalid. Root depth is zero.
	MaxSequenceDepth int
	// MaxElements bounds element visits/copies across the entire operation,
	// including repeated DAG nodes and a newly appended root offset element.
	// It also bounds the number of additional configured pairs before copying
	// their list/index. Zero selects 100000; a negative value is invalid.
	MaxElements int
	// MaxItems bounds copied sequence items across the entire operation,
	// including repeated DAG nodes and empty items. Zero selects 100000;
	// a negative value is invalid.
	MaxItems int
}

// UnpairedDatePolicy chooses the treatment of a DA without a usable paired TM.
type UnpairedDatePolicy uint8

const (
	// RejectAmbiguous refuses a context change for DA without a usable TM.
	RejectAmbiguous UnpairedDatePolicy = iota
	// PreserveCalendarDate retains civil DA and reports that semantic choice.
	PreserveCalendarDate
)

// PreservedDate identifies a DA deliberately retained as a calendar date.
// It contains locations, not the original date value.
type PreservedDate struct {
	ItemPath  string
	Tag       core.Tag
	Component int
}

// Report records semantic preservation choices rather than claiming instant
// equivalence for DA values whose time of day is unknown.
type Report struct {
	PreservedDates []PreservedDate
}

type offset struct {
	text    string
	seconds int
}

// Convert changes the dataset's fixed UTC offset and converts implicit DT and
// DA/TM values recursively. Explicit DT suffixes remain unchanged. TM without a
// paired DA is converted as a clock modulo 24 hours, not as a dated instant.
// Nonempty standalone DA defaults to rejection; PreserveCalendarDate explicitly
// retains its civil date instead. Use ConvertWithReport to inspect those choices.
// Partial values convert only if the shift is representable at their original
// precision; leap seconds are preserved only when no shift is needed.
//
// The operation is atomic: errors return a zero DataSet and never modify src.
// Element lists and sequence/item structure are copied. Modified text values
// are owned by the result; all other value payloads, including pixels, remain
// shared and must be treated as read-only. Item offsets and element order are
// retained. Modified text/SQ headers discard stale encoded lengths. Only the
// root and items that already have (0008,0201) receive a new offset element.
// A dataset must not be concurrently mutated while Convert reads it.
func Convert(src core.DataSet, targetOffset string, opts Options) (core.DataSet, error) {
	out, _, err := ConvertWithReport(src, targetOffset, opts)
	return out, err
}

// ConvertWithReport has Convert's ownership/atomicity contract and additionally
// reports every DA retained under PreserveCalendarDate when its context changes
// or was undefined. Errors return both a zero DataSet and an empty Report.
func ConvertWithReport(src core.DataSet, targetOffset string, opts Options) (core.DataSet, Report, error) {
	out, report, err := convertWithReport(src, targetOffset, opts)
	if err != nil {
		return core.DataSet{}, Report{}, err
	}
	return out, report, nil
}

func convertWithReport(src core.DataSet, targetOffset string, opts Options) (core.DataSet, Report, error) {
	target, err := parseOffset(targetOffset)
	if err != nil {
		return core.DataSet{}, Report{}, err
	}
	var fallback *offset
	if opts.SourceOffset != "" {
		value, err := parseOffset(opts.SourceOffset)
		if err != nil {
			return core.DataSet{}, Report{}, err
		}
		fallback = &value
	}
	depth := opts.MaxSequenceDepth
	if depth == 0 {
		depth = 64
	}
	if depth < 0 {
		return core.DataSet{}, Report{}, fmt.Errorf("%w: negative sequence depth", ErrLimitExceeded)
	}
	elements, items := opts.MaxElements, opts.MaxItems
	if elements == 0 {
		elements = 100000
	}
	if items == 0 {
		items = 100000
	}
	if elements < 0 || items < 0 {
		return core.DataSet{}, Report{}, fmt.Errorf("%w: negative elements/items budget", ErrLimitExceeded)
	}
	// Additional relationships are caller-authored input too; bound the copied
	// pair list and index before allocating them, independently of source size.
	if len(opts.Pairs) > elements {
		return core.DataSet{}, Report{}, fmt.Errorf("%w: additional pair count", ErrLimitExceeded)
	}
	if opts.UnpairedDates > PreserveCalendarDate {
		return core.DataSet{}, Report{}, fmt.Errorf("%w: unknown unpaired date policy", ErrAmbiguous)
	}
	pairs := StandardPairs()
	pairs = append(pairs, opts.Pairs...)
	seen := make(map[core.Tag]bool, len(pairs)*2)
	for _, p := range pairs {
		if p.Date == p.Time || seen[p.Date] || seen[p.Time] || p.Date == timezoneTag || p.Time == timezoneTag {
			return core.DataSet{}, Report{}, fmt.Errorf("%w: overlapping DA/TM pair %s/%s", ErrAmbiguous, p.Date, p.Time)
		}
		seen[p.Date], seen[p.Time] = true, true
	}
	c := converter{target: target, pairs: pairs, pairTags: seen, maxDepth: depth, unpairedDates: opts.UnpairedDates, elementsRemaining: elements, itemsRemaining: items}
	out, err := c.convert(src, fallback, 0, "dataset")
	return out, c.report, err
}

func parseOffset(s string) (offset, error) {
	// Only legal SH padding is ignored. A minus sign is illegal for zero.
	s = strings.TrimRight(s, " ")
	if len(s) != 5 || (s[0] != '+' && s[0] != '-') {
		return offset{}, fmt.Errorf("%w: expected &ZZXX", ErrInvalidOffset)
	}
	for i := 1; i < 5; i++ {
		if s[i] < '0' || s[i] > '9' {
			return offset{}, ErrInvalidOffset
		}
	}
	hours, _ := strconv.Atoi(s[1:3])
	minutes, _ := strconv.Atoi(s[3:5])
	if minutes > 59 {
		return offset{}, ErrInvalidOffset
	}
	n := hours*3600 + minutes*60
	if s[0] == '-' {
		if n == 0 || n > 12*3600 {
			return offset{}, ErrInvalidOffset
		}
		n = -n
	} else if n > 14*3600 {
		return offset{}, ErrInvalidOffset
	}
	return offset{s, n}, nil
}

type converter struct {
	target            offset
	pairs             []Pair
	pairTags          map[core.Tag]bool
	maxDepth          int
	unpairedDates     UnpairedDatePolicy
	report            Report
	elementsRemaining int
	itemsRemaining    int
}

func (c *converter) convert(src core.DataSet, inherited *offset, depth int, path string) (core.DataSet, error) {
	fail := func(tag core.Tag, err error) (core.DataSet, error) {
		return core.DataSet{}, fmt.Errorf("%s/%s: %w", path, tag, err)
	}
	if depth > c.maxDepth {
		return core.DataSet{}, fmt.Errorf("%w: %s exceeds sequence depth", ErrLimitExceeded, path)
	}
	// Charge visits before allocating this node's map and element slice. DAG
	// aliases are charged again on every visit, since each visit creates a copy.
	if len(src.Elements) > c.elementsRemaining {
		return core.DataSet{}, fmt.Errorf("%w: %s element visits", ErrLimitExceeded, path)
	}
	c.elementsRemaining -= len(src.Elements)
	if depth == 0 {
		hasZone := false
		for _, e := range src.Elements {
			if e.Tag() == timezoneTag {
				hasZone = true
				break
			}
		}
		if !hasZone {
			if c.elementsRemaining == 0 {
				return core.DataSet{}, fmt.Errorf("%w: root offset element", ErrLimitExceeded)
			}
			c.elementsRemaining--
		}
	}
	indices := make(map[core.Tag]int, len(src.Elements))
	for i, e := range src.Elements {
		if old, ok := indices[e.Tag()]; ok {
			previous := src.Elements[old]
			if temporal(e.VR()) || temporal(previous.VR()) || c.pairTags[e.Tag()] || e.Tag() == timezoneTag || e.Tag() == mappingResourceTag || e.Tag() == mappingResourceUIDTag {
				return fail(e.Tag(), fmt.Errorf("%w: duplicate context/temporal tag", ErrAmbiguous))
			}
		}
		indices[e.Tag()] = i
	}
	source := inherited
	zoneIndex, hasZone := indices[timezoneTag]
	if hasZone {
		e := src.Elements[zoneIndex]
		if e.VR() != core.VRSH {
			return fail(timezoneTag, ErrInvalidValue)
		}
		values, err := textValues(e)
		if err != nil {
			return fail(timezoneTag, err)
		}
		if len(values) > 1 {
			return fail(timezoneTag, ErrInvalidOffset)
		}
		if len(values) == 1 && values[0] != "" {
			value, err := parseOffset(values[0])
			if err != nil {
				return fail(timezoneTag, err)
			}
			source = &value
		}
	}
	out := core.DataSet{Elements: append([]core.Element(nil), src.Elements...), ItemOffset: src.ItemOffset, ItemOffsetSet: src.ItemOffsetSet}
	handled := make(map[int]bool)
	for _, p := range c.pairs {
		di, datePresent := indices[p.Date]
		ti, timePresent := indices[p.Time]
		if !datePresent || !timePresent {
			continue
		}
		d, t := src.Elements[di], src.Elements[ti]
		if d.VR() != core.VRDA || t.VR() != core.VRTM {
			return fail(p.Date, fmt.Errorf("%w: DA/TM pair VR", ErrInvalidValue))
		}
		dates, err := textValues(d)
		if err != nil {
			return fail(p.Date, err)
		}
		clocks, err := textValues(t)
		if err != nil {
			return fail(p.Time, err)
		}
		// Two entirely empty attributes have no temporal meaning to adjust.
		if emptyValues(dates) && emptyValues(clocks) {
			handled[di], handled[ti] = true, true
			continue
		}
		if c.unpairedDates == PreserveCalendarDate && emptyValues(clocks) {
			for i, date := range dates {
				if date != "" {
					if err := c.preserveDate(date, source, path, p.Date, i); err != nil {
						return fail(p.Date, err)
					}
				}
			}
			handled[di], handled[ti] = true, true
			continue
		}
		if len(dates) != len(clocks) {
			return fail(p.Date, fmt.Errorf("%w: pair multiplicity differs", ErrAmbiguous))
		}
		newDates, newClocks := append([]string(nil), dates...), append([]string(nil), clocks...)
		for i := range dates {
			if dates[i] == "" && clocks[i] == "" {
				continue
			}
			if dates[i] == "" || clocks[i] == "" {
				if dates[i] != "" && c.unpairedDates == PreserveCalendarDate {
					if err := c.preserveDate(dates[i], source, path, p.Date, i); err != nil {
						return fail(p.Date, err)
					}
					continue
				}
				return fail(p.Date, fmt.Errorf("%w: incomplete pair component %d", ErrAmbiguous, i))
			}
			newDates[i], newClocks[i], err = c.shiftPair(dates[i], clocks[i], source)
			if err != nil {
				return fail(p.Date, fmt.Errorf("component %d: %w", i, err))
			}
		}
		out.Elements[di] = replaceText(d, dates, newDates)
		out.Elements[ti] = replaceText(t, clocks, newClocks)
		handled[di], handled[ti] = true, true
	}
	for i, e := range src.Elements {
		if handled[i] {
			continue
		}
		seq, isSequence := e.Value.(core.SequenceValue)
		if isSequence || e.VR() == core.VRSQ || e.Value != nil && e.Value.Kind() == core.ValueSequence {
			// Recovered UN sequences also contain datasets whose temporal values
			// inherit this context. The payload, not just its VR, defines traversal.
			if e.VR() != core.VRSQ && e.VR() != core.VRUN {
				return fail(e.Tag(), ErrInvalidValue)
			}
			if !isSequence {
				return fail(e.Tag(), ErrUnsupported)
			}
			// Empty items also consume structural memory. Reserve their entire
			// slice before make, not after attempting each recursive conversion.
			if len(seq.Items) > c.itemsRemaining {
				return fail(e.Tag(), fmt.Errorf("%w: sequence item copies", ErrLimitExceeded))
			}
			c.itemsRemaining -= len(seq.Items)
			newSeq := core.SequenceValue{Items: make([]core.DataSet, len(seq.Items))}
			for j, item := range seq.Items {
				value, err := c.convert(item, source, depth+1, fmt.Sprintf("%s/%s[%d]", path, e.Tag(), j))
				if err != nil {
					return core.DataSet{}, err
				}
				newSeq.Items[j] = value
			}
			out.Elements[i].Value = newSeq
			out.Elements[i].Header.Length, out.Elements[i].Header.LengthSet = 0, false
			continue
		}
		if !temporal(e.VR()) {
			continue
		}
		values, err := textValues(e)
		if err != nil {
			return fail(e.Tag(), err)
		}
		converted := append([]string(nil), values...)
		for j, value := range values {
			if value == "" {
				continue
			}
			if e.Tag() == contextVersionTag && e.VR() == core.VRDT {
				exempt, err := contextVersionExempt(src, indices, value)
				if err != nil {
					return fail(e.Tag(), err)
				}
				if exempt {
					continue
				}
			}
			switch e.VR() {
			case core.VRDT:
				converted[j], err = c.shiftDT(value, source)
			case core.VRTM:
				converted[j], err = c.shiftClock(value, source)
			case core.VRDA:
				err = c.preserveDate(value, source, path, e.Tag(), j)
			}
			if err != nil {
				return fail(e.Tag(), fmt.Errorf("component %d: %w", j, err))
			}
		}
		out.Elements[i] = replaceText(e, values, converted)
	}
	if hasZone {
		out.Elements[zoneIndex] = changedText(src.Elements[zoneIndex], []string{c.target.text})
	} else if depth == 0 {
		out.Elements = append(out.Elements, core.Element{Header: core.ElementHeader{Tag: timezoneTag, VR: core.VRSH}, Value: core.StringValue{c.target.text}})
	}
	return out, nil
}

func (c *converter) preserveDate(value string, source *offset, path string, tag core.Tag, component int) error {
	if _, err := dcmtime.ParseDate(value); err != nil || len(value) != 8 {
		return ErrInvalidValue
	}
	if source != nil && source.seconds == c.target.seconds {
		return nil
	}
	if c.unpairedDates == PreserveCalendarDate {
		c.report.PreservedDates = append(c.report.PreservedDates, PreservedDate{path, tag, component})
		return nil
	}
	if source == nil {
		return ErrMissingOffset
	}
	return fmt.Errorf("%w: DA without paired TM", ErrAmbiguous)
}

func temporal(vr core.VR) bool { return vr == core.VRDA || vr == core.VRTM || vr == core.VRDT }

func textValues(e core.Element) ([]string, error) {
	switch e.Value.(type) {
	case core.RawValue, core.StringValue:
		return e.StringValues(), nil
	default:
		return nil, ErrUnsupported
	}
}

func emptyValues(values []string) bool {
	for _, s := range values {
		if s != "" {
			return false
		}
	}
	return true
}

func replaceText(e core.Element, before, after []string) core.Element {
	for i := range before {
		if before[i] != after[i] {
			return changedText(e, after)
		}
	}
	return e
}

func changedText(e core.Element, values []string) core.Element {
	e.Value = core.StringValue(values)
	e.Header.Length, e.Header.LengthSet = 0, false
	return e
}

func (c *converter) delta(source *offset) (time.Duration, error) {
	if source == nil {
		return 0, ErrMissingOffset
	}
	return time.Duration(c.target.seconds-source.seconds) * time.Second, nil
}

func shiftAllowed(precision dcmtime.PrecisionLevel, delta time.Duration, leap bool) error {
	if delta == 0 {
		return nil
	}
	if leap {
		return fmt.Errorf("%w: leap-second arithmetic", ErrAmbiguous)
	}
	if precision >= dcmtime.PrecisionDay {
		return fmt.Errorf("%w: date-only precision", ErrAmbiguous)
	}
	if precision == dcmtime.PrecisionHours && delta%time.Hour != 0 {
		return fmt.Errorf("%w: hour precision with fractional-hour shift", ErrAmbiguous)
	}
	return nil
}

func leapSecond(value string, dt bool) bool {
	start := 4
	if dt {
		start = 12
	}
	return len(value) >= start+2 && value[start:start+2] == "60"
}

func (c *converter) shiftPair(date, clock string, source *offset) (string, string, error) {
	da, err := dcmtime.ParseDate(date)
	if err != nil || len(date) != 8 {
		return "", "", ErrInvalidValue
	}
	tm, err := dcmtime.ParseTime(clock)
	if err != nil {
		return "", "", ErrInvalidValue
	}
	delta, err := c.delta(source)
	if err != nil {
		return "", "", err
	}
	if err := shiftAllowed(tm.Precision, delta, leapSecond(clock, false)); err != nil {
		return "", "", err
	}
	if delta == 0 {
		return date, clock, nil
	}
	value := time.Date(da.Time.Year(), da.Time.Month(), da.Time.Day(), tm.Time.Hour(), tm.Time.Minute(), tm.Time.Second(), tm.Time.Nanosecond(), time.UTC).Add(delta)
	if value.Year() < 0 || value.Year() > 9999 {
		return "", "", fmt.Errorf("%w: date outside four-digit year", ErrAmbiguous)
	}
	return (dcmtime.Date{Time: value, Precision: dcmtime.PrecisionFull}).DCM(), (dcmtime.Time{Time: value, Precision: tm.Precision}).DCM(), nil
}

func (c *converter) shiftClock(value string, source *offset) (string, error) {
	tm, err := dcmtime.ParseTime(value)
	if err != nil {
		return "", ErrInvalidValue
	}
	delta, err := c.delta(source)
	if err != nil {
		return "", err
	}
	if err := shiftAllowed(tm.Precision, delta, leapSecond(value, false)); err != nil {
		return "", err
	}
	if delta == 0 {
		return value, nil
	}
	tm.Time = tm.Time.Add(delta)
	return tm.DCM(), nil
}

func (c *converter) shiftDT(value string, source *offset) (string, error) {
	dt, err := dcmtime.ParseDatetime(value)
	if err != nil {
		return "", ErrInvalidValue
	}
	// A suffix is independent of the dataset's offset, even for a partial DT.
	if !dt.NoOffset {
		return value, nil
	}
	delta, err := c.delta(source)
	if err != nil {
		return "", err
	}
	if err := shiftAllowed(dt.Precision, delta, leapSecond(value, true)); err != nil {
		return "", err
	}
	if delta == 0 {
		return value, nil
	}
	dt.Time = dt.Time.Add(delta)
	if dt.Time.Year() < 0 || dt.Time.Year() > 9999 {
		return "", fmt.Errorf("%w: date outside four-digit year", ErrAmbiguous)
	}
	return dt.DCM(), nil
}

// PS3.3 8.4/8.5: PS3.16 context group versions use day precision and do not
// use the timezone offset. Day+0000, if supplied, is retained just as every
// explicit-offset DT is; it is not rewritten as an implicit local version.
// The exception is local to the identified mapping resource.
func contextVersionExempt(ds core.DataSet, indices map[core.Tag]int, value string) (bool, error) {
	day := value
	if len(value) == 13 && strings.HasSuffix(value, "+0000") {
		day = value[:8]
	}
	if len(day) != 8 {
		return false, nil
	}
	if _, err := dcmtime.ParseDatetime(value); err != nil {
		return false, ErrInvalidValue
	}
	resource, uid := "", ""
	for _, entry := range []struct {
		tag core.Tag
		dst *string
		vr  core.VR
	}{{mappingResourceTag, &resource, core.VRCS}, {mappingResourceUIDTag, &uid, core.VRUI}} {
		if i, ok := indices[entry.tag]; ok {
			if ds.Elements[i].VR() != entry.vr {
				return false, ErrInvalidValue
			}
			values, err := textValues(ds.Elements[i])
			if err != nil {
				return false, err
			}
			if len(values) > 1 {
				return false, ErrAmbiguous
			}
			if len(values) == 1 {
				*entry.dst = values[0]
				if entry.tag == mappingResourceTag {
					*entry.dst = strings.Trim(values[0], " ")
				}
			}
		}
	}
	const dcmrUID = "1.2.840.10008.8.1.1"
	if resource == "DCMR" && uid != "" && uid != dcmrUID || uid == dcmrUID && resource != "" && resource != "DCMR" {
		return false, fmt.Errorf("%w: conflicting mapping resource", ErrAmbiguous)
	}
	return resource == "DCMR" || uid == dcmrUID, nil
}
