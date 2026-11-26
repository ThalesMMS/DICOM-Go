package dicomxml

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const streamRoot = `<NativeDicomModel xmlns="` + Namespace + `" xml:space="preserve">`
const streamEnd = `</NativeDicomModel>`

func streamAttr(tag, vr, value string) string {
	return fmt.Sprintf(`<DicomAttribute tag="%s" vr="%s"><Value number="1">%s</Value></DicomAttribute>`, tag, vr, value)
}

type smallChunkReader struct {
	reader      io.Reader
	chunk, read int
}

func (r *smallChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.chunk {
		p = p[:r.chunk]
	}
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestStreamingDecodeChunkBoundariesAndExactIntegers(t *testing.T) {
	document := streamRoot + streamAttr("00080005", "CS", "ISO_IR 192") +
		`<DicomAttribute tag="00100010" vr="PN"><PersonName number="1"><Alphabetic><FamilyName>Doe&amp;Co</FamilyName></Alphabetic><Ideographic><GivenName>花子</GivenName></Ideographic><Phonetic><FamilyName>ヤマダ</FamilyName></Phonetic></PersonName></DicomAttribute>` +
		streamAttr("77760001", "SV", "-9223372036854775808") + streamAttr("77760002", "UV", "18446744073709551615") +
		streamAttr("77760003", "AT", "00100010") + streamAttr("77760004", "FD", "-Infinity") + streamEnd
	for _, size := range []int{1, 2, 3, 7, 31} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			t.Run(fmt.Sprintf("chunk%d-%s", size, order), func(t *testing.T) {
				r := &smallChunkReader{reader: strings.NewReader(document), chunk: size}
				obj, err := Decode(r, std.Dictionary, UnmarshalOptions{AllowMissingKeyword: true, ByteOrder: order})
				if err != nil {
					t.Fatal(err)
				}
				if pn, _ := obj.GetString(core.NewTag(0x0010, 0x0010)); pn != "Doe&Co=^花子=ヤマダ" {
					t.Fatalf("PN %q", pn)
				}
				for tag, want := range map[core.Tag]uint64{core.NewTag(0x7776, 1): 1 << 63, core.NewTag(0x7776, 2): math.MaxUint64} {
					raw, _ := obj.GetRaw(tag)
					if len(raw) != 8 || order.Uint64(raw) != want {
						t.Fatalf("%s: %x", tag, raw)
					}
				}
				raw, _ := obj.GetRaw(core.NewTag(0x7776, 3))
				if len(raw) != 4 || order.Uint16(raw) != 0x0010 || order.Uint16(raw[2:]) != 0x0010 {
					t.Fatalf("AT %x", raw)
				}
				raw, _ = obj.GetRaw(core.NewTag(0x7776, 4))
				if len(raw) != 8 || !math.IsInf(math.Float64frombits(order.Uint64(raw)), -1) {
					t.Fatalf("FD %x", raw)
				}
			})
		}
	}
}

func TestStreamingDecodeRejectsBeforeDrainingReader(t *testing.T) {
	first := streamAttr("77760001", "LO", "first")
	huge := streamAttr("77760002", "LO", strings.Repeat("x", 1<<20))
	for _, tc := range []struct {
		name, first string
		limits      Limits
		want        error
	}{
		{"element count", first, Limits{MaxElements: 1}, ErrMaxElementsExceeded},
		{"value length", streamAttr("77760001", "LO", "oversized"), Limits{MaxValueBytes: 3}, ErrMaxValueBytesExceeded},
		{"inline length", `<DicomAttribute tag="77760001" vr="OB"><InlineBinary>YWJjZA==</InlineBinary></DicomAttribute>`, Limits{MaxInlineBinaryBytes: 3}, ErrMaxInlineBytesExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &smallChunkReader{reader: strings.NewReader(streamRoot + tc.first + huge + streamEnd), chunk: 17}
			_, err := Decode(r, std.Dictionary, UnmarshalOptions{Limits: tc.limits})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if r.read > 1024 {
				t.Fatalf("drained %d bytes before early rejection", r.read)
			}
		})
	}
}

func TestStreamingDecodeXMLByteBoundaries(t *testing.T) {
	doc := streamRoot + streamAttr("77760001", "LO", "ok") + streamEnd
	for _, n := range []int64{int64(len(doc) - 1), int64(len(doc)), math.MaxInt64} {
		obj, err := Decode(strings.NewReader(doc), nil, UnmarshalOptions{Limits: Limits{MaxXMLBytes: n}})
		if n < int64(len(doc)) {
			if !errors.Is(err, ErrMaxXMLBytesExceeded) {
				t.Fatalf("limit %d: %v", n, err)
			}
		} else if err != nil || obj == nil {
			t.Fatalf("limit %d: %v", n, err)
		}
	}
	_, err := Decode(strings.NewReader(doc+" "), nil, UnmarshalOptions{Limits: Limits{MaxXMLBytes: int64(len(doc))}})
	if !errors.Is(err, ErrMaxXMLBytesExceeded) {
		t.Fatalf("trailing byte: %v", err)
	}
}

