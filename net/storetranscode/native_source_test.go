package storetranscode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type nativeCountCodec struct {
	codec pixeldata.Codec
	calls *atomic.Int32
}

func (c nativeCountCodec) Decode(p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	c.calls.Add(1)
	return c.codec.Decode(p, o)
}
func nativeFixtureSource(t testing.TB, maxSpool int64) (*NativeSource, *atomic.Int32, []byte, string) {
	t.Helper()
	tc := codecfixture.JPEGLosslessSmall()
	raw, err := tc.Part10Bytes()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic.dcm")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	d, err := dimse.NewPathStoreSource(path).Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	codec, _ := registry.GetCodec(d.TransferSyntaxUID)
	_ = registry.RegisterCodec(d.TransferSyntaxUID, nativeCountCodec{codec, calls})
	resources, err := NewNativeResources(1, maxSpool)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	source, err := NewNativePathSource(path, d, hex.EncodeToString(digest[:]), NativeOptions{SpoolDirectory: t.TempDir(), Resources: resources, Transcode: pixeldata.TranscodeOptions{DecoderRegistry: registry, Limits: pixeldata.TranscodeLimits{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20}}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	header, err := object.ReadPart10Header(f, object.ReadFileOptions{})
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	return source, calls, raw[header.DataSetOffset:], path
}

func TestNativeRetrieveRevalidatesBeforeSourceReadAfterQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	source, _, _, path := nativeFixtureSource(t, 4<<20)
	resources := source.opts.Resources
	var checks atomic.Int32
	var revoked atomic.Bool
	source.opts.BeforeSourceRead = func(context.Context) error {
		checks.Add(1)
		if revoked.Load() {
			return errors.New("private authorization detail")
		}
		return nil
	}
	source.opts.BeforeStore = func(storeCtx context.Context) error { return storeCtx.Err() }
	if _, err := source.Inspect(ctx); err != nil || checks.Load() != 0 {
		t.Fatalf("detached inspection checked source authorization %d times, error = %v", checks.Load(), err)
	}
	opened, err := source.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opened.BeforeStore == nil || opened.BeforeStore(ctx) != nil || checks.Load() != 1 {
		t.Error("live wire guard or preparation guard missing")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	resources.gate <- struct{}{}
	done := make(chan error, 1)
	go func() {
		opened, err := source.Open(ctx)
		if opened.Close != nil {
			err = errors.Join(err, opened.Close())
		}
		done <- err
	}()
	queued := time.NewTicker(time.Millisecond)
	defer queued.Stop()
	for resources.SpoolBytes() == 0 {
		select {
		case <-queued.C:
		case <-ctx.Done():
			t.Fatal("source did not reach preparation queue")
		}
	}
	revoked.Store(true)
	// A path read first would instead report that the archive object changed.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	<-resources.gate
	select {
	case err := <-done:
		if err != dimse.ErrStoreAccessDenied || checks.Load() != 2 {
			t.Fatalf("Open() = %v, checks = %d", err, checks.Load())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	entries, err := os.ReadDir(source.opts.SpoolDirectory)
	if err != nil || len(entries) != 0 || resources.SpoolBytes() != 0 {
		t.Fatalf("denied preparation left %d spool entries, reserved %d bytes, read error %v", len(entries), resources.SpoolBytes(), err)
	}
}

func TestNativeRetrieveMetadataUsesSelectedImmutableSpool(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.JPEGLosslessNonHierarchical, transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			source, calls, original, path := nativeFixtureSource(t, 4<<20)
			descriptor, err := source.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			opened, err := source.OpenForTransferSyntax(context.Background(), syntax)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("different source generation"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := opened.ReadMetadata(context.Background(), index.Options{
				RawDataSet: &index.RawDataSetOptions{TransferSyntax: transfer.ExplicitVRBigEndian},
			})
			if err != nil || result.Record.Instance.SOPClassUID != descriptor.SOPClassUID || result.Record.Instance.SOPInstanceUID != descriptor.SOPInstanceUID {
				t.Fatalf("selected spool metadata failed: %v", err)
			}
			wantRaw := syntax.UID != descriptor.TransferSyntaxUID
			if result.RawDataSet != wantRaw || !result.StoppedBeforePixelData || result.Record.FileMeta.TransferSyntaxUID != syntax.UID {
				t.Fatalf("incorrect selected representation: raw=%v stopped=%v syntax=%s", result.RawDataSet, result.StoppedBeforePixelData, result.Record.FileMeta.TransferSyntaxUID)
			}
			if _, err := opened.ReadMetadata(context.Background(), index.Options{Limits: index.Limits{MaxElements: 1}}); !errors.Is(err, index.ErrResourceLimit) {
				t.Fatalf("spool observation ignored budget: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := opened.ReadMetadata(ctx, index.Options{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("spool observation ignored cancellation: %v", err)
			}
			var payload bytes.Buffer
			if err := opened.WriteDataSet(context.Background(), &payload, syntax); err != nil {
				t.Fatal(err)
			}
			wantCalls := int32(1)
			if !wantRaw {
				wantCalls = 0
				if !bytes.Equal(payload.Bytes(), original) {
					t.Fatal("metadata observation changed original payload")
				}
			}
			if calls.Load() != wantCalls {
				t.Fatalf("metadata observation performed extra decode: calls=%d want=%d", calls.Load(), wantCalls)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := opened.ReadMetadata(context.Background(), index.Options{}); !errors.Is(err, dimse.ErrStoreInvalidSource) {
				t.Fatalf("metadata hook survived source Close: %v", err)
			}
			if source.opts.Resources.SpoolBytes() != 0 {
				t.Fatal("metadata observation retained spool quota")
			}
		})
	}
}

func TestNativeRetrieveMetadataOriginalRawDataSet(t *testing.T) {
	fixture := codecfixture.NativeSmall().File()
	source, err := NewNativeSource(dimse.NewFileStoreSource(fixture, 0), NativeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := source.OpenForTransferSyntax(context.Background(), fixture.TransferSyntax)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	result, err := opened.ReadMetadata(context.Background(), index.Options{})
	if err != nil || !result.RawDataSet || result.Record.FileMeta.TransferSyntaxUID != fixture.TransferSyntax.UID || result.Record.Instance.SOPInstanceUID != opened.Descriptor.SOPInstanceUID {
		t.Fatalf("raw original metadata failed: %v", err)
	}
}

func TestNativeRetrieveOriginalExactNoDecodeAndNativeFallback(t *testing.T) {
	source, calls, original, path := nativeFixtureSource(t, 4<<20)
	before, _ := os.ReadFile(path)
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.WritableTransferSyntaxUIDs[0] != transfer.JPEGLosslessNonHierarchical.UID || len(descriptor.WritableTransferSyntaxUIDs) != 3 || calls.Load() != 0 || source.opts.Resources.SpoolBytes() != 0 {
		t.Fatalf("Inspect mutated or decoded: %+v calls=%d", descriptor, calls.Load())
	}
	for _, syntax := range []transfer.Syntax{transfer.JPEGLosslessNonHierarchical, transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		opened, err := source.OpenForTransferSyntax(context.Background(), syntax)
		if err != nil {
			t.Fatal(err)
		}
		raw := encodeSource(t, opened, syntax)
		if syntax.UID == descriptor.TransferSyntaxUID {
			if !bytes.Equal(raw, original) || calls.Load() != 0 {
				t.Fatal("original dataset bytes changed or decoder called")
			}
		} else {
			dataset, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), syntax, object.ReadFileOptions{})
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := pixeldata.DecompressDataSetContext(context.Background(), codecfixture.JPEGLosslessSmall().Object(), transfer.JPEGLosslessNonHierarchical, pixeldata.DecompressOptions{Registry: source.opts.Transcode.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			a, err := canonicalPixels(dataset, syntax)
			if err != nil {
				t.Fatal(err)
			}
			b, err := canonicalPixels(decoded, transfer.ExplicitVRLittleEndian)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatal("native retrieve pixels differ from independently decoded fixture")
			}
			_ = dataset.Close()
			_ = decoded.Close()
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if source.opts.Resources.SpoolBytes() != 0 {
			t.Fatal("quota leak")
		}
		assertEmpty(t, source.opts.SpoolDirectory)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("archive modified")
	}
}

func canonicalPixels(o *object.Object, syntax transfer.Syntax) ([]byte, error) {
	return canonicalNativePixelBytesForTest(o, syntax)
}
func canonicalNativePixelBytesForTest(o *object.Object, syntax transfer.Syntax) ([]byte, error) {
	element, ok := o.Get(core.TagPixelData)
	if !ok {
		return nil, pixeldata.ErrPixelDataNotFound
	}
	raw, ok := element.RawBytes()
	if !ok {
		return nil, pixeldata.ErrIncompatiblePixelData
	}
	return append([]byte(nil), raw...), nil
}

func TestNativeRetrieveQuotaCanceledChangedAndImmutableHandle(t *testing.T) {
	source, _, original, path := nativeFixtureSource(t, 2<<20)
	native, err := source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("occupied quota should fail immediately: %v", err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodeSource(t, opened, transfer.JPEGLosslessNonHierarchical), original) {
		t.Fatal("handle followed mutated source")
	}
	_ = opened.Close()
	if _, err := source.Open(context.Background()); !errors.Is(err, dimse.ErrStoreSourceChanged) || strings.Contains(err.Error(), path) {
		t.Fatalf("changed source/redaction: %v", err)
	}
	if source.opts.Resources.SpoolBytes() != 0 {
		t.Fatal("quota leaked on errors")
	}
	assertEmpty(t, source.opts.SpoolDirectory)
}

func TestNativeRetrieveConcurrentOriginalHandles(t *testing.T) {
	source, calls, original, _ := nativeFixtureSource(t, 8<<20)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opened, err := source.Open(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer opened.Close()
			var b bytes.Buffer
			if err := opened.WriteDataSet(context.Background(), &b, transfer.JPEGLosslessNonHierarchical); err != nil || !bytes.Equal(original, b.Bytes()) {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 0 || source.opts.Resources.SpoolBytes() != 0 {
		t.Fatal("decoder/quota concurrency regression")
	}
}

func TestNativeRetrieveMissingRegistryOriginalRemainsAvailable(t *testing.T) {
	source, _, original, _ := nativeFixtureSource(t, 2<<20)
	source.opts.Transcode.DecoderRegistry = nil
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptor.WritableTransferSyntaxUIDs) != 1 {
		t.Fatalf("unsupported fallback advertised: %+v", descriptor)
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !bytes.Equal(original, encodeSource(t, opened, transfer.JPEGLosslessNonHierarchical)) {
		t.Fatal("original changed")
	}
	if _, err := source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian); !errors.Is(err, dimse.ErrStoreTransferSyntax) {
		t.Fatal(err)
	}
}

func TestNativeRetrieveWriteFailureCancelAndCleanup(t *testing.T) {
	source, _, _, _ := nativeFixtureSource(t, 2<<20)
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("secret filesystem value")
	if err := opened.WriteDataSet(context.Background(), failingWriter{cause}, transfer.JPEGLosslessNonHierarchical); !errors.Is(err, cause) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := opened.WriteDataSet(ctx, io.Discard, transfer.JPEGLosslessNonHierarchical); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = opened.Close()
	assertEmpty(t, source.opts.SpoolDirectory)
}

type canceledNativeCodec struct{ started chan struct{} }

func (c canceledNativeCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	return pixeldata.Frames{}, errors.New("context required")
}
func (c canceledNativeCodec) DecodeContext(ctx context.Context, _ pixeldata.PixelData, _ *object.Object) (pixeldata.Frames, error) {
	close(c.started)
	<-ctx.Done()
	return pixeldata.Frames{}, context.Cause(ctx)
}
func TestNativeRetrieveCancelDuringDecodeAndBoundedFailures(t *testing.T) {
	source, _, _, _ := nativeFixtureSource(t, 2<<20)
	started := make(chan struct{})
	_ = source.opts.Transcode.DecoderRegistry.RegisterCodec(transfer.JPEGLosslessNonHierarchical.UID, canceledNativeCodec{started})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := source.OpenForTransferSyntax(ctx, transfer.ExplicitVRLittleEndian); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if source.opts.Resources.SpoolBytes() != 0 {
		t.Fatal("cancel quota leak")
	}
	assertEmpty(t, source.opts.SpoolDirectory)
	for _, mode := range []string{"output", "pixels", "frames", "spool", "elements", "expansion", "fragments"} {
		t.Run(mode, func(t *testing.T) {
			source, _, _, _ := nativeFixtureSource(t, 2<<20)
			switch mode {
			case "output":
				source.opts.Transcode.Limits.MaxOutputBytes = 1
			case "pixels":
				source.opts.Transcode.Limits.MaxPixels = 1
			case "frames":
				source.opts.Transcode.Limits.MaxFrames = 1
				file := codecfixture.JPEGLosslessSmall().File()
				file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}})
				source, _ = NewNativeSource(dimse.NewFileStoreSource(file, 0), source.opts)
			case "elements":
				source.opts.Transcode.Limits.MaxElements = 1
			case "expansion":
				source.opts.Transcode.Limits.MaxExpansionRatio = 1
				file := codecfixture.NativeMedium().File()
				file.TransferSyntax = transfer.DeflatedExplicitVRLittleEndian
				source, _ = NewNativeSource(dimse.NewFileStoreSource(file, 0), source.opts)
			case "fragments":
				source.opts.Transcode.Limits.MaxFragments = 1
				for _, tc := range codecfixture.CorpusCases() {
					if tc.Name == "encapsulated-uncompressed-multiframe" {
						source, _ = NewNativeSource(dimse.NewFileStoreSource(tc.File(), 0), source.opts)
						break
					}
				}
			case "spool":
				source.opts.Resources, _ = NewNativeResources(1, 1)
			}
			_, err := source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian)
			if !errors.Is(err, dimse.ErrStoreResourceLimit) {
				t.Fatalf("bounded %s: %v", mode, err)
			}
			if source.opts.Resources.SpoolBytes() != 0 {
				t.Fatal("bounded error quota leak")
			}
			assertEmpty(t, source.opts.SpoolDirectory)
		})
	}
}

func TestNativeRetrieveNativeAndOpaqueSyntaxOffers(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian, transfer.JPIPReferenced, transfer.JPIPReferencedDeflate, transfer.MPEG2MPML} {
		t.Run(syntax.Name, func(t *testing.T) {
			source, err := NewNativeSource(dimse.NewFileStoreSource(&object.File{Dataset: codecfixture.NativeSmall().Object(), TransferSyntax: syntax}, 0), NativeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			d, err := source.Inspect(context.Background())
			if err != nil && syntax.MediaPayload {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			opaque := syntax.UID == transfer.JPIPReferenced.UID || syntax.UID == transfer.JPIPReferencedDeflate.UID || syntax.MediaPayload
			if opaque {
				if len(d.WritableTransferSyntaxUIDs) != 1 {
					t.Fatalf("opaque syntax falsely offered native: %+v", d)
				}
				return
			}
			opened, err := source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			var b bytes.Buffer
			if err := opened.WriteDataSet(context.Background(), &b, transfer.ExplicitVRLittleEndian); err != nil {
				t.Fatal(err)
			}
			decoded, err := object.ReadDataSetWithOptions(bytes.NewReader(b.Bytes()), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer decoded.Close()
			actual, _ := canonicalPixels(decoded, transfer.ExplicitVRLittleEndian)
			if !bytes.Equal(actual, []byte{0, 64, 128, 255}) {
				t.Fatal("native fallback changed samples")
			}
		})
	}
	for _, tc := range codecfixture.CorpusCases() {
		if tc.Syntax.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
			continue
		}
		source, err := NewNativeSource(dimse.NewFileStoreSource(tc.File(), 0), NativeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		opened, err := source.OpenForTransferSyntax(context.Background(), transfer.ImplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		_ = opened.Close()
	}
}
