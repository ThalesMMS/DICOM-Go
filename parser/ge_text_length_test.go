package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Independently constructed malformed headers; no external corpus bytes.
func geTextLengthFixture(tag core.Tag, declared uint32, text []byte, next core.Tag, value []byte) []byte {
	data := implicitEl(tag.Group, tag.Element, text)
	binary.LittleEndian.PutUint32(data[4:8], declared)
	return append(data, implicitEl(next.Group, next.Element, value)...)
}

type geNonSeekable struct{ io.Reader }

// A large, sized synthetic source without allocating its zero-filled tail.
type geZeroTail struct{ prefix []byte }

func (r geZeroTail) ReadAt(p []byte, offset int64) (int, error) {
	clear(p)
	if offset < int64(len(r.prefix)) {
		copy(p, r.prefix[offset:])
	}
	return len(p), nil
}

func TestGETextLengthContextualVRAtDeclaredBoundaryIsAmbiguous(t *testing.T) {
	const nextLength = uint32(0x00981000)
	data := geTextLengthFixture(core.NewTag(0x0008, 0x103e), 13, []byte("SYNTHETIC "), core.NewTag(0x0018, 0x1800), nil)
	binary.LittleEndian.PutUint32(data[22:26], nextLength)
	// +10 identifies AcquisitionTimeSynchronized (CS), but +13 overlaps
	// ZeroVelocityPixelValue (0018,9810), whose contextual VR is US or SS.
	// Both public tags are known, so a unique VR at +10 cannot choose a VL.
	contextual := core.NewTag(0x0018, 0x9810)
	if _, known := std.Dictionary.ByTag(contextual); !known {
		t.Fatal("contextual ambiguity fixture no longer has a standard tag")
	}
	source := io.NewSectionReader(geZeroTail{prefix: data}, 0, int64(len(data))+int64(nextLength))
	r := NewReader(source, transfer.ImplicitVRLittleEndian, ReaderOptions{
		Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
	})
	h, err := r.readHeader()
	recs, _ := r.BoundaryRecoveries()
	if err != nil || h.Length != 13 || r.Position() != 8 || len(recs) != 0 {
		t.Fatalf("contextual VR at +13 was ignored: header=%+v recs=%v err=%v", h, recs, err)
	}
	remaining := make([]byte, len(data)-8)
	if _, err := io.ReadFull(r.counter, remaining); err != nil || !bytes.Equal(remaining, data[8:]) {
		t.Fatalf("contextual ambiguity refusal lost input: %v", err)
	}
}

func TestGETextLengthRecovery(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	data := geTextLengthFixture(tag, 13, []byte("SYNTHETIC "), next, []byte("NO^PHI  "))
	for _, streaming := range []bool{false, true} {
		for _, recovery := range []bool{false, true} {
			opts := ReaderOptions{Dictionary: std.Dictionary, BaseOffset: 700, VRRecovery: VRRecoveryOptions{CorrectGETextLength: recovery}}
			var source io.Reader = bytes.NewReader(data)
			if streaming {
				source = geNonSeekable{source}
			}
			r := NewReader(source, transfer.ImplicitVRLittleEndian, opts)
			elements, err := r.ReadAll()
			recs, truncated := r.BoundaryRecoveries()
			if !recovery {
				if !errors.Is(err, ErrOddElementLength) || len(recs) != 0 {
					t.Fatalf("strict streaming=%v: err=%v, recoveries=%v", streaming, err, recs)
				}
				continue
			}
			if err != nil || len(elements) != 2 || elements[0].Header.Length != 10 || elements[1].Header.Tag != next {
				t.Fatalf("recovered streaming=%v: elements=%v err=%v", streaming, elements, err)
			}
			if r.Position() != 700+int64(len(data)) {
				t.Fatalf("lookahead changed position: %d", r.Position())
			}
			if truncated || len(recs) != 1 || recs[0] != (BoundaryRecovery{
				Path: tag.String(), Offset: 708, DeclaredLength: 13, AvailableLength: 10,
				Reason: BoundaryGETextLength, DeclaredVR: core.VRLO, UsedVR: core.VRLO,
			}) {
				t.Fatalf("diagnostic: %+v, truncated=%v", recs, truncated)
			}
			for i, want := range [][]byte{[]byte("SYNTHETIC "), []byte("NO^PHI  ")} {
				raw, ok := elements[i].Value.(core.RawValue)
				if !ok || !bytes.Equal(raw, want) {
					t.Fatalf("value %d changed: %T", i, elements[i].Value)
				}
			}
		}
	}
}

type geTagOverlay struct{ tag core.Tag }

func (d geTagOverlay) ByTag(tag core.Tag) (dictionary.Entry, bool) {
	return dictionary.Entry{Tag: tag, VR: core.VRLO}, tag == d.tag
}

