package tractography_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/tractography"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

type fixture struct {
	Results tractography.Results
	Golden  struct {
		FirstPointsOF, SubsetIndicesOL, FirstColorsOW string
		TrackSets, Tracks, Points, Coordinates        int
	}
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func testTag(g, e uint16) core.Tag { return core.NewTag(g, e) }
func wireText(o *object.Object, g, e uint16, vr core.VR, v ...string) {
	o.Put(core.Element{Header: core.ElementHeader{Tag: testTag(g, e), VR: vr}, Value: core.StringValue(v)})
}
func wireRaw(o *object.Object, g, e uint16, vr core.VR, v []byte) {
	o.Put(core.NewRawElement(testTag(g, e), vr, v))
}
func wireSeq(o *object.Object, g, e uint16, items ...*object.Object) {
	ds := make([]core.DataSet, len(items))
	for i, x := range items {
		ds[i] = x.ToDataSet()
		sort.Slice(ds[i].Elements, func(a, b int) bool { return ds[i].Elements[a].Tag().Less(ds[i].Elements[b].Tag()) })
	}
	o.Put(core.Element{Header: core.ElementHeader{Tag: testTag(g, e), VR: core.VRSQ}, Value: core.SequenceValue{Items: ds}})
}
func wireWords(order binary.ByteOrder, width int, values ...uint64) []byte {
	b := make([]byte, width*len(values))
	for i, v := range values {
		switch width {
		case 2:
			order.PutUint16(b[i*width:], uint16(v))
		case 4:
			order.PutUint32(b[i*width:], uint32(v))
		case 8:
			order.PutUint64(b[i*width:], v)
		}
	}
	return b
}
func wireFloats(order binary.ByteOrder, v []float32) []byte {
	words := make([]uint64, len(v))
	for i, f := range v {
		words[i] = uint64(math.Float32bits(f))
	}
	return wireWords(order, 4, words...)
}
func wireCode(c tractography.Code) *object.Object {
	o := object.New(nil)
	g, e, vr := uint16(8), uint16(0x100), core.VRSH
	if c.Kind == tractography.LongCode {
		e, vr = 0x119, core.VRUC
	}
	if c.Kind == tractography.URNCode {
		e, vr = 0x120, core.VRUR
	}
	wireText(o, g, e, vr, c.Value)
	if c.Scheme != "" {
		wireText(o, 8, 0x102, core.VRSH, c.Scheme)
	}
	if c.SchemeVersion != "" {
		wireText(o, 8, 0x103, core.VRSH, c.SchemeVersion)
	}
	wireText(o, 8, 0x104, core.VRLO, c.Meaning)
	if c.Context != nil {
		x := c.Context
		for _, f := range []struct {
			e  uint16
			vr core.VR
			s  string
		}{{0x10f, core.VRCS, x.Identifier}, {0x117, core.VRUI, x.UID}, {0x105, core.VRCS, x.MappingResource}, {0x118, core.VRUI, x.MappingResourceUID}, {0x122, core.VRLO, x.MappingResourceName}, {0x106, core.VRDT, x.Version}, {0x10b, core.VRCS, x.ExtensionFlag}, {0x107, core.VRDT, x.LocalVersion}, {0x10d, core.VRUI, x.ExtensionCreatorUID}} {
			if f.s != "" {
				wireText(o, 8, f.e, f.vr, f.s)
			}
		}
	}
	if c.Equivalent != nil {
		xs := make([]*object.Object, len(c.Equivalent))
		for i, x := range c.Equivalent {
			xs[i] = wireCode(x)
		}
		wireSeq(o, 8, 0x121, xs...)
	}
	return o
}
func wireQuantity(o *object.Object, q, u tractography.Code) {
	wireSeq(o, 0x40, 0xa043, wireCode(q))
	wireSeq(o, 0x40, 0x8ea, wireCode(u))
}
func wireColor(o *object.Object, c *[3]uint16, order binary.ByteOrder) {
	if c != nil {
		wireRaw(o, 0x62, 0xd, core.VRUS, wireWords(order, 2, uint64(c[0]), uint64(c[1]), uint64(c[2])))
	}
}

// This fixture construction intentionally uses literal wire tags and bytes,
// never Write, the package's tag table or its OF/OL/OW conversion helpers.
func wireFixture(t *testing.T, order binary.ByteOrder) (*object.Object, tractography.Results) {
	t.Helper()
	r := loadFixture(t).Results
	o := object.New(std.Dictionary)
	o.SetValueByteOrder(order)
	for _, f := range []struct {
		g, e uint16
		vr   core.VR
		s    string
	}{
		{8, 5, core.VRCS, "ISO_IR 192"}, {0x70, 0x84, core.VRPN, ""}, {0x20, 0x60, core.VRCS, ""}, {8, 0x16, core.VRUI, "1.2.840.10008.5.1.4.1.1.66.6"}, {8, 0x18, core.VRUI, r.SOPInstanceUID}, {0x20, 0xd, core.VRUI, r.StudyInstanceUID}, {0x20, 0xe, core.VRUI, r.SeriesInstanceUID}, {0x20, 0x52, core.VRUI, r.FrameOfReferenceUID},
		{8, 0x60, core.VRCS, "MR"}, {0x20, 0x11, core.VRIS, "12"}, {0x20, 0x13, core.VRIS, "4"}, {8, 0x23, core.VRDA, r.ContentDate}, {8, 0x33, core.VRTM, r.ContentTime}, {0x70, 0x80, core.VRCS, r.Label}, {0x70, 0x81, core.VRLO, r.Description},
		{0x10, 0x10, core.VRPN, "Tract^Synthetic"}, {0x10, 0x20, core.VRLO, "TRACT-SYN"}, {0x10, 0x30, core.VRDA, "19880316"}, {0x10, 0x40, core.VRCS, "O"}, {8, 0x20, core.VRDA, "20261009"}, {8, 0x30, core.VRTM, "065900"}, {8, 0x50, core.VRSH, ""}, {8, 0x90, core.VRPN, ""}, {0x20, 0x10, core.VRSH, "SYN"},
		{8, 0x70, core.VRLO, "Authored tools"}, {8, 0x1090, core.VRLO, "Synthetic model"}, {0x18, 0x1000, core.VRLO, "SYN-001"}, {0x18, 0x1020, core.VRLO, "1.0"}, {0x20, 0x1040, core.VRLO, ""}, {8, 0x12, core.VRDA, "20261009"}, {8, 0x13, core.VRTM, "071100"},
	} {
		wireText(o, f.g, f.e, f.vr, f.s)
	}
	sets := make([]*object.Object, len(r.TrackSets))
	for i, s := range r.TrackSets {
		x := object.New(nil)
		x.SetValueByteOrder(order)
		wireRaw(x, 0x66, 0x105, core.VRUL, wireWords(order, 4, uint64(s.Number)))
		wireText(x, 0x66, 0x106, core.VRLO, s.Label)
		if s.Description != "" {
			wireText(x, 0x66, 0x107, core.VRUT, s.Description)
		}
		anatomy := wireCode(s.Anatomy)
		if s.AnatomyModifiers != nil {
			ms := make([]*object.Object, len(s.AnatomyModifiers))
			for j, c := range s.AnatomyModifiers {
				ms[j] = wireCode(c)
			}
			wireSeq(anatomy, 0x40, 0xa195, ms...)
		}
		wireSeq(x, 0x66, 0x108, anatomy)
		wireColor(x, s.Color, order)
		if s.LineThickness != nil {
			wireRaw(x, 0x66, 0x38, core.VRFL, wireFloats(order, []float32{*s.LineThickness}))
		}
		tracks := make([]*object.Object, len(s.Tracks))
		for j, tr := range s.Tracks {
			v := object.New(nil)
			v.SetValueByteOrder(order)
			points := []float32{}
			for _, p := range tr.Points {
				points = append(points, p[:]...)
			}
			wireRaw(v, 0x66, 0x16, core.VROF, wireFloats(order, points))
			wireColor(v, tr.Color, order)
			if tr.PointColors != nil {
				colors := []uint64{}
				for _, c := range tr.PointColors {
					for _, a := range c {
						colors = append(colors, uint64(a))
					}
				}
				wireRaw(v, 0x66, 0x103, core.VROW, wireWords(order, 2, colors...))
			}
			tracks[j] = v
		}
		wireSeq(x, 0x66, 0x102, tracks...)
		wireSeq(x, 0x66, 0x134, wireCode(s.Model))
		if s.Acquisition != nil {
			wireSeq(x, 0x66, 0x133, wireCode(*s.Acquisition))
		}
		algs := make([]*object.Object, len(s.Algorithms))
		for j, a := range s.Algorithms {
			v := object.New(nil)
			wireSeq(v, 0x66, 0x2f, wireCode(a.Family))
			if a.NameCode != nil {
				wireSeq(v, 0x66, 0x30, wireCode(*a.NameCode))
			}
			wireText(v, 0x66, 0x36, core.VRLO, a.Name)
			wireText(v, 0x66, 0x31, core.VRLO, a.Version)
			if a.Parameters != "" {
				wireText(v, 0x66, 0x32, core.VRLT, a.Parameters)
			}
			if a.Source != "" {
				wireText(v, 0x24, 0x202, core.VRLO, a.Source)
			}
			algs[j] = v
		}
		wireSeq(x, 0x66, 0x104, algs...)
		if s.Measurements != nil {
			ms := make([]*object.Object, len(s.Measurements))
			for j, m := range s.Measurements {
				v := object.New(nil)
				wireQuantity(v, m.Quantity, m.Units)
				values := make([]*object.Object, len(m.Tracks))
				for k, mv := range m.Tracks {
					w := object.New(nil)
					wireRaw(w, 0x66, 0x125, core.VROF, wireFloats(order, mv.Values))
					if mv.PointIndices != nil {
						n := make([]uint64, len(mv.PointIndices))
						for z, a := range mv.PointIndices {
							n[z] = uint64(a)
						}
						wireRaw(w, 0x66, 0x129, core.VROL, wireWords(order, 4, n...))
					}
					values[k] = w
				}
				wireSeq(v, 0x66, 0x132, values...)
				ms[j] = v
			}
			wireSeq(x, 0x66, 0x121, ms...)
		}
		if s.TrackStatistics != nil {
			vs := make([]*object.Object, len(s.TrackStatistics))
			for j, m := range s.TrackStatistics {
				v := object.New(nil)
				wireQuantity(v, m.Quantity, m.Units)
				wireSeq(v, 0x40, 0xa195, wireCode(m.Modifier))
				wireRaw(v, 0x66, 0x125, core.VROF, wireFloats(order, m.Values))
				vs[j] = v
			}
			wireSeq(x, 0x66, 0x130, vs...)
		}
		if s.Statistics != nil {
			vs := make([]*object.Object, len(s.Statistics))
			for j, m := range s.Statistics {
				v := object.New(nil)
				wireQuantity(v, m.Quantity, m.Units)
				wireSeq(v, 0x40, 0xa195, wireCode(m.Modifier))
				wireRaw(v, 0x40, 0xa161, core.VRFD, wireWords(order, 8, math.Float64bits(m.Value)))
				vs[j] = v
			}
			wireSeq(x, 0x66, 0x124, vs...)
		}
		sets[i] = x
	}
	wireSeq(o, 0x66, 0x101, sets...)
	flat := make([]*object.Object, len(r.References))
	for i, ref := range r.References {
		v := wirePair(ref.SOPReference)
		if ref.Frames != nil {
			ss := make([]string, len(ref.Frames))
			for j, n := range ref.Frames {
				ss[j] = strconv.Itoa(int(n))
			}
			wireText(v, 8, 0x1160, core.VRIS, ss...)
		}
		if ref.Segments != nil {
			ns := make([]uint64, len(ref.Segments))
			for j, n := range ref.Segments {
				ns[j] = uint64(n)
			}
			wireRaw(v, 0x62, 0xb, core.VRUS, wireWords(order, 2, ns...))
		}
		flat[i] = v
	}
	wireSeq(o, 8, 0x114a, flat...)
	same := object.New(nil)
	wireText(same, 0x20, 0xe, core.VRUI, r.References[0].SeriesInstanceUID)
	wireSeq(same, 8, 0x114a, wirePair(r.References[0].SOPReference))
	wireSeq(o, 8, 0x1115, same)
	other := object.New(nil)
	wireText(other, 0x20, 0xd, core.VRUI, r.References[1].StudyInstanceUID)
	series := object.New(nil)
	wireText(series, 0x20, 0xe, core.VRUI, r.References[1].SeriesInstanceUID)
	wireSeq(series, 8, 0x114a, wirePair(r.References[1].SOPReference))
	wireSeq(other, 8, 0x1115, series)
	wireSeq(o, 8, 0x1200, other)
	return o, r
}
func wirePair(r tractography.SOPReference) *object.Object {
	o := object.New(nil)
	wireText(o, 8, 0x1150, core.VRUI, r.SOPClassUID)
	wireText(o, 8, 0x1155, core.VRUI, r.SOPInstanceUID)
	return o
}
func comparable(r *tractography.Results) tractography.Results { v := *r; v.Metadata = nil; return v }
func readGood(t *testing.T, o *object.Object) *tractography.Results {
	t.Helper()
	r, e := tractography.Read(nil, o, tractography.ReadOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func writeGood(t *testing.T, r *tractography.Results) *object.Object {
	t.Helper()
	o, e := tractography.Write(nil, r, tractography.WriteOptions{Common: object.DerivedCommon{Now: time.Date(2026, 10, 9, 7, 11, 0, 0, time.UTC)}})
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func requireIOD(t *testing.T, o *object.Object) {
	t.Helper()
	vr, e := validation.ValidateDataSet(context.Background(), o.ToDataSet(), validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, ByteOrder: o.ValueByteOrder(), MaxFindings: 1000})
	if e != nil || vr.Report.HasErrors() {
		t.Fatalf("encoding: %v %+v", e, vr.Report)
	}
	report, e := iod.Validate(o, iod.Options{CheckValues: true, MaxFindings: 2000})
	if e != nil || len(report.Errors()) != 0 || report.Truncated != 0 {
		t.Fatalf("IOD: %v %+v", e, report)
	}
	if name, ok := iod.IODForSOPClass(tractography.Storage); !ok || name != "Tractography Results" {
		t.Fatal(name, ok)
	}
}
func TestTractographyIndependentFixtureIODRoundTrip(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			o, want := wireFixture(t, order)
			requireIOD(t, o)
			r := readGood(t, o)
			if !reflect.DeepEqual(comparable(r), want) {
				t.Fatalf("fixture mismatch\ngot=%+v\nwant=%+v", comparable(r), want)
			}
			for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
				t.Run(ts.UID, func(t *testing.T) {
					out := writeGood(t, r)
					requireIOD(t, out)
					var encoded bytes.Buffer
					if e := object.WriteDataSet(&encoded, out, ts); e != nil {
						t.Fatal(e)
					}
					parsed, e := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), ts)
					if e != nil {
						t.Fatal(e)
					}
					got := readGood(t, parsed)
					if !reflect.DeepEqual(comparable(got), want) {
						t.Fatalf("serialized semantic mismatch")
					}
					if !bytes.Equal(objectBytes(t, r.Metadata), objectBytes(t, got.Metadata)) {
						t.Fatal("authored common metadata changed across library round trip")
					}
					if !got.Metadata.Has(testTag(0x20, 0x60)) {
						t.Fatal("authored unknown laterality dropped")
					}
					if v, _ := got.Metadata.GetString(testTag(8, 0x70)); v != "Authored tools" {
						t.Fatal(v)
					}
				})
			}
		})
	}
}
func TestTractographyGoldenStreams(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	out := writeGood(t, r)
	var buf bytes.Buffer
	if e := object.WriteDataSet(&buf, out, transfer.ExplicitVRLittleEndian); e != nil {
		t.Fatal(e)
	}
	parsed, e := object.ReadDataSet(bytes.NewReader(buf.Bytes()), transfer.ExplicitVRLittleEndian)
	if e != nil {
		t.Fatal(e)
	}
	sets, _ := parsed.GetSequence(testTag(0x66, 0x101))
	tracks, _ := sets[0].GetSequence(testTag(0x66, 0x102))
	ms, _ := sets[0].GetSequence(testTag(0x66, 0x121))
	vs, _ := ms[1].GetSequence(testTag(0x66, 0x132))
	g := loadFixture(t).Golden
	for _, c := range []struct {
		o   *object.Object
		t   core.Tag
		hex string
	}{{tracks[0], testTag(0x66, 0x16), g.FirstPointsOF}, {tracks[0], testTag(0x66, 0x103), g.FirstColorsOW}, {vs[0], testTag(0x66, 0x129), g.SubsetIndicesOL}} {
		el, _ := c.o.Get(c.t)
		raw, ok := el.Value.(core.RawValue)
		want, _ := hex.DecodeString(c.hex)
		if !ok || !bytes.Equal(raw, want) {
			t.Fatalf("%s: %x want %x", c.t, raw, want)
		}
	}
	if g.TrackSets != 2 || g.Tracks != 3 || g.Points != 9 || g.Coordinates != 27 {
		t.Fatal("fixture cardinality changed")
	}
}
func TestTractographyNegativeIOD(t *testing.T) {
	for _, tag := range []core.Tag{testTag(8, 0x60), testTag(8, 0x70), testTag(0x18, 0x1000), testTag(0x10, 0x20), testTag(0x20, 0x52)} {
		t.Run(tag.String(), func(t *testing.T) {
			o, _ := wireFixture(t, binary.LittleEndian)
			o.Remove(tag)
			report, e := iod.Validate(o, iod.Options{CheckValues: true})
			if e != nil || len(report.Errors()) == 0 {
				t.Fatalf("missing attribute undetected: %v %+v", e, report)
			}
			if r, e := tractography.Read(nil, o, tractography.ReadOptions{}); e == nil || r != nil {
				t.Fatal("reader accepted missing IOD attribute")
			}
		})
	}
	o, _ := wireFixture(t, binary.LittleEndian)
	wireText(o, 8, 0x60, core.VRCS, "CT")
	if r, e := tractography.Read(nil, o, tractography.ReadOptions{}); e == nil || r != nil {
		t.Fatal("non-MR accepted")
	}
}
