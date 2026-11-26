package tractography

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Limits are cumulative per operation, counting repeated DAG visits. Zero uses
// DefaultLimits; positive overrides can only lower the finite ceilings.
type Limits struct {
	MaxBytes, MaxValueBytes, MaxTextBytes                        int64
	MaxTrackSets, MaxTracks, MaxPoints, MaxValues, MaxReferences int
	MaxElements, MaxItems, MaxSequenceDepth                      int
}

func DefaultLimits() Limits {
	return Limits{512 << 20, 128 << 20, 8 << 20, 1024, 100000, 8000000, 16000000, 100000, 100000, 100000, 16}
}

func normalize(l Limits) (Limits, error) {
	d := DefaultLimits()
	for _, p := range []struct {
		v   *int64
		cap int64
	}{{&l.MaxBytes, d.MaxBytes}, {&l.MaxValueBytes, d.MaxValueBytes}, {&l.MaxTextBytes, d.MaxTextBytes}} {
		if *p.v < 0 || *p.v > p.cap {
			return l, ErrInvalidOptions
		}
		if *p.v == 0 {
			*p.v = p.cap
		}
	}
	for _, p := range []struct {
		v   *int
		cap int
	}{{&l.MaxTrackSets, d.MaxTrackSets}, {&l.MaxTracks, d.MaxTracks}, {&l.MaxPoints, d.MaxPoints}, {&l.MaxValues, d.MaxValues}, {&l.MaxReferences, d.MaxReferences}, {&l.MaxElements, d.MaxElements}, {&l.MaxItems, d.MaxItems}, {&l.MaxSequenceDepth, d.MaxSequenceDepth}} {
		if *p.v < 0 || *p.v > p.cap {
			return l, ErrInvalidOptions
		}
		if *p.v == 0 {
			*p.v = p.cap
		}
	}
	return l, nil
}

func operation(ctx context.Context, l Limits) (*budget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := normalize(l)
	if err != nil {
		return nil, err
	}
	return &budget{ctx: ctx, l: n}, nil
}

func invalid(path string) error     { return fmt.Errorf("%w: %s", ErrInvalidData, path) }
func limited(path string) error     { return fmt.Errorf("%w: %s", ErrLimitExceeded, path) }
func unsupported(path string) error { return fmt.Errorf("%w: %s", ErrUnsupportedValue, path) }

type budget struct {
	ctx                                     context.Context
	l                                       Limits
	bytes, text                             int64
	elements, items, tracks, points, values int
	signatures, nestedSignatures            bool
}

func (b *budget) add(n int64) error {
	if n < 0 || n > b.l.MaxBytes-b.bytes {
		return limited("bytes")
	}
	b.bytes += n
	return b.ctx.Err()
}
func (b *budget) payload(n int64, text bool) error {
	if n < 0 || n > b.l.MaxValueBytes {
		return limited("value bytes")
	}
	if text {
		if n > b.l.MaxTextBytes-b.text {
			return limited("text bytes")
		}
		b.text += n
	}
	// Reserve eight binary lengths or 64 text lengths for simultaneous copies,
	// decoded text and VM helper storage. Structure is charged separately.
	factor := int64(8)
	if text {
		factor = 64
	}
	if n > (b.l.MaxBytes-b.bytes)/factor {
		return limited("copy bytes")
	}
	return b.add(n * factor)
}
func (b *budget) structure(elems, items int) error {
	if elems < 0 || elems > b.l.MaxElements-b.elements || items < 0 || items > b.l.MaxItems-b.items {
		return limited("structure")
	}
	b.elements += elems
	b.items += items
	// Conservative request-local element/map/header and item/facade storage;
	// this is a logical reservation, not an allocator/RSS measurement.
	return b.add(int64(elems)*512 + int64(items)*1024)
}
func (b *budget) textValues(ss ...string) error {
	for _, s := range ss {
		if err := b.payload(int64(len(s)), true); err != nil {
			return err
		}
	}
	return nil
}