func (geTagOverlay) ByKeyword(string) (dictionary.Entry, bool) {
	return dictionary.Entry{}, false
}

func TestGETextLengthRefusesOtherLayouts(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	text, value := []byte("SYNTHETIC "), []byte("NO^PHI  ")
	base := geTextLengthFixture(tag, 13, text, next, value)
	withNextLength := func(length uint32) []byte {
		data := bytes.Clone(base)
		binary.LittleEndian.PutUint32(data[22:26], length)
		return data
	}
	ambiguous := geTextLengthFixture(tag, 13, text, next, bytes.Repeat([]byte{' '}, 258))
	// +10 is (0008,1060), VL258; its overlapping +13 tag is (0210,0001).
	// A caller dictionary recognizing both must not let the parser guess.
	ambiguousDict := dictionary.Chain{geTagOverlay{core.NewTag(0x0210, 0x0001)}, std.Dictionary}
	private := core.NewTag(0x0009, 0x1001)
	for _, tc := range []struct {
		name       string
		data       []byte
		dict       dictionary.DataDictionary
		maxElement int64
	}{
		{"ambiguous known boundaries", ambiguous, ambiguousDict, 0},
		{"fabricated header inside value", geTextLengthFixture(tag, 13, append(bytes.Clone(text), 0x08, 0x00, 0x60), core.NewTag(0x0010, 0x0010), value), nil, 0},
		{"manufacturer exception", geTextLengthFixture(core.NewTag(0x0008, 0x0070), 13, text, next, value), nil, 0},
		{"institution exception", geTextLengthFixture(core.NewTag(0x0008, 0x0080), 13, text, next, value), nil, 0},
		{"numeric VR", geTextLengthFixture(core.NewTag(0x0008, 0x0001), 13, text, next, value), nil, 0},
		{"private text", geTextLengthFixture(private, 13, text, next, value), dictionary.Chain{geTagOverlay{private}, std.Dictionary}, 0},
		{"unknown public tag", geTextLengthFixture(core.NewTag(0x0008, 0x103f), 13, text, next, value), nil, 0},
		{"non ASCII text", geTextLengthFixture(tag, 13, []byte("SYNTHETIC\n"), next, value), nil, 0},
		{"unknown next tag", geTextLengthFixture(tag, 13, text, core.NewTag(0x0008, 0xffff), value), nil, 0},
		{"private next tag", geTextLengthFixture(tag, 13, text, private, value), dictionary.Chain{geTagOverlay{private}, std.Dictionary}, 0},
		{"unordered next tag", geTextLengthFixture(tag, 13, text, core.NewTag(0x0008, 0x0060), value), nil, 0},
		{"duplicate next tag", geTextLengthFixture(tag, 13, text, tag, value), nil, 0},
		{"next sequence", geTextLengthFixture(tag, 13, text, core.NewTag(0x0008, 0x1115), nil), nil, 0},
		{"next undefined length", withNextLength(uint32(core.UndefinedLength)), nil, 0},
		{"next odd length", withNextLength(7), nil, 0},
		{"next truncated value", base[:len(base)-1], nil, 0},
		{"next header truncated", base[:25], nil, 0},
		{"next excessive length", withNextLength(1 << 30), nil, 0},
		{"next exceeds element budget", base, nil, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				var source io.Reader = bytes.NewReader(tc.data)
				if streaming {
					source = geNonSeekable{source}
				}
				dict := tc.dict
				if dict == nil {
					dict = std.Dictionary
				}
				r := NewReader(source, transfer.ImplicitVRLittleEndian, ReaderOptions{
					Dictionary: dict, MaxElementBytes: tc.maxElement,
					VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
				})
				h, err := r.readHeader()
				if err != nil || h.Length != 13 || r.Position() != 8 {
					t.Fatalf("streaming=%v altered header/position: %+v %d %v", streaming, h, r.Position(), err)
				}
				if recs, _ := r.BoundaryRecoveries(); len(recs) != 0 {
					t.Fatalf("unjustified recovery: %+v", recs)
				}
				remaining, err := io.ReadAll(r.counter)
				if err != nil || !bytes.Equal(remaining, tc.data[8:]) {
					t.Fatalf("streaming=%v lookahead lost bytes: %v", streaming, err)
				}
			}
		})
	}
}

