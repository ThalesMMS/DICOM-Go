package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// These wire helpers encode the public grammar directly, independently of the
// writer and the pruning implementation. Declared lengths can be adversarial.
func pruningWire(order binary.ByteOrder, tag core.Tag, vr string, length uint32, body []byte) []byte {
	b := make([]byte, 4)
	order.PutUint16(b, tag.Group)
	order.PutUint16(b[2:], tag.Element)
	if vr != "" {
		b = append(b, vr...)
		if vr == "SQ" || vr == "UN" || vr == "OB" || vr == "OW" {
			b = append(b, 0, 0)
		} else {
			at := len(b)
			b = append(b, 0, 0)
			order.PutUint16(b[at:], uint16(length))
			return append(b, body...)
		}
	}
	at := len(b)
	b = append(b, 0, 0, 0, 0)
	order.PutUint32(b[at:], length)
	return append(b, body...)
}

func pruningControl(order binary.ByteOrder, element uint16, length uint32) []byte {
	return pruningWire(order, core.NewTag(0xfffe, element), "", length, nil)
}

func pruningReader(t *testing.T, ctx context.Context, source io.Reader, syntax transfer.Syntax, opts ReaderOptions, maxTokens int) *Reader {
	t.Helper()
	if opts.Dictionary == nil {
		opts.Dictionary = std.Dictionary
	}
	r, err := NewSelectiveReader(ctx, source, syntax, opts, SelectiveReaderOptions{MaxTokens: maxTokens,
		Select: func(_ context.Context, _ validation.Path, h core.ElementHeader, _ int64) (SelectiveDisposition, error) {
			if h.VR == core.VRSQ || h.VR == core.VRUN && h.Length.IsUndefined() {
				return SelectiveSkipSequence, nil
			}
			return SelectiveMaterialize, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSelectiveSequenceDefinedOpaqueAndDiscarded(t *testing.T) {
	seq, tail := core.NewTag(8, 0x1111), core.NewTag(0x10, 0x10)
	body := bytes.Repeat([]byte{0x91, 0x92}, 32<<10) // deliberately not Item grammar
	wire := pruningWire(binary.LittleEndian, seq, "SQ", uint32(len(body)), body)
	wire = append(wire, pruningWire(binary.LittleEndian, tail, "PN", 2, []byte("AB"))...)
	before := append([]byte(nil), wire...)
	source := &selectiveCountingReadSeeker{Reader: bytes.NewReader(wire)}
	r := pruningReader(t, context.Background(), source, transfer.ExplicitVRLittleEndian, ReaderOptions{MaxElementBytes: 2}, 2)
	tok, err := r.Next()
	if err != nil || tok.Kind != TokenElement || tok.Header.Tag != seq || tok.Element.Value.Kind() != core.ValueDiscarded {
		t.Fatalf("discarded = %+v, %v", tok, err)
	}
	var output bytes.Buffer
	if err := NewWriter(&output, transfer.ExplicitVRLittleEndian).WriteElement(tok.Element); !errors.Is(err, core.ErrDiscardedValue) || output.Len() != 0 {
		t.Fatalf("discarded sequence serialized %d bytes: %v", output.Len(), err)
	}
	if r.Position() != int64(12+len(body)) || source.bytesRead != 12 {
		t.Fatalf("position/read = %d/%d", r.Position(), source.bytesRead)
	}
	tok, err = r.Next()
	if err != nil || !bytes.Equal([]byte(tok.Element.Value.(core.RawValue)), []byte("AB")) {
		t.Fatalf("tail = %+v, %v", tok, err)
	}
	if _, err = r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF = %v", err)
	}
	if !bytes.Equal(before, wire) {
		t.Fatal("source mutated")
	}
	strict := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{MaxElementBytes: 2})
	if _, err := strict.ReadDataSet(); err == nil {
		t.Fatal("strict traversal accepted malformed hidden body")
	}
}

func TestSelectiveSequenceDefinedBoundsBeforeBody(t *testing.T) {
	seq := core.NewTag(8, 0x1111)
	for _, tc := range []struct {
		name   string
		length uint32
		body   []byte
		opts   ReaderOptions
		want   error
	}{
		{"source", 8, []byte{1, 2}, ReaderOptions{}, io.ErrUnexpectedEOF},
		{"total", 8, make([]byte, 8), ReaderOptions{MaxTotalBytes: 16}, ErrMaxTotalBytesExceeded},
		{"odd", 3, make([]byte, 3), ReaderOptions{}, ErrOddElementLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &selectiveCountingReadSeeker{Reader: bytes.NewReader(pruningWire(binary.LittleEndian, seq, "SQ", tc.length, tc.body))}
			r := pruningReader(t, context.Background(), s, transfer.ExplicitVRLittleEndian, tc.opts, 20)
			if _, err := r.Next(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if r.Position() != 12 || s.bytesRead != 12 {
				t.Fatalf("body touched: pos/read %d/%d", r.Position(), s.bytesRead)
			}
		})
	}
	child := pruningWire(binary.LittleEndian, core.NewTag(8, 0x1140), "SQ", 8, nil)
	body := append(pruningControl(binary.LittleEndian, 0xe000, uint32(len(child))), child...)
	wire := pruningWire(binary.LittleEndian, seq, "SQ", uint32(len(body)), body)
	r, err := NewSelectiveReader(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{}, SelectiveReaderOptions{Select: func(_ context.Context, p validation.Path, h core.ElementHeader, _ int64) (SelectiveDisposition, error) {
		if len(p) > 1 && h.VR == core.VRSQ {
			return SelectiveSkipSequence, nil
		}
		return SelectiveMaterialize, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Next(); !errors.Is(err, ErrContainerOverrun) || r.Position() != 32 {
		t.Fatalf("parent fence: position %d, error %v", r.Position(), err)
	}
}

func TestSelectiveSequenceUndefinedGrammarAndCP246(t *testing.T) {
	seq, tail := core.NewTag(8, 0x1111), core.NewTag(0x10, 0x10)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			// Undefined UN always switches its complete Item grammar to implicit LE.
			payload := pruningControl(binary.LittleEndian, 0xe0dd, 0) // delimiter bytes inside an OB value
			item := pruningControl(binary.LittleEndian, 0xe000, 0xffffffff)
			item = append(item, pruningWire(binary.LittleEndian, core.NewTag(0x11, 0x1005), "", uint32(len(payload)), payload)...)
			item = append(item, pruningControl(binary.LittleEndian, 0xe00d, 0)...)
			item = append(item, pruningControl(binary.LittleEndian, 0xe0dd, 0)...)
			wire := pruningWire(syntax.ByteOrder, seq, "UN", 0xffffffff, item)
			wire = append(wire, pruningWire(syntax.ByteOrder, tail, "PN", 2, []byte("AB"))...)
			r := pruningReader(t, context.Background(), bytes.NewReader(wire), syntax, ReaderOptions{MaxSequenceDepth: 4}, 20)
			if _, err := r.Next(); err != nil {
				t.Fatal(err)
			}
			tok, err := r.Next()
			if err != nil || tok.Header.Tag != tail || !bytes.Equal([]byte(tok.Element.Value.(core.RawValue)), []byte("AB")) {
				t.Fatalf("tail %+v %v", tok, err)
			}
			if len(r.seqDelimiters) != 0 || len(r.selective.frames) != 0 {
				t.Fatal("hidden frame leaked")
			}
		})
	}
	for _, tc := range []struct {
		name string
		body []byte
		want error
	}{
		{"wrong item end", append(pruningControl(binary.LittleEndian, 0xe000, 0xffffffff), pruningControl(binary.LittleEndian, 0xe0dd, 0)...), ErrUnexpectedSequenceDelimiter},
		{"nonzero delimiter", pruningControl(binary.LittleEndian, 0xe0dd, 2), ErrUnexpectedDelimiterLength},
		{"truncated", pruningControl(binary.LittleEndian, 0xe000, 0xffffffff), io.ErrUnexpectedEOF},
		{"primitive outside item", pruningWire(binary.LittleEndian, tail, "PN", 2, []byte("AB")), ErrUnexpectedSequenceControlTag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pruningReader(t, context.Background(), bytes.NewReader(pruningWire(binary.LittleEndian, seq, "SQ", 0xffffffff, tc.body)), transfer.ExplicitVRLittleEndian, ReaderOptions{}, 20)
			if _, err := r.Next(); !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSelectiveSequenceHiddenTokensDepthAndFragments(t *testing.T) {
	seq := core.NewTag(8, 0x1111)
	body := []byte{}
	for i := 0; i < 20; i++ {
		body = append(body, pruningControl(binary.LittleEndian, 0xe000, 0xffffffff)...)
		body = append(body, pruningControl(binary.LittleEndian, 0xe00d, 0)...)
	}
	body = append(body, pruningControl(binary.LittleEndian, 0xe0dd, 0)...)
	wire := pruningWire(binary.LittleEndian, seq, "SQ", 0xffffffff, body)
	for _, limit := range []int{41, 42} {
		r := pruningReader(t, context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{}, limit)
		_, err := r.Next()
		if limit == 41 && !errors.Is(err, ErrMaxSelectiveTokensExceeded) || limit == 42 && err != nil {
			t.Fatalf("budget %d: %v", limit, err)
		}
		if limit == 42 {
			if _, err = r.Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("EOF charged: %v", err)
			}
		}
	}
	r := pruningReader(t, context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{MaxSequenceDepth: 1}, 100)
	if _, err := r.Next(); !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("depth: %v", err)
	}
	if cap(r.seqDelimiters) > 2 || cap(r.selective.frames) > 2 {
		t.Fatalf("capacity beyond admitted depth: %d/%d", cap(r.seqDelimiters), cap(r.selective.frames))
	}
	pixel := pruningControl(binary.LittleEndian, 0xe000, 0)
	for i := 0; i < 2; i++ {
		pixel = append(pixel, pruningControl(binary.LittleEndian, 0xe000, 2)...)
		pixel = append(pixel, 1, 2)
	}
	pixel = append(pixel, pruningControl(binary.LittleEndian, 0xe0dd, 0)...)
	item := pruningControl(binary.LittleEndian, 0xe000, 0xffffffff)
	item = append(item, pruningWire(binary.LittleEndian, core.TagPixelData, "OB", 0xffffffff, pixel)...)
	item = append(item, pruningControl(binary.LittleEndian, 0xe00d, 0)...)
	item = append(item, pruningControl(binary.LittleEndian, 0xe0dd, 0)...)
	r = pruningReader(t, context.Background(), bytes.NewReader(pruningWire(binary.LittleEndian, seq, "SQ", 0xffffffff, item)), transfer.ExplicitVRLittleEndian, ReaderOptions{MaxFragments: 1}, 30)
	if _, err := r.Next(); !errors.Is(err, ErrMaxFragmentsExceeded) {
		t.Fatalf("fragments: %v", err)
	}
}

func TestSelectiveSequencePrivateScopeIsolation(t *testing.T) {
	seq, creator, data := core.NewTag(8, 0x1111), core.NewTag(0x11, 0x10), core.NewTag(0x11, 0x1001)
	item := pruningControl(binary.LittleEndian, 0xe000, 0xffffffff)
	item = append(item, pruningWire(binary.LittleEndian, creator, "", 6, []byte("OTHER "))...)
	item = append(item, pruningWire(binary.LittleEndian, data, "", 2, []byte("AB"))...)
	item = append(item, pruningControl(binary.LittleEndian, 0xe00d, 0)...)
	item = append(item, pruningControl(binary.LittleEndian, 0xe0dd, 0)...)
	wire := pruningWire(binary.LittleEndian, creator, "", 4, []byte("ACME"))
	wire = append(wire, pruningWire(binary.LittleEndian, seq, "", 0xffffffff, item)...)
	wire = append(wire, pruningWire(binary.LittleEndian, data, "", 2, []byte{7, 0})...)
	r := pruningReader(t, context.Background(), bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, ReaderOptions{Dictionary: privateTestDictionary(t), MaxPrivateCreators: 1}, 30)
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	tok, err := r.Next()
	if err != nil || tok.Header.VR != core.VRUS {
		t.Fatalf("root scope %+v %v", tok, err)
	}
	if diag, trunc := r.PrivateDiagnostics(); len(diag) != 0 || trunc {
		t.Fatalf("diagnostics %v, truncated %v", diag, trunc)
	}
	// A hidden creator must not resolve an unreserved root tag.
	r = pruningReader(t, context.Background(), bytes.NewReader(append(pruningWire(binary.LittleEndian, seq, "", 0xffffffff, item), pruningWire(binary.LittleEndian, data, "", 2, []byte{7, 0})...)), transfer.ImplicitVRLittleEndian, ReaderOptions{Dictionary: privateTestDictionary(t)}, 30)
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	tok, err = r.Next()
	if err != nil || tok.Header.VR != core.VRUN {
		t.Fatalf("unreserved %+v %v", tok, err)
	}
	if diag, _ := r.PrivateDiagnostics(); len(diag) != 1 || !errors.Is(diag[0].Err, dictionary.ErrPrivateCreatorMissing) {
		t.Fatalf("missing root creator %v", diag)
	}
}

type pruningCancelReader struct {
	r      *bytes.Reader
	cancel context.CancelFunc
	after  int64
}

func (r *pruningCancelReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	position, _ := r.r.Seek(0, io.SeekCurrent)
	if position >= r.after {
		r.cancel()
	}
	return n, err
}

func TestSelectiveSequenceChunkCancellationAndDisposition(t *testing.T) {
	wire := pruningWire(binary.LittleEndian, core.NewTag(8, 0x1111), "SQ", 100_000, make([]byte, 100_000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &pruningCancelReader{r: bytes.NewReader(wire), cancel: cancel, after: 12 + definedValueReadChunkSize}
	r := pruningReader(t, ctx, s, transfer.ExplicitVRLittleEndian, ReaderOptions{}, 10)
	if _, err := r.Next(); !errors.Is(err, context.Canceled) || r.Position() >= int64(len(wire)) {
		t.Fatalf("cancel at %d: %v", r.Position(), err)
	}
	primitive := pruningWire(binary.LittleEndian, core.NewTag(0x10, 0x10), "PN", 2, []byte("AB"))
	r, err := NewSelectiveReader(context.Background(), bytes.NewReader(primitive), transfer.ExplicitVRLittleEndian, ReaderOptions{}, SelectiveReaderOptions{Select: func(context.Context, validation.Path, core.ElementHeader, int64) (SelectiveDisposition, error) {
		return SelectiveSkipSequence, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Next(); !errors.Is(err, ErrSelectiveDisposition) {
		t.Fatalf("primitive disposition %v", err)
	}
}

func TestSelectiveSequenceUnlimitedCancellation(t *testing.T) {
	for _, budget := range []int{0, 10} {
		t.Run(fmt.Sprintf("hidden-header-budget-%d", budget), func(t *testing.T) {
			wire := pruningWire(binary.LittleEndian, core.NewTag(8, 0x1111), "SQ", 0xffffffff,
				pruningControl(binary.LittleEndian, 0xe0dd, 0))
			before := append([]byte(nil), wire...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &pruningCancelReader{r: bytes.NewReader(wire), cancel: cancel, after: int64(len(wire))}
			r := pruningReader(t, ctx, source, transfer.ExplicitVRLittleEndian, ReaderOptions{}, budget)
			if _, err := r.Next(); !errors.Is(err, context.Canceled) || r.Position() != int64(len(wire)) {
				t.Fatalf("cancel delivered with hidden closing header at %d: %v", r.Position(), err)
			}
			if !bytes.Equal(before, wire) {
				t.Fatal("source mutated")
			}
		})
		t.Run(fmt.Sprintf("synthetic-close-budget-%d", budget), func(t *testing.T) {
			wire := pruningWire(binary.LittleEndian, core.NewTag(8, 0x1111), "SQ", 8,
				pruningControl(binary.LittleEndian, 0xe000, 0))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, err := NewSelectiveReader(ctx, bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{},
				SelectiveReaderOptions{MaxTokens: budget, Select: func(context.Context, validation.Path, core.ElementHeader, int64) (SelectiveDisposition, error) {
					return SelectiveMaterialize, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range []TokenKind{TokenStartSequence, TokenStartItem} {
				if tok, err := r.Next(); err != nil || tok.Kind != kind {
					t.Fatalf("start %s: %+v/%v", kind, tok, err)
				}
			}
			cancel()
			if _, err := r.Next(); !errors.Is(err, context.Canceled) {
				t.Fatalf("synthetic close published after cancel: %v", err)
			}
		})
	}
}