// First walk checks raw structure before facade/cache/clone allocations.
// Duplicate detection happens only after the entire tree has passed quotas.
func (b *budget) walk(es []core.Element, depth int, root bool) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if depth > b.l.MaxSequenceDepth {
		return limited("sequence depth")
	}
	if err := b.sourceStructure(len(es), 0); err != nil {
		return err
	}
	for _, e := range es {
		if signatureTag(e.Tag()) {
			b.signatures = true
			if depth > 0 {
				b.nestedSignatures = true
			}
		}
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if root && bulkTag(e.Tag()) {
			continue
		}
		if seq, ok := e.Value.(core.SequenceValue); ok {
			if e.Tag() == tSets && len(seq.Items) > b.l.MaxTrackSets {
				return limited("sets")
			}
			if e.Tag() == tTracks {
				if len(seq.Items) > b.l.MaxTracks-b.tracks {
					return limited("tracks")
				}
				b.tracks += len(seq.Items)
			}
			if root && e.Tag() == tRefs && len(seq.Items) > b.l.MaxReferences {
				return limited("references")
			}
			if e.VR() != core.VRSQ {
				return invalid("sequence VR")
			}
			if len(seq.Items) > 0 && depth == b.l.MaxSequenceDepth {
				return limited("sequence depth")
			}
			if err := b.sourceStructure(0, len(seq.Items)); err != nil {
				return err
			}
			for _, item := range seq.Items {
				if err := b.walk(item.Elements, depth+1, false); err != nil {
					return err
				}
			}
			continue
		}
		if e.VR() == core.VRSQ {
			return unsupported("sequence")
		}
		if e.Value == nil {
			if !e.Header.HasLength() || e.Length() != 0 {
				return unsupported("deferred value")
			}
			continue
		}
		n, err := b.primitiveSize(e)
		if err != nil {
			return err
		}
		if e.Header.HasLength() && uint64(e.Length()) != uint64(n) {
			return invalid("value length")
		}
		if e.Tag() == tPoints {
			if e.VR() != core.VROF || uint64(n)%12 != 0 {
				return invalid("point stream")
			}
			count := int(uint64(n) / 12)
			if count > b.l.MaxPoints-b.points {
				return limited("points")
			}
			b.points += count
		}
		if e.Tag() == tFloats {
			if e.VR() != core.VROF || uint64(n)%4 != 0 {
				return invalid("measurement stream")
			}
			count := int(uint64(n) / 4)
			if count > b.l.MaxValues-b.values {
				return limited("values")
			}
			b.values += count
		}
		if v, ok := e.Value.(core.StringValue); ok {
			if len(v) > b.l.MaxValues {
				return limited("text VM")
			}
			if err := b.add(int64(len(v)) * 128); err != nil {
				return err
			}
		}
		if err := b.payload(int64(n), e.VR().IsStringLike()); err != nil {
			return err
		}
	}
	return nil
}
func duplicates(ctx context.Context, es []core.Element, root bool) error {
	seen := make(map[core.Tag]bool, len(es))
	for _, e := range es {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[e.Tag()] {
			return invalid("duplicate attribute")
		}
		seen[e.Tag()] = true
		if root && bulkTag(e.Tag()) {
			continue
		}
		if seq, ok := e.Value.(core.SequenceValue); ok {
			for _, item := range seq.Items {
				if err := duplicates(ctx, item.Elements, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

var snapshotGate = make(chan struct{}, 1)

func (b *budget) facade(src *object.Object) (*object.Object, error) {
	if src == nil {
		return nil, invalid("nil Object")
	}
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	// Len is non-allocating; charge before Elements allocates/compacts.
	if src.Len() > (b.l.MaxElements-b.elements)/4 {
		return nil, limited("root elements")
	}
	if err := b.add(int64(src.Len()) * 512); err != nil {
		return nil, err
	}
	select {
	case snapshotGate <- struct{}{}:
	case <-b.ctx.Done():
		return nil, b.ctx.Err()
	}
	es := src.Elements()
	<-snapshotGate
	if err := b.walk(es, 0, true); err != nil {
		return nil, err
	}
	if err := duplicates(b.ctx, es, true); err != nil {
		return nil, err
	}
	// Elements returned independent headers. Exclude ignored root bulk before
	// Object construction: its generic bookkeeping traverses SequenceValues,
	// including unexpected payload kinds under a bulk tag. Never pass those
	// unadmitted graphs to a helper after skipping them during preflight.
	admitted := es[:0]
	for _, e := range es {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		if !bulkTag(e.Tag()) {
			admitted = append(admitted, e)
		}
	}
	clear(es[len(admitted):])
	o := object.FromElements(admitted, nil)
	o.SetValueByteOrder(src.ValueByteOrder())
	if _, err := o.CharacterSet(); err != nil {
		return nil, invalid("character set")
	}
	return o, nil
}
func bulkTag(t core.Tag) bool {
	return t == core.TagPixelData || t == tag(0x7fe0, 8) || t == tag(0x7fe0, 9) || t == tag(0x5600, 0x20)
}

func (b *budget) code(c Code, depth int) error {
	if depth > b.l.MaxSequenceDepth {
		return limited("code depth")
	}
	if err := b.structure(20, 1); err != nil {
		return err
	}
	if err := b.textValues(c.Value, c.Scheme, c.Meaning, c.SchemeVersion); err != nil {
		return err
	}
	if c.Context != nil {
		x := c.Context
		if err := b.textValues(x.Identifier, x.UID, x.MappingResource, x.MappingResourceUID, x.MappingResourceName, x.Version, x.ExtensionFlag, x.LocalVersion, x.ExtensionCreatorUID); err != nil {
			return err
		}
	}
	if len(c.Equivalent) > b.l.MaxItems-b.items {
		return limited("equivalent codes")
	}
	for _, v := range c.Equivalent {
		if err := b.code(v, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func (b *budget) model(r *Results) error {
	if r == nil {
		return invalid("nil Results")
	}
	if len(r.TrackSets) > b.l.MaxTrackSets || len(r.References) > b.l.MaxReferences {
		return limited("sets or references")
	}
	if err := b.structure(64, 4); err != nil {
		return err
	}
	if err := b.textValues(r.SOPInstanceUID, r.StudyInstanceUID, r.SeriesInstanceUID, r.FrameOfReferenceUID, r.ContentDate, r.ContentTime, r.Label, r.Description); err != nil {
		return err
	}
	for _, s := range r.TrackSets {
		if err := b.structure(20, 1); err != nil {
			return err
		}
		if err := b.textValues(s.Label, s.Description); err != nil {
			return err
		}
		if len(s.Tracks) > b.l.MaxTracks-b.tracks {
			return limited("tracks")
		}
		b.tracks += len(s.Tracks)
		for _, c := range s.AnatomyModifiers {
			if err := b.code(c, 3); err != nil {
				return err
			}
		}
		for _, c := range []Code{s.Anatomy, s.Model} {
			if err := b.code(c, 2); err != nil {
				return err
			}
		}
		if s.Acquisition != nil {
			if err := b.code(*s.Acquisition, 2); err != nil {
				return err
			}
		}
		for _, a := range s.Algorithms {
			if err := b.structure(6, 1); err != nil {
				return err
			}
			if err := b.textValues(a.Name, a.Version, a.Parameters, a.Source); err != nil {
				return err
			}
			if err := b.code(a.Family, 3); err != nil {
				return err
			}
			if a.NameCode != nil {
				if err := b.code(*a.NameCode, 3); err != nil {
					return err
				}
			}
		}
		for _, t := range s.Tracks {
			if len(t.Points) > b.l.MaxPoints-b.points {
				return limited("points")
			}
			b.points += len(t.Points)
			if err := b.structure(3, 1); err != nil {
				return err
			}
			if err := b.array(len(t.Points), 12); err != nil {
				return err
			}
			if err := b.array(len(t.PointColors), 6); err != nil {
				return err
			}
		}
		for _, m := range s.Measurements {
			if err := b.structure(3, 1); err != nil {
				return err
			}
			for _, c := range []Code{m.Quantity, m.Units} {
				if err := b.code(c, 3); err != nil {
					return err
				}
			}
			for _, v := range m.Tracks {
				if err := b.structure(2, 1); err != nil {
					return err
				}
				if err := b.floatValues(len(v.Values)); err != nil {
					return err
				}
				if err := b.array(len(v.PointIndices), 4); err != nil {
					return err
				}
			}
		}
		for _, v := range s.TrackStatistics {
			if err := b.structure(4, 1); err != nil {
				return err
			}
			for _, c := range []Code{v.Quantity, v.Modifier, v.Units} {
				if err := b.code(c, 3); err != nil {
					return err
				}
			}
			if err := b.floatValues(len(v.Values)); err != nil {
				return err
			}
		}
		for _, v := range s.Statistics {
			if err := b.structure(4, 1); err != nil {
				return err
			}
			for _, c := range []Code{v.Quantity, v.Modifier, v.Units} {
				if err := b.code(c, 3); err != nil {
					return err
				}
			}
			if err := b.payload(8, false); err != nil {
				return err
			}
		}
	}
	for _, r := range r.References {
		if b.l.MaxSequenceDepth < 3 {
			return limited("reference depth")
		}
		if err := b.structure(10, 4); err != nil {
			return err
		}
		if err := b.textValues(r.SOPClassUID, r.SOPInstanceUID, r.StudyInstanceUID, r.SeriesInstanceUID); err != nil {
			return err
		}
		if err := b.array(len(r.Frames), 16); err != nil {
			return err
		}
		if err := b.array(len(r.Segments), 2); err != nil {
			return err
		}
	}
	if r.PerformedProcedureStep != nil {
		if err := b.textValues(r.PerformedProcedureStep.SOPClassUID, r.PerformedProcedureStep.SOPInstanceUID); err != nil {
			return err
		}
	}
	return b.ctx.Err()
}
func (b *budget) floatValues(n int) error {
	if n > b.l.MaxValues-b.values {
		return limited("values")
	}
	b.values += n
	return b.array(n, 4)
}

// Source headers are reserved for snapshot/facade, metadata clone, common clone
// and validation traversal. Repeated shared items are charged on every visit.
func (b *budget) sourceStructure(elems, items int) error {
	if elems < 0 || items < 0 || elems > (b.l.MaxElements-b.elements)/4 || items > (b.l.MaxItems-b.items)/4 {
		return limited("source structure")
	}
	return b.structure(elems*4, items*4)
}
func (b *budget) array(n int, width int64) error {
	if n < 0 || width <= 0 || int64(n) > b.l.MaxValueBytes/width {
		return limited("array bytes")
	}
	return b.payload(int64(n)*width, false)
}

// Inspect actual slice lengths before core's encoded-length conversions: raw
// and joined text may otherwise wrap the 32-bit DICOM VL on authored Objects.
func (b *budget) primitiveSize(e core.Element) (int64, error) {
	var count int
	var width int64
	compatible := true
	switch v := e.Value.(type) {
	case core.RawValue:
		if len(v)%2 != 0 {
			return 0, invalid("odd primitive length")
		}
		count, width = len(v), 1
	case core.StringValue:
		if !e.VR().IsStringLike() {
			return 0, invalid("text kind/VR")
		}
		if len(v) > b.l.MaxValues {
			return 0, limited("text VM")
		}
		n := int64(0)
		for i, x := range v {
			if err := b.ctx.Err(); err != nil {
				return 0, err
			}
			if int64(len(x)) > b.l.MaxValueBytes-n {
				return 0, limited("text length")
			}
			n += int64(len(x))
			if i > 0 {
				if n == b.l.MaxValueBytes {
					return 0, limited("text length")
				}
				n++
			}
		}
		if n%2 != 0 {
			n++
		}
		if n > b.l.MaxValueBytes {
			return 0, limited("text length")
		}
		return n, nil
	case core.Uint16Value:
		count, width, compatible = len(v), 2, e.VR() == core.VRUS || e.VR() == core.VROW
	case core.Int16Value:
		count, width, compatible = len(v), 2, e.VR() == core.VRSS
	case core.Uint32Value:
		count, width, compatible = len(v), 4, e.VR() == core.VRUL || e.VR() == core.VROL
	case core.Int32Value:
		count, width, compatible = len(v), 4, e.VR() == core.VRSL
	case core.Uint64Value:
		count, width, compatible = len(v), 8, e.VR() == core.VRUV || e.VR() == core.VROV
	case core.Int64Value:
		count, width, compatible = len(v), 8, e.VR() == core.VRSV
	case core.Float32Value:
		count, width, compatible = len(v), 4, e.VR() == core.VRFL || e.VR() == core.VROF
	case core.Float64Value:
		count, width, compatible = len(v), 8, e.VR() == core.VRFD || e.VR() == core.VROD
	case core.TagValue:
		count, width, compatible = len(v), 4, e.VR() == core.VRAT
	default:
		return 0, unsupported("value kind")
	}
	if !compatible {
		return 0, invalid("value kind/VR")
	}
	if int64(count) > b.l.MaxValueBytes/width {
		return 0, limited("value bytes")
	}
	return int64(count) * width, nil
}

func signatureTag(t core.Tag) bool { return t == tag(0x4ffe, 1) || t == tag(0xfffa, 0xfffa) }
