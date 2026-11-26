package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func outputOptionsFile(t testing.TB, frames, samples, bits int) *object.File {
	t.Helper()
	dataset := streamTestSource(12, 16, frames, samples, bits)
	for tag, value := range map[core.Tag]string{core.NewTag(0x0008, 0x0016): "1.2.840.10008.5.1.4.1.1.2", core.NewTag(0x0020, 0x000d): "1.2.826.0.1.3680043.10.1.1323.1", core.NewTag(0x0020, 0x000e): "1.2.826.0.1.3680043.10.1.1323.2"} {
		dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue{value}})
	}
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
}

func outputOptionsBytes(t testing.TB, file *object.File) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := object.WriteFile(&b, file); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestTranscodeOutputFragmentsMaterializedStreamingAndRepack(t *testing.T) {
	input := outputOptionsFile(t, 3, 1, 8)
	before := outputOptionsBytes(t, input)
	dir := t.TempDir()
	path := writeStreamTestFile(t, dir, "input.dcm", input)
	for _, policy := range []object.OffsetTablePolicy{object.OffsetTableAuto, object.OffsetTableBasic, object.OffsetTableEmpty} {
		t.Run(string(rune('0'+policy)), func(t *testing.T) {
			opts := streamTestRegistries(t)
			opts.MaxFragmentBytes = 32
			opts.OffsetTable = policy
			mat, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts)
			if err != nil {
				t.Fatal(err)
			}
			pixel, err := pixeldata.ExtractView(mat.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(pixel.Sequence.Fragments) <= 3 {
				t.Fatalf("did not split: %d", len(pixel.Sequence.Fragments))
			}
			for _, fragment := range pixel.Sequence.Fragments {
				if len(fragment) > 32 {
					t.Fatalf("fragment exceeds cap: %d", len(fragment))
				}
			}
			if policy == object.OffsetTableEmpty && len(pixel.Sequence.OffsetTable) != 0 || policy != object.OffsetTableEmpty && len(pixel.Sequence.OffsetTable) != 12 {
				t.Fatalf("BOT=%v", pixel.Sequence.OffsetTable)
			}
			for _, workers := range []int{1, 3} {
				opts.Pipeline.Workers = workers
				out := filepath.Join(dir, "out"+string(rune('0'+workers))+".dcm")
				report, err := pixeldata.TranscodePath(context.Background(), path, out, transfer.JPEGLSLossless, opts)
				if err != nil {
					t.Fatal(err)
				}
				if !report.Streaming || report.Verification != pixeldata.TranscodeVerificationPixelExact {
					t.Fatalf("report=%+v", report)
				}
				got, _ := os.ReadFile(out)
				if !bytes.Equal(got, outputOptionsBytes(t, mat)) {
					t.Fatal("materialized/stream output differs")
				}
			}
			// Same-syntax repacking uses no decoder or encoder and preserves UID
			// and lossy history rather than creating a compression generation.
			repack := pixeldata.TranscodeOptions{MaxFragmentBytes: 16, OffsetTable: object.OffsetTableEmpty}
			repacked, report, err := pixeldata.TranscodeFile(context.Background(), mat, transfer.JPEGLSLossless, repack)
			if err != nil {
				t.Fatal(err)
			}
			if !report.PixelDataPreserved || report.Lossy {
				t.Fatalf("repack=%+v", report)
			}
			encodedPath := writeStreamTestFile(t, dir, "encoded.dcm", mat)
			repackedPath := filepath.Join(dir, "repacked.dcm")
			report, err = pixeldata.TranscodePath(context.Background(), encodedPath, repackedPath, transfer.JPEGLSLossless, repack)
			if err != nil || !report.Streaming {
				t.Fatalf("repack path=%+v %v", report, err)
			}
			got, _ := os.ReadFile(repackedPath)
			if !bytes.Equal(got, outputOptionsBytes(t, repacked)) {
				t.Fatal("repack routes differ")
			}
			reader, err := pixeldata.NewFrameReader(context.Background(), repacked, nil, pixeldata.FrameReaderOptions{Registry: opts.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			original, _ := pixeldata.ExtractNativeFramesView(input.Dataset)
			for i := 0; i < 3; i++ {
				frame, _, err := reader.Frame(context.Background(), i)
				if err != nil || !bytes.Equal(frame, original.Data[i]) {
					t.Fatalf("frame%d differs: %v", i, err)
				}
			}
		})
	}
	if !bytes.Equal(before, outputOptionsBytes(t, input)) {
		t.Fatal("source changed")
	}
}

func TestTranscodeOutputEOTPoliciesAndInvalidLayouts(t *testing.T) {
	input := outputOptionsFile(t, 2, 1, 8)
	opts := streamTestRegistries(t)
	opts.OffsetTable = object.OffsetTableExtended
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts)
	if err != nil {
		t.Fatal(err)
	}
	pixel, _ := pixeldata.ExtractView(encoded.Dataset)
	eot, _ := encoded.Dataset.GetRaw(core.NewTag(0x7fe0, 0x0001))
	lengths, _ := encoded.Dataset.GetRaw(core.NewTag(0x7fe0, 0x0002))
	if len(pixel.Sequence.OffsetTable) != 0 || len(eot) != 16 || len(lengths) != 16 || binary.LittleEndian.Uint64(eot[8:]) != uint64(8+len(pixel.Sequence.Fragments[0])+len(pixel.Sequence.Fragments[0])%2) {
		t.Fatal("invalid EOT")
	}
	for _, syntax := range []transfer.Syntax{transfer.RLELossless, transfer.DeflatedImageFrameCompression, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
		opts := pipelineTestOptions(t)
		opts.MaxFragmentBytes = 2
		if _, _, err := pixeldata.TranscodeFile(context.Background(), input, syntax, opts); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
			t.Fatalf("%s split=%v", syntax.Name, err)
		}
	}
	for _, options := range []pixeldata.TranscodeOptions{{MaxFragmentBytes: 3}, {OffsetTable: object.OffsetTablePolicy(8)}, {PlanarOutput: pixeldata.PlanarOutputPolicy(8)}, {UIDPolicy: pixeldata.TranscodeUIDPolicy(8)}} {
		if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, options); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
			t.Fatalf("invalid policy=%v", err)
		}
	}
	opts.MaxFragmentBytes = 2
	if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Fatalf("split EOT=%v", err)
	}
	for _, options := range []pixeldata.TranscodeOptions{{MaxFragmentBytes: 2}, {OffsetTable: object.OffsetTableEmpty}} {
		if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.ExplicitVRLittleEndian, options); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
			t.Fatalf("native encapsulation option=%v", err)
		}
	}
}

