package rtplan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

var ErrResourceLimit = errors.New("dicom/rtplan: resource limit exceeded")
var ErrUnmaterialized = errors.New("dicom/rtplan: metadata must be materialized")
var ErrUnsupportedFeature = errors.New("dicom/rtplan: unsupported feature")

// Limits are cumulative logical admissions, not an RSS bound. Zero selects a
// finite default. Input Objects remain caller owned; model fields are owned.
type Limits struct {
	MaxMetadataBytes, MaxValueBytes, MaxModelBytes, MaxPixelBytes                         int64
	MaxElements, MaxDepth, MaxBeams, MaxEntities, MaxControlPoints, MaxSpots, MaxProblems int
}

func DefaultLimits() Limits {
	return Limits{64 << 20, 16 << 20, 128 << 20, 64 << 20, 100000, 32, 1024, 10000, 100000, 1000000, 256}
}

// Normalized returns finite effective admissions; zero fields select defaults.
func (l Limits) Normalized() (Limits, error) { return l.normalized() }
func (l Limits) normalized() (Limits, error) {
	d := DefaultLimits()
	pv := []*int64{&l.MaxMetadataBytes, &l.MaxValueBytes, &l.MaxModelBytes, &l.MaxPixelBytes}
	dv := []int64{d.MaxMetadataBytes, d.MaxValueBytes, d.MaxModelBytes, d.MaxPixelBytes}
	for i, p := range pv {
		if *p < 0 || *p > math.MaxInt64/16 {
			return l, fmt.Errorf("%w: negative limit", ErrResourceLimit)
		}
		if *p == 0 {
			*p = dv[i]
		}
	}
	pi := []*int{&l.MaxElements, &l.MaxDepth, &l.MaxBeams, &l.MaxEntities, &l.MaxControlPoints, &l.MaxSpots, &l.MaxProblems}
	di := []int{d.MaxElements, d.MaxDepth, d.MaxBeams, d.MaxEntities, d.MaxControlPoints, d.MaxSpots, d.MaxProblems}
	for i, p := range pi {
		if *p < 0 || int64(*p) > math.MaxInt32 {
			return l, fmt.Errorf("%w: negative limit", ErrResourceLimit)
		}
		if *p == 0 {
			*p = di[i]
		}
	}
	if l.MaxDepth > 128 {
		return l, fmt.Errorf("%w: depth exceeds 128", ErrResourceLimit)
	}
	return l, nil
}

type ReadOptions struct {
	Limits    Limits
	StrictIOD bool
}

// Attribute distinguishes absent, Type 2 empty, and a declared value.
type Attribute[T any] struct {
	Present bool
	Value   *T
}

func Value[T any](v T) Attribute[T] { return Attribute[T]{true, &v} }

