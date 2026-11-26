package tractography

import (
	"context"
	"encoding/binary"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
)

func tag(g, e uint16) core.Tag { return core.NewTag(g, e) }

var (
	tSOPClass        = tag(8, 0x16)
	tSOPInstance     = tag(8, 0x18)
	tStudy           = tag(0x20, 0xd)
	tSeries          = tag(0x20, 0xe)
	tFrame           = tag(0x20, 0x52)
	tSeriesNumber    = tag(0x20, 0x11)
	tInstanceNumber  = tag(0x20, 0x13)
	tDate            = tag(8, 0x23)
	tTime            = tag(8, 0x33)
	tLabel           = tag(0x70, 0x80)
	tDescription     = tag(0x70, 0x81)
	tModality        = tag(8, 0x60)
	tCharset         = tag(8, 5)
	tSets            = tag(0x66, 0x101)
	tNumber          = tag(0x66, 0x105)
	tSetLabel        = tag(0x66, 0x106)
	tSetDescription  = tag(0x66, 0x107)
	tAnatomy         = tag(0x66, 0x108)
	tModifier        = tag(0x40, 0xa195)
	tTracks          = tag(0x66, 0x102)
	tPoints          = tag(0x66, 0x16)
	tColor           = tag(0x62, 0xd)
	tColors          = tag(0x66, 0x103)
	tThickness       = tag(0x66, 0x38)
	tMeasurements    = tag(0x66, 0x121)
	tQuantity        = tag(0x40, 0xa043)
	tUnits           = tag(0x40, 0x8ea)
	tValuesSeq       = tag(0x66, 0x132)
	tFloats          = tag(0x66, 0x125)
	tIndices         = tag(0x66, 0x129)
	tTrackStats      = tag(0x66, 0x130)
	tSetStats        = tag(0x66, 0x124)
	tScalar          = tag(0x40, 0xa161)
	tAcquisition     = tag(0x66, 0x133)
	tModel           = tag(0x66, 0x134)
	tAlgorithms      = tag(0x66, 0x104)
	tFamily          = tag(0x66, 0x2f)
	tNameCode        = tag(0x66, 0x30)
	tName            = tag(0x66, 0x36)
	tVersion         = tag(0x66, 0x31)
	tParameters      = tag(0x66, 0x32)
	tAlgorithmSource = tag(0x24, 0x202)
	tRefs            = tag(8, 0x114a)
	tRefClass        = tag(8, 0x1150)
	tRefInstance     = tag(8, 0x1155)
	tFrames          = tag(8, 0x1160)
	tSegments        = tag(0x62, 0xb)
	tRefSeries       = tag(8, 0x1115)
	tOtherStudies    = tag(8, 0x1200)
	tPPS             = tag(8, 0x1111)
	tCodeValue       = tag(8, 0x100)
	tCodeScheme      = tag(8, 0x102)
	tCodeVersion     = tag(8, 0x103)
	tCodeMeaning     = tag(8, 0x104)
	tLongCode        = tag(8, 0x119)
	tURN             = tag(8, 0x120)
	tEquivalent      = tag(8, 0x121)
)
var managed = map[core.Tag]bool{
	tSOPClass: true, tSOPInstance: true, tStudy: true, tSeries: true, tFrame: true, tSeriesNumber: true, tInstanceNumber: true, tDate: true, tTime: true, tLabel: true, tDescription: true, tModality: true, tSets: true, tRefs: true, tRefSeries: true, tOtherStudies: true, tPPS: true,
}
var metadataTags = func() map[core.Tag]struct{} {
	v, _ := iodmodules.TopLevelTags("Tractography Results")
	v[tCharset] = struct{}{}
	return v
}()

