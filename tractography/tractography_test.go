package tractography_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/tractography"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func editSet(o *object.Object, f func(*object.Object)) {
	ss, _ := o.GetSequence(testTag(0x66, 0x101))
	f(ss[0])
	wireSeq(o, 0x66, 0x101, ss...)
}
func editTrack(o *object.Object, f func(*object.Object)) {
	editSet(o, func(s *object.Object) {
		ts, _ := s.GetSequence(testTag(0x66, 0x102))
		f(ts[0])
		wireSeq(s, 0x66, 0x102, ts...)
	})
}
func objectBytes(t *testing.T, o *object.Object) []byte {
	t.Helper()
	var b bytes.Buffer
	if e := object.WriteDataSet(&b, o, transfer.ExplicitVRLittleEndian); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestTractographyInvalidModelsAtomic(t *testing.T) {
	cases := []struct {
		name string
		edit func(*tractography.Results)
		want error
	}{
		{"sets", func(r *tractography.Results) { r.TrackSets = nil }, tractography.ErrInvalidData},
		{"set-order", func(r *tractography.Results) { r.TrackSets[0].Number = 2 }, tractography.ErrInvalidData},
		{"one-point", func(r *tractography.Results) { r.TrackSets[0].Tracks[0].Points = r.TrackSets[0].Tracks[0].Points[:1] }, tractography.ErrInvalidData},
		{"color", func(r *tractography.Results) { r.TrackSets[0].Tracks[0].PointColors = nil }, tractography.ErrInvalidData},
		{"color-count", func(r *tractography.Results) {
			r.TrackSets[0].Tracks[0].PointColors = r.TrackSets[0].Tracks[0].PointColors[:1]
		}, tractography.ErrInvalidData},
		{"all-point-count", func(r *tractography.Results) { r.TrackSets[0].Measurements[0].Tracks[0].Values = []float32{1} }, tractography.ErrInvalidData},
		{"measure-track-count", func(r *tractography.Results) {
			r.TrackSets[0].Measurements[0].Tracks = r.TrackSets[0].Measurements[0].Tracks[:1]
		}, tractography.ErrInvalidData},
		{"zero-index", func(r *tractography.Results) { r.TrackSets[0].Measurements[1].Tracks[0].PointIndices[0] = 0 }, tractography.ErrInvalidData},
		{"past-end-index", func(r *tractography.Results) { r.TrackSets[0].Measurements[1].Tracks[0].PointIndices[0] = 4 }, tractography.ErrInvalidData},
		{"empty-indices", func(r *tractography.Results) { r.TrackSets[0].Measurements[1].Tracks[0].PointIndices = []uint32{} }, tractography.ErrInvalidData},
		{"stat-count", func(r *tractography.Results) { r.TrackSets[0].TrackStatistics[0].Values = []float32{.5} }, tractography.ErrInvalidData},
		{"algo", func(r *tractography.Results) { r.TrackSets[0].Algorithms = nil }, tractography.ErrInvalidData},
		{"code-meaning", func(r *tractography.Results) { r.TrackSets[0].Model.Meaning = "" }, tractography.ErrInvalidData},
		{"nonext-laterality", func(r *tractography.Results) { r.TrackSets[0].AnatomyModifiers[0].Value = "UNREGISTERED" }, tractography.ErrInvalidData},
		{"units", func(r *tractography.Results) { r.TrackSets[0].Measurements[0].Units.Scheme = "99LOCAL" }, tractography.ErrInvalidData},
		{"empty-optional", func(r *tractography.Results) { r.TrackSets[0].Measurements = []tractography.Measurement{} }, tractography.ErrInvalidData},
		{"uid-role", func(r *tractography.Results) { r.SOPInstanceUID = r.StudyInstanceUID }, tractography.ErrInvalidData},
		{"label-CS", func(r *tractography.Results) { r.Label = "MixedCase" }, tractography.ErrInvalidData},
		{"partial-date", func(r *tractography.Results) { r.ContentDate = "202610" }, tractography.ErrInvalidData},
		{"invalid-time", func(r *tractography.Results) { r.ContentTime = "250000" }, tractography.ErrInvalidData},
		{"reference-class", func(r *tractography.Results) { r.References[0].SOPClassUID = tractography.Storage }, tractography.ErrInvalidReference},
		{"reference-instance-role", func(r *tractography.Results) { r.References[0].SOPInstanceUID = r.StudyInstanceUID }, tractography.ErrInvalidReference},
		{"single-frame", func(r *tractography.Results) { r.References[0].SOPClassUID = "1.2.840.10008.5.1.4.1.1.4" }, tractography.ErrInvalidReference},
		{"frame-zero", func(r *tractography.Results) { r.References[0].Frames[0] = 0 }, tractography.ErrInvalidReference},
		{"duplicate-frame", func(r *tractography.Results) { r.References[0].Frames[1] = 2 }, tractography.ErrInvalidReference},
		{"segment-class", func(r *tractography.Results) { r.References[0].Segments = []uint16{1} }, tractography.ErrInvalidReference},
		{"segment-zero", func(r *tractography.Results) { r.References[1].Segments[0] = 0 }, tractography.ErrInvalidReference},
		{"conflicting-duplicate", func(r *tractography.Results) {
			v := r.References[0]
			v.Frames = []int32{1}
			r.References = append(r.References, v)
		}, tractography.ErrInvalidReference},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := loadFixture(t).Results
			c.edit(&r)
			before, _ := json.Marshal(r)
			err := tractography.Validate(nil, &r, tractography.Limits{})
			if !errors.Is(err, c.want) {
				t.Fatalf("Validate=%v want %v", err, c.want)
			}
			o, err := tractography.Write(nil, &r, tractography.WriteOptions{})
			if !errors.Is(err, c.want) || o != nil {
				t.Fatalf("Write=%v output=%v", err, o)
			}
			if !bytes.Equal(modelJSON(&r), before) {
				t.Fatal("model changed after rejection")
			}
		})
	}
}
func TestTractographyMalformedWireAtomic(t *testing.T) {
	cases := []struct {
		name string
		edit func(*object.Object)
	}{
		{"wrong-SOP", func(o *object.Object) { wireText(o, 8, 0x16, core.VRUI, "1.2.840.10008.5.1.4.1.1.4") }},
		{"VM", func(o *object.Object) { wireText(o, 0x70, 0x80, core.VRCS, "ONE", "TWO") }},
		{"OF-VR", func(o *object.Object) {
			editTrack(o, func(v *object.Object) { wireRaw(v, 0x66, 0x16, core.VROB, make([]byte, 36)) })
		}},
		{"OF-length", func(o *object.Object) {
			editTrack(o, func(v *object.Object) { wireRaw(v, 0x66, 0x16, core.VROF, make([]byte, 14)) })
		}},
		{"header-mismatch", func(o *object.Object) {
			editTrack(o, func(v *object.Object) { e, _ := v.Get(testTag(0x66, 0x16)); e.Header.Length = 12; v.Put(e) })
		}},
		{"typed-kind", func(o *object.Object) {
			editTrack(o, func(v *object.Object) {
				v.Put(core.Element{Header: core.ElementHeader{Tag: testTag(0x66, 0x16), VR: core.VROF}, Value: core.Uint32Value{1, 2, 3}})
			})
		}},
		{"undefined-primitive", func(o *object.Object) {
			editTrack(o, func(v *object.Object) {
				e, _ := v.Get(testTag(0x66, 0x16))
				e.Header.Length = core.UndefinedLength
				v.Put(e)
			})
		}},
		{"OW-length", func(o *object.Object) {
			editTrack(o, func(v *object.Object) { wireRaw(v, 0x66, 0x103, core.VROW, make([]byte, 8)) })
		}},
		{"FD-VM", func(o *object.Object) {
			editSet(o, func(s *object.Object) {
				ss, _ := s.GetSequence(testTag(0x66, 0x124))
				wireRaw(ss[0], 0x40, 0xa161, core.VRFD, make([]byte, 16))
				wireSeq(s, 0x66, 0x124, ss...)
			})
		}},
		{"code-VM", func(o *object.Object) {
			editSet(o, func(s *object.Object) {
				a, _ := s.GetSequence(testTag(0x66, 0x134))
				wireSeq(s, 0x66, 0x134, a[0], a[0])
			})
		}},
		{"nested-duplicate", func(o *object.Object) {
			editSet(o, func(s *object.Object) {
				e, _ := s.Get(testTag(0x66, 0x102))
				sq := e.Value.(core.SequenceValue)
				sq.Items[0].Elements = append(sq.Items[0].Elements, sq.Items[0].Elements[0])
				e.Value = sq
				s.Put(e)
			})
		}},
		{"reference-hierarchy-class", func(o *object.Object) {
			ss, _ := o.GetSequence(testTag(8, 0x1115))
			rs, _ := ss[0].GetSequence(testTag(8, 0x114a))
			wireText(rs[0], 8, 0x1150, core.VRUI, "1.2.840.10008.5.1.4.1.1.2")
			wireSeq(ss[0], 8, 0x114a, rs...)
			wireSeq(o, 8, 0x1115, ss...)
		}},
		{"reference-extra-hierarchy", func(o *object.Object) {
			ss, _ := o.GetSequence(testTag(8, 0x1115))
			rs, _ := ss[0].GetSequence(testTag(8, 0x114a))
			rs = append(rs, wirePair(tractography.SOPReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.4.1", SOPInstanceUID: "1.2.3.99"}))
			wireSeq(ss[0], 8, 0x114a, rs...)
			wireSeq(o, 8, 0x1115, ss...)
		}},
		{"partial-present-module", func(o *object.Object) { wireText(o, 0x12, 0x10, core.VRLO, "Study sponsor") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := wireFixture(t, binary.LittleEndian)
			c.edit(o)
			es, _ := json.Marshal(o.ToDataSet())
			r, err := tractography.Read(nil, o, tractography.ReadOptions{})
			if err == nil || r != nil {
				t.Fatal("accepted malformed wire")
			}
			if !bytes.Equal(es, modelJSON(o.ToDataSet())) {
				t.Fatal("source changed after error")
			}
		})
	}
}
func TestTractographyOriginReferencesAndPPS(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	for _, x := range []core.Tag{testTag(8, 0x114a), testTag(8, 0x1115), testTag(8, 0x1200)} {
		o.Remove(x)
	}
	r := readGood(t, o)
	if r.Origin != tractography.UnknownOrigin {
		t.Fatal(r.Origin)
	}
	if _, e := tractography.Write(nil, r, tractography.WriteOptions{}); !errors.Is(e, tractography.ErrInvalidData) {
		t.Fatal(e)
	}
	if r, e := tractography.Read(nil, o, tractography.ReadOptions{RequireDICOMReferences: true}); !errors.Is(e, tractography.ErrInvalidReference) || r != nil {
		t.Fatal(e, r)
	}
	r.Origin = tractography.OtherSource
	out := writeGood(t, r)
	if out.Has(testTag(8, 0x114a)) {
		t.Fatal("invented refs")
	}
	r = readGood(t, func() *object.Object { x, _ := wireFixture(t, binary.LittleEndian); return x }())
	r.References = append(r.References, r.References[0])
	r.PerformedProcedureStep = &tractography.SOPReference{SOPClassUID: "1.2.840.10008.3.1.2.3.3", SOPInstanceUID: "1.2.826.0.1.3680043.10.999.400"}
	got := readGood(t, writeGood(t, r))
	if len(got.References) != 2 || !reflect.DeepEqual(got.PerformedProcedureStep, r.PerformedProcedureStep) {
		t.Fatal("reference/PPS mismatch")
	}
}
func TestTractographyLimits(t *testing.T) {
	d := tractography.DefaultLimits()
	dv := reflect.ValueOf(d)
	typ := dv.Type()
	for i := 0; i < dv.NumField(); i++ {
		name := typ.Field(i).Name
		t.Run(name, func(t *testing.T) {
			for _, n := range []int64{-1, dv.Field(i).Int() + 1, 1} {
				l := tractography.Limits{}
				reflect.ValueOf(&l).Elem().Field(i).SetInt(n)
				r := loadFixture(t).Results
				o, e := tractography.Write(nil, &r, tractography.WriteOptions{Limits: l})
				want := tractography.ErrInvalidOptions
				if n == 1 {
					want = tractography.ErrLimitExceeded
				}
				if o != nil || !errors.Is(e, want) {
					t.Fatalf("n=%d: %v %v", n, o, e)
				}
				src, _ := wireFixture(t, binary.LittleEndian)
				got, e := tractography.Read(nil, src, tractography.ReadOptions{Limits: l})
				if got != nil || !errors.Is(e, want) {
					t.Fatalf("read n=%d: %v %v", n, got, e)
				}
			}
		})
	}
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	l := tractography.Limits{MaxTrackSets: 2, MaxTracks: 3, MaxPoints: 9, MaxValues: 10, MaxReferences: 2}
	if e := tractography.Validate(nil, r, l); e != nil {
		t.Fatal(e)
	}
	if got, e := tractography.Read(nil, o, tractography.ReadOptions{Limits: l}); e != nil || got == nil {
		t.Fatal(e)
	}
	if got, e := tractography.Write(nil, r, tractography.WriteOptions{Limits: l}); e != nil || got == nil {
		t.Fatal(e)
	}
	// Small DAG has only four backing nodes but 3^4 visits. Repeated visits and
	// empty items are charged before facade/sequence/clone allocations.
	for _, dag := range []bool{false, true} {
		x := object.New(nil)
		if dag {
			leaf := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: testTag(8, 0x104), VR: core.VRLO}, Value: core.StringValue{"bounded"}}}}
			for depth := 0; depth < 4; depth++ {
				leaf = core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: testTag(8, 0x121), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{leaf, leaf, leaf}}}}}
			}
			x = object.FromDataSet(leaf, nil)
		} else {
			x.Put(core.Element{Header: core.ElementHeader{Tag: testTag(8, 0x121), VR: core.VRSQ}, Value: core.SequenceValue{Items: make([]core.DataSet, 100)}})
		}
		before := x.Elements()
		if got, e := tractography.Read(nil, x, tractography.ReadOptions{Limits: tractography.Limits{MaxItems: 20}}); got != nil || !errors.Is(e, tractography.ErrLimitExceeded) {
			t.Fatal(got, e)
		}
		if !reflect.DeepEqual(before, x.Elements()) {
			t.Fatal("DAG source changed")
		}
	}
}