type stopReader struct {
	prefix *strings.Reader
	err    error
}

func (r *stopReader) Read(p []byte) (int, error) {
	if r.prefix.Len() == 0 {
		return 0, r.err
	}
	return r.prefix.Read(p)
}

func TestStreamingDecodePreservesReadAndCancellationErrors(t *testing.T) {
	boom := errors.New("synthetic read failure")
	_, err := Decode(&stopReader{prefix: strings.NewReader(streamRoot + `<DicomAttribute tag="77760001" vr="LO"><Value number="1">text`), err: boom}, nil, UnmarshalOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("read cause lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	err = WalkContext(ctx, strings.NewReader(streamRoot+streamAttr("77760001", "LO", "one")+streamAttr("77760002", "LO", "two")+streamEnd), nil, UnmarshalOptions{}, func(Attribute) error { count++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancel: count=%d err=%v", count, err)
	}
}

func TestStreamingPrivateCreatorsCanFollowTheirData(t *testing.T) {
	doc := streamRoot +
		`<DicomAttribute tag="00090010" vr="LO" privateCreator="NEW"><Value number="1">payload</Value></DicomAttribute>` +
		`<DicomAttribute tag="00090001" vr="LO" privateCreator="X"><Value number="1">x</Value></DicomAttribute>` +
		`<DicomAttribute tag="00090010" vr="UN"><InlineBinary>AAEC</InlineBinary></DicomAttribute>` +
		streamAttr("00090012", "LO", "X") + streamAttr("00090011", "LO", "X") + streamEnd
	obj, err := Decode(strings.NewReader(doc), nil, UnmarshalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The first explicit occurrence of X wins even when creator slots are out of order.
	if value, _ := obj.GetString(core.NewTag(0x0009, 0x1201)); value != "x" {
		t.Fatalf("X value %q", value)
	}
	if value, _ := obj.GetString(core.NewTag(0x0009, 0x1310)); value != "payload" {
		t.Fatalf("NEW value %q", value)
	}
	if value, _ := obj.GetString(core.NewTag(0x0009, 0x0013)); value != "NEW" {
		t.Fatalf("NEW creator %q", value)
	}
}

func TestWalkDeliversBeforeEOFAndHonorsCallbackFailure(t *testing.T) {
	stop := errors.New("consumer stop")
	tailRead := errors.New("tail must not be read")
	prefix := streamRoot + streamAttr("77760001", "LO", "owned")
	r := &stopReader{prefix: strings.NewReader(prefix), err: tailRead}
	count := 0
	err := Walk(r, nil, UnmarshalOptions{}, func(a Attribute) error {
		count++
		if a.Element.Tag() != core.NewTag(0x7776, 1) || a.Element.Value.(core.StringValue)[0] != "owned" || !strings.Contains(a.Path, "77760001") {
			t.Fatalf("attribute %+v", a)
		}
		return stop
	})
	if count != 1 || !errors.Is(err, stop) || errors.Is(err, tailRead) {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestWalkPrivateIdentityValueOwnershipAndLateValidation(t *testing.T) {
	doc := streamRoot + `<DicomAttribute tag="00090001" vr="LO" privateCreator="X"><Value number="1">private</Value></DicomAttribute>` + streamAttr("00090010", "LO", "X") + streamEnd
	var seen []Attribute
	err := Walk(strings.NewReader(doc), nil, UnmarshalOptions{}, func(a Attribute) error {
		seen = append(seen, a)
		if a.Element.Tag() == core.NewTag(0x0009, 0x0010) {
			a.Element.Value.(core.StringValue)[0] = "consumer mutation"
		}
		return nil
	})
	if err != nil || len(seen) != 2 {
		t.Fatalf("walk len=%d err=%v", len(seen), err)
	}
	if seen[0].PrivateCreator != "X" || seen[0].Element.Tag() != core.NewTag(0x0009, 1) {
		t.Fatalf("private %+v", seen[0])
	}
	count := 0
	duplicate := streamRoot + streamAttr("77760001", "LO", "one") + streamAttr("77760001", "LO", "two") + streamEnd
	err = Walk(strings.NewReader(duplicate), nil, UnmarshalOptions{}, func(Attribute) error { count++; return nil })
	if !errors.Is(err, ErrInvalidXML) || count != 2 {
		t.Fatalf("late duplicate count=%d err=%v", count, err)
	}
}

func TestStreamingBulkLimitsAndWalkReferenceIdentity(t *testing.T) {
	doc := streamRoot + `<DicomAttribute tag="00090001" vr="OB" privateCreator="X"><BulkData uri="memory:one"/></DicomAttribute>` + streamEnd
	var tag core.Tag
	opts := UnmarshalOptions{BulkDataPolicy: ResolveBulkData, Limits: Limits{MaxTotalBinaryBytes: 4}, BulkDataResolver: func(_ context.Context, r BulkDataReference) (io.ReadCloser, error) {
		tag = r.Tag
		return io.NopCloser(strings.NewReader("data")), nil
	}}
	var data []byte
	err := Walk(strings.NewReader(doc), nil, opts, func(a Attribute) error { data = append(data, a.Element.Value.(core.RawValue)...); return nil })
	if err != nil || tag != core.NewTag(0x0009, 1) || string(data) != "data" {
		t.Fatalf("walk tag=%s data=%q err=%v", tag, data, err)
	}
	obj, err := Decode(strings.NewReader(doc), nil, opts)
	if err != nil || tag != core.NewTag(0x0009, 0x1001) || obj == nil {
		t.Fatalf("decode tag=%s err=%v", tag, err)
	}
	opts.Limits.MaxTotalBinaryBytes = 3
	if _, err = Decode(strings.NewReader(doc), nil, opts); !errors.Is(err, ErrMaxBinaryBytesExceeded) {
		t.Fatalf("bulk limit %v", err)
	}
	opts.Limits.MaxTotalBinaryBytes = math.MaxInt64
	if _, err = Decode(strings.NewReader(doc), nil, opts); err != nil {
		t.Fatalf("max int limit %v", err)
	}
}

func TestStreamingDecodeEncapsulatedSharedItemBudget(t *testing.T) {
	// Empty BOT, one two-byte fragment, sequence delimiter.
	field := []byte{0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0, 0xfe, 0xff, 0x00, 0xe0, 2, 0, 0, 0, 1, 2, 0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0}
	for _, limit := range []int{1, 2} {
		b, _ := newBudget(context.Background(), Limits{MaxSequenceItems: limit})
		b.items = 1
		got, err := decodeFragments(field, std.Dictionary, UnmarshalOptions{TransferSyntax: transfer.JPEGBaseline}, b)
		if limit == 1 {
			if !errors.Is(err, ErrMaxItemsExceeded) {
				t.Fatalf("shared limit %v", err)
			}
		} else if err != nil || len(got.Fragments) != 1 || !bytes.Equal(got.Fragments[0], []byte{1, 2}) {
			t.Fatalf("fragments %+v %v", got, err)
		}
	}
}

func TestStreamingInlineBinaryTokenSeamsAndStrictPadding(t *testing.T) {
	for _, size := range []int{1, 2, 3, 24575, 24576, 24577, 49152, 49153} {
		raw := bytes.Repeat([]byte{0x9a}, size)
		encoded := base64.StdEncoding.EncodeToString(raw)
		seam := min(3, len(encoded))
		encoded = encoded[:seam] + "<!-- token seam -->\n \t" + encoded[seam:]
		doc := streamRoot + `<DicomAttribute tag="77760001" vr="OB"><InlineBinary>` + encoded + `</InlineBinary></DicomAttribute>` + streamEnd
		obj, err := Decode(&smallChunkReader{reader: strings.NewReader(doc), chunk: 13}, nil, UnmarshalOptions{Limits: Limits{MaxInlineBinaryBytes: int64(size), MaxTotalBinaryBytes: int64(size)}})
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		got, _ := obj.GetRaw(core.NewTag(0x7776, 1))
		if !bytes.Equal(got, raw) {
			t.Fatalf("size %d mismatch", size)
		}
	}
	for _, encoded := range []string{"", " \n", "AB==", "AAA=AAAA", "AA=A", "YQ=", "YQ===", "YQ==<!-- seam -->YQ==", strings.Repeat("AAAA", 8191) + "YQ==AAAA"} {
		doc := streamRoot + `<DicomAttribute tag="77760001" vr="OB"><InlineBinary>` + encoded + `</InlineBinary></DicomAttribute>` + streamEnd
		if _, err := Decode(strings.NewReader(doc), nil, UnmarshalOptions{}); !errors.Is(err, ErrInvalidXML) {
			t.Fatalf("bad base64 len %d: %v", len(encoded), err)
		}
	}
}

func TestStreamingElementLimitCountsXMLAndFinalDatasetSeparately(t *testing.T) {
	// The existing decoder permits a repeated matching creator. Its XML count
	// is four and its final dataset also has four elements: X and Y creators,
	// plus their private values. Do not charge a discarded duplicate twice.
	doc := streamRoot + streamAttr("00090010", "LO", "X") + streamAttr("00090010", "LO", "X") +
		`<DicomAttribute tag="00090001" vr="LO" privateCreator="X"><Value number="1">x</Value></DicomAttribute>` +
		`<DicomAttribute tag="00090001" vr="LO" privateCreator="Y"><Value number="1">y</Value></DicomAttribute>` + streamEnd
	obj, err := Decode(strings.NewReader(doc), nil, UnmarshalOptions{Limits: Limits{MaxElements: 4}})
	if err != nil || len(obj.Elements()) != 4 {
		t.Fatalf("four XML/final elements: %v", err)
	}
	one := streamRoot + `<DicomAttribute tag="00090001" vr="LO" privateCreator="X"><Value number="1">x</Value></DicomAttribute>` + streamEnd
	if _, err := Decode(strings.NewReader(one), nil, UnmarshalOptions{Limits: Limits{MaxElements: 1}}); !errors.Is(err, ErrMaxElementsExceeded) {
		t.Fatalf("implicit creator limit: %v", err)
	}
}
