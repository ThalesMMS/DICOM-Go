package rtplan

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// WriteOptions requires an explicit source/common-module template. Semantic
// changes require a distinct SOPInstanceUID. No destination is published.
type WriteOptions struct {
	Template       *object.File
	Limits         Limits
	TransferSyntax transfer.Syntax
	SOPInstanceUID string
}
type writeBuilder struct {
	ctx    context.Context
	limits Limits
	err    error
}

func (w *writeBuilder) str(o *object.Object, t core.Tag, vr core.VR, s string) {
	o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: vr}, Value: core.StringValue{s}})
}
func (w *writeBuilder) integer(o *object.Object, t core.Tag, n int) {
	if e, ok := std.Dictionary.ByTag(t); ok {
		switch e.VR {
		case core.VRSS:
			if n < math.MinInt16 || n > math.MaxInt16 {
				w.err = ErrInvalidObject
				return
			}
			o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: core.Int16Value{int16(n)}})
			return
		case core.VRUS:
			if n < 0 || n > math.MaxUint16 {
				w.err = ErrInvalidObject
				return
			}
			o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: core.Uint16Value{uint16(n)}})
			return
		}
	}

	s, err := object.FormatIS(int64(n))
	if err != nil {
		w.err = pathError(ErrInvalidObject, t.String(), "integer cannot be encoded")
		return
	}
	w.str(o, t, core.VRIS, s)
}
func (w *writeBuilder) numbers(o *object.Object, t core.Tag, values []float64) {
	if e, ok := std.Dictionary.ByTag(t); ok && e.VR == core.VRFD {
		o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: core.Float64Value(append([]float64(nil), values...))})
		return
	}
	if e, ok := std.Dictionary.ByTag(t); ok && e.VR == core.VRFL {
		out := make(core.Float32Value, len(values))
		for i, v := range values {
			if !finite(v) || float64(float32(v)) != v {
				w.err = pathError(ErrInvalidObject, t.String(), "value is not exactly representable as FL")
				return
			}
			out[i] = float32(v)
		}
		o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: out})
		return
	}

	v := make(core.StringValue, len(values))
	for i, n := range values {
		var err error
		v[i], err = object.FormatDS(n)
		if err != nil {
			w.err = pathError(ErrInvalidObject, t.String(), "decimal cannot be encoded")
			return
		}
	}
	o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: core.VRDS}, Value: v})
}
func (w *writeBuilder) float(o *object.Object, t core.Tag, a Attribute[float64]) {
	o.Remove(t)
	if a.Present {
		if a.Value == nil {
			w.numbers(o, t, nil)
		} else {
			w.numbers(o, t, []float64{*a.Value})
		}
	}
}
func (w *writeBuilder) int(o *object.Object, t core.Tag, a Attribute[int]) {
	o.Remove(t)
	if a.Present {
		if a.Value == nil {
			if e, ok := std.Dictionary.ByTag(t); ok && e.VR == core.VRSS {
				o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: core.Int16Value(nil)})
			} else if ok && e.VR == core.VRUS {
				o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: e.VR}, Value: core.Uint16Value(nil)})
			} else {
				w.str(o, t, core.VRIS, "")
			}
		} else {
			w.integer(o, t, *a.Value)
		}
	}
}
func (w *writeBuilder) fl(o *object.Object, t core.Tag, v []float32) {
	o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: core.VRFL}, Value: core.Float32Value(append([]float32(nil), v...))})
}
func (w *writeBuilder) optionalText(o *object.Object, t core.Tag, vr core.VR, s string) {
	o.Remove(t)
	if s != "" {
		w.str(o, t, vr, s)
	}
}
func putSequence(o *object.Object, t core.Tag, items []*object.Object) {
	ds := make([]core.DataSet, len(items))
	for i, it := range items {
		es := it.Elements()
		sort.Slice(es, func(i, j int) bool { return es[i].Tag().Less(es[j].Tag()) })
		ds[i] = core.DataSet{Elements: es}
	}
	o.Put(core.Element{Header: core.ElementHeader{Tag: t, VR: core.VRSQ}, Value: core.SequenceValue{Items: ds}})
}
func (w *writeBuilder) keyed(o *object.Object, seq, key core.Tag) map[int]*object.Object {
	out := map[int]*object.Object{}
	items, _ := o.GetSequence(seq)
	for _, it := range items {
		v, err := it.GetInts(key)
		if err != nil || len(v) != 1 || int64(int(v[0])) != v[0] {
			w.err = pathError(ErrInvalidObject, key.String(), "template item key is invalid")
			continue
		}
		n := int(v[0])
		if out[n] != nil {
			w.err = pathError(ErrInvalidObject, key.String(), "template item key is duplicated")
		}
		out[n] = it
	}
	return out
}
func itemForKey(items map[int]*object.Object, n int) *object.Object {
	if o := items[n]; o != nil {
		return o
	}
	return object.New(std.Dictionary)
}
func beginWrite(ctx context.Context, opts WriteOptions, sop string, pixels bool, model any) (*object.File, *writeBuilder, error) {
	if ctx == nil {
		return nil, nil, pathError(ErrInvalidObject, "", "context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	limits, err := opts.Limits.normalized()
	if err != nil {
		return nil, nil, err
	}
	modelBytes, err := modelAdmission(ctx, reflect.ValueOf(model), limits)
	if err != nil {
		return nil, nil, err
	}
	src := opts.Template
	if src == nil || src.Dataset == nil {
		return nil, nil, pathError(ErrInvalidObject, "", "explicit template is required")
	}
	remaining := limits
	remaining.MaxModelBytes -= modelBytes
	datasetAdmission, err := preflightPrepared(ctx, src.Dataset, remaining, pixels)
	if err != nil {
		return nil, nil, err
	}
	remaining.MaxMetadataBytes -= datasetAdmission.bytes
	remaining.MaxModelBytes -= datasetAdmission.model
	remaining.MaxElements -= datasetAdmission.elements
	dataset, err := freshFacade(ctx, src.Dataset, pixels)
	if err != nil {
		return nil, nil, err
	}
	class, _ := dataset.GetUID(tagSOPClassUID)
	instance, _ := dataset.GetUID(tagSOPInstanceUID)
	if class != sop || instance == "" {
		return nil, nil, pathError(ErrInvalidObject, tagSOPClassUID.String(), "template SOP identity")
	}
	var meta *object.Object
	if src.Meta != nil {
		if _, err := preflightPrepared(ctx, src.Meta, remaining, false); err != nil {
			return nil, nil, err
		}
		if err := validateFileMetaGroups(ctx, src.Meta); err != nil {
			return nil, nil, err
		}
		meta, err = freshFacade(ctx, src.Meta, false)
		if err != nil {
			return nil, nil, err
		}
		mc, _ := meta.GetUID(ft(0x00020002))
		mi, _ := meta.GetUID(ft(0x00020003))
		if mc != class || mi != instance {
			return nil, nil, pathError(ErrInvalidObject, "(0002,0002)", "template meta identity")
		}
	} else {
		meta = object.New(std.Dictionary)
	}
	// Validate the original declared representations before normalizing either
	// meta or raw bytes. A mismatch cannot be repaired by template cloning.
	if src.TransferSyntax.UID != "" {
		if !nativeSyntax(src.TransferSyntax) {
			return nil, nil, ErrUnsupportedFeature
		}
		if dataset.ValueByteOrder() != src.TransferSyntax.ByteOrder {
			return nil, nil, pathError(ErrInvalidObject, "(0002,0010)", "template syntax and byte order differ")
		}
		if src.Meta != nil {
			ts, _ := meta.GetUID(ft(0x00020010))
			if ts != src.TransferSyntax.UID {
				return nil, nil, pathError(ErrInvalidObject, "(0002,0010)", "template transfer syntax identity differs")
			}
		}
	}
	if err := refuseRetainedSignatures(ctx, dataset); err != nil {
		return nil, nil, err
	}
	if err := refuseRetainedSignatures(ctx, meta); err != nil {
		return nil, nil, err
	}
	w := &writeBuilder{ctx: ctx, limits: limits}
	w.str(meta, ft(0x00020002), core.VRUI, class)
	w.str(meta, ft(0x00020003), core.VRUI, instance)
	w.str(meta, ft(0x00020010), core.VRUI, transfer.ExplicitVRLittleEndian.UID)
	// RawValue bytes are already encoded in the source byte order. Normalize
	// fixed-width values explicitly using the existing value encoder helper.
	dataset, err = nativeOrderFacade(ctx, dataset, binary.LittleEndian)
	if err != nil {
		return nil, nil, err
	}
	// The independent bounded clone retains the source Unicode context.
	f, err := object.CloneFileWithOptions(&object.File{Meta: meta, Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}, object.CloneFileOptions{MaxElementBytes: limits.MaxValueBytes, MaxPixelDataBytes: limits.MaxPixelBytes, MaxTotalBytes: limits.MaxMetadataBytes + limits.MaxPixelBytes + (1 << 20), MaxSequenceDepth: limits.MaxDepth, MaxElements: limits.MaxElements, MaxItems: limits.MaxElements})
	if err != nil {
		return nil, nil, pathError(ErrInvalidObject, "", "bounded template clone failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return f, w, nil
}
func finishWrite(f *object.File, w *writeBuilder, opts WriteOptions, sop string, changed bool) (*object.File, error) {
	if w.err != nil {
		return nil, w.err
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	original, _ := f.Dataset.GetUID(tagSOPInstanceUID)
	uid := opts.SOPInstanceUID
	if uid == "" {
		uid = original
	}
	if !core.IsValidUID(uid) {
		return nil, pathError(ErrInvalidObject, tagSOPInstanceUID.String(), "instance UID is invalid")
	}
	for _, t := range []core.Tag{tagStudyInstanceUID, tagSeriesInstanceUID, tagFrameOfReferenceUID} {
		role, _ := f.Dataset.GetUID(t)
		if role == uid {
			return nil, pathError(ErrInvalidObject, tagSOPInstanceUID.String(), "instance UID collides with another identity role")
		}
	}
	if changed && uid == original {
		return nil, pathError(ErrInvalidObject, tagSOPInstanceUID.String(), "semantic change requires a new instance UID")
	}
	w.str(f.Dataset, tagSOPClassUID, core.VRUI, sop)
	w.str(f.Dataset, tagSOPInstanceUID, core.VRUI, uid)
	w.str(f.Meta, ft(0x00020002), core.VRUI, sop)
	w.str(f.Meta, ft(0x00020003), core.VRUI, uid)
	syntax := opts.TransferSyntax
	if syntax.UID == "" {
		syntax = transfer.ExplicitVRLittleEndian
	}
	if syntax.UID != transfer.ExplicitVRLittleEndian.UID && syntax.UID != transfer.ImplicitVRLittleEndian.UID && syntax.UID != transfer.ExplicitVRBigEndian.UID {
		return nil, ErrUnsupportedFeature
	}
	var err error
	f.Dataset, err = nativeOrderFacade(w.ctx, f.Dataset, syntax.ByteOrder)
	if err != nil {
		return nil, err
	}
	f.TransferSyntax = syntax
	w.str(f.Meta, ft(0x00020010), core.VRUI, syntax.UID)
	admitted, err := preflight(w.ctx, f.Dataset, w.limits, sop == RTImageStorage)
	if err != nil {
		return nil, err
	}
	remaining := w.limits
	remaining.MaxMetadataBytes -= admitted.bytes
	remaining.MaxModelBytes -= admitted.model
	remaining.MaxElements -= admitted.elements
	if _, err := preflightPrepared(w.ctx, f.Meta, remaining, false); err != nil {
		return nil, err
	}
	if err := strictObject(w.ctx, f.Dataset, w.limits); err != nil {
		return nil, err
	}
	return f, nil
}
func semanticEqual(a, b any) bool { return semanticValues(reflect.ValueOf(a), reflect.ValueOf(b)) }
func semanticValues(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return semanticValues(a.Elem(), b.Elem())
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			name := a.Type().Field(i).Name
			if name == "ModelBytes" || name == "Warnings" || name == "SOPInstanceUID" || name == "Encoded" || name == "EncodedDeviceTypes" || name == "EncodedSnoutPosition" || name == "EncodedGantryPitchAngle" || name == "EncodedRangeShifterSettings" {
				continue
			}
			if !semanticValues(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !semanticValues(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Float64:
		if a.Float() == b.Float() {
			return true
		}
		sa, ea := object.FormatDS(a.Float())
		sb, eb := object.FormatDS(b.Float())
		if ea != nil || eb != nil {
			return false
		}
		qa, _ := strconv.ParseFloat(sa, 64)
		qb, _ := strconv.ParseFloat(sb, 64)
		return qa == qb
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}
func admitModel(ctx context.Context, v reflect.Value, l Limits) error {
	_, err := modelAdmission(ctx, v, l)
	return err
}
func modelAdmission(ctx context.Context, v reflect.Value, l Limits) (int64, error) {
	var bytes int64
	var beams, entities, points, spots int
	active := map[struct {
		typ reflect.Type
		ptr uintptr
	}]bool{}
	var visit func(reflect.Value, int) error
	visit = func(v reflect.Value, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !v.IsValid() {
			return nil
		}
		if depth > 128 {
			return ErrResourceLimit
		}
		reserve := func(n int64) error {
			if n < 0 || n > l.MaxModelBytes-bytes {
				return ErrResourceLimit
			}
			bytes += n
			return nil
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return nil
			}
			if v.Kind() == reflect.Pointer {
				key := struct {
					typ reflect.Type
					ptr uintptr
				}{v.Type(), v.Pointer()}
				if active[key] {
					return pathError(ErrInvalidObject, "", "cyclic model")
				}
				active[key] = true
				defer delete(active, key)
			}
			return visit(v.Elem(), depth+1)
		case reflect.Struct:
			if strings.HasPrefix(v.Type().Name(), "Attribute[") && !v.FieldByName("Present").Bool() && !v.FieldByName("Value").IsNil() {
				return pathError(ErrInvalidObject, "", "absent attribute has a value")
			}
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).Name == "SpotPositions" {
					n := v.Field(i).Len()
					if n%2 != 0 {
						return pathError(ErrInvalidObject, "", "spot position map must contain pairs")
					}
					n /= 2
					if n > l.MaxSpots-spots {
						return ErrResourceLimit
					}
					spots += n
				}
				if err := visit(v.Field(i), depth+1); err != nil {
					return err
				}
			}
		case reflect.String:
			return reserve(int64(v.Len())*4 + 32)
		case reflect.Slice, reflect.Array:
			switch v.Type().Elem().Name() {
			case "Beam", "IonBeam", "BeamSession", "RecordBeamSession":
				beams += v.Len()
			case "BrachySource", "BrachySetup", "BrachyChannel", "RangeShifter", "BrachySession", "RecordedBrachySource", "RecordBrachySession", "RecordBrachyChannel", "RecordBrachyPulse", "RecordedRangeShifter", "RecordedSourceApplicator", "RecordFractionGroupSummary", "RecordFractionStatus":
				entities += v.Len()
			case "ControlPoint", "IonControlPoint", "BrachyControlPoint", "DeliveryControlPoint", "RecordControlPoint", "RecordBrachyControlPoint":
				points += v.Len()
			}
			if beams > l.MaxBeams || entities > l.MaxEntities || points > l.MaxControlPoints {
				return ErrResourceLimit
			}
			if v.Type().Elem().Kind() == reflect.Uint8 {
				if int64(v.Len()) > l.MaxPixelBytes {
					return ErrResourceLimit
				}
				return reserve(int64(v.Len()))
			}
			if v.Len() > l.MaxElements && v.Type().Elem().Kind() != reflect.Float32 && v.Type().Elem().Kind() != reflect.Uint8 {
				return ErrResourceLimit
			}
			for i := 0; i < v.Len(); i++ {
				if err := visit(v.Index(i), depth+1); err != nil {
					return err
				}
			}
		case reflect.Float32, reflect.Float64:
			if !finite(v.Float()) {
				return pathError(ErrInvalidObject, "", "non-finite model value")
			}
			return reserve(8)
		default:
			return reserve(16)
		}
		return nil
	}
	if err := visit(v, 0); err != nil {
		return 0, fmt.Errorf("%w: model admission", err)
	}
	return bytes, nil
}

// nativeOrderFacade detaches sequence containers and fixed-width raw values
// only when their byte order changes. Admission has already bounded this tree.
func nativeOrderFacade(ctx context.Context, source *object.Object, order binary.ByteOrder) (*object.Object, error) {
	if source.ValueByteOrder() == order {
		return source, nil
	}
	var visit func([]core.Element) ([]core.Element, error)
	visit = func(input []core.Element) ([]core.Element, error) {
		output := make([]core.Element, len(input))
		for i, e := range input {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			switch v := e.Value.(type) {
			case core.RawValue:
				width := valueencode.ByteOrderWidth(e.VR())
				if width > 1 {
					if len(v)%width != 0 {
						return nil, pathError(ErrInvalidObject, e.Tag().String(), "raw fixed-width value extent is invalid")
					}
					e.Value = core.RawValue(valueencode.ReverseFixedWidth(v, width))
				}
			case core.SequenceValue:
				items := make([]core.DataSet, len(v.Items))
				for j, it := range v.Items {
					es, err := visit(it.Elements)
					if err != nil {
						return nil, err
					}
					items[j] = core.DataSet{Elements: es}
				}
				e.Value = core.SequenceValue{Items: items}
			}
			output[i] = e
		}
		return output, nil
	}
	elements, err := visit(source.Elements())
	if err != nil {
		return nil, err
	}
	out := object.FromElements(elements, std.Dictionary)
	out.SetValueByteOrder(order)
	return out, nil
}

// withoutIgnoredBulk removes ignored bulk at every sequence depth without
// invoking its Value implementation or any source replay provider.
func withoutIgnoredBulk(ctx context.Context, source *object.Object) (*object.Object, error) {
	var visit func([]core.Element) ([]core.Element, error)
	visit = func(input []core.Element) ([]core.Element, error) {
		out := make([]core.Element, 0, len(input))
		for _, e := range input {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if bulkTag(e.Tag()) {
				continue
			}
			if sq, ok := e.Value.(core.SequenceValue); ok {
				items := make([]core.DataSet, len(sq.Items))
				for i, it := range sq.Items {
					es, err := visit(it.Elements)
					if err != nil {
						return nil, err
					}
					items[i] = core.DataSet{Elements: es}
				}
				e.Value = core.SequenceValue{Items: items}
			}
			out = append(out, e)
		}
		return out, nil
	}
	snapshot, err := snapshotElements(ctx, source)
	if err != nil {
		return nil, err
	}
	es, err := visit(snapshot)
	if err != nil {
		return nil, err
	}
	out := object.FromElements(es, std.Dictionary)
	out.SetValueByteOrder(source.ValueByteOrder())
	return out, nil
}

// Writers cannot retain authentication bytes across template re-encoding or
// typed edits. Inspection readers neither verify nor remove signatures.
func refuseRetainedSignatures(ctx context.Context, obj *object.Object) error {
	var visit func([]core.Element) error
	visit = func(es []core.Element) error {
		for _, e := range es {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Tag() == ft(0x4ffe0001) || e.Tag() == ft(0xfffafffa) {
				return pathError(ErrUnsupportedFeature, e.Tag().String(), "retained digital signature metadata")
			}
			if sq, ok := e.Value.(core.SequenceValue); ok {
				for _, it := range sq.Items {
					if err := visit(it.Elements); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return visit(obj.Elements())
}
