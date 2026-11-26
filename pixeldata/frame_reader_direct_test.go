package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type frameReaderDirectCodec func(context.Context, PixelData, *object.Object) (Frames, error)

func (f frameReaderDirectCodec) Decode(PixelData, *object.Object) (Frames, error) {
	return Frames{}, errors.New("legacy Decode called instead of ContextCodec")
}

func (f frameReaderDirectCodec) DecodeContext(ctx context.Context, pixel PixelData, obj *object.Object) (Frames, error) {
	return f(ctx, pixel, obj)
}

func frameReaderDirectFile(frames int) *object.File {
	count := strconv.Itoa(frames)
	fragments := make([][]byte, frames)
	for i := range fragments {
		fragments[i] = []byte{byte(i), 2, 3, 4}
	}
	elements := pixelMetadataElements(2, 2, 1, 8, 8, 7, 0, &count,
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
		dicomtest.NewStringElement(core.NewTag(0x0009, 0x0010), core.VRLO, "FRAME_TEST"),
		dicomtest.NewStringElement(core.NewTag(0x0009, 0x1001), core.VRLO, "custom metadata"),
		dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1115), core.DataSet{Elements: []core.Element{
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.3.4"),
		}}),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragments...),
	)
	return &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.RLELossless}
}

func newDirectFrameReader(t *testing.T, file *object.File, options FrameReaderOptions) *FrameReader {
	t.Helper()
	r, err := NewFrameReader(context.Background(), file, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func directCopyFrames(_ context.Context, pixel PixelData, _ *object.Object) (Frames, error) {
	return Frames{Rows: 2, Columns: 2, Data: [][]byte{bytes.Clone(pixel.Sequence.Fragments[0])}, Diagnostics: []string{"frame diagnostic"}}, nil
}

func TestFrameReaderDirectRegistryContextAndOwnership(t *testing.T) {
	file := frameReaderDirectFile(3)
	before := file.Dataset.Elements()
	registry := NewMemoryRegistry()
	r := newDirectFrameReader(t, file, FrameReaderOptions{Registry: registry})
	if _, _, err := r.Frame(nil, 0); !errors.Is(err, ErrCodecNotFound) {
		t.Fatalf("unregistered codec: %v", err)
	}
	codec := frameReaderDirectCodec(func(ctx context.Context, pixel PixelData, obj *object.Object) (Frames, error) {
		if !decoderecovery.From(ctx).RLESegmentOverrun || JPEGColorConversionFrom(ctx) != JPEGColorNever {
			return Frames{}, errors.New("context options lost")
		}
		if got, _ := obj.GetString(tagNumberOfFrames); got != "1" {
			return Frames{}, fmt.Errorf("NumberOfFrames = %q", got)
		}
		if got, _ := obj.GetString(core.NewTag(0x0009, 0x1001)); got != "custom metadata" {
			return Frames{}, errors.New("custom metadata lost")
		}
		items, ok := obj.GetSequence(core.NewTag(0x0008, 0x1115))
		if !ok || len(items) != 1 {
			return Frames{}, errors.New("sequence lost")
		}
		if got, _ := items[0].GetString(core.NewTag(0x0008, 0x1155)); got != "1.2.3.4" {
			return Frames{}, errors.New("sequence item lost")
		}
		view, err := ExtractView(obj)
		if err != nil || !bytes.Equal(view.Sequence.Fragments[0], pixel.Sequence.Fragments[0]) {
			return Frames{}, errors.New("Object and PixelData disagree")
		}
		return directCopyFrames(ctx, pixel, obj)
	})
	if err := registry.RegisterCodec(transfer.RLELossless.UID, codec); err != nil {
		t.Fatal(err)
	}
	ctx := WithJPEGColorConversion(DecodeContextWithRecovery(context.Background(), DecodeRecovery{RLESegmentOverrun: true}), JPEGColorNever)
	for i := 0; i < 48; i++ {
		got, metadata, err := r.Frame(ctx, i%3)
		if err != nil || !bytes.Equal(got, []byte{byte(i % 3), 2, 3, 4}) || metadata.NumberOfFrames != 1 {
			t.Fatalf("frame %d = %v, %#v, %v", i, got, metadata, err)
		}
		got[0] = 255
	}
	if !reflect.DeepEqual(before, file.Dataset.Elements()) {
		t.Fatal("caller-owned dataset changed")
	}
	// Replacing the registered codec after the first decode remains observable.
	wantErr := errors.New("replacement failure")
	if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(func(context.Context, PixelData, *object.Object) (Frames, error) {
		return Frames{}, wantErr
	})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Frame(ctx, 1); !errors.Is(err, wantErr) {
		t.Fatalf("replacement not used: %v", err)
	}
}

func TestFrameReaderDirectConcurrentObjectReads(t *testing.T) {
	const workers = 12
	registry := NewMemoryRegistry()
	entered := make(chan struct{}, workers)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	active := map[*object.Object]bool{}
	codec := frameReaderDirectCodec(func(ctx context.Context, pixel PixelData, obj *object.Object) (Frames, error) {
		mu.Lock()
		duplicate := active[obj]
		active[obj] = true
		mu.Unlock()
		defer func() { mu.Lock(); delete(active, obj); mu.Unlock() }()
		entered <- struct{}{}
		<-release
		if duplicate {
			return Frames{}, errors.New("concurrent decode shared an Object")
		}
		// These are reads in the Codec contract but may populate Object caches.
		for i := 0; i < 3; i++ {
			if _, ok := obj.GetSequence(core.NewTag(0x0008, 0x1115)); !ok {
				return Frames{}, errors.New("missing sequence")
			}
			_ = obj.Elements()
		}
		return directCopyFrames(ctx, pixel, obj)
	})
	if err := registry.RegisterCodec(transfer.RLELossless.UID, codec); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(workers), FrameReaderOptions{Registry: registry})
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			got, _, err := r.Frame(context.Background(), i)
			if err == nil && !bytes.Equal(got, []byte{byte(i), 2, 3, 4}) {
				err = fmt.Errorf("frame %d changed to %v", i, got)
			}
			errs <- err
		}(i)
	}
	for i := 0; i < workers; i++ {
		select {
		case <-entered:
		case err := <-errs:
			t.Fatalf("decode returned before the codec barrier: %v", err)
		}
	}
	unblock()
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

