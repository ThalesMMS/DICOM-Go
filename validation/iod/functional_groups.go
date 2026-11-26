package iod

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrFunctionalGroupLimit indicates an incomplete check, never a valid object.
var ErrFunctionalGroupLimit = errors.New("dicom/validation/iod: functional group resource limit")
var ErrFunctionalGroupOptions = errors.New("dicom/validation/iod: invalid functional group options")

// FunctionalGroupOptions selects a finite, metadata-only structural check.
// Zero limits use finite defaults; negative or excessive limits are rejected.
// IOD is Enhanced CT Image, Enhanced MR Image or Segmentation; empty derives it
// from SOP Class UID. No pixels, deferred values or external context are loaded.
type FunctionalGroupOptions struct {
	// ByteOrder is used only by the raw dataset entry for binary AT/UL values;
	// nil is Explicit VR Little Endian. The Object entry retains its byte order.
	ByteOrder                                                              binary.ByteOrder
	IOD                                                                    string
	MaxFrames, MaxElements, MaxItems, MaxDepth, MaxOperations, MaxFindings int
}

func DefaultFunctionalGroupOptions() FunctionalGroupOptions {
	return FunctionalGroupOptions{MaxFrames: 4096, MaxElements: 100000, MaxItems: 100000, MaxDepth: 16, MaxOperations: 1000000, MaxFindings: 1000}
}

const (
	InvalidFunctionalGroup  Kind = "invalid-functional-group"
	MissingFunctionalGroup  Kind = "missing-functional-group"
	FunctionalGroupConflict Kind = "functional-group-conflict"
	ManualFunctionalGroup   Kind = "manual-functional-group"
)

var fgShared = core.NewTag(0x5200, 0x9229)
var fgPerFrame = core.NewTag(0x5200, 0x9230)
var fgFrameContent = core.NewTag(0x0020, 0x9111)