func ft(v uint32) core.Tag { return core.NewTag(uint16(v>>16), uint16(v)) }
func pathError(err error, path, reason string) error {
	return fmt.Errorf("%w: %s: %s", err, path, reason)
}
func finite(v float64) bool   { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func bulkTag(t core.Tag) bool { return t.Group == 0x7fe0 || t == ft(0x54001010) }

type admission struct {
	ctx                                             context.Context
	limits                                          Limits
	bytes, pixels, model                            int64
	elements, items, beams, entities, points, spots int
	inheritedBytes                                  int64
	shifterSettings                                 int
	active                                          map[uintptr]bool
}

func preflight(ctx context.Context, obj *object.Object, l Limits, pixels bool) (*admission, error) {
	if ctx == nil {
		return nil, pathError(ErrInvalidObject, "", "context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := l.normalized()
	if err != nil {
		return nil, err
	}
	return preflightPrepared(ctx, obj, limits, pixels)
}

// preflightPrepared uses normalized remaining budgets, including a zero balance.
func preflightPrepared(ctx context.Context, obj *object.Object, limits Limits, pixels bool) (*admission, error) {
	if obj == nil {
		return nil, pathError(ErrInvalidObject, "", "dataset is nil")
	}
	b := &admission{ctx: ctx, limits: limits, active: map[uintptr]bool{}}
	// Diagnostics have independent error/warning caps and depth-bounded paths.
	if err := b.reserve(int64(limits.MaxProblems), 8192); err != nil {
		return nil, err
	}
	if obj.Len() > limits.MaxElements {
		return nil, pathError(ErrResourceLimit, "", "element count")
	}
	if err := b.reserve(int64(obj.Len()), 64); err != nil {
		return nil, err
	}
	es, err := snapshotElements(ctx, obj)
	if err != nil {
		return nil, err
	}
	if err := b.visit(es, "", 0, pixels); err != nil {
		return nil, err
	}
	// CP decoding owns inherited numeric buffers and settings per item.
	if err := b.reserve(b.inheritedBytes*6, int64(b.points)); err != nil {
		return nil, err
	}
	if err := b.reserve(int64(b.shifterSettings)*256, int64(b.points)); err != nil {
		return nil, err
	}

	return b, nil
}
func (b *admission) reserve(n, m int64) error {
	if n < 0 || m < 0 || m != 0 && n > (b.limits.MaxModelBytes-b.model)/m {
		return pathError(ErrResourceLimit, "", "model allowance")
	}
	b.model += n * m
	return nil
}
func (b *admission) visit(es []core.Element, path string, depth int, pixels bool) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if depth > b.limits.MaxDepth || len(es) > b.limits.MaxElements-b.elements {
		return pathError(ErrResourceLimit, path, "depth or element count")
	}
	key := reflect.ValueOf(es).Pointer()
	if key != 0 {
		if b.active[key] {
			return pathError(ErrInvalidObject, path, "cyclic sequence")
		}
		b.active[key] = true
		defer delete(b.active, key)
	}
	// Six header/container copies cover stripping, independent facades,
	// endian normalization, clone and validation staging after admission.
	// This is a conservative logical allowance, not an RSS estimate.
	if err := b.reserve(int64(len(es)), 576); err != nil {
		return err
	}
	b.elements += len(es)
	seen := make(map[core.Tag]bool, len(es))
	for _, e := range es {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		p := join(path, e.Tag())
		if seen[e.Tag()] {
			return pathError(ErrInvalidObject, p, "duplicate attribute")
		}
		seen[e.Tag()] = true
		if bulkTag(e.Tag()) {
			if !pixels {
				continue
			}
			if e.Tag() != ft(0x7fe00010) {
				return pathError(ErrUnsupportedFeature, p, "non-integer Pixel Data")
			}
			raw, ok := e.Value.(core.RawValue)
			if !ok {
				return pathError(ErrUnmaterialized, p, "native Pixel Data is required")
			}
			if int64(len(raw)) > b.limits.MaxPixelBytes-b.pixels {
				return pathError(ErrResourceLimit, p, "pixel bytes")
			}
			if err := b.reserve(int64(len(raw)), 1); err != nil {
				return err
			}
			b.pixels += int64(len(raw))
			continue
		}
		if sq, ok := e.Value.(core.SequenceValue); ok {
			if e.VR() != core.VRSQ {
				return pathError(ErrInvalidObject, p, "sequence VR")
			}
			n := len(sq.Items)
			if e.Tag() == ft(0x300a0360) && n > b.shifterSettings {
				b.shifterSettings = n
			}
			if n > b.limits.MaxElements-b.items {
				return pathError(ErrResourceLimit, p, "item count")
			}
			if err := b.reserve(int64(n), 192); err != nil {
				return err
			}
			b.items += n
			switch uint32(e.Tag().Group)<<16 | uint32(e.Tag().Element) {
			case 0x300a00b0, 0x300a03a2, 0x30080020, 0x30080021:
				b.beams += n
			case 0x300a0314, 0x300a0210, 0x300a0230, 0x300a0280, 0x30080100, 0x30080110, 0x30080130, 0x300800f2, 0x30080220, 0x30080240, 0x30080171:
				b.entities += n
			case 0x300a0111, 0x300a03a8, 0x300a02d0, 0x30080040, 0x30080041, 0x30080160, 0x30080173:
				if err := b.reserve(int64(n), 1024); err != nil {
					return err
				}
				b.points += n
			}
			if b.beams > b.limits.MaxBeams || b.entities > b.limits.MaxEntities || b.points > b.limits.MaxControlPoints {
				return pathError(ErrResourceLimit, p, "family item count")
			}
			for i, it := range sq.Items {
				if err := b.visit(it.Elements, fmt.Sprintf("%s[%d]", p, i), depth+1, pixels); err != nil {
					return err
				}
			}
			continue
		}
		if e.VR() == core.VRSQ {
			return pathError(ErrInvalidObject, p, "SQ requires a sequence value")
		}
		if e.Value == nil {
			if e.Header.HasLength() && e.Header.Length == 0 {
				continue
			}
			return pathError(ErrUnmaterialized, p, "nil metadata value")
		}
		n, ok := primitiveBytes(e.Value)
		if !ok {
			return pathError(ErrUnmaterialized, p, "source-backed or unsupported metadata value")
		}
		if n > b.limits.MaxValueBytes || n > b.limits.MaxMetadataBytes-b.bytes {
			return pathError(ErrResourceLimit, p, "metadata bytes")
		}
		if err := b.reserve(n, 4); err != nil {
			return err
		}
		switch x := e.Value.(type) {
		case core.StringValue:
			if err := b.reserve(int64(len(x)), 48); err != nil {
				return err
			}
		case core.RawValue:
			switch e.VR() {
			case core.VRDS, core.VRIS, core.VRCS, core.VRLO, core.VRSH, core.VRUI, core.VRPN, core.VRDA, core.VRTM, core.VRDT:
				if err := b.reserve(int64(bytes.Count(x, []byte{92}))+1, 48); err != nil {
					return err
				}
			}
		}
		if e.Tag() == tagLeafJawPositions {
			count := int64(0)
			switch x := e.Value.(type) {
			case core.StringValue:
				count = int64(len(x))
			case core.RawValue:
				switch e.VR() {
				case core.VRFL:
					count = int64(len(x)) / 4
				case core.VRFD:
					count = int64(len(x)) / 8
				default:
					count = int64(bytes.Count(x, []byte{92})) + 1
				}
			case core.Float32Value:
				count = int64(len(x))
			case core.Float64Value:
				count = int64(len(x))
			}
			if count*8 > b.inheritedBytes {
				b.inheritedBytes = count * 8
			}
		}
		b.bytes += n
		if n == 0 && e.Header.HasLength() && e.Header.Length != 0 {
			return pathError(ErrUnmaterialized, p, "nonzero backing with empty value")
		}
		if e.Tag() == ft(0x300a0394) {
			if n%8 != 0 {
				return pathError(ErrInvalidObject, p, "spot map length")
			}
			n /= 8
			if n > int64(b.limits.MaxSpots-b.spots) {
				return pathError(ErrResourceLimit, p, "spot count")
			}
			if err := b.reserve(n, 24); err != nil {
				return err
			}
			b.spots += int(n)
		}
	}
	return nil
}
func primitiveBytes(v core.Value) (int64, bool) {
	switch x := v.(type) {
	case core.RawValue:
		return int64(len(x)), true
	case core.StringValue:
		var n int64
		for _, s := range x {
			if int64(len(s)) > math.MaxInt64-n-1 {
				return 0, false
			}
			n += int64(len(s)) + 1
		}
		return n, true
	case core.Uint16Value:
		return int64(len(x)) * 2, true
	case core.Int16Value:
		return int64(len(x)) * 2, true
	case core.Uint32Value:
		return int64(len(x)) * 4, true
	case core.Int32Value:
		return int64(len(x)) * 4, true
	case core.Uint64Value:
		return int64(len(x)) * 8, true
	case core.Int64Value:
		return int64(len(x)) * 8, true
	case core.Float32Value:
		return int64(len(x)) * 4, true
	case core.Float64Value:
		return int64(len(x)) * 8, true
	case core.TagValue:
		return int64(len(x)) * 4, true
	}
	return 0, false
}

// snapshotElements serializes the Object's lazy order compaction only. Input
// mutation while reading remains outside the contract; no I/O or parsing holds
// this gate. Callers admit the root header copy from Len before invoking it.
var snapshotGate = make(chan struct{}, 1)

func snapshotElements(ctx context.Context, obj *object.Object) ([]core.Element, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case snapshotGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	es := obj.Elements()
	<-snapshotGate
	return es, ctx.Err()
}

// freshFacade isolates mutable caches after admission. Generic Object
// bookkeeping must never see an ignored bulk graph. The image path has admitted
// only native Pixel Data and therefore retains that bounded value.
func freshFacade(ctx context.Context, obj *object.Object, pixels bool) (*object.Object, error) {
	if !pixels {
		return withoutIgnoredBulk(ctx, obj)
	}
	es, err := snapshotElements(ctx, obj)
	if err != nil {
		return nil, err
	}
	out := object.FromElements(es, std.Dictionary)
	out.SetValueByteOrder(obj.ValueByteOrder())
	return out, nil
}
func strictObject(ctx context.Context, obj *object.Object, l Limits) error {
	result, err := validation.ValidateDataSet(ctx, core.DataSet{Elements: obj.Elements()}, validation.Options{MaxElements: l.MaxElements, MaxDepth: l.MaxDepth, MaxFindings: l.MaxProblems, Dictionary: std.Dictionary, ByteOrder: obj.ValueByteOrder()})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return pathError(ErrInvalidObject, "", "VR/VM validation failed")
	}
	if result.Report.HasErrors() || result.Report.Truncated {
		return pathError(ErrInvalidObject, "", "VR/VM validation failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	report, err := iod.Validate(obj, iod.Options{CheckValues: true, MaxFindings: l.MaxProblems})
	if err != nil || len(report.Errors()) != 0 || report.Truncated != 0 {
		return pathError(ErrInvalidObject, "", "IOD validation failed")
	}
	return ctx.Err()
}
func ReadWithOptions(ctx context.Context, file *object.File, opts ReadOptions) (*Plan, error) {
	if file == nil {
		return nil, pathError(ErrInvalidObject, "", "file is nil")
	}
	adjusted, err := fileReadOptions(ctx, file, opts)
	if err != nil {
		return nil, err
	}
	plan, err := ReadDatasetWithOptions(ctx, file.Dataset, adjusted)
	if err != nil {
		return nil, err
	}
	effective, err := opts.Limits.normalized()
	if err != nil {
		return nil, err
	}
	plan.ModelBytes += effective.MaxModelBytes - adjusted.Limits.MaxModelBytes
	return plan, nil
}
func ReadDatasetWithOptions(ctx context.Context, obj *object.Object, opts ReadOptions) (*Plan, error) {
	b, err := preflight(ctx, obj, opts.Limits, false)
	if err != nil {
		return nil, err
	}
	obj, err = freshFacade(ctx, obj, false)
	if err != nil {
		return nil, err
	}
	if opts.StrictIOD {
		if err := strictObject(ctx, obj, b.limits); err != nil {
			return nil, err
		}
	}
	sop, _ := obj.GetUID(tagSOPClassUID)
	if sop != RTPlanStorage && sop != RTIonPlanStorage {
		return nil, ErrUnsupportedSOPClass
	}
	r := &reader{ctx: ctx, maxProblems: b.limits.MaxProblems}
	plan := r.readPlan(obj)
	plan.SOPClassUID = sop
	if sop == RTIonPlanStorage {
		readIon(r, obj, plan)
	}
	readBrachy(r, obj, plan)
	validatePlan(r, plan)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(r.errors) > 0 {
		return nil, &ValidationError{Problems: r.errors}
	}
	plan.ModelBytes = b.model
	plan.Warnings = r.warnings
	return plan, nil
}

// fileReadOptions admits meta and dataset jointly before identity helpers and
// leaves only the remaining allowance to typed decoding of the dataset.
func fileReadOptions(ctx context.Context, f *object.File, opts ReadOptions) (ReadOptions, error) {
	if ctx == nil || f == nil || f.Dataset == nil {
		return opts, ErrInvalidObject
	}
	if err := ctx.Err(); err != nil {
		return opts, err
	}
	l, err := opts.Limits.normalized()
	if err != nil {
		return opts, err
	}
	remaining := l
	if nativeSyntax(f.TransferSyntax) && f.Dataset.ValueByteOrder() != f.TransferSyntax.ByteOrder {
		return opts, pathError(ErrInvalidObject, "(0002,0010)", "file syntax and dataset byte order differ")
	}
	if f.Meta != nil {
		a, err := preflightPrepared(ctx, f.Meta, l, false)
		if err != nil {
			return opts, err
		}
		if err := validateFileMetaGroups(ctx, f.Meta); err != nil {
			return opts, err
		}
		remaining.MaxMetadataBytes -= a.bytes
		remaining.MaxElements -= a.elements
		remaining.MaxModelBytes -= a.model
	}
	if remaining.MaxMetadataBytes <= 0 || remaining.MaxElements <= 0 || remaining.MaxModelBytes <= 0 {
		return opts, ErrResourceLimit
	}
	if _, err := preflightPrepared(ctx, f.Dataset, remaining, false); err != nil {
		return opts, err
	}
	if f.Meta != nil {
		meta, err := freshFacade(ctx, f.Meta, false)
		if err != nil {
			return opts, err
		}
		ds, err := freshFacade(ctx, f.Dataset, false)
		if err != nil {
			return opts, err
		}
		for _, pair := range []struct{ m, d core.Tag }{{ft(0x00020002), tagSOPClassUID}, {ft(0x00020003), tagSOPInstanceUID}} {
			m, _ := meta.GetUID(pair.m)
			d, _ := ds.GetUID(pair.d)
			if m == "" || d == "" || m != d {
				return opts, pathError(ErrInvalidObject, pair.m.String(), "file meta and dataset identity differ")
			}
		}
		ts, _ := meta.GetUID(ft(0x00020010))
		if f.TransferSyntax.UID != "" && ts != f.TransferSyntax.UID {
			return opts, pathError(ErrInvalidObject, "(0002,0010)", "file transfer syntax identity differs")
		}
	}
	opts.Limits = remaining
	return opts, nil
}

// File meta is a separate group-0002 namespace. In particular, bulk outside
// that namespace cannot bypass metadata admission as ignored plan bulk.
func validateFileMetaGroups(ctx context.Context, meta *object.Object) error {
	es, err := snapshotElements(ctx, meta)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Tag().Group != 0x0002 {
			return pathError(ErrInvalidObject, e.Tag().String(), "file meta contains a dataset attribute")
		}
	}
	return nil
}