type directCountedSource struct {
	encapsulated.Source
	reads atomic.Int64
}

func (s *directCountedSource) ReadFragmentAt(ctx context.Context, i int, p []byte, offset int64) (int, error) {
	s.reads.Add(1)
	return s.Source.ReadFragmentAt(ctx, i, p, offset)
}

func TestFrameReaderDirectLimitsBeforePayloadRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits DecompressLimits
		large  bool
	}{
		{"MaxInputBytes", DecompressLimits{MaxInputBytes: 3}, false},
		{"MaxPixels", DecompressLimits{MaxPixels: 3}, false},
		{"MaxNativeBytes", DecompressLimits{MaxNativeBytes: 3}, false},
		{"MaxExpansionRatio", DecompressLimits{MaxExpansionRatio: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := frameReaderDirectFile(2)
			if tc.large {
				file.Dataset.Put(uint16RawElement(tagRows, 4))
				file.Dataset.Put(uint16RawElement(tagColumns, 4))
			}
			r := newDirectFrameReader(t, file, FrameReaderOptions{Limits: tc.limits})
			source := &directCountedSource{Source: r.fragments}
			r.fragments = source
			_, _, err := r.Frame(nil, 0)
			var limit *DecompressResourceLimitError
			if !errors.As(err, &limit) || limit.Limit != tc.name || source.reads.Load() != 0 {
				t.Fatalf("limit = %v, payload reads = %d", err, source.reads.Load())
			}
		})
	}
}

func TestFrameReaderDirectInvalidLimitsAndNilRegistry(t *testing.T) {
	file := frameReaderDirectFile(2)
	r := newDirectFrameReader(t, file, FrameReaderOptions{Limits: DecompressLimits{MaxNativeBytes: -1}})
	if _, _, err := r.Frame(nil, 0); !errors.Is(err, ErrInvalidDecompressLimits) {
		t.Fatalf("negative limit: %v", err)
	}
	var registry *MemoryRegistry
	r = newDirectFrameReader(t, file, FrameReaderOptions{Registry: registry})
	if _, _, err := r.Frame(nil, 0); !errors.Is(err, ErrCodecRegistryNil) {
		t.Fatalf("typed nil registry: %v", err)
	}
	registry = NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(directCopyFrames)); err != nil {
		t.Fatal(err)
	}
	// As in the old path, the per-frame MaxFrames is forced to one; it is
	// independent of both the series frame count and the supplied value.
	r = newDirectFrameReader(t, file, FrameReaderOptions{Registry: registry, Limits: DecompressLimits{MaxFrames: -1}})
	if _, _, err := r.Frame(nil, 1); err != nil {
		t.Fatalf("one-frame limit override: %v", err)
	}
}