func TestGETextLengthPreservesOtherLengthsAndSyntaxes(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ImplicitVRBigEndianACRNEMA} {
		for _, length := range []uint32{10, 12, 13, 15} {
			data := geTextLengthFixture(tag, length, []byte("SYNTHETIC "), next, []byte("NO^PHI  "))
			if syntax.ExplicitVR {
				data = explicitShort(tag.Group, tag.Element, "LO", []byte("SYNTHETIC "))
				binary.LittleEndian.PutUint16(data[6:8], uint16(length))
			} else if syntax.ByteOrder == binary.BigEndian {
				binary.BigEndian.PutUint16(data[0:2], tag.Group)
				binary.BigEndian.PutUint16(data[2:4], tag.Element)
				binary.BigEndian.PutUint32(data[4:8], length)
			}
			r := NewReader(bytes.NewReader(data), syntax, ReaderOptions{Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true}})
			h, err := r.readHeader()
			want := length
			if length == 13 && syntax == transfer.ImplicitVRLittleEndian {
				want = 10
			}
			if err != nil || uint32(h.Length) != want || r.Position() != 8 {
				t.Fatalf("syntax=%s length=%d: %+v %v, position %d", syntax.UID, length, h, err, r.Position())
			}
		}
	}
}

func TestGETextLengthKeepsAlignedOddText(t *testing.T) {
	for _, tag := range []core.Tag{core.NewTag(0x0008, 0x103e), core.NewTag(0x0018, 0x0080)} {
		next := core.NewTag(0x0018, 0x0081)
		// A valid DS character sequence can have 13 bytes in a nonconformant
		// odd-length field. AcceptOddLength still preserves all thirteen;
		// CorrectGETextLength must not truncate it or accept it strictly.
		data := geTextLengthFixture(tag, 13, []byte("1.23456789012"), next, []byte("2.0 "))
		for _, acceptOdd := range []bool{false, true} {
			opts := ReaderOptions{Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true}}
			if acceptOdd {
				opts.OddLengthPolicy = AcceptOddLength
			}
			r := NewReader(bytes.NewReader(data), transfer.ImplicitVRLittleEndian, opts)
			elements, err := r.ReadAll()
			if !acceptOdd {
				if !errors.Is(err, ErrOddElementLength) {
					t.Fatalf("GE option accepted aligned odd text strictly: %v", err)
				}
			} else if err != nil || len(elements) != 2 || elements[0].Header.Length != 13 || elements[1].Header.Tag != next {
				t.Fatalf("aligned odd text changed: %v, %v", elements, err)
			}
			if recs, _ := r.BoundaryRecoveries(); len(recs) != 0 {
				t.Fatalf("aligned text was repaired: %v", recs)
			}
		}
	}
}

func TestGETextLengthChainBudgetAndTerminal(t *testing.T) {
	chain := func(length int) []byte {
		var data []byte
		for i := 0; i < length; i++ {
			el := implicitEl(0x0018, uint16(0x0080+i), []byte("1.23456789"))
			binary.LittleEndian.PutUint32(el[4:8], 13)
			data = append(data, el...)
		}
		return append(data, implicitEl(0x0018, 0x0086, []byte("1 "))...)
	}
	for length := 1; length <= 5; length++ {
		for _, streaming := range []bool{false, true} {
			data := chain(length)
			var source io.Reader = bytes.NewReader(data)
			if streaming {
				source = geNonSeekable{source}
			}
			r := NewReader(source, transfer.ImplicitVRLittleEndian, ReaderOptions{
				Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
			})
			if length > 4 {
				h, err := r.readHeader()
				if err != nil || h.Length != 13 || r.Position() != 8 {
					t.Fatalf("chain budget exceeded: %+v %v", h, err)
				}
				remaining, err := io.ReadAll(r.counter)
				if err != nil || !bytes.Equal(remaining, data[8:]) {
					t.Fatalf("overlong chain peek was not restored: %v", err)
				}
			} else {
				elements, err := r.ReadAll()
				recs, _ := r.BoundaryRecoveries()
				if err != nil || len(elements) != length+1 || len(recs) != length {
					t.Fatalf("chain=%d streaming=%v: elements=%d recs=%v err=%v", length, streaming, len(elements), recs, err)
				}
			}
		}
	}
	data := chain(3)
	for _, end := range []int{len(data) - 1, len(data) - 5, len(data) - 10} {
		for _, streaming := range []bool{false, true} {
			var source io.Reader = bytes.NewReader(data[:end])
			if streaming {
				source = geNonSeekable{source}
			}
			r := NewReader(source, transfer.ImplicitVRLittleEndian, ReaderOptions{
				Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
			})
			h, err := r.readHeader()
			recs, _ := r.BoundaryRecoveries()
			if err != nil || h.Length != 13 || r.Position() != 8 || len(recs) != 0 {
				t.Fatalf("truncated chain authorized recovery: %+v %v %v", h, recs, err)
			}
			remaining, err := io.ReadAll(r.counter)
			if err != nil || !bytes.Equal(remaining, data[8:end]) {
				t.Fatalf("truncated chain peek was not restored: %v", err)
			}
		}
	}
}

