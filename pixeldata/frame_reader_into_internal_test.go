package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type intoReadAt struct {
	data   []byte
	calls  int
	fail   bool
	cancel context.CancelFunc
}

func (s *intoReadAt) ReadAt(p []byte, offset int64) (int, error) {
	s.calls++
	n := len(p)
	if s.fail {
		n = 3
	}
	copy(p[:n], s.data[int(offset):int(offset)+n])
	if s.cancel != nil {
		s.cancel()
	}
	if s.fail {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}
func newIntoNativeTest(t *testing.T, rows, cols int, order binary.ByteOrder) (*FrameReader, *intoReadAt) {
	t.Helper()
	count := "1"
	data := make([]byte, rows*cols*2)
	for i := 0; i < len(data); i += 2 {
		order.PutUint16(data[i:], uint16(i/2))
	}
	// Constructor validates the ordinary complete native data before injecting a
	// ReaderAt that models an I/O failure/cancellation during the later read.
	elements := pixelMetadataElements(uint16(rows), uint16(cols), 1, 16, 16, 15, 0, &count, core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("MONOCHROME2")), core.NewRawElement(core.TagPixelData, core.VROW, data))
	obj := object.FromElements(elements, nil)
	obj.SetValueByteOrder(binary.LittleEndian)
	r := newDirectFrameReader(t, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}, FrameReaderOptions{})
	source := &intoReadAt{data: data}
	r.native = source
	r.nativeOrder = order
	return r, source
}
func TestFrameReaderIntoPartialSourceAndCancellation(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run("source-failure", func(t *testing.T) {
			r, src := newIntoNativeTest(t, 300, 300, order)
			src.fail = true
			dst := bytes.Repeat([]byte{0xf1}, 180011)
			before := bytes.Clone(dst)
			got, err := r.FrameInto(nil, 0, dst)
			if !errors.Is(err, io.ErrUnexpectedEOF) || got.Written != 3 || src.calls != 1 || !bytes.Equal(dst[3:], before[3:]) {
				t.Fatalf("%+v %v calls%d", got, err, src.calls)
			}
		})
		t.Run("cancel-after-chunk", func(t *testing.T) {
			r, src := newIntoNativeTest(t, 300, 300, order)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			src.cancel = cancel
			dst := bytes.Repeat([]byte{0xf2}, 180011)
			before := bytes.Clone(dst)
			got, err := r.FrameInto(ctx, 0, dst)
			if !errors.Is(err, context.Canceled) || got.Written != 65536 || src.calls != 1 || !bytes.Equal(dst[got.Written:], before[got.Written:]) {
				t.Fatalf("%+v %v calls%d", got, err, src.calls)
			}
			for at := 0; at < got.Written; at += 2 {
				if binary.LittleEndian.Uint16(dst[at:]) != uint16(at/2) {
					t.Fatalf("sample%d not little-endian", at/2)
				}
			}
		})
		t.Run("region-cancel-after-row", func(t *testing.T) {
			r, src := newIntoNativeTest(t, 20, 20, order)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			src.cancel = cancel
			dst := bytes.Repeat([]byte{0xf3}, 200)
			before := bytes.Clone(dst)
			got, err := r.RegionInto(ctx, 0, image.Rect(2, 3, 10, 8), dst)
			if !errors.Is(err, context.Canceled) || got.Written != 16 || src.calls != 1 || !bytes.Equal(dst[16:], before[16:]) {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

type intoTestCodec struct {
	metadata func(context.Context, Metadata, image.Rectangle) (Metadata, error)
	decode   func(context.Context, []byte, Metadata, image.Rectangle, []byte) (int, []string, error)
}

func (c intoTestCodec) Decode(p PixelData, o *object.Object) (Frames, error) {
	return directCopyFrames(context.Background(), p, o)
}
func (c intoTestCodec) BufferMetadata(ctx context.Context, m Metadata, request FrameBufferRequest) (Metadata, error) {
	r := request.Region
	if c.metadata != nil {
		return c.metadata(ctx, m, r)
	}
	m.Rows, m.Columns = uint16(r.Dy()), uint16(r.Dx())
	return m, nil
}
func (c intoTestCodec) DecodeFrameInto(ctx context.Context, b []byte, m Metadata, request FrameBufferRequest, d []byte) (int, []string, error) {
	return c.decode(ctx, b, m, request.Region, d)
}
func intoCodecReader(t *testing.T, c Codec, limits DecompressLimits) *FrameReader {
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, c); err != nil {
		t.Fatal(err)
	}
	return newDirectFrameReader(t, frameReaderDirectFile(1), FrameReaderOptions{Registry: registry, Limits: limits})
}

func TestFrameReaderIntoRLEReusesPrivateInputWithoutBlockingConcurrentDecode(t *testing.T) {
	type pauseKey struct{}
	entered, resume := make(chan struct{}), make(chan struct{})
	codec := intoTestCodec{decode: func(ctx context.Context, b []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
		if len(b) != 4 || cap(b) != len(b) {
			return 0, nil, fmt.Errorf("unexpected encoded extent %d/%d", len(b), cap(b))
		}
		if ctx.Value(pauseKey{}) != nil {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			}
		}
		n := copy(d, b)
		b[0] = 0xff
		return n, nil, nil
	}}
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, codec); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{Registry: registry})
	decode := func(ctx context.Context, frame int) error {
		var dst [4]byte
		result, err := r.FrameInto(ctx, frame, dst[:])
		if err != nil || result.Written != len(dst) || dst != [4]byte{byte(frame), 2, 3, 4} {
			return fmt.Errorf("frame %d = %v, result %+v, err %v", frame, dst, result, err)
		}
		return nil
	}
	if err := decode(nil, 0); err != nil {
		t.Fatal(err)
	}
	if len(r.rleInput) != 4 {
		t.Fatal("no private input retained")
	}
	retained := &r.rleInput[0]
	if err := decode(nil, 1); err != nil {
		t.Fatal(err)
	}
	if &r.rleInput[0] != retained {
		t.Fatal("sequential decode replaced the reusable allocation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- decode(context.WithValue(ctx, pauseKey{}, true), 0) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first decode never acquired the input")
	}
	go func() { second <- decode(ctx, 1) }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent decode waited for the retained input")
	}
	close(resume)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if raw, err := r.RawFrame(nil, 1); err != nil || !bytes.Equal(raw, []byte{1, 2, 3, 4}) {
		t.Fatalf("codec changed source frame: %v, %v", raw, err)
	}
	if err := r.Close(); err != nil || r.rleInput != nil {
		t.Fatalf("Close retained private input: %v", err)
	}
}