func textElement(t core.Tag, vr core.VR, s string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: t, VR: vr}, Value: core.StringValue{s}}
}
func numberElement(t core.Tag, vr core.VR, v core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: t, VR: vr}, Value: v}
}
func sequenceElement(t core.Tag, items ...*object.Object) core.Element {
	ds := make([]core.DataSet, len(items))
	for i, o := range items {
		ds[i] = o.ToDataSet()
		sort.Slice(ds[i].Elements, func(a, b int) bool { return ds[i].Elements[a].Tag().Less(ds[i].Elements[b].Tag()) })
	}
	return numberElement(t, core.VRSQ, core.SequenceValue{Items: ds})
}
func newObject() *object.Object { return object.New(nil) }
func stringValue(o *object.Object, t core.Tag, vr core.VR, required bool) (string, error) {
	e, ok := o.Get(t)
	if !ok {
		if required {
			return "", invalid(t.String())
		}
		return "", nil
	}
	if e.VR() != vr {
		return "", invalid(t.String() + " VR")
	}
	v, err := o.LookupStrings(t)
	if err != nil || len(v) > 1 || required && (len(v) != 1 || v[0] == "") {
		return "", invalid(t.String() + " text/VM")
	}
	if len(v) == 0 {
		return "", nil
	}
	return v[0], nil
}
func sequence(o *object.Object, t core.Tag, required bool) ([]*object.Object, error) {
	e, ok := o.Get(t)
	if !ok {
		if required {
			return nil, invalid(t.String())
		}
		return nil, nil
	}
	if e.VR() != core.VRSQ {
		return nil, invalid(t.String() + " VR")
	}
	seq, ok := e.Value.(core.SequenceValue)
	if !ok || len(seq.Items) == 0 {
		return nil, invalid(t.String() + " items")
	}
	v, ok := o.GetSequence(t)
	if !ok {
		return nil, invalid(t.String())
	}
	return v, nil
}
func ints(o *object.Object, t core.Tag, vr core.VR) ([]int64, error) {
	e, ok := o.Get(t)
	if !ok || e.VR() != vr {
		return nil, invalid(t.String() + " VR")
	}
	v, err := o.GetInts(t)
	if err != nil || len(v) == 0 {
		return nil, invalid(t.String() + " values")
	}
	return v, nil
}
func scalarFloat(o *object.Object, t core.Tag, vr core.VR) (float64, error) {
	e, ok := o.Get(t)
	if !ok || e.VR() != vr {
		return 0, invalid(t.String() + " VR")
	}
	v, err := o.GetFloats(t)
	if err != nil || len(v) != 1 {
		return 0, invalid(t.String() + " VM")
	}
	return v[0], nil
}
func f32(ctx context.Context, o *object.Object, t core.Tag) ([]float32, error) {
	e, ok := o.Get(t)
	if !ok || e.VR() != core.VROF || e.Length() == 0 || uint64(e.Length())%4 != 0 {
		return nil, invalid(t.String() + " OF")
	}
	n := int(uint64(e.Length()) / 4)
	v := make([]float32, n)
	switch src := e.Value.(type) {
	case core.Float32Value:
		for i := range v {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v[i] = src[i]
		}
	case core.RawValue:
		for i := range v {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v[i] = math.Float32frombits(o.ValueByteOrder().Uint32(src[i*4:]))
		}
	default:
		return nil, invalid(t.String() + " OF kind")
	}
	return v, ctx.Err()
}
func u32(ctx context.Context, o *object.Object, t core.Tag) ([]uint32, error) {
	e, ok := o.Get(t)
	if !ok || e.VR() != core.VROL || e.Length() == 0 || uint64(e.Length())%4 != 0 {
		return nil, invalid(t.String() + " OL")
	}
	n := int(uint64(e.Length()) / 4)
	v := make([]uint32, n)
	switch src := e.Value.(type) {
	case core.Uint32Value:
		for i := range v {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v[i] = src[i]
		}
	case core.RawValue:
		for i := range v {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v[i] = o.ValueByteOrder().Uint32(src[i*4:])
		}
	default:
		return nil, invalid(t.String() + " OL kind")
	}
	return v, ctx.Err()
}
func color(o *object.Object) (*[3]uint16, error) {
	if !o.Has(tColor) {
		return nil, nil
	}
	v, err := ints(o, tColor, core.VRUS)
	if err != nil || len(v) != 3 {
		return nil, invalid("color VM")
	}
	out := &[3]uint16{}
	for i, n := range v {
		out[i] = uint16(n)
	}
	return out, nil
}
func pointColors(ctx context.Context, o *object.Object) ([][3]uint16, error) {
	if !o.Has(tColors) {
		return nil, nil
	}
	e, _ := o.Get(tColors)
	if e.VR() != core.VROW || e.Length() == 0 || uint64(e.Length())%6 != 0 {
		return nil, invalid("point colors OW")
	}
	v := make([][3]uint16, int(uint64(e.Length())/6))
	switch src := e.Value.(type) {
	case core.Uint16Value:
		for i := range v {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			copy(v[i][:], src[i*3:i*3+3])
		}
	case core.RawValue:
		for i := range v {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for j := 0; j < 3; j++ {
				v[i][j] = o.ValueByteOrder().Uint16(src[(i*3+j)*2:])
			}
		}
	default:
		return nil, invalid("point colors OW kind")
	}
	return v, nil
}
func cloneObject(ctx context.Context, o *object.Object, filter bool) (*object.Object, error) {
	out := newObject()
	out.Put(textElement(tCharset, core.VRCS, "ISO_IR 192"))
	for _, e := range o.Elements() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Tag() == tCharset || bulkTag(e.Tag()) {
			continue
		}
		if filter {
			if _, ok := metadataTags[e.Tag()]; !ok || managed[e.Tag()] {
				continue
			}
		}
		if seq, ok := e.Value.(core.SequenceValue); ok {
			children, good := o.GetSequence(e.Tag())
			if !good {
				return nil, invalid("metadata sequence")
			}
			own := make([]*object.Object, len(seq.Items))
			for i, c := range children {
				var err error
				own[i], err = cloneObject(ctx, c, false)
				if err != nil {
					return nil, err
				}
			}
			out.Put(sequenceElement(e.Tag(), own...))
			continue
		}
		if e.VR().IsStringLike() {
			v, err := o.LookupStrings(e.Tag())
			if err != nil {
				return nil, invalid("metadata text")
			}
			out.Put(numberElement(e.Tag(), e.VR(), core.StringValue(append([]string(nil), v...))))
			continue
		}
		value, err := cloneValue(ctx, e.Value)
		if raw, ok := value.(core.RawValue); ok && err == nil && o.ValueByteOrder() == binary.BigEndian {
			err = canonicalRaw(ctx, raw, e.VR())
		}
		if err != nil {
			return nil, err
		}
		out.Put(numberElement(e.Tag(), e.VR(), value))
	}
	return out, nil
}
func cloneValue(ctx context.Context, v core.Value) (core.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch s := v.(type) {
	case nil:
		return core.RawValue{}, nil
	case core.RawValue:
		return copySlice(ctx, s)
	case core.Uint16Value:
		return copySlice(ctx, s)
	case core.Int16Value:
		return copySlice(ctx, s)
	case core.Uint32Value:
		return copySlice(ctx, s)
	case core.Int32Value:
		return copySlice(ctx, s)
	case core.Uint64Value:
		return copySlice(ctx, s)
	case core.Int64Value:
		return copySlice(ctx, s)
	case core.Float32Value:
		return copySlice(ctx, s)
	case core.Float64Value:
		return copySlice(ctx, s)
	case core.TagValue:
		return copySlice(ctx, s)
	default:
		return nil, unsupported("metadata value")
	}
}
func copySlice[S ~[]E, E any](ctx context.Context, s S) (S, error) {
	out := make(S, len(s))
	for start := 0; start < len(s); start += 1024 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1024
		if end > len(s) {
			end = len(s)
		}
		copy(out[start:end], s[start:end])
	}
	return out, ctx.Err()
}
func readCode(o *object.Object, t core.Tag) (Code, error) {
	seq, err := sequence(o, t, true)
	if err != nil || len(seq) != 1 {
		return Code{}, invalid(t.String() + " code cardinality")
	}
	return readCodeItem(seq[0])
}
func readCodeItem(o *object.Object) (Code, error) {
	var c Code
	count := 0
	for _, f := range []struct {
		t  core.Tag
		vr core.VR
		k  CodeKind
	}{{tCodeValue, core.VRSH, ShortCode}, {tLongCode, core.VRUC, LongCode}, {tURN, core.VRUR, URNCode}} {
		if o.Has(f.t) {
			v, err := stringValue(o, f.t, f.vr, true)
			if err != nil {
				return c, err
			}
			c.Value = v
			c.Kind = f.k
			count++
		}
	}
	if count != 1 {
		return c, invalid("code value form")
	}
	var err error
	c.Scheme, err = stringValue(o, tCodeScheme, core.VRSH, c.Kind != URNCode)
	if err != nil {
		return c, err
	}
	c.SchemeVersion, err = stringValue(o, tCodeVersion, core.VRSH, false)
	if err != nil {
		return c, err
	}
	c.Meaning, err = stringValue(o, tCodeMeaning, core.VRLO, true)
	if err != nil {
		return c, err
	}
	x := &CodeContext{}
	present := false
	for _, f := range contextFields(x) {
		if o.Has(f.t) {
			present = true
		}
		*f.v, err = stringValue(o, f.t, f.vr, false)
		if err != nil {
			return c, err
		}
	}
	if present {
		c.Context = x
	}
	if o.Has(tEquivalent) {
		es, e := sequence(o, tEquivalent, true)
		if e != nil {
			return c, e
		}
		c.Equivalent = make([]Code, len(es))
		for i, item := range es {
			c.Equivalent[i], err = readCodeItem(item)
			if err != nil {
				return c, err
			}
			if len(c.Equivalent[i].Equivalent) > 0 {
				return c, invalid("nested equivalent code")
			}
		}
	}
	return c, nil
}