// expectedPlanar is an independent coordinate/sample oracle, including
// 16-bit byte pairs; no product reorder helper is used here.
func expectedPlanar(interleaved []byte, width int) []byte {
	out := make([]byte, len(interleaved))
	pixels := len(interleaved) / (3 * width)
	for c := 0; c < 3; c++ {
		for p := 0; p < pixels; p++ {
			for b := 0; b < width; b++ {
				out[(c*pixels+p)*width+b] = interleaved[(p*3+c)*width+b]
			}
		}
	}
	return out
}

func TestTranscodeOutputPlanarNativeDecodeAndByteOrder(t *testing.T) {
	for _, bits := range []int{8, 16} {
		input := outputOptionsFile(t, 3, 3, bits)
		before := outputOptionsBytes(t, input)
		original, _ := pixeldata.ExtractNativeFramesView(input.Dataset)
		opts := streamTestRegistries(t)
		encoded, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.RLELossless, opts)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := writeStreamTestFile(t, dir, "encoded.dcm", encoded)
		for _, source := range []*object.File{input, encoded} {
			for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				opts.PlanarOutput = pixeldata.PlanarOutputPlanar
				out, _, err := pixeldata.TranscodeFile(context.Background(), source, syntax, opts)
				if err != nil {
					t.Fatal(err)
				}
				m, _ := pixeldata.ExtractMetadata(out.Dataset)
				if m.PlanarConfiguration != 1 {
					t.Fatal("missing planar metadata")
				}
				frames, _ := pixeldata.ExtractNativeFramesView(out.Dataset)
				for i := range frames.Data {
					want := expectedPlanar(original.Data[i], bits/8)
					if bits == 16 && syntax.ByteOrder == binary.BigEndian {
						for j := 0; j < len(want); j += 2 {
							want[j], want[j+1] = want[j+1], want[j]
						}
					}
					if !bytes.Equal(frames.Data[i], want) {
						t.Fatal("planar sample/byte order changed")
					}
				}
			}
		}
		for _, workers := range []int{1, 3} {
			opts.Pipeline.Workers = workers
			opts.PlanarOutput = pixeldata.PlanarOutputPlanar
			out := filepath.Join(dir, "planar"+string(rune('0'+workers))+".dcm")
			report, err := pixeldata.TranscodePath(context.Background(), path, out, transfer.ExplicitVRLittleEndian, opts)
			if err != nil || !report.Streaming {
				t.Fatalf("planar path=%+v %v", report, err)
			}
			read, err := object.OpenFile(out)
			if err != nil {
				t.Fatal(err)
			}
			frames, _ := pixeldata.ExtractNativeFramesView(read.Dataset)
			for i := range frames.Data {
				if !bytes.Equal(frames.Data[i], expectedPlanar(original.Data[i], bits/8)) {
					t.Fatal("stream planar samples differ")
				}
			}
			read.Close()
		}
		if !bytes.Equal(before, outputOptionsBytes(t, input)) {
			t.Fatal("source changed")
		}
	}
	for _, test := range []struct {
		class, pi string
		want      uint16
	}{{"1.2.840.10008.5.1.1.30", "RGB", 1}, {"1.2.840.10008.5.1.4.1.1.6.1", "YBR_FULL", 1}, {"1.2.840.10008.5.1.4.1.1.3.1", "YBR_FULL", 1}, {"1.2.840.10008.5.1.4.1.1.6.1", "RGB", 0}, {"1.2.840.10008.5.1.4.1.1.2", "RGB", 0}} {
		input := outputOptionsFile(t, 1, 3, 8)
		input.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{test.class}})
		input.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{test.pi}})
		out, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{PlanarOutput: pixeldata.PlanarOutputIOD})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := pixeldata.ExtractMetadata(out.Dataset)
		if m.PlanarConfiguration != test.want {
			t.Fatalf("IOD %s %s planar=%d", test.class, test.pi, m.PlanarConfiguration)
		}
	}
}