func TestFrameReaderIntoCodecContractsAndLimits(t *testing.T) {
	ctxKey := struct{}{}
	codec := intoTestCodec{decode: func(ctx context.Context, b []byte, m Metadata, r image.Rectangle, d []byte) (int, []string, error) {
		if ctx.Value(ctxKey) != "preserved" {
			t.Fatal("context was replaced")
		}
		if len(d) != 4 || cap(d) != 4 {
			t.Fatalf("unbounded destination %d/%d", len(d), cap(d))
		}
		return copy(d, b), []string{"test-diagnostic"}, nil
	}}
	r := intoCodecReader(t, codec, DecompressLimits{})
	dst := bytes.Repeat([]byte{0x8d}, 17)
	got, err := r.FrameInto(context.WithValue(context.Background(), ctxKey, "preserved"), 0, dst)
	if err != nil || got.Written != 4 || got.Layout.Mode != BufferCodecDirect || len(got.Diagnostics) != 1 || !bytes.Equal(dst[4:], bytes.Repeat([]byte{0x8d}, 13)) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		codec  intoTestCodec
		limits DecompressLimits
		want   error
	}{
		{"bad-layout", intoTestCodec{metadata: func(_ context.Context, m Metadata, _ image.Rectangle) (Metadata, error) { m.Rows = 3; return m, nil }}, DecompressLimits{}, ErrFrameLayout},
		{"limit", codec, DecompressLimits{MaxNativeBytes: 2}, ErrDecompressResourceLimit},
		{"short-output", intoTestCodec{decode: func(_ context.Context, _ []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
			d[0] = 5
			return 1, nil, nil
		}}, DecompressLimits{}, ErrPixelDataSizeMismatch},
		{"invalid-extent", intoTestCodec{decode: func(_ context.Context, _ []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
			d[0] = 5
			return -1, nil, nil
		}}, DecompressLimits{}, ErrPixelDataSizeMismatch},
		{"partial-error", intoTestCodec{decode: func(_ context.Context, _ []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
			d[1] = 5
			return 2, nil, io.ErrUnexpectedEOF
		}}, DecompressLimits{}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := intoCodecReader(t, tc.codec, tc.limits)
			dst := bytes.Repeat([]byte{0xa9}, 17)
			before := bytes.Clone(dst)
			got, err := r.FrameInto(nil, 0, dst)
			if !errors.Is(err, tc.want) || !bytes.Equal(dst[got.Written:], before[got.Written:]) {
				t.Fatalf("%+v %v", got, err)
			}
			if tc.name == "bad-layout" || tc.name == "limit" {
				if got.Written != 0 || !bytes.Equal(dst, before) {
					t.Fatal("preflight wrote destination")
				}
			}
		})
	}
}