func TestGETextLengthRespectsContainerAndByteBudget(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	data := geTextLengthFixture(tag, 13, []byte("SYNTHETIC "), next, []byte("NO^PHI  "))
	for _, container := range []bool{false, true} {
		source := bytes.NewReader(data)
		opts := ReaderOptions{Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true}}
		if !container {
			opts.MaxTotalBytes = 28 // The following value does not fit the budget.
		}
		r := NewReader(geNonSeekable{source}, transfer.ImplicitVRLittleEndian, opts)
		if container {
			r.seqDelimiters = []seqToken{{typ: seqTokenTypeItem, length: 28, definedIndex: 0, itemIndex: 0}}
		}
		h, err := r.readHeader()
		physical, _ := source.Seek(0, io.SeekCurrent)
		if err != nil || h.Length != 13 || r.Position() != 8 || physical > 28 {
			t.Fatalf("container=%v: header %+v, position=%d physical=%d, err=%v", container, h, r.Position(), physical, err)
		}
		if recs, _ := r.BoundaryRecoveries(); len(recs) != 0 {
			t.Fatalf("out-of-bounds next value authorized recovery: %v", recs)
		}
	}
	// A real defined-length item exercises the active fence and path. The
	// sentinel outside it must still be read at its own boundary.
	item := append(bControl(0xe000, uint32(len(data))), data...)
	sequence := implicitEl(0x0008, 0x1115, item)
	sequence = append(sequence, implicitEl(0x0010, 0x0020, []byte("ID00"))...)
	r := NewReader(bytes.NewReader(sequence), transfer.ImplicitVRLittleEndian, ReaderOptions{
		Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
	})
	ds, err := r.ReadDataSet()
	recs, _ := r.BoundaryRecoveries()
	if err != nil || len(ds.Elements) != 2 || len(recs) != 1 || recs[0].Path != "(0008,1115)[0].(0008,103E)" || recs[0].Offset != 24 {
		t.Fatalf("nested repair/path: elements=%d recoveries=%v err=%v", len(ds.Elements), recs, err)
	}
}

func TestGETextLengthBoundsDoNotProveTerminalBytesExist(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	// The peek is filled exactly, so io.ReadFull cannot observe EOF. The
	// terminal VL fits both large bounds but not the bytes actually present.
	data := geTextLengthFixture(tag, 13, []byte("SYNTHETIC "), next, bytes.Repeat([]byte{' '}, 64))
	binary.LittleEndian.PutUint32(data[22:26], 100)
	for _, container := range []bool{false, true} {
		opts := ReaderOptions{Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true}}
		if !container {
			opts.MaxTotalBytes = 1000
		}
		r := NewReader(geNonSeekable{bytes.NewReader(data)}, transfer.ImplicitVRLittleEndian, opts)
		if container {
			r.seqDelimiters = []seqToken{{typ: seqTokenTypeItem, length: 1000, definedIndex: 0, itemIndex: 0}}
		}
		h, err := r.readHeader()
		recs, _ := r.BoundaryRecoveries()
		if err != nil || h.Length != 13 || r.Position() != 8 || len(recs) != 0 {
			t.Fatalf("bound mistaken for available bytes: container=%v header=%+v recs=%v err=%v", container, h, recs, err)
		}
		remaining, err := io.ReadAll(r.counter)
		if err != nil || !bytes.Equal(remaining, data[8:]) {
			t.Fatalf("bound-only refusal lost input: %v", err)
		}
	}
}

type geFailingSource struct {
	data []byte
	err  error
}

func (r *geFailingSource) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestGETextLengthRestoresLookaheadOnSourceError(t *testing.T) {
	tag, next := core.NewTag(0x0008, 0x103e), core.NewTag(0x0008, 0x1060)
	data := geTextLengthFixture(tag, 13, []byte("SYNTHETIC "), next, []byte("NO^PHI  "))
	failure := errors.New("synthetic input failure")
	source := io.MultiReader(bytes.NewReader(data[:8]), &geFailingSource{data: bytes.Clone(data[8:]), err: failure})
	r := NewReader(source, transfer.ImplicitVRLittleEndian, ReaderOptions{
		Dictionary: std.Dictionary, VRRecovery: VRRecoveryOptions{CorrectGETextLength: true},
	})
	if _, err := r.readHeader(); !errors.Is(err, failure) || r.Position() != 8 {
		t.Fatalf("peek hid source failure or changed position: %v position=%d", err, r.Position())
	}
	remaining, err := io.ReadAll(r.counter)
	if err != nil || !bytes.Equal(remaining, data[8:]) {
		t.Fatalf("peek lost input on source failure: %v", err)
	}
}