func TestTranscodeOutputUIDAndSecondaryCaptureFastPaths(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 16)
	before := outputOptionsBytes(t, input)
	sourceUID, _ := input.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	opts := streamTestRegistries(t)
	for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless} {
		for _, sc := range []bool{false, true} {
			opts.UIDPolicy = pixeldata.TranscodeUIDAlways
			opts.SecondaryCapture = sc
			out, report, err := pixeldata.TranscodeFile(context.Background(), input, target, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertOutputIdentity(t, out, sourceUID, sc)
			if !report.IdentityChanged {
				t.Fatal("identity not reported")
			}
			dir := t.TempDir()
			path := writeStreamTestFile(t, dir, "in.dcm", input)
			dest := filepath.Join(dir, "out.dcm")
			report, err = pixeldata.TranscodePath(context.Background(), path, dest, target, opts)
			if err != nil || !report.Streaming {
				t.Fatalf("identity path=%+v %v", report, err)
			}
			read, err := object.OpenFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			assertOutputIdentity(t, read, sourceUID, sc)
			read.Close()
		}
	}
	if !bytes.Equal(before, outputOptionsBytes(t, input)) {
		t.Fatal("source changed")
	}
	opts.SecondaryCapture = false
	opts.UIDPolicy = pixeldata.TranscodeUIDPreserve
	opts.AllowLossy = true
	if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGBaseline, opts); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Fatalf("lossy preserve=%v", err)
	}
	for _, source := range []*object.File{outputOptionsFile(t, 2, 1, 8), outputOptionsFile(t, 1, 3, 16)} {
		opts.SecondaryCapture = true
		opts.UIDPolicy = pixeldata.TranscodeUIDDefault
		if _, _, err := pixeldata.TranscodeFile(context.Background(), source, transfer.ExplicitVRLittleEndian, opts); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
			t.Fatalf("SC incompatible=%v", err)
		}
	}
}