// ValidateFunctionalGroups checks an already materialized Object. Its root may
// already have last-wins duplicate semantics; discarded source elements cannot
// be reconstructed. Use ValidateFunctionalGroupDataSet for raw duplicate checks.
func ValidateFunctionalGroups(ctx context.Context, obj *object.Object, opts FunctionalGroupOptions) (Report, error) {
	if obj == nil {
		return Report{}, ErrUnknownIOD
	}
	normalized, err := normalizeFGOptions(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	if obj.Len() > normalized.MaxElements {
		return Report{}, ErrFunctionalGroupLimit
	}
	return validateFunctionalGroupDataSet(ctx, obj.ToDataSet(), normalized, obj)
}

func withFGIOD(opts FunctionalGroupOptions, obj *object.Object) FunctionalGroupOptions {
	if opts.IOD == "" {
		u := fgText(obj, 0x00080016)
		opts.IOD, _ = IODForSOPClass(u)
	}
	return opts
}

// ValidateFunctionalGroupDataSet checks raw items before creating last-wins
// Object facades. Duplicate tags are reported without normalization.
func ValidateFunctionalGroupDataSet(ctx context.Context, ds core.DataSet, opts FunctionalGroupOptions) (Report, error) {
	return validateFunctionalGroupDataSet(ctx, ds, opts, nil)
}
func validateFunctionalGroupDataSet(ctx context.Context, ds core.DataSet, opts FunctionalGroupOptions, facade *object.Object) (Report, error) {
	opts, err := normalizeFGOptions(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	s := fgState{ctx: ctx, opts: opts, report: Report{IOD: opts.IOD, StandardVersion: StandardVersion}}
	if err = s.scan(ds, 0, ""); err != nil {
		return s.report, err
	}
	if s.ambiguous {
		return s.report, nil
	}
	obj := object.FromElements(ds.Elements, nil)
	if opts.ByteOrder != nil {
		obj.SetValueByteOrder(opts.ByteOrder)
	}
	if facade != nil {
		obj = facade
	}
	opts = withFGIOD(opts, obj)
	s.opts = opts
	s.report.IOD = opts.IOD
	rules, ok := fgRules(opts.IOD)
	if !ok {
		return s.report, fmt.Errorf("%w: functional groups profile", ErrUnknownIOD)
	}
	// Number of Frames is IS VM1, positive, bounded before frame wrappers.
	n, valid := fgInteger(obj, core.NewTag(0x0028, 0x0008))
	if !valid || n < 1 {
		s.add(InvalidFunctionalGroup, core.NewTag(0x0028, 0x0008).String(), core.NewTag(0x0028, 0x0008), "number-of-frames", "C.7.6.6.1.1", "Number of Frames must be one positive IS value")
		return s.report, nil
	}
	if n > opts.MaxFrames {
		return s.report, ErrFunctionalGroupLimit
	}
	shared, validShared := fgRawSequence(obj, fgShared)
	if !validShared || len(shared) != 1 {
		s.add(InvalidFunctionalGroup, fgShared.String(), fgShared, "shared-cardinality", "C.7.6.16", "Shared Functional Groups must have exactly one item")
		return s.report, nil
	}
	frames, presentFrames := fgRawSequence(obj, fgPerFrame)
	if obj.Has(fgPerFrame) && (!presentFrames || len(frames) != n) {
		s.add(InvalidFunctionalGroup, fgPerFrame.String(), fgPerFrame, "per-frame-cardinality", "C.7.6.16", "Per-Frame item count must match Number of Frames")
		return s.report, nil
	}
	if len(frames) > opts.MaxFrames {
		return s.report, ErrFunctionalGroupLimit
	}
	// Existing conflict resolver is O(shared macros * frames). Admit its exact
	// Cartesian upper bound first; no unbounded intermediate conflict lists.
	sharedCost := len(shared[0].Elements) + 1 + len(rules)
	for _, e := range shared[0].Elements {
		if sq, ok := e.Value.(core.SequenceValue); ok {
			sharedCost += len(sq.Items)
		}
	}
	if n > opts.MaxOperations/sharedCost {
		return s.report, ErrFunctionalGroupLimit
	}
	if err = ctx.Err(); err != nil {
		return s.report, err
	}
	if err = s.bindInstanceConditions(obj, rules, n); err != nil {
		return s.report, err
	}
	for _, conflict := range obj.FunctionalGroupConflicts() {
		for _, frame := range conflict.Frames {
			s.add(FunctionalGroupConflict, fgPath(frame, conflict.Macro), conflict.Macro, "shared-per-frame-conflict", "C.7.6.16.1.1", "Macro occurs in Shared and Per-Frame; existing resolver reports a conflict")
		}
	}
	allowed := map[core.Tag]bool{}
	for _, r := range rules {
		allowed[r.tag] = true
	}
	known := map[core.Tag]bool{}
	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image", "Segmentation"} {
		catalog, _ := fgRules(profile)
		for _, r := range catalog {
			known[r.tag] = true
		}
	}
	for i, group := range append([]core.DataSet{shared[0]}, frames...) {
		for _, e := range group.Elements {
			if known[e.Tag()] && !allowed[e.Tag()] {
				path := fgShared.String() + "[1]/" + e.Tag().String()
				if i > 0 {
					path = fgPath(i-1, e.Tag())
				}
				s.add(InvalidFunctionalGroup, path, e.Tag(), "macro-profile-location", fgProfileSection(opts.IOD), "Known macro is outside this IOD's permitted functional group profile")
			}
		}
	}
	sharedObj, _ := obj.GetSequence(fgShared)
	if sharedObj[0].Has(fgFrameContent) {
		s.add(InvalidFunctionalGroup, fgShared.String()+"[1]/"+fgFrameContent.String(), fgFrameContent, "frame-content-location", fgProfileSection(opts.IOD), "Frame Content shall not be Shared")
	}
	s.projections = make([]*object.Object, n)
	for frame := 0; frame < n; frame++ {
		// A one-frame projection reuses the canonical resolver without repeatedly
		// allocating facades for every other frame (GetSequence returns fresh ones).
		elements := []core.Element{{Header: core.ElementHeader{Tag: fgShared, VR: core.VRSQ}, Value: core.SequenceValue{Items: shared}}}
		if len(frames) > 0 {
			elements = append(elements, core.Element{Header: core.ElementHeader{Tag: fgPerFrame, VR: core.VRSQ}, Value: core.SequenceValue{Items: frames[frame : frame+1]}})
		}
		s.projections[frame] = object.FromElements(elements, nil)
		s.projections[frame].SetValueByteOrder(obj.ValueByteOrder())
	}
	for frame := 0; frame < n; frame++ {
		if err = ctx.Err(); err != nil {
			return s.report, err
		}
		for _, r := range rules {
			if err = s.work(); err != nil {
				return s.report, err
			}
			items, source, resolveErr := s.projections[frame].FunctionalGroupItems(0, r.tag)
			// The conflict list above owns conflict diagnostics; do not duplicate it.
			if resolveErr != nil && !errors.Is(resolveErr, object.ErrFunctionalGroupConflict) {
				return s.report, resolveErr
			}
			path := fgPath(frame, r.tag)
			if source == object.FunctionalGroupShared {
				path = fgShared.String() + "[1]/" + r.tag.String()
			}
			present := len(items) > 0
			if !present {
				present, source = fgMacroPresent(s.projections[frame], r.tag)
				if source == object.FunctionalGroupShared {
					path = fgShared.String() + "[1]/" + r.tag.String()
				}
			}
			required, known := r.required(obj, frame)
			if !present && known && required {
				s.add(MissingFunctionalGroup, path, r.tag, "required-macro", fgProfileSection(opts.IOD), "Required macro is absent for this frame")
			}
			if !present && !known {
				s.add(ManualFunctionalGroup, path, r.tag, "manual-macro-condition", fgProfileSection(opts.IOD), "Macro presence needs context not established by this dataset")
			}
			if present && !errors.Is(resolveErr, object.ErrFunctionalGroupConflict) {
				minimum, maximum, countKnown := fgMacroCardinality(obj, r)
				if len(items) < minimum || maximum > 0 && len(items) > maximum {
					s.add(InvalidFunctionalGroup, path, r.tag, "macro-cardinality", fgCardinalitySection(r), "Macro item count is outside its normative range")
				} else if !countKnown && len(items) > 1 {
					s.add(ManualFunctionalGroup, path, r.tag, "manual-macro-cardinality", fgCardinalitySection(r), "Macro maximum needs a Multi-energy CT Acquisition condition not established by this dataset")
				}
			}
			if present && r.forbidden != nil && r.forbidden(obj) {
				s.add(InvalidFunctionalGroup, path, r.tag, "macro-forbidden", fgProfileSection(opts.IOD), "Macro is forbidden under the dataset condition")
			}
		}
		if opts.IOD == "Segmentation" {
			s.checkSegGeometry(obj, frame)
		}
	}
	if err = s.dimensions(obj, n); err != nil {
		return s.report, err
	}
	v := validator{root: obj, report: s.report, max: opts.MaxFindings, seen: map[string]bool{}, checkValues: true}
	v.checkFunctionalGroupValues("Enhanced Functional Groups")
	s.report = v.report
	if err = ctx.Err(); err != nil {
		return s.report, err
	}
	return s.report, nil
}

func normalizeFGOptions(ctx context.Context, o FunctionalGroupOptions) (FunctionalGroupOptions, error) {
	if ctx == nil {
		return o, ErrFunctionalGroupOptions
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	d := DefaultFunctionalGroupOptions()
	fields := []*int{&o.MaxFrames, &o.MaxElements, &o.MaxItems, &o.MaxDepth, &o.MaxOperations, &o.MaxFindings}
	defaults := []int{d.MaxFrames, d.MaxElements, d.MaxItems, d.MaxDepth, d.MaxOperations, d.MaxFindings}
	hard := []int{65536, 1000000, 1000000, 64, 4000000, 10000}
	for i, p := range fields {
		if *p < 0 || *p > hard[i] {
			return o, ErrFunctionalGroupOptions
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	return o, nil
}

type fgState struct {
	seen                        map[string]bool
	projections                 []*object.Object
	ctx                         context.Context
	opts                        FunctionalGroupOptions
	report                      Report
	elements, items, operations int
	primitiveBytes              int
	ambiguous                   bool
}

func (s *fgState) work() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.operations++
	if s.operations > s.opts.MaxOperations {
		return ErrFunctionalGroupLimit
	}
	return nil
}
func (s *fgState) add(k Kind, path string, tag core.Tag, rule, section, msg string) {
	key := string(k) + " " + path + " " + rule
	if s.seen[key] {
		return
	}
	if len(s.report.Findings) >= s.opts.MaxFindings {
		s.report.Truncated++
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[key] = true
	if len(s.report.Findings) >= s.opts.MaxFindings {
		s.report.Truncated++
		return
	}
	severity := SeverityError
	if k == ManualFunctionalGroup {
		severity = SeverityInfo
	}
	source := fgSource(section, tag)
	s.report.Findings = append(s.report.Findings, Finding{Kind: k, Severity: severity, Module: "Enhanced Functional Groups", Path: path, Tag: tag, Rule: rule, Source: &source, Message: msg})
}
func (s *fgState) scan(ds core.DataSet, depth int, path string) error {
	if err := s.work(); err != nil {
		return err
	}
	if depth > s.opts.MaxDepth || len(ds.Elements) > s.opts.MaxElements-s.elements {
		return ErrFunctionalGroupLimit
	}
	seen := make(map[core.Tag]bool, len(ds.Elements))
	s.elements += len(ds.Elements)
	for _, e := range ds.Elements {
		if err := s.work(); err != nil {
			return err
		}
		p := path + e.Tag().String()
		if seen[e.Tag()] {
			s.ambiguous = true
			s.add(InvalidFunctionalGroup, p, e.Tag(), "duplicate-tag", "PS3.5 7.1", "Duplicate raw tag is ambiguous; no last-wins validation")
		}
		seen[e.Tag()] = true
		if e.Tag() != fgTag(0x7fe00010) && e.Tag() != fgTag(0x7fe00008) && e.Tag() != fgTag(0x7fe00009) {
			size, supported, within := fgPrimitiveSize(e.Value, MaxConstraintValueBytes)
			if !supported {
				return ErrFunctionalGroupOptions
			}
			if !within || size > MaxFunctionalGroupMetadataBytes-s.primitiveBytes {
				return ErrFunctionalGroupLimit
			}
			s.primitiveBytes += size
		}
		seq, isSeq := e.Value.(core.SequenceValue)
		if !isSeq {
			if e.VR() == core.VRSQ {
				s.ambiguous = true
				s.add(ManualFunctionalGroup, p, e.Tag(), "sequence-unavailable", "PS3.5 7.1", "Sequence items are not materialized; structural validation is incomplete")
			}
			continue
		}
		if e.VR() != core.VRSQ {
			s.ambiguous = true
			s.add(InvalidFunctionalGroup, p, e.Tag(), "sequence-vr", "PS3.5 7.1", "Sequence value requires SQ VR")
		}
		if depth == 0 && e.Tag() == fgPerFrame && len(seq.Items) > s.opts.MaxFrames {
			return ErrFunctionalGroupLimit
		}
		if len(seq.Items) > s.opts.MaxItems-s.items {
			return ErrFunctionalGroupLimit
		}
		s.items += len(seq.Items)
		for i, item := range seq.Items {
			if err := s.scan(item, depth+1, fmt.Sprintf("%s[%d]/", p, i+1)); err != nil {
				return err
			}
		}
	}
	return nil
}
func fgRawSequence(obj *object.Object, tag core.Tag) ([]core.DataSet, bool) {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != core.VRSQ {
		return nil, false
	}
	seq, ok := e.Value.(core.SequenceValue)
	return seq.Items, ok
}
func fgInteger(obj *object.Object, tag core.Tag) (int, bool) {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != core.VRIS || !fgPrimitiveWithin(e, 12) {
		return 0, false
	}
	values := e.StringValues()
	if len(values) != 1 {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(values[0]))
	return v, err == nil
}
func fgPath(frame int, macro core.Tag) string {
	return fmt.Sprintf("%s[%d]/%s", fgPerFrame, frame+1, macro)
}

// ValidateWithFunctionalGroups performs the bounded structural preflight before
// the existing IOD checks. Defaults of Validate itself are unchanged.
func ValidateWithFunctionalGroups(ctx context.Context, obj *object.Object, opts Options, fg FunctionalGroupOptions) (Report, error) {
	if fg.IOD == "" {
		fg.IOD = opts.IOD
	}
	structural, err := ValidateFunctionalGroups(ctx, obj, fg)
	if err != nil {
		return structural, err
	}
	legacy, err := Validate(obj, opts)
	if err != nil {
		return legacy, err
	}
	max := opts.MaxFindings
	if max <= 0 {
		max = 1000
	}
	for _, f := range structural.Findings {
		duplicate := false
		for _, old := range legacy.Findings {
			if old.Path == f.Path && old.Kind == f.Kind && old.Rule == f.Rule {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if len(legacy.Findings) >= max {
			legacy.Truncated++
		} else {
			legacy.Findings = append(legacy.Findings, f)
		}
	}
	legacy.Truncated += structural.Truncated
	return legacy, nil
}

func fgMacroPresent(o *object.Object, tag core.Tag) (bool, object.FunctionalGroupSource) {
	for _, location := range []struct {
		tag    core.Tag
		source object.FunctionalGroupSource
	}{{fgPerFrame, object.FunctionalGroupPerFrame}, {fgShared, object.FunctionalGroupShared}} {
		items, ok := fgRawSequence(o, location.tag)
		if !ok || len(items) != 1 {
			continue
		}
		for _, e := range items[0].Elements {
			if e.Tag() == tag {
				return true, location.source
			}
		}
	}
	return false, object.FunctionalGroupNone
}

// MaxFunctionalGroupMetadataBytes bounds aggregate actual primitive payloads;
// pixels and unresolved values are never decoded to calculate this budget.
const MaxFunctionalGroupMetadataBytes = 16 << 20

func fgPrimitiveWithin(e core.Element, max int) bool {
	_, supported, within := fgPrimitiveSize(e.Value, max)
	return supported && within
}
func fgPrimitiveSize(value core.Value, max int) (int, bool, bool) {
	count, width := 0, 1
	switch v := value.(type) {
	case nil:
		return 0, true, true
	case core.RawValue:
		return len(v), true, len(v) <= max
	case core.StringValue:
		if len(v) > 100000 {
			return 0, true, false
		}
		size := 0
		for i, s := range v {
			if len(s) > max-size {
				return 0, true, false
			}
			size += len(s)
			if i > 0 {
				if size == max {
					return 0, true, false
				}
				size++
			}
		}
		return size, true, true
	case core.Uint16Value:
		count, width = len(v), 2
	case core.Int16Value:
		count, width = len(v), 2
	case core.Uint32Value:
		count, width = len(v), 4
	case core.Int32Value:
		count, width = len(v), 4
	case core.Uint64Value:
		count, width = len(v), 8
	case core.Int64Value:
		count, width = len(v), 8
	case core.Float32Value:
		count, width = len(v), 4
	case core.Float64Value:
		count, width = len(v), 8
	case core.TagValue:
		count, width = len(v), 4
	case core.SequenceValue, core.BulkDataValue, core.DiscardedValue:
		return 0, true, true
	default:
		return 0, false, false
	}
	if count > 100000 || count > max/width {
		return 0, true, false
	}
	return count * width, true, true
}