func TestFrameReaderDirectCancellationAndMalformedOutputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames Frames
	}{
		{"geometry", Frames{Rows: 1, Columns: 4, Data: [][]byte{{1, 2, 3, 4}}}},
		{"count", Frames{Rows: 2, Columns: 2, Data: [][]byte{{1, 2, 3, 4}, {1, 2, 3, 4}}}},
		{"size", Frames{Rows: 2, Columns: 2, Data: [][]byte{{1, 2, 3}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewMemoryRegistry()
			if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(func(context.Context, PixelData, *object.Object) (Frames, error) { return tc.frames, nil })); err != nil {
				t.Fatal(err)
			}
			r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{Registry: registry})
			if raw, _, err := r.Frame(nil, 0); raw != nil || !errors.Is(err, ErrPixelDataSizeMismatch) {
				t.Fatalf("raw=%v, error=%v", raw, err)
			}
		})
	}
	registry := NewMemoryRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(func(ctx context.Context, pixel PixelData, obj *object.Object) (Frames, error) {
		calls++
		cancel()
		return directCopyFrames(ctx, pixel, obj)
	})); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{Registry: registry})
	for i := 0; i < 2; i++ {
		if raw, _, err := r.Frame(ctx, 0); raw != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled decode = %v, %v", raw, err)
		}
	}
	if calls != 1 {
		t.Fatalf("codec invoked with canceled context: %d calls", calls)
	}
}

func TestFrameReaderDirectObjectReportAndBigEndian(t *testing.T) {
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(directCopyFrames)); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{Registry: registry})
	report := DecompressReport{Diagnostics: []string{"stale"}, Reconciliations: []MetadataReconciliation{{}}}
	decoded, raw, err := r.decodeFrameObject(nil, 1, &report)
	if err != nil || !reflect.DeepEqual(report.Diagnostics, []string{"frame diagnostic"}) || len(report.Reconciliations) != 0 {
		t.Fatalf("object decode error=%v report=%#v", err, report)
	}
	stored, ok := decoded.GetRaw(core.TagPixelData)
	if !ok || !bytes.Equal(stored, raw) {
		t.Fatal("native output differs from returned frame")
	}
	raw[0] = 255
	stored, _ = decoded.GetRaw(core.TagPixelData)
	if stored[0] != 1 {
		t.Fatal("returned samples alias the native output dataset")
	}
	if pooled, _ := r.decodeProxies.Get().(*object.Object); pooled != nil {
		if pixel, ok := pooled.Get(core.TagPixelData); !ok || pixel.Value != nil {
			t.Fatal("pooled facade retained the decoded payload")
		}
		r.decodeProxies.Put(pooled)
	}
	count := "2"
	file := &object.File{TransferSyntax: transfer.ExplicitVRBigEndian, Dataset: object.FromElements(
		pixelMetadataElementsWithOrder(binary.BigEndian, 1, 2, 1, 16, 16, 15, 0, &count,
			dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
			core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4, 5, 6, 7, 8})), nil)}
	file.Dataset.SetValueByteOrder(binary.BigEndian)
	r = newDirectFrameReader(t, file, FrameReaderOptions{})
	got, metadata, err := r.Frame(nil, 1)
	if err != nil || !bytes.Equal(got, []byte{6, 5, 8, 7}) || metadata.BitsAllocated != 16 || metadata.Columns != 2 {
		t.Fatalf("Big Endian = %v, %#v, %v", got, metadata, err)
	}
	// Full-object decompression rejected malformed non-pixel binary values;
	// the sample-only path must retain that rejection as well.
	file.Dataset.Put(core.NewRawElement(core.NewTag(0x7776, 0x0001), core.VRUS, []byte{1}))
	r = newDirectFrameReader(t, file, FrameReaderOptions{})
	if _, _, err := r.Frame(nil, 0); !errors.Is(err, ErrPixelDataSizeMismatch) {
		t.Fatalf("malformed Big Endian value: %v", err)
	}
}

func TestFrameReaderDirectNormalizesCodecPhotometric(t *testing.T) {
	for _, photometric := range []string{" RGB \x00", " \x00"} {
		t.Run(strconv.Quote(photometric), func(t *testing.T) {
			file := frameReaderDirectFile(2)
			file.Dataset.Put(uint16RawElement(tagSamplesPerPixel, 3))
			file.Dataset.Put(uint16RawElement(tagPlanarConfiguration, 1))
			registry := NewMemoryRegistry()
			if err := registry.RegisterCodec(transfer.RLELossless.UID, frameReaderDirectCodec(func(context.Context, PixelData, *object.Object) (Frames, error) {
				return Frames{Rows: 2, Columns: 2, Data: [][]byte{make([]byte, 12)}, PhotometricInterpretation: photometric}, nil
			})); err != nil {
				t.Fatal(err)
			}
			r := newDirectFrameReader(t, file, FrameReaderOptions{Registry: registry})
			raw, metadata, err := r.Frame(nil, 0)
			if photometric == " \x00" {
				if raw != nil || !errors.Is(err, ErrMissingMetadata) {
					t.Fatalf("empty photometric: %v, %v", raw, err)
				}
			} else if err != nil || metadata.PhotometricInterpretation != "RGB" || metadata.PlanarConfiguration != 0 || !metadata.PlanarConfigurationPresent {
				t.Fatalf("codec photometric: %#v, %v", metadata, err)
			}
		})
	}
}