type countingReader struct {
	*bytes.Reader
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }
func TestTractographyDeferredZeroReadAndIgnoredBulk(t *testing.T) {
	for _, tag := range []core.Tag{testTag(8, 0x18), testTag(0x66, 0x16), testTag(0x10, 0x20)} {
		t.Run(tag.String(), func(t *testing.T) {
			o, _ := wireFixture(t, binary.LittleEndian)
			if tag != testTag(0x66, 0x16) {
				wireRaw(o, 0x7fe0, 0x10, core.VROB, []byte{1, 2, 3, 4})
			}
			encoded := objectBytes(t, o)
			source := &countingReader{Reader: bytes.NewReader(encoded)}
			opts := object.ReadFileOptions{InlineValueBytesThreshold: 1, DeferNestedValues: true, DeferValueTags: []core.Tag{tag}}
			if tag != testTag(0x66, 0x16) {
				opts = object.ReadFileOptions{DeferPixelData: true}
			}
			deferred, e := object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, opts)
			if e == nil && tag != testTag(0x66, 0x16) {
				el, _ := deferred.Get(tag)
				el.Value = nil
				deferred.Put(el)
			}

			if e != nil {
				t.Fatal(e)
			}
			defer deferred.Close()
			source.reads = 0
			if r, e := tractography.Read(nil, deferred, tractography.ReadOptions{}); r != nil || !errors.Is(e, tractography.ErrUnsupportedValue) {
				t.Fatal(r, e)
			}
			r := loadFixture(t).Results
			if out, e := tractography.Write(nil, &r, tractography.WriteOptions{Common: object.DerivedCommon{Source: deferred}}); out != nil || !errors.Is(e, tractography.ErrUnsupportedValue) {
				t.Fatal(out, e)
			}
			if source.reads != 0 {
				t.Fatalf("deferred provider read %d times", source.reads)
			}
		})
	}
	o, _ := wireFixture(t, binary.LittleEndian)
	pixels := bytes.Repeat([]byte{3, 4}, 256)
	wireRaw(o, 0x7fe0, 0x10, core.VROB, pixels)
	encoded := objectBytes(t, o)
	source := &countingReader{Reader: bytes.NewReader(encoded)}
	deferred, e := object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{DeferPixelData: true})
	if e != nil {
		t.Fatal(e)
	}
	defer deferred.Close()
	source.reads = 0
	r := readGood(t, deferred)
	out, e := tractography.Write(nil, r, tractography.WriteOptions{Common: object.DerivedCommon{Source: deferred}})
	if e != nil || out.Has(core.TagPixelData) || source.reads != 0 {
		t.Fatal(e, source.reads)
	}
	var copied bytes.Buffer
	if _, e = deferred.CopyValueTo(core.TagPixelData, &copied); e != nil || !bytes.Equal(copied.Bytes(), pixels) {
		t.Fatal("source provider ownership lost", e)
	}
	o, _ = wireFixture(t, binary.LittleEndian)
	editTrack(o, func(v *object.Object) {
		v.Put(core.Element{Header: core.ElementHeader{Tag: testTag(0x66, 0x16), VR: core.VROF}, Value: core.BulkDataValue{URI: "urn:example:unresolved"}})
	})
	if r, e := tractography.Read(nil, o, tractography.ReadOptions{}); r != nil || !errors.Is(e, tractography.ErrUnsupportedValue) {
		t.Fatal(r, e)
	}
}
func TestTractographyFiniteBulkDepthIgnoredAndProviderRetained(t *testing.T) {
	for _, tag := range []core.Tag{core.TagPixelData, testTag(0x7fe0, 8), testTag(0x7fe0, 9), testTag(0x5600, 0x20)} {
		t.Run(tag.String(), func(t *testing.T) {
			o, _ := wireFixture(t, binary.LittleEndian)
			pixels := bytes.Repeat([]byte{3, 4}, 256)
			wireRaw(o, 0x7fe0, 0x10, core.VROB, pixels)
			reader := &countingReader{Reader: bytes.NewReader(objectBytes(t, o))}
			source, err := object.ReadDataSetWithOptions(reader, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{DeferPixelData: true})
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			originalPixels, _ := source.Get(core.TagPixelData)
			payload := core.RawValue{11, 12, 13, 14}
			nested := core.Element{Header: core.ElementHeader{Tag: testTag(9, 0x1001), VR: core.VROB, Length: 4}, Value: payload}
			for depth := 0; depth < 64; depth++ {
				nested = core.Element{
					Header: core.ElementHeader{Tag: testTag(9, 0x1002), VR: core.VRSQ, Length: core.UndefinedLength},
					Value:  core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{nested}}}},
				}
			}
			ignored := core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VROB}, Value: nested.Value}
			source.Put(ignored) // A finite 64-level graph, never cyclic.
			reader.reads = 0
			limits := tractography.Limits{MaxSequenceDepth: 6}
			results, err := tractography.Read(nil, source, tractography.ReadOptions{Limits: limits})
			if err != nil || results == nil {
				t.Fatal("read with ignored finite bulk", err)
			}
			output, err := tractography.Write(nil, results, tractography.WriteOptions{Limits: limits, Common: object.DerivedCommon{Source: source}})
			if err != nil || output == nil {
				t.Fatal("common source with ignored finite bulk", err)
			}
			for _, bulk := range []core.Tag{core.TagPixelData, testTag(0x7fe0, 8), testTag(0x7fe0, 9), testTag(0x5600, 0x20)} {
				if results.Metadata.Has(bulk) || output.Has(bulk) {
					t.Fatalf("ignored bulk %s retained", bulk)
				}
			}
			el, ok := source.Get(tag)
			if !ok || reader.reads != 0 || !bytes.Equal(payload, []byte{11, 12, 13, 14}) {
				t.Fatal("source or provider changed", reader.reads)
			}
			got := el.Value.(core.SequenceValue)
			before := ignored.Value.(core.SequenceValue)
			if &got.Items[0].Elements[0] != &before.Items[0].Elements[0] {
				t.Fatal("source bulk backing was copied or replaced")
			}
			// Restore the original deferred pixel header, then prove the caller
			// can still read through its provider after both library operations.
			if tag == core.TagPixelData {
				source.Put(originalPixels)
			}
			var copied bytes.Buffer
			if _, err := source.CopyValueTo(core.TagPixelData, &copied); err != nil || !bytes.Equal(copied.Bytes(), pixels) {
				t.Fatal("source provider ownership lost", err)
			}
		})
	}
}