func assertOutputIdentity(t testing.TB, file *object.File, sourceUID string, sc bool) {
	t.Helper()
	sop, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	meta, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0003))
	if sop == sourceUID || !core.IsValidUID(core.NormalizeUID(sop)) || sop != meta {
		t.Fatal("UID/meta identity mismatch")
	}
	sources, ok := file.Dataset.GetSequence(core.NewTag(0x0008, 0x2112))
	if !ok || len(sources) != 1 {
		t.Fatal("missing predecessor")
	}
	ref, _ := sources[0].GetString(core.NewTag(0x0008, 0x1155))
	class, _ := sources[0].GetString(core.NewTag(0x0008, 0x1150))
	if ref != sourceUID || class != "1.2.840.10008.5.1.4.1.1.2" {
		t.Fatal("wrong predecessor")
	}
	if sc {
		class, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0016))
		meta, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0002))
		conversion, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0064))
		if class != "1.2.840.10008.5.1.4.1.1.7" || class != meta || conversion != "WSD" || file.Dataset.Has(core.NewTag(0x0028, 0x0008)) {
			t.Fatal("SC metadata mismatch")
		}
	}
}

func TestTranscodeOutputCancellationAndRejectedPublication(t *testing.T) {
	input := outputOptionsFile(t, 2, 1, 8)
	dir := t.TempDir()
	path := writeStreamTestFile(t, dir, "in.dcm", input)
	dest := filepath.Join(dir, "out.dcm")
	sentinel := []byte("existing destination")
	if err := os.WriteFile(dest, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	opts := streamTestRegistries(t)
	opts.MaxFragmentBytes = 2
	opts.OffsetTable = object.OffsetTableExtended
	if _, err := pixeldata.TranscodePath(context.Background(), path, dest, transfer.JPEGLSLossless, opts); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Fatalf("invalid publication=%v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, sentinel) {
		t.Fatal("published rejected layout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := pixeldata.TranscodeFile(ctx, input, transfer.JPEGLSLossless, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestTranscodeOutputBoundsAndCompressedIconIsolation(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 8)
	withIcon(input.Dataset, outputOptionsFile(t, 1, 1, 8).Dataset)
	opts := streamTestRegistries(t)
	opts.MaxFragmentBytes = 16
	opts.OffsetTable = object.OffsetTableEmpty
	opts.CompressIcons = true
	opts.UIDPolicy = pixeldata.TranscodeUIDAlways
	before := outputOptionsBytes(t, input)
	out, report, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Icons) != 1 || report.Icons[0].Action != pixeldata.IconPixelsCompressed {
		t.Fatalf("icons=%+v", report.Icons)
	}
	icon := firstIcon(t, out.Dataset)
	pixel, _ := pixeldata.ExtractView(icon)
	if len(pixel.Sequence.Fragments) != 1 || len(pixel.Sequence.OffsetTable) != 4 || icon.Has(core.NewTag(0x0008, 0x2112)) {
		t.Fatal("main output policy leaked into icon")
	}
	iconUID, _ := icon.GetString(core.NewTag(0x0008, 0x0018))
	sourceIconUID, _ := firstIcon(t, input.Dataset).GetString(core.NewTag(0x0008, 0x0018))
	if iconUID != sourceIconUID {
		t.Fatal("icon UID changed")
	}
	if !bytes.Equal(before, outputOptionsBytes(t, input)) {
		t.Fatal("source icon changed")
	}
	input = outputOptionsFile(t, 2, 1, 8)
	opts = streamTestRegistries(t)
	opts.MaxFragmentBytes = 2
	opts.Limits.MaxFragments = 2
	if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts); !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("fragment limit=%v", err)
	}
	opts = streamTestRegistries(t)
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, opts)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := writeStreamTestFile(t, dir, "source.dcm", encoded)
	dest := filepath.Join(dir, "dest.dcm")
	sentinel := []byte("authoritative destination")
	os.WriteFile(dest, sentinel, 0600)
	repack := pixeldata.TranscodeOptions{MaxFragmentBytes: 2, Pipeline: pixeldata.TranscodePipelineOptions{MaxInFlightBytes: 1}}
	if _, err := pixeldata.TranscodePath(context.Background(), path, dest, transfer.JPEGLSLossless, repack); !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("repack in-flight=%v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, sentinel) {
		t.Fatal("published over budget repack")
	}
}