func TestFrameReaderIntoPreservesCodecCancellation(t *testing.T) {
	codec := intoTestCodec{decode: func(_ context.Context, _ []byte, _ Metadata, _ image.Rectangle, dst []byte) (int, []string, error) {
		dst[0] = 5
		return 1, nil, context.Canceled
	}}
	r := intoCodecReader(t, codec, DecompressLimits{})
	dst := bytes.Repeat([]byte{0xac}, 8)
	result, err := r.FrameInto(nil, 0, dst)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCodecDecodeFailed) {
		t.Fatalf("cancellation must not be reported as codec failure: %v", err)
	}
	if result.Written != 1 || !bytes.Equal(dst, []byte{5, 0xac, 0xac, 0xac, 0xac, 0xac, 0xac, 0xac}) {
		t.Fatalf("partial cancellation result = %+v, bytes %v", result, dst)
	}
}

type intoUnsupportedRegional struct{ intoTestCodec }

func (c intoUnsupportedRegional) DecodeFrameRegion(context.Context, []byte, Metadata, image.Rectangle) (Frames, error) {
	return Frames{}, ErrRegionUnsupported
}
func TestFrameReaderIntoDeclaredFallback(t *testing.T) {
	codec := intoUnsupportedRegional{intoTestCodec{metadata: func(context.Context, Metadata, image.Rectangle) (Metadata, error) {
		return Metadata{}, ErrRegionUnsupported
	}}}
	r := intoCodecReader(t, codec, DecompressLimits{})
	dst := bytes.Repeat([]byte{0xac}, 8)
	got, err := r.RegionInto(nil, 0, image.Rect(0, 0, 1, 2), dst)
	if err != nil || got.Layout.Mode != BufferFrameRegionCopy || got.Written != 2 || !bytes.Equal(dst[:2], []byte{0, 3}) || !bytes.Equal(dst[2:], bytes.Repeat([]byte{0xac}, 6)) {
		t.Fatalf("%+v %v bytes%v", got, err, dst)
	}
	// A legacy regional decoder remains useful without the new direct contract.
	legacy := intoLegacyRegional{frameReaderDirectCodec: frameReaderDirectCodec(directCopyFrames)}
	r = intoCodecReader(t, legacy, DecompressLimits{})
	dst = bytes.Repeat([]byte{0xac}, 8)
	got, err = r.RegionInto(nil, 0, image.Rect(0, 0, 1, 2), dst)
	if err != nil || got.Layout.Mode != BufferRegionCopy || got.Written != 2 || !bytes.Equal(dst[:2], []byte{0, 3}) || !bytes.Equal(dst[2:], bytes.Repeat([]byte{0xac}, 6)) {
		t.Fatalf("regional fallback %+v %v", got, err)
	}

}

