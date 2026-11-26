package storetranscode

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func preparedTestSource(t testing.TB, file *object.File, target transfer.Syntax, lossy bool) (*PreparedSession, *PreparedSource, *PreparedBinding, Options) {
	t.Helper()
	opts := Options{Target: target, SpoolDirectory: t.TempDir(), MaxSpoolBytes: 1 << 20, Transcode: pixeldata.TranscodeOptions{AllowLossy: lossy}}
	if lossy {
		limit := 4.
		opts.Transcode.MaxPixelError = &limit
	}
	binding, err := NewBuiltinPreparedBinding(BuiltinPreparedBindingOptions{AllowLossy: lossy, JPEGQuality: 95})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewPreparedSession(context.Background(), PreparedSessionOptions{MaxRetainedBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	source, err := session.NewSource(dimse.NewFileStoreSource(file, 0), opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(context.Background()); err != nil {
			t.Error(err)
		}
		_ = binding.Close()
		assertEmpty(t, opts.SpoolDirectory)
	})
	return session, source, binding, opts
}
func TestPreparedSourceEncoderOnceInspectOpenAuthorizedRetry(t *testing.T) {
	file := testFile()
	session, source, binding, opts := preparedTestSource(t, file, transfer.RLELossless, false)
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := binding.EncoderCalls()
	if calls == 0 {
		t.Fatal("encoder not actually invoked")
	}
	var first []byte
	for range 3 {
		opened, err := source.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wire := encodeSource(t, opened, opts.Target)
		if len(first) == 0 {
			first = wire
		} else if !bytes.Equal(first, wire) {
			t.Fatal("prepared bytes changed")
		}
		if !preparedSameDescriptor(descriptor, opened.Descriptor) {
			t.Fatal("identity/size changed")
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if binding.EncoderCalls() != calls {
		t.Fatal("reuse reencoded")
	}
	output, err := object.ReadDataSetWithOptions(bytes.NewReader(first), opts.Target, object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	decompressed, _, err := pixeldata.TranscodeDataSet(context.Background(), output, opts.Target, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: binding.decoders})
	if err != nil {
		t.Fatal(err)
	}
	defer decompressed.Close()
	compareDataSet(t, file.Dataset, decompressed)
	if session.Stats().RetainedBytes == 0 {
		t.Fatal("spool not retained between attempts")
	}
}
func TestPreparedSourceMutationRejectedBeforeCommand(t *testing.T) {
	for _, kind := range []string{"pixels", "identity", "policy", "binding-close", "spool"} {
		t.Run(kind, func(t *testing.T) {
			file := testFile()
			_, source, binding, _ := preparedTestSource(t, file, transfer.RLELossless, false)
			if _, err := source.Inspect(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := binding.EncoderCalls()
			switch kind {
			case "pixels":
				e, _ := file.Dataset.Get(core.TagPixelData)
				raw, _ := e.RawBytes()
				raw = append([]byte(nil), raw...)
				raw[0] ^= 1
				file.Dataset.Put(core.NewRawElement(core.TagPixelData, e.VR(), raw))
			case "identity":
				file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tags.SOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.3.99"}})
			case "policy":
				source.source.opts.Transcode.ForceReencode = !source.source.opts.Transcode.ForceReencode
			case "binding-close":
				_ = binding.Close()
			case "spool":
				for _, path := range source.cache.paths {
					if err := os.WriteFile(path, []byte("corrupted"), 0600); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			if opened, err := source.Open(context.Background()); err == nil {
				_ = opened.Close()
				t.Fatal("changed representation opened")
			}
			if binding.EncoderCalls() != calls {
				t.Fatal("invalid reuse invoked encoder")
			}
		})
	}
}
func TestPreparedSourceLossyExactBytesAndStableDerivedUID(t *testing.T) {
	file := testFile()
	original, _ := file.Dataset.GetUID(tags.SOPInstanceUID)
	_, source, binding, opts := preparedTestSource(t, file, transfer.JPEGBaseline, true)
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.SOPInstanceUID == original {
		t.Fatal("lossy retained originalUID")
	}
	calls := binding.EncoderCalls()
	var first []byte
	for range 2 {
		opened, err := source.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wire := encodeSource(t, opened, opts.Target)
		if first == nil {
			first = wire
		} else if !bytes.Equal(first, wire) {
			t.Fatal("lossy retry not exactsame representation")
		}
		parsed, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), opts.Target, object.ReadFileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := parsed.GetUID(tags.SOPInstanceUID)
		lossy, _ := parsed.GetString(core.NewTag(0x28, 0x2110))
		if uid != descriptor.SOPInstanceUID || lossy != "01" {
			t.Fatal("lossy command/data identity/history")
		}
		decoded, _, decodeErr := pixeldata.TranscodeDataSet(context.Background(), parsed, opts.Target, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: binding.decoders})
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		originalPixels, _ := file.Dataset.Get(core.TagPixelData)
		gotPixels, _ := decoded.Get(core.TagPixelData)
		wanted, _ := originalPixels.RawBytes()
		got, _ := gotPixels.RawBytes()
		if len(got) != len(wanted) {
			t.Fatal("lossy decodedsize changed")
		}
		maxError := 0
		for i, v := range wanted {
			diff := int(v) - int(got[i])
			if diff < 0 {
				diff = -diff
			}
			if diff > maxError {
				maxError = diff
			}
		}
		metrics := source.Report().LossyVerification
		if metrics == nil || metrics.MaximumError != float64(maxError) || metrics.MaximumSampleError != int64(maxError) || metrics.Frames != 2 {
			t.Fatal("lossy pixels/aggregate verification differ", metrics, maxError)
		}
		_ = decoded.Close()
		_ = parsed.Close()
		_ = opened.Close()
	}
	if binding.EncoderCalls() != calls {
		t.Fatal("lossy retry invoked encoder")
	}
	if uid, _ := file.Dataset.GetUID(tags.SOPInstanceUID); uid != original {
		t.Fatal("original changed")
	}
}
func TestPreparedSourceConcurrentIndependentReaders(t *testing.T) {
	_, source, binding, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	if _, err := source.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := binding.EncoderCalls()
	var wg sync.WaitGroup
	results := make(chan []byte, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opened, err := source.Open(context.Background())
			if err != nil {
				errs <- err
				return
			}
			var dst bytes.Buffer
			err = opened.WriteDataSet(context.Background(), &dst, opts.Target)
			err = errors.Join(err, opened.Close())
			if err != nil {
				errs <- err
				return
			}
			results <- dst.Bytes()
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var first []byte
	for wire := range results {
		if first == nil {
			first = wire
		} else if !bytes.Equal(first, wire) {
			t.Fatal("concurrent cursor interference")
		}
	}
	if binding.EncoderCalls() != calls {
		t.Fatal("concurrent opens reencode")
	}
}
func TestPreparedBindingRejectsOpaqueCodecAndLossyFallback(t *testing.T) {
	binding, err := NewBuiltinPreparedBinding(BuiltinPreparedBindingOptions{AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	session, err := NewPreparedSession(context.Background(), PreparedSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	opts := testOptions(t, transfer.RLELossless)
	if _, err := session.NewSource(dimse.NewFileStoreSource(testFile(), 0), opts, binding); !errors.Is(err, ErrPreparedBinding) {
		t.Fatal("opaque callerregistries trusted", err)
	}
	opts.Transcode.DecoderRegistry = nil
	opts.Transcode.EncoderRegistry = nil
	opts.Transcode.AllowLossy = true
	opts.FallbackOriginal = true
	if _, err := session.NewSource(dimse.NewFileStoreSource(testFile(), 0), opts, binding); !errors.Is(err, ErrOptions) {
		t.Fatal("lossy fallback accepted", err)
	}
}