type contextField struct {
	t  core.Tag
	vr core.VR
	v  *string
}

func contextFields(x *CodeContext) []contextField {
	return []contextField{
		{tag(8, 0x10f), core.VRCS, &x.Identifier}, {tag(8, 0x117), core.VRUI, &x.UID}, {tag(8, 0x105), core.VRCS, &x.MappingResource}, {tag(8, 0x118), core.VRUI, &x.MappingResourceUID}, {tag(8, 0x122), core.VRLO, &x.MappingResourceName},
		{tag(8, 0x106), core.VRDT, &x.Version}, {tag(8, 0x10b), core.VRCS, &x.ExtensionFlag}, {tag(8, 0x107), core.VRDT, &x.LocalVersion}, {tag(8, 0x10d), core.VRUI, &x.ExtensionCreatorUID},
	}
}
func codeItem(c Code) *object.Object {
	o := newObject()
	t, vr := tCodeValue, core.VRSH
	if c.Kind == LongCode {
		t, vr = tLongCode, core.VRUC
	}
	if c.Kind == URNCode {
		t, vr = tURN, core.VRUR
	}
	o.Put(textElement(t, vr, c.Value))
	if c.Scheme != "" {
		o.Put(textElement(tCodeScheme, core.VRSH, c.Scheme))
	}
	if c.SchemeVersion != "" {
		o.Put(textElement(tCodeVersion, core.VRSH, c.SchemeVersion))
	}
	o.Put(textElement(tCodeMeaning, core.VRLO, c.Meaning))
	if c.Context != nil {
		x := *c.Context
		for _, f := range contextFields(&x) {
			if *f.v != "" {
				o.Put(textElement(f.t, f.vr, *f.v))
			}
		}
	}
	if len(c.Equivalent) > 0 {
		items := make([]*object.Object, len(c.Equivalent))
		for i, c := range c.Equivalent {
			items[i] = codeItem(c)
		}
		o.Put(sequenceElement(tEquivalent, items...))
	}
	return o
}
func putCode(o *object.Object, t core.Tag, c Code) { o.Put(sequenceElement(t, codeItem(c))) }

// Canonical metadata uses LE raw words; typed values are already endian-neutral.
func canonicalRaw(ctx context.Context, v core.RawValue, vr core.VR) error {
	width := 1
	switch vr {
	case core.VRUS, core.VRSS, core.VROW, core.VRAT:
		width = 2
	case core.VRUL, core.VRSL, core.VRFL, core.VROF, core.VROL:
		width = 4
	case core.VRUV, core.VRSV, core.VRFD, core.VROD, core.VROV:
		width = 8
	}
	if len(v)%width != 0 {
		return invalid("metadata word length")
	}
	for i := 0; i < len(v); i += width {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for j := 0; j < width/2; j++ {
			v[i+j], v[i+width-1-j] = v[i+width-1-j], v[i+j]
		}
	}
	return ctx.Err()
}
func scalarFL(o *object.Object, t core.Tag) (float32, error) {
	v, err := scalarFloat(o, t, core.VRFL)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) { // Preserve FL NaN payload/signaling bits across widening.
		e, _ := o.Get(t)
		switch x := e.Value.(type) {
		case core.Float32Value:
			return x[0], nil
		case core.RawValue:
			return math.Float32frombits(o.ValueByteOrder().Uint32(x)), nil
		}
	}
	return float32(v), nil
}