func TestTractographyOwnershipAndConcurrentCalls(t *testing.T) {
	o, want := wireFixture(t, binary.LittleEndian)
	before := objectBytes(t, o)
	r := readGood(t, o)
	out := writeGood(t, r)
	r.TrackSets[0].Tracks[0].Points[0][0] = 123
	r.TrackSets[0].Tracks[0].PointColors[0][0] = 999
	r.TrackSets[0].Measurements[1].Tracks[0].PointIndices[0] = 2
	r.TrackSets[0].Measurements[0].Tracks[0].Values[0] = 99
	r.References[0].Frames[0] = 10
	wireText(r.Metadata, 0x10, 0x20, core.VRLO, "changed")
	if !bytes.Equal(before, objectBytes(t, o)) {
		t.Fatal("read buffers alias source")
	}
	got := readGood(t, out)
	if !reflect.DeepEqual(comparable(got), want) {
		t.Fatal("output aliases supplied model")
	}
	if v, _ := got.Metadata.GetString(testTag(0x10, 0x20)); v != "TRACT-SYN" {
		t.Fatal("output aliases metadata", v)
	}
	stable := readGood(t, o)
	var wg sync.WaitGroup
	fail := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				r, e := tractography.Read(context.Background(), o, tractography.ReadOptions{})
				if e != nil {
					fail <- e
					return
				}
				if !reflect.DeepEqual(comparable(r), want) {
					fail <- errors.New("concurrent read mismatch")
					return
				}
				out, e := tractography.Write(nil, stable, tractography.WriteOptions{})
				if e != nil || out == nil {
					fail <- e
					return
				}
			}
		}()
	}
	wg.Wait()
	close(fail)
	for e := range fail {
		t.Error(e)
	}
}
func TestTractographyIEEEBitFidelity(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	bits := []uint32{0x80000000, 0x7f800000, 0xff800000, 0x7f812345, 0xffc12345, 0}
	for i, b := range bits {
		r.TrackSets[0].Tracks[0].Points[i/3][i%3] = math.Float32frombits(b)
	}
	r.TrackSets[0].Measurements[0].Tracks[0].Values[0] = math.Float32frombits(0x7f812345)
	f := math.Float32frombits(0xff812345)
	r.TrackSets[0].LineThickness = &f
	r.TrackSets[0].Statistics[0].Value = math.Float64frombits(0x7ff0123456789abc)
	out := writeGood(t, r)
	parsed, e := object.ReadDataSet(bytes.NewReader(objectBytes(t, out)), transfer.ExplicitVRLittleEndian)
	if e != nil {
		t.Fatal(e)
	}
	got := readGood(t, parsed)
	for i, b := range bits {
		if v := math.Float32bits(got.TrackSets[0].Tracks[0].Points[i/3][i%3]); v != b {
			t.Fatalf("coordinate bits %x want %x", v, b)
		}
	}
	if math.Float32bits(got.TrackSets[0].Measurements[0].Tracks[0].Values[0]) != 0x7f812345 || math.Float32bits(*got.TrackSets[0].LineThickness) != 0xff812345 || math.Float64bits(got.TrackSets[0].Statistics[0].Value) != 0x7ff0123456789abc {
		t.Fatal("IEEE bits changed")
	}
}
func TestTractographyRichCodesAndOptionalContent(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	c := tractography.Code{Code: codes.Code{Value: "authored.algorithm.name.2026", Scheme: "99AUTHORED", Meaning: "Synthetic algorithm"}, Kind: tractography.LongCode, SchemeVersion: "V1", Context: &tractography.CodeContext{Identifier: "7262", MappingResource: "DCMR", UID: "1.2.840.10008.6.1.1061", Version: "20261009000000+0000", ExtensionFlag: "Y", LocalVersion: "20261009000000+0000", ExtensionCreatorUID: "1.2.826.0.1.3680043.10.999.501"}, Equivalent: []tractography.Code{{Code: codes.Code{Value: "urn:example:synthetic:algorithm", Meaning: "Synthetic equivalent"}, Kind: tractography.URNCode}}}
	r.TrackSets[0].Algorithms[0].NameCode = &c
	wireText(r.Metadata, 0x70, 0x84, core.VRPN, "Authored^Creator")
	wireSeq(r.Metadata, 0x40, 0xa043, wireCode(tractography.Code{Code: codes.Code{Value: "SYNTHETIC", Scheme: "99AUTHORED", Meaning: "Synthetic results"}}))
	got := readGood(t, writeGood(t, r))
	if !reflect.DeepEqual(got.TrackSets[0].Algorithms[0].NameCode, &c) {
		t.Fatal("rich code changed")
	}
	if v, _ := got.Metadata.GetString(testTag(0x70, 0x84)); v != "Authored^Creator" {
		t.Fatal(v)
	}
	for _, bad := range []func(*tractography.Code){func(x *tractography.Code) { x.Context.Version = "20261009" }, func(x *tractography.Code) { x.Equivalent[0].Equivalent = []tractography.Code{*x} }, func(x *tractography.Code) { x.Kind = tractography.ShortCode }} {
		fresh := readGood(t, o)
		clone := c
		clone.Equivalent = append([]tractography.Code(nil), c.Equivalent...)
		cx := *c.Context
		clone.Context = &cx
		bad(&clone)
		fresh.TrackSets[0].Algorithms[0].NameCode = &clone
		if out, e := tractography.Write(nil, fresh, tractography.WriteOptions{}); out != nil || e == nil {
			t.Fatal("accepted malformed enhanced code")
		}
	}
}
func TestTractographyBigEndianMetadataAndEquipment(t *testing.T) {
	o, _ := wireFixture(t, binary.BigEndian)
	wireRaw(o, 0x28, 0x120, core.VRUS, wireWords(binary.BigEndian, 2, 1234))
	r := readGood(t, o)
	el, _ := r.Metadata.Get(testTag(0x28, 0x120))
	raw, _ := el.Value.(core.RawValue)
	if !bytes.Equal(raw, []byte{0xd2, 0x04}) {
		t.Fatalf("metadata endian changed: %x", raw)
	}
	got := readGood(t, writeGood(t, r))
	v, e := got.Metadata.GetInts(testTag(0x28, 0x120))
	if e != nil || len(v) != 1 || v[0] != 1234 {
		t.Fatal(v, e)
	}
	r.Metadata.Remove(testTag(8, 0x1090))
	if out, e := tractography.Write(nil, r, tractography.WriteOptions{}); out != nil || e == nil {
		t.Fatal("mixed default/partial retained equipment")
	}
	opts := tractography.WriteOptions{Common: object.DerivedCommon{Equipment: &object.Equipment{Manufacturer: "Explicit", ModelName: "Model", DeviceSerialNumber: "SN", SoftwareVersions: "1"}}}
	out, e := tractography.Write(nil, r, opts)
	if e != nil || out == nil {
		t.Fatal(e)
	}
	if v, _ := out.GetString(testTag(8, 0x70)); v != "Explicit" {
		t.Fatal(v)
	}
	opts.Common.Now = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if out, e := tractography.Write(nil, r, opts); out != nil || e == nil {
		t.Fatal("invalid creation year")
	}
}