type intoRecordedRead struct {
	*bytes.Reader
	offsets []int64
	lengths []int
}

func (r *intoRecordedRead) ReadAt(p []byte, off int64) (int, error) {
	r.offsets = append(r.offsets, off)
	r.lengths = append(r.lengths, len(p))
	return r.Reader.ReadAt(p, off)
}
func TestFrameReaderIntoNativeRegionReadsOnlyRanges(t *testing.T) {
	count := "1"
	for _, planar := range []uint16{0, 1} {
		elements := pixelMetadataElements(4, 6, 3, 16, 16, 15, 0, &count,
			core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("RGB ")),
			core.NewRawElement(tagPlanarConfiguration, core.VRUS, binary.LittleEndian.AppendUint16(nil, planar)),
			core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 4*6*3*2)))
		r := newDirectFrameReader(t, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian}, FrameReaderOptions{})
		source := &intoRecordedRead{Reader: bytes.NewReader(make([]byte, 4*6*3*2))}
		r.native = source
		dst := make([]byte, 3*2*3*2)
		result, err := r.RegionInto(nil, 0, image.Rect(1, 1, 4, 3), dst)
		if err != nil || result.Written != len(dst) {
			t.Fatalf("%+v %v", result, err)
		}
		planes, perPixel := 1, 3
		if planar == 1 {
			planes, perPixel = 3, 1
		}
		at := 0
		for p := 0; p < planes; p++ {
			for y := 1; y < 3; y++ {
				offset := int64(((p*4+y)*6 + 1) * perPixel * 2)
				if at >= len(source.offsets) || source.offsets[at] != offset || source.lengths[at] != 3*perPixel*2 {
					t.Fatalf("planar%d read%d offsets%v lengths%v", planar, at, source.offsets, source.lengths)
				}
				at++
			}
		}
		if at != len(source.offsets) {
			t.Fatal("read outside requested rows")
		}
	}
}

func TestFrameReaderIntoRegistryLayoutChanges(t *testing.T) {
	registry := NewMemoryRegistry()
	first := intoTestCodec{decode: func(_ context.Context, b []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
		return copy(d, b), nil, nil
	}}
	if err := registry.RegisterCodec(transfer.RLELossless.UID, first); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(1), FrameReaderOptions{Registry: registry})
	queried, err := r.FrameBufferLayout(nil, 0)
	if err != nil || queried.Bytes != 4 {
		t.Fatalf("%+v %v", queried, err)
	}
	second := intoTestCodec{metadata: func(_ context.Context, m Metadata, _ image.Rectangle) (Metadata, error) {
		m.SamplesPerPixel = 3
		m.PhotometricInterpretation = "RGB"
		m.PlanarConfiguration = 0
		m.PlanarConfigurationPresent = true
		return m, nil
	}, decode: func(_ context.Context, _ []byte, _ Metadata, _ image.Rectangle, d []byte) (int, []string, error) {
		for at := range d {
			d[at] = byte(at)
		}
		return len(d), nil, nil
	}}
	if err := registry.RegisterCodec(transfer.RLELossless.UID, second); err != nil {
		t.Fatal(err)
	}
	dst := bytes.Repeat([]byte{0xab}, 17)
	before := bytes.Clone(dst)
	result, err := r.FrameInto(nil, 0, dst[:int(queried.Bytes)])
	var size *BufferSizeError
	if !errors.As(err, &size) || size.Required != 12 || result.Written != 0 || !bytes.Equal(dst, before) {
		t.Fatalf("stale layout %+v %v", result, err)
	}
	result, err = r.FrameInto(nil, 0, dst)
	if err != nil || result.Written != 12 || result.Layout.Metadata.SamplesPerPixel != 3 || !bytes.Equal(dst[12:], before[12:]) {
		t.Fatalf("new layout %+v %v", result, err)
	}
}

