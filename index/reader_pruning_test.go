package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func indexPruneWire(tag core.Tag, vr string, length uint32, value []byte) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, tag.Group)
	binary.LittleEndian.PutUint16(b[2:], tag.Element)
	if vr != "" {
		b = append(b, vr...)
		if vr == "SQ" || vr == "OB" || vr == "UN" {
			b = append(b, 0, 0)
		} else {
			at := len(b)
			b = append(b, 0, 0)
			binary.LittleEndian.PutUint16(b[at:], uint16(length))
			return append(b, value...)
		}
	}
	at := len(b)
	b = append(b, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[at:], length)
	return append(b, value...)
}

func indexPruneRead(t *testing.T, wire []byte, options Options) Result {
	t.Helper()
	options.RawDataSet = &RawDataSetOptions{TransferSyntax: transfer.ExplicitVRLittleEndian}
	got, err := ReadSeeker(context.Background(), "synthetic", bytes.NewReader(wire), int64(len(wire)), options)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHeaderPruningRootMaximumProfilesAndAncestors(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		want    core.Tag
	}{
		{ProfileNone, core.NewTag(8, 5)}, {ProfileCore, tags.NumberOfFrames}, {ProfilePatient, tags.PatientSex},
		{ProfileDescriptions, tags.SeriesDescription}, {ProfileGeometry, tags.PixelSpacing}, {ProfileAll, tags.PixelSpacing},
	} {
		opts, err := normalizeOptions(Options{Profile: tc.profile, AssumeSortedTags: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := newHeaderReadPlan(opts).maximum; got != tc.want {
			t.Fatalf("profile %d maximum %s want %s", tc.profile, got, tc.want)
		}
	}
	for _, tc := range []struct{ ancestor, leaf core.Tag }{
		{core.NewTag(4, 0x1220), core.NewTag(0x10, 0x10)}, // ancestor below charset, leaf above it
		{core.NewTag(0x5200, 0x9230), core.NewTag(0x28, 0x10)},
	} {
		opts, err := normalizeOptions(Options{Profile: ProfileNone, AssumeSortedTags: true, Selectors: []Selector{{ID: "leaf", Path: []core.Tag{tc.ancestor}, Tag: tc.leaf}}})
		if err != nil {
			t.Fatal(err)
		}
		want := tc.ancestor
		if want.Compare(tagSpecificCharacterSet) < 0 {
			want = tagSpecificCharacterSet
		}
		if got := newHeaderReadPlan(opts).maximum; got != want {
			t.Fatalf("ancestor maximum %s want %s", got, want)
		}
	}
}

type indexPruneBomb struct {
	reader     *bytes.Reader
	valueStart int64
	reads      int
}

func (r *indexPruneBomb) Read(p []byte) (int, error) {
	pos, _ := r.reader.Seek(0, io.SeekCurrent)
	if pos >= r.valueStart || int64(len(p)) > r.valueStart-pos {
		return 0, errors.New("stopped value read")
	}
	r.reads++
	return r.reader.Read(p)
}
func (r *indexPruneBomb) Seek(n int64, w int) (int64, error) { return r.reader.Seek(n, w) }

func TestHeaderPruningSortedHeaderOnlyAndDefaultUnsorted(t *testing.T) {
	charset := indexPruneWire(tagSpecificCharacterSet, "CS", 10, []byte("ISO_IR 100"))
	tail := indexPruneWire(tags.PatientName, "PN", 200, nil)
	wire := append(charset, tail...)
	s := &indexPruneBomb{reader: bytes.NewReader(wire), valueStart: int64(len(wire))}
	got, err := ReadSeeker(context.Background(), "bomb", s, int64(len(wire)), Options{Profile: ProfileNone, AssumeSortedTags: true, RawDataSet: &RawDataSetOptions{TransferSyntax: transfer.ExplicitVRLittleEndian}})
	if err != nil || !got.StoppedAfterRequestedTags || got.StoppedBeforePixelData || !got.StoppedHeaderSet || got.StoppedHeader.Tag != tags.PatientName || got.BytesScanned != int64(len(wire)) {
		t.Fatalf("stop %+v error %v", got, err)
	}
	// A higher tag preceding a wanted root tag remains supported by default.
	wire = indexPruneWire(core.NewTag(0x40, 0x1001), "LO", 2, []byte("XY"))
	wire = append(wire, indexPruneWire(tags.PatientName, "PN", 2, []byte("AB"))...)
	wire = append(wire, indexPruneWire(tags.PatientName, "PN", 2, []byte("CD"))...)
	got = indexPruneRead(t, wire, Options{Profile: ProfilePatient})
	if got.Record.Patient.Name != "CD" || got.StoppedAfterRequestedTags {
		t.Fatalf("default unsorted %+v", got)
	}
	got = indexPruneRead(t, wire, Options{Profile: ProfilePatient, AssumeSortedTags: true})
	if got.Record.Patient.Name != "" || !got.StoppedAfterRequestedTags {
		t.Fatal("sorted opt-in did not expose caller's ordering assumption")
	}
}

func TestHeaderPruningNestedOccurrenceOffsetsAndOpaqueBranch(t *testing.T) {
	directory, leaf, icon := core.NewTag(4, 0x1220), core.NewTag(4, 0x1511), core.NewTag(0x88, 0x200)
	body := []byte{}
	offsets := []int64{}
	for i := 0; i < 3; i++ {
		value := []byte{'1', '.', '2', '.', byte('1' + i), 0}
		item := indexPruneWire(leaf, "UI", 6, value)
		hidden := indexPruneWire(tags.Rows, "US", 2, []byte{1, 0})
		hidden = append(indexPruneWire(core.NewTag(0xfffe, 0xe000), "", uint32(len(hidden)), nil), hidden...)
		item = append(item, indexPruneWire(icon, "SQ", uint32(len(hidden)), hidden)...)
		offsets = append(offsets, int64(12+len(body)+8))
		body = append(body, indexPruneWire(core.NewTag(0xfffe, 0xe000), "", uint32(len(item)), item)...)
	}
	wire := indexPruneWire(directory, "SQ", uint32(len(body)), body)
	wire = append(wire, indexPruneWire(tagSpecificCharacterSet, "CS", 10, []byte("ISO_IR 100"))...)
	for _, occurrence := range []Occurrence{OccurrenceFirst, OccurrenceLast, OccurrenceAll} {
		var called []SelectedElement
		opts := Options{Profile: ProfileNone, Selectors: []Selector{{ID: "uid", Path: []core.Tag{directory}, Tag: leaf, Occurrence: occurrence, Handle: func(_ context.Context, e SelectedElement) error { called = append(called, e); return nil }}}}
		strict := indexPruneRead(t, wire, opts)
		opts.AssumeSortedTags = true
		opts.SequencePruning = SequencePruningOpaque
		fast := indexPruneRead(t, wire, opts)
		if !reflect.DeepEqual(strict.Record, fast.Record) {
			t.Fatal("pruning changed extracted record")
		}
		selected := fast.Record.Selected["uid"]
		indices := []int{0, 1, 2}
		if occurrence == OccurrenceFirst {
			indices = []int{0}
		}
		if occurrence == OccurrenceLast {
			indices = []int{2}
		}
		if len(selected) != len(indices) || len(called) != 2*len(indices) {
			t.Fatalf("occurrence %d selected/handlers %d/%d", occurrence, len(selected), len(called))
		}
		for j, i := range indices {
			e := selected[j]
			if e.Offset != offsets[i] || len(e.Path) != 2 || e.Path[0].Tag != directory || e.Path[0].ItemIndex != i || !bytes.Equal([]byte(e.Element.Value.(core.RawValue)), []byte{'1', '.', '2', '.', byte('1' + i), 0}) {
				t.Fatalf("selected %+v", e)
			}
		}
	}
	// Ordinary object extraction is an independent complete-sequence oracle.
	obj, err := object.ReadDataSet(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := obj.Get(directory)
	if len(e.Value.(core.SequenceValue).Items) != 3 {
		t.Fatal("ordinary reader fixture differs")
	}
}

func TestHeaderPruningHiddenValidationAndInnerBudget(t *testing.T) {
	seq := core.NewTag(8, 0x1111)
	wire := indexPruneWire(seq, "SQ", 4, []byte{1, 2, 3, 4})
	opts := Options{Profile: ProfileNone, RawDataSet: &RawDataSetOptions{TransferSyntax: transfer.ExplicitVRLittleEndian}}
	if _, err := ReadAt(context.Background(), "bad", bytes.NewReader(wire), int64(len(wire)), opts); err == nil {
		t.Fatal("traversal accepted hidden malformed body")
	}
	opts.SequencePruning = SequencePruningOpaque
	if _, err := ReadAt(context.Background(), "bad", bytes.NewReader(wire), int64(len(wire)), opts); err != nil {
		t.Fatalf("opaque declared body: %v", err)
	}
	body := []byte{}
	for i := 0; i < 5; i++ {
		body = append(body, indexPruneWire(core.NewTag(0xfffe, 0xe000), "", 0xffffffff, nil)...)
		body = append(body, indexPruneWire(core.NewTag(0xfffe, 0xe00d), "", 0, nil)...)
	}
	body = append(body, indexPruneWire(core.NewTag(0xfffe, 0xe0dd), "", 0, nil)...)
	wire = indexPruneWire(seq, "SQ", 0xffffffff, body)
	opts.Limits.MaxTokens = 11
	if _, err := ReadAt(context.Background(), "budget", bytes.NewReader(wire), int64(len(wire)), opts); !errors.Is(err, ErrResourceLimit) || !errors.Is(err, parser.ErrMaxSelectiveTokensExceeded) {
		t.Fatalf("hidden budget %v", err)
	}
}

func TestHeaderPruningConcurrentDetachedOptionsAndInput(t *testing.T) {
	wire := indexPruneWire(tags.PatientName, "PN", 2, []byte("AB"))
	before := append([]byte(nil), wire...)
	opts := Options{Profile: ProfilePatient, AssumeSortedTags: true, SequencePruning: SequencePruningOpaque, Selectors: []Selector{{ID: "name", Tag: tags.PatientName}}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := indexPruneRead(t, wire, opts)
			e := got.Record.Selected["name"][0]
			if got.Record.Patient.Name != "AB" || e.Offset != 0 {
				t.Errorf("record %+v", got)
			}
			e.Element.Value.(core.RawValue)[0] = 'Z'
		}()
	}
	wg.Wait()
	if !bytes.Equal(wire, before) || opts.Selectors[0].MaxValues != 0 {
		t.Fatal("caller input/options mutated")
	}
	if _, err := Read(context.Background(), PathSource("must-not-open"), Options{SequencePruning: 99}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid policy: %v", err)
	}
}

func TestHeaderPruningCharsetAndPixelBarrier(t *testing.T) {
	wire := indexPruneWire(tagSpecificCharacterSet, "CS", 10, []byte("ISO_IR 100"))
	wire = append(wire, indexPruneWire(tags.PatientName, "PN", 6, []byte{'M', 0xfc, 'l', 'l', 'e', 'r'})...)
	for _, mode := range []Options{{Profile: ProfilePatient}, {Profile: ProfilePatient, AssumeSortedTags: true}, {Profile: ProfilePatient, SequencePruning: SequencePruningOpaque}, {Profile: ProfilePatient, AssumeSortedTags: true, SequencePruning: SequencePruningOpaque}} {
		got := indexPruneRead(t, wire, mode)
		if got.Record.Patient.Name != "Müller" {
			t.Fatalf("charset name %q", got.Record.Patient.Name)
		}
	}
	for _, pixel := range []core.Tag{tags.PixelData, core.NewTag(0x7fe0, 8), core.NewTag(0x7fe0, 9)} {
		data := indexPruneWire(pixel, "OB", 500, nil)
		got := indexPruneRead(t, data, Options{Profile: ProfileNone, AssumeSortedTags: true, SequencePruning: SequencePruningOpaque, Selectors: []Selector{{ID: "after_pixel", Tag: core.NewTag(0xfffc, 0xfffc)}}})
		if !got.StoppedBeforePixelData || got.StoppedAfterRequestedTags || got.StoppedHeader.Tag != pixel || got.BytesScanned != 12 {
			t.Fatalf("pixel barrier %+v", got)
		}
	}
}

type indexPruneCancelSeeker struct {
	*bytes.Reader
	cancel context.CancelFunc
}

func (s *indexPruneCancelSeeker) Seek(offset int64, whence int) (int64, error) {
	position, err := s.Reader.Seek(offset, whence)
	if err == nil && offset > 0 && whence == io.SeekCurrent {
		s.cancel()
	}
	return position, err
}

func TestHeaderPruningOpaqueCancelClosesOnce(t *testing.T) {
	wire := indexPruneWire(core.NewTag(8, 0x1111), "SQ", 4096, make([]byte, 4096))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened, closed := 0, 0
	source := SourceFunc(func(context.Context) (OpenedSource, error) {
		opened++
		return OpenedSource{Reader: &indexPruneCancelSeeker{Reader: bytes.NewReader(wire), cancel: cancel}, Info: SourceInfo{Origin: "cancel", Size: int64(len(wire))}, Close: func() error { closed++; return nil }}, nil
	})
	_, err := Read(ctx, source, Options{Profile: ProfileNone, SequencePruning: SequencePruningOpaque, RawDataSet: &RawDataSetOptions{TransferSyntax: transfer.ExplicitVRLittleEndian}})
	if !errors.Is(err, context.Canceled) || opened != 1 || closed != 1 {
		t.Fatalf("cancel %v, open/close %d/%d", err, opened, closed)
	}
}