type cancelCounter struct {
	calls, after int
	done         chan struct{}
	once         sync.Once
}

func (c *cancelCounter) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelCounter) Done() <-chan struct{}       { return c.done }
func (c *cancelCounter) Value(any) any               { return nil }
func (c *cancelCounter) Err() error {
	c.calls++
	if c.calls >= c.after {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}
func TestTractographyNilAndCancellation(t *testing.T) {
	if r, e := tractography.Read(nil, nil, tractography.ReadOptions{}); r != nil || !errors.Is(e, tractography.ErrInvalidData) {
		t.Fatal(r, e)
	}
	if o, e := tractography.Write(nil, nil, tractography.WriteOptions{}); o != nil || !errors.Is(e, tractography.ErrInvalidData) {
		t.Fatal(o, e)
	}
	if e := tractography.Validate(nil, nil, tractography.Limits{}); !errors.Is(e, tractography.ErrInvalidData) {
		t.Fatal(e)
	}
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	before := objectBytes(t, o)
	for _, after := range []int{1, 50, 250} {
		ctx := &cancelCounter{after: after, done: make(chan struct{})}
		if got, e := tractography.Read(ctx, o, tractography.ReadOptions{}); got != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("read counter=%d calls=%d: %v", after, ctx.calls, e)
		}
		ctx = &cancelCounter{after: after, done: make(chan struct{})}
		if got, e := tractography.Write(ctx, r, tractography.WriteOptions{}); got != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("write counter=%d calls=%d: %v", after, ctx.calls, e)
		}
	}
	if !bytes.Equal(before, objectBytes(t, o)) {
		t.Fatal("cancel changed source")
	}
	// Larger track forces chunked copy cancellation, independently of elapsed time.
	fresh := loadFixture(t).Results
	r = &fresh
	r.TrackSets = r.TrackSets[1:]
	r.TrackSets[0].Number = 1
	r.TrackSets[0].Tracks[0].Points = make([][3]float32, 100000)
	ctx := &cancelCounter{after: 100, done: make(chan struct{})}
	if got, e := tractography.Write(ctx, r, tractography.WriteOptions{}); got != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(got, e, ctx.calls)
	}
}

func modelJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}

func TestTractographySignedInspectionAndRewriteRefusal(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	mac := object.New(nil)
	wireRaw(mac, 0x400, 5, core.VRUS, wireWords(binary.LittleEndian, 2, 1))
	wireText(mac, 0x400, 0x10, core.VRUI, transfer.ExplicitVRLittleEndian.UID)
	wireText(mac, 0x400, 0x15, core.VRCS, "SHA256")
	mac.Put(core.Element{Header: core.ElementHeader{Tag: testTag(0x400, 0x20), VR: core.VRAT}, Value: core.TagValue{testTag(0x20, 0xd)}})
	sig := object.New(nil)
	wireRaw(sig, 0x400, 5, core.VRUS, wireWords(binary.LittleEndian, 2, 1))
	wireText(sig, 0x400, 0x100, core.VRUI, "1.2.826.0.1.3680043.10.999.601")
	wireText(sig, 0x400, 0x105, core.VRDT, "20261009070000+0000")
	wireText(sig, 0x400, 0x110, core.VRCS, "X509_1993_SIG")
	wireRaw(sig, 0x400, 0x115, core.VROB, []byte{1, 2, 3, 4})
	wireRaw(sig, 0x400, 0x120, core.VROB, []byte{5, 6, 7, 8})
	wireSeq(o, 0x4ffe, 1, mac)
	wireSeq(o, 0xfffa, 0xfffa, sig)
	// Deliberately synthetic certificate/signature bytes: inspection performs no
	// cryptographic verification. Retention does not mean signature validity.
	before := modelJSON(o.ToDataSet())
	r := readGood(t, o)
	if !r.Metadata.Has(testTag(0xfffa, 0xfffa)) {
		t.Fatal("inspection dropped signature")
	}
	if out, e := tractography.Write(nil, r, tractography.WriteOptions{Limits: tractography.Limits{MaxElements: 1}}); out != nil || !errors.Is(e, tractography.ErrUnsupportedValue) {
		t.Fatal("signed metadata was cloned or accepted", out, e)
	}
	sigItems, _ := r.Metadata.GetSequence(testTag(0xfffa, 0xfffa))
	cert, _ := sigItems[0].Get(testTag(0x400, 0x115))
	cert.Value.(core.RawValue)[0] = 99
	macItems, _ := r.Metadata.GetSequence(testTag(0x4ffe, 1))
	signedTags, _ := macItems[0].Get(testTag(0x400, 0x20))
	signedTags.Value.(core.TagValue)[0] = testTag(8, 0x18)
	if !bytes.Equal(before, modelJSON(o.ToDataSet())) {
		t.Fatal("signed metadata aliases source")
	}

	r.Metadata.Remove(testTag(0x4ffe, 1))
	r.Metadata.Remove(testTag(0xfffa, 0xfffa))
	writeGood(t, r)
}
func TestTractographyPartialTMAndUTF8(t *testing.T) {
	o, _ := wireFixture(t, binary.LittleEndian)
	r := readGood(t, o)
	for _, tm := range []string{"07", "0710", "071060", "071000.000001"} {
		r.ContentTime = tm
		r.TrackSets[0].Label = "Fibras sintéticas 左"
		got := readGood(t, writeGood(t, r))
		if got.ContentTime != tm || got.TrackSets[0].Label != r.TrackSets[0].Label {
			t.Fatal("precision/UTF8 changed")
		}
	}
}