type intoLegacyRegional struct{ frameReaderDirectCodec }

func (c intoLegacyRegional) DecodeFrameRegion(_ context.Context, b []byte, _ Metadata, rect image.Rectangle) (Frames, error) {
	return Frames{Rows: rect.Dy(), Columns: rect.Dx(), Data: [][]byte{{b[0], b[2]}}}, nil
}

func TestFrameReaderIntoNativeAdmissionPolicy(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, tc := range []struct {
			name      string
			limits    DecompressLimits
			binaryBad bool
		}{{"native-bytes", DecompressLimits{MaxNativeBytes: 3}, false}, {"input-bytes", DecompressLimits{MaxInputBytes: 3}, false}, {"pixels", DecompressLimits{MaxPixels: 1}, false}, {"invalid-limits", DecompressLimits{MaxNativeBytes: -1}, false}, {"malformed-binary-metadata", DecompressLimits{}, true}} {
			count := "1"
			elements := pixelMetadataElementsWithOrder(order, 1, 2, 1, 16, 16, 15, 0, &count, core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("MONOCHROME2")), core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}))
			if tc.binaryBad {
				elements = append(elements, core.NewRawElement(core.NewTag(0x7776, 0x0001), core.VRUS, []byte{1}))
			}
			syntax := transfer.ExplicitVRLittleEndian
			if order == binary.BigEndian {
				syntax = transfer.ExplicitVRBigEndian
			}
			file := &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: syntax}
			file.Dataset.SetValueByteOrder(order)
			for _, regional := range []bool{false, true} {
				old := newDirectFrameReader(t, file, FrameReaderOptions{Limits: tc.limits})
				current := newDirectFrameReader(t, file, FrameReaderOptions{Limits: tc.limits})
				source := &intoRecordedRead{Reader: bytes.NewReader([]byte{1, 2, 3, 4})}
				current.native = source
				dst := bytes.Repeat([]byte{0xfa}, 9)
				before := bytes.Clone(dst)
				var want []byte
				var metadata Metadata
				var oldErr, errorInto error
				var result BufferResult
				if regional {
					want, metadata, oldErr = old.Region(nil, 0, image.Rect(0, 0, 2, 1))
					result, errorInto = current.RegionInto(nil, 0, image.Rect(0, 0, 2, 1), dst)
				} else {
					want, metadata, oldErr = old.Frame(nil, 0)
					result, errorInto = current.FrameInto(nil, 0, dst)
				}
				if !tc.binaryBad || order == binary.BigEndian && !regional {
					expected := ErrDecompressResourceLimit
					if tc.name == "invalid-limits" {
						expected = ErrInvalidDecompressLimits
					}
					if tc.binaryBad {
						expected = ErrPixelDataSizeMismatch
					}
					if !errors.Is(oldErr, expected) || !errors.Is(errorInto, expected) || result.Written != 0 || len(source.offsets) != 0 || !bytes.Equal(dst, before) {
						t.Fatalf("%s order%v regional%v: old%v new%v result%+v payload%v", tc.name, order, regional, oldErr, errorInto, result, source.offsets)
					}
				} else if oldErr != nil || errorInto != nil || result.Written != 4 || result.Layout.Metadata != metadata || !bytes.Equal(dst[:4], want) || !bytes.Equal(dst[4:], before[4:]) {
					t.Fatalf("%s order%v regional%v old%v new%v result%+v", tc.name, order, regional, oldErr, errorInto, result)
				}
			}
		}
	}
}