func TestTranscodeOutputDefaultAndPreservedIdentity(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 8)
	opts := streamTestRegistries(t)
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.RLELossless, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []*object.File{input, encoded} {
		for _, policy := range []pixeldata.TranscodeUIDPolicy{pixeldata.TranscodeUIDDefault, pixeldata.TranscodeUIDPreserve} {
			opts.UIDPolicy = policy
			out, report, err := pixeldata.TranscodeFile(context.Background(), source, source.TransferSyntax, opts)
			if err != nil {
				t.Fatal(err)
			}
			if report.IdentityChanged || report.Lossy || !bytes.Equal(outputOptionsBytes(t, out), outputOptionsBytes(t, source)) {
				t.Fatal("default/preserve copy changed output")
			}
		}
		opts.UIDPolicy = pixeldata.TranscodeUIDAlways
		dir := t.TempDir()
		path := writeStreamTestFile(t, dir, "input.dcm", source)
		dest := filepath.Join(dir, "out.dcm")
		report, err := pixeldata.TranscodePath(context.Background(), path, dest, source.TransferSyntax, opts)
		if err != nil || !report.Streaming || !report.PixelDataPreserved || !report.IdentityChanged {
			t.Fatalf("fast identity=%+v %v", report, err)
		}
		read, err := object.OpenFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := source.Dataset.GetString(core.NewTag(0x0008, 0x0018))
		assertOutputIdentity(t, read, uid, false)
		read.Close()
	}
}

func TestTranscodeOutputSecondaryCaptureAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*object.Object)
	}{
		{"private IOD", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1.1323.99"}})
		}},
		{"enhanced IOD", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.2.1"}})
		}},
		{"functional groups", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x5200, 0x9229), VR: core.VRSQ}, Value: core.SequenceValue{}})
		}},
		{"missing study UID", func(o *object.Object) { o.Remove(core.NewTag(0x0020, 0x000d)) }},
		{"missing palette", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"PALETTE COLOR"}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := outputOptionsFile(t, 1, 1, 8)
			test.mutate(input.Dataset)
			before := outputOptionsBytes(t, input)
			if _, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{SecondaryCapture: true}); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("SC admission=%v", err)
			}
			if !bytes.Equal(before, outputOptionsBytes(t, input)) {
				t.Fatal("rejected source changed")
			}
		})
	}
	// Even validation of a palette must follow the whole-object input bound.
	input := outputOptionsFile(t, 1, 1, 8)
	input.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"PALETTE COLOR"}})
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), input.Dataset, input.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{SecondaryCapture: true, Limits: pixeldata.TranscodeLimits{MaxInputBytes: 1}}); !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("SC input admission=%v", err)
	}
}

func TestTranscodeOutputLossyRequestedIdentityReferencesOriginal(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 8)
	// A constant image isolates identity policy from codec quality. The
	// encoder is still lossy even when this image has zero sample error.
	input.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, bytes.Repeat([]byte{64}, 12*16)))
	sourceUID, _ := input.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	before := outputOptionsBytes(t, input)
	for _, sc := range []bool{false, true} {
		opts := streamTestRegistries(t)
		opts.AllowLossy = true
		opts.UIDPolicy = pixeldata.TranscodeUIDAlways
		opts.SecondaryCapture = sc
		limit := float64(0)
		opts.MaxPixelError = &limit
		out, report, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGBaseline, opts)
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := out.Dataset.GetString(core.NewTag(0x0008, 0x0018))
		meta, _ := out.Meta.GetString(core.NewTag(0x0002, 0x0003))
		if !report.Lossy || !report.IdentityChanged || uid == sourceUID || uid != meta {
			t.Fatalf("lossy identity report=%+v UID=%s meta=%s", report, uid, meta)
		}
		sources, ok := out.Dataset.GetSequence(core.NewTag(0x0008, 0x2112))
		if !ok || len(sources) == 0 {
			t.Fatal("missing lossy predecessor")
		}
		for _, source := range sources {
			ref, _ := source.GetString(core.NewTag(0x0008, 0x1155))
			class, _ := source.GetString(core.NewTag(0x0008, 0x1150))
			if ref != sourceUID || class != "1.2.840.10008.5.1.4.1.1.2" {
				t.Fatal("derivation references an intermediate identity")
			}
		}
	}
	if !bytes.Equal(before, outputOptionsBytes(t, input)) {
		t.Fatal("source changed")
	}
}
