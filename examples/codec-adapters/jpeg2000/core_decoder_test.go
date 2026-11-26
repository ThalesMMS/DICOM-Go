package jpeg2000

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func coreFixture(t testing.TB, name string) *object.File {
	t.Helper()
	file, err := object.OpenFile(filepath.Join("..", "..", "..", "pixeldata", "codecfixture", "testdata", "codecfull", "pydicom", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func coreDecode(t testing.TB, codec *Codec, dataset *object.Object) (pixeldata.Frames, error) {
	t.Helper()
	pixel, err := pixeldata.Extract(dataset)
	if err != nil {
		t.Fatal(err)
	}
	return codec.DecodeContext(context.Background(), pixel, dataset)
}

// Independent references: pydicom corpus files whose native twins were
// encoded separately. Lossless streams must match exactly; the irreversible
// 9/7 stream is held to ±1 (ISO/IEC 15444-4 grades such decoders by error).
func TestCoreDecoderMatchesIndependentNativeReferences(t *testing.T) {
	for _, tc := range []struct {
		encoded, native string
		tolerance       int
		signed          bool
		frames          int
	}{
		{"693_J2KR.dcm", "693_UNCR.dcm", 0, true, 1},
		{"emri_small_jpeg_2k_lossless.dcm", "emri_small.dcm", 0, false, 10},
		{"US1_J2KI.dcm", "US1_UNCI.dcm", 1, false, 1},
	} {
		t.Run(tc.encoded, func(t *testing.T) {
			encoded := coreFixture(t, tc.encoded)
			metadata, err := pixeldata.ExtractMetadata(encoded.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if (metadata.PixelRepresentation == 1) != tc.signed {
				t.Fatalf("fixture PixelRepresentation %d", metadata.PixelRepresentation)
			}
			frames, err := coreDecode(t, NewCore(CoreOptions{}), encoded.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			native, err := pixeldata.ExtractNativeFrames(coreFixture(t, tc.native).Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(frames.Data) != tc.frames || len(native.Data) != tc.frames {
				t.Fatalf("frames %d/%d", len(frames.Data), len(native.Data))
			}
			sampleBytes := int(metadata.BitsAllocated / 8)
			for f := range frames.Data {
				if len(frames.Data[f]) != len(native.Data[f]) {
					t.Fatalf("frame %d is %d bytes, want %d", f, len(frames.Data[f]), len(native.Data[f]))
				}
				for i := 0; i < len(frames.Data[f]); i += sampleBytes {
					got, want := sampleAt(frames.Data[f][i:], sampleBytes, tc.signed), sampleAt(native.Data[f][i:], sampleBytes, tc.signed)
					if d := got - want; d > tc.tolerance || d < -tc.tolerance {
						t.Fatalf("frame %d sample %d = %d, want %d", f, i/sampleBytes, got, want)
					}
				}
			}
		})
	}
}

func sampleAt(b []byte, width int, signed bool) int {
	if width == 1 {
		if signed {
			return int(int8(b[0]))
		}
		return int(b[0])
	}
	v := binary.LittleEndian.Uint16(b)
	if signed {
		return int(int16(v))
	}
	return int(v)
}

func TestCoreRegistrationIsExplicitAndLimitedToPart1(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := RegisterCore(registry, CoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID} {
		if _, ok := registry.GetCodec(uid); !ok {
			t.Fatalf("%s not registered", uid)
		}
	}
	for _, uid := range []string{transfer.JPEG2000Part2Lossless.UID, transfer.JPEG2000Part2.UID, transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID, transfer.HTJ2K.UID} {
		if _, ok := registry.GetCodec(uid); ok {
			t.Fatalf("%s registered by analogy", uid)
		}
	}
	if _, ok := pixeldata.DefaultRegistry.GetCodec(transfer.JPEG2000LosslessOnly.UID); ok {
		t.Fatal("RegisterCore changed the package default registry")
	}
	if err := RegisterCore(nil, CoreOptions{}); !errors.Is(err, pixeldata.ErrCodecRegistryNil) {
		t.Fatalf("nil registry error = %v", err)
	}
}

func withMetadata(t *testing.T, name string, changes ...core.Element) *object.Object {
	t.Helper()
	source := coreFixture(t, name).Dataset
	dataset := object.FromElements(source.Elements(), nil)
	for _, change := range changes {
		dataset.Put(change)
	}
	return dataset
}

func us(group, element, v uint16) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
}

func TestCoreSignednessPolicy(t *testing.T) {
	// Signed codestream described as unsigned: always refused.
	if _, err := coreDecode(t, NewCore(CoreOptions{SignednessMismatch: ReinterpretUnsignedAsSigned}), withMetadata(t, "693_J2KR.dcm", us(0x0028, 0x0103, 0))); !errors.Is(err, ErrSignednessMismatch) {
		t.Fatalf("signed stream with PixelRepresentation 0: %v", err)
	}
	// Unsigned codestream described as signed: refused by default...
	legacy := withMetadata(t, "emri_small_jpeg_2k_lossless.dcm", us(0x0028, 0x0103, 1))
	if _, err := coreDecode(t, NewCore(CoreOptions{}), legacy); !errors.Is(err, ErrSignednessMismatch) {
		t.Fatalf("unsigned stream with PixelRepresentation 1: %v", err)
	}
	// ...and reinterpreted as two's complement of BitsStored only on request.
	frames, err := coreDecode(t, NewCore(CoreOptions{SignednessMismatch: ReinterpretUnsignedAsSigned}), legacy)
	if err != nil {
		t.Fatal(err)
	}
	native, err := pixeldata.ExtractNativeFrames(coreFixture(t, "emri_small.dcm").Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(frames.Data[0]); i += 2 {
		stored := int(binary.LittleEndian.Uint16(native.Data[0][i:]))
		if stored >= 1<<11 {
			stored -= 1 << 12
		}
		if got := int(int16(binary.LittleEndian.Uint16(frames.Data[0][i:]))); got != stored {
			t.Fatalf("sample %d = %d, want %d", i/2, got, stored)
		}
	}
}

func coreFrame(t *testing.T, name string) []byte {
	t.Helper()
	element, _ := coreFixture(t, name).Dataset.Get(core.TagPixelData)
	return element.Value.(core.FragmentSequence).Fragments[0]
}

func TestCoreRefusesInvalidMetadataStreamsAndFeatures(t *testing.T) {
	metadata, err := pixeldata.ExtractMetadata(withMetadata(t, "693_J2KR.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	frame := coreFrame(t, "693_J2KR.dcm")
	decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
	decode := func(payload []byte, md pixeldata.Metadata) error {
		_, err := decoder.DecodeFrameContext(context.Background(), payload, md)
		return err
	}
	wrongRows := metadata
	wrongRows.Rows++
	narrow := metadata
	narrow.BitsAllocated, narrow.BitsStored, narrow.HighBit = 8, 8, 7
	// A region of interest of a style Part 1 does not define (RGN: component 0,
	// Srgn 1, shift 5) in the main header, before the first tile-part.
	sotStart := bytes.Index(frame, []byte{0xFF, 0x90})
	withRGN := append(append(append([]byte(nil), frame[:sotStart]...), 0xFF, 0x5E, 0x00, 0x05, 0, 1, 5), frame[sotStart:]...)
	jp2 := append(append([]byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}, beUint32(uint32(len(frame)+8))...), append([]byte("jp2c"), frame...)...)
	for name, tc := range map[string]struct {
		payload  []byte
		metadata pixeldata.Metadata
		want     error
	}{
		"size mismatch":             {frame, wrongRows, ErrImageSizeMismatch},
		"precision above container": {frame, narrow, ErrUnsupportedMetadata},
		"truncated codestream":      {frame[:len(frame)/2], metadata, ErrMalformedCodestream},
		"empty payload":             {nil, metadata, ErrMalformedCodestream},
		"RGN style 1 is refused":    {withRGN, metadata, ErrUnsupportedCodestreamFeature},
		"JP2 container by default":  {jp2, metadata, ErrJP2Wrapped},
	} {
		if err := decode(tc.payload, tc.metadata); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
	// JP2 unwrapping is an explicit recovery option.
	recovering := NewCoreDecoder(CoreOptions{AllowJP2Wrapper: true}).(ContextDecoder)
	if _, err := recovering.DecodeFrameContext(context.Background(), jp2, metadata); err != nil {
		t.Fatalf("JP2 with AllowJP2Wrapper: %v", err)
	}
}

func beUint32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

func TestCorePlansJP2WithExplicitRecovery(t *testing.T) {
	frame := coreFrame(t, "693_J2KR.dcm")
	jp2 := append(append([]byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}, beUint32(uint32(len(frame)+8))...), append([]byte("jp2c"), frame...)...)
	pixel := pixeldata.PixelData{Sequence: core.FragmentSequence{Fragments: [][]byte{jp2[:20], jp2[20:]}}}
	for _, frames := range []int{1, 2} {
		if _, err := planJPEG2000Frames(context.Background(), pixel, nil, frames, false); !errors.Is(err, ErrJP2Wrapped) {
			t.Fatalf("%d frames without recovery: %v", frames, err)
		}
	}
	got, err := planJPEG2000Frames(context.Background(), pixel, nil, 1, true)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0], jp2) {
		t.Fatalf("single-frame recovery: %v", err)
	}
	if _, err := planJPEG2000Frames(context.Background(), pixel, nil, 2, true); !errors.Is(err, ErrMalformedCodestream) {
		t.Fatalf("multiple-frame planning: %v", err)
	}
}

func TestCoreCOCBeforeCODPreservesSamples(t *testing.T) {
	frame := coreFrame(t, "693_J2KR.dcm")
	original, err := j2kcore.ParseCodestream(frame)
	if err != nil {
		t.Fatal(err)
	}
	want, err := j2kcore.Decode(original)
	if err != nil {
		t.Fatal(err)
	}
	codStart := bytes.Index(frame, []byte{0xff, 0x52})
	codEnd := codStart + 2 + int(binary.BigEndian.Uint16(frame[codStart+2:]))
	cod := frame[codStart:codEnd]
	coc := append([]byte{0xff, 0x53, 0, 9, 0, cod[4] & 1}, cod[9:]...)
	for _, tileHeader := range []bool{false, true} {
		modified := append([]byte(nil), frame...)
		insertAt, insert := codStart, coc
		if tileHeader {
			sot := bytes.Index(modified, []byte{0xff, 0x90})
			insertAt = sot + 12
			insert = append(append([]byte(nil), coc...), cod...)
			psot := binary.BigEndian.Uint32(modified[sot+6:])
			if psot != 0 {
				binary.BigEndian.PutUint32(modified[sot+6:], psot+uint32(len(insert)))
			}
			// The tile COD restores the original fields after the COC has
			// been parsed against a different main COD.
			modified[codStart+4] |= 6 // SOP and EPH
			modified[codStart+5] = 1 - cod[5]
			binary.BigEndian.PutUint16(modified[codStart+6:], 2)
		}
		modified = append(append(append([]byte(nil), modified[:insertAt]...), insert...), modified[insertAt:]...)
		stream, err := j2kcore.ParseCodestream(modified)
		if err != nil {
			t.Fatal(err)
		}
		got, err := j2kcore.Decode(stream)
		if err != nil {
			t.Fatalf("tile header %v: %v", tileHeader, err)
		}
		for c, samples := range want.Components {
			for i, sample := range samples {
				if got.Components[c][i] != sample {
					t.Fatalf("tile header %v: component %d sample %d differs", tileHeader, c, i)
				}
			}
		}
	}
}

func TestCoreBudgetsRefuseBeforeDecoding(t *testing.T) {
	metadata, err := pixeldata.ExtractMetadata(withMetadata(t, "693_J2KR.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	frame := coreFrame(t, "693_J2KR.dcm")
	for name, limits := range map[string]CoreLimits{
		"input bytes":     {MaxInputBytes: int64(len(frame) - 1)},
		"samples":         {MaxSamples: 512*512 - 1},
		"tile work bytes": {MaxTileWorkBytes: 1 << 10},
		"resolutions":     {MaxResolutions: 1},
		"code-blocks":     {MaxCodeBlocksPerTile: 1},
	} {
		decoder := NewCoreDecoder(CoreOptions{Limits: limits}).(ContextDecoder)
		if _, err := decoder.DecodeFrameContext(context.Background(), frame, metadata); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// countingContext cancels after a fixed number of Err calls, so cancellation
// lands inside the decoder's work loops rather than before or after it.
type countingContext struct {
	context.Context
	calls, after atomic.Int64
}

func (c *countingContext) Err() error {
	if c.calls.Add(1) > c.after.Load() {
		return context.Canceled
	}
	return nil
}

func TestCoreCancellationStopsWorkAndPublishesNothing(t *testing.T) {
	encoded := coreFixture(t, "emri_small_jpeg_2k_lossless.dcm")
	pixel, err := pixeldata.Extract(encoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	full := &countingContext{Context: context.Background()}
	full.after.Store(1 << 40)
	if _, err := NewCore(CoreOptions{}).DecodeContext(full, pixel, encoded.Dataset); err != nil {
		t.Fatal(err)
	}
	total := full.calls.Load()
	if total < 100 {
		t.Fatalf("decoder checks cancellation only %d times", total)
	}
	for _, after := range []int64{total / 4, total / 2, total - 5} {
		ctx := &countingContext{Context: context.Background()}
		ctx.after.Store(after)
		frames, err := NewCore(CoreOptions{}).DecodeContext(ctx, pixel, encoded.Dataset)
		if !errors.Is(err, context.Canceled) || len(frames.Data) != 0 {
			t.Fatalf("cancel after %d/%d checks: %v, %d frames", after, total, err, len(frames.Data))
		}
		if ctx.calls.Load() > after+2 {
			t.Fatalf("decoder kept working after cancellation: %d checks", ctx.calls.Load())
		}
	}
	deadline, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := NewCore(CoreOptions{}).DecodeContext(deadline, pixel, encoded.Dataset); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestCoreDecoderIsConcurrentAndOwnsItsOutput(t *testing.T) {
	encoded := coreFixture(t, "693_J2KR.dcm")
	pixel, err := pixeldata.Extract(encoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	input := sha256.Sum256(pixel.Sequence.Fragments[0])
	codec := NewCore(CoreOptions{})
	reference, err := codec.DecodeContext(context.Background(), pixel, encoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames, err := codec.DecodeContext(context.Background(), pixel, encoded.Dataset)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(frames.Data[0], reference.Data[0]) {
				errs <- errors.New("concurrent decode differs")
			}
			for j := range frames.Data[0] {
				frames.Data[0][j] = 0xAA // output is caller-owned
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if sha256.Sum256(pixel.Sequence.Fragments[0]) != input {
		t.Fatal("decoder mutated the borrowed codestream")
	}
}

func FuzzCoreDecodeFrame(f *testing.F) {
	file, err := object.OpenFile(filepath.Join("..", "..", "..", "pixeldata", "codecfixture", "testdata", "codecfull", "pydicom", "emri_small_jpeg_2k_lossless.dcm"))
	if err != nil {
		f.Fatal(err)
	}
	element, _ := file.Dataset.Get(core.TagPixelData)
	seed := element.Value.(core.FragmentSequence).Fragments[0]
	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	_ = file.Close()
	if err != nil {
		f.Fatal(err)
	}
	metadata.NumberOfFrames = 1
	f.Add(seed)
	f.Add(seed[:len(seed)/3])
	decoder := NewCoreDecoder(CoreOptions{Limits: CoreLimits{MaxTileWorkBytes: 16 << 20}}).(ContextDecoder)
	f.Fuzz(func(t *testing.T, payload []byte) {
		out, err := decoder.DecodeFrameContext(context.Background(), payload, metadata)
		if err == nil && len(out) != int(metadata.Rows)*int(metadata.Columns)*2 {
			t.Fatalf("output of %d bytes", len(out))
		}
		if err != nil && !errors.Is(err, ErrMalformedCodestream) && !errors.Is(err, ErrUnsupportedCodestreamFeature) &&
			!errors.Is(err, ErrResourceLimit) && !errors.Is(err, ErrImageSizeMismatch) && !errors.Is(err, ErrUnsupportedMetadata) &&
			!errors.Is(err, ErrSignednessMismatch) && !errors.Is(err, ErrSampleOutOfRange) && !errors.Is(err, ErrJP2Wrapped) {
			t.Fatalf("untyped error: %v", err)
		}
	})
}
