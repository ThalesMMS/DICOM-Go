package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var iconSequenceTag = core.NewTag(0x0088, 0x0200)

func iconRegistries(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	if err := jpegls.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders}
}

func withIcon(main, icon *object.Object) *object.Object {
	main.Put(core.Element{Header: core.ElementHeader{Tag: iconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}})
	return main
}

func firstIcon(t *testing.T, dataset *object.Object) *object.Object {
	t.Helper()
	items, ok := dataset.GetSequence(iconSequenceTag)
	if !ok || len(items) != 1 {
		t.Fatalf("icon items=%d present=%v", len(items), ok)
	}
	icon := items[0]
	icon.SetValueByteOrder(dataset.ValueByteOrder())
	return icon
}

func iconNativeBytes(t *testing.T, icon *object.Object, syntax transfer.Syntax, options pixeldata.TranscodeOptions) []byte {
	t.Helper()
	p, err := pixeldata.ExtractView(icon)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Encapsulated {
		if syntax.Encapsulated {
			syntax = transfer.ExplicitVRLittleEndian
		}
	}
	native, _, err := pixeldata.TranscodeDataSet(context.Background(), icon, syntax, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	p, err = pixeldata.ExtractView(native)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), p.Raw...)
}

func TestTranscodeIconsNativeAndCompressedPolicies(t *testing.T) {
	for _, target := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
		for _, compress := range []bool{false, true} {
			t.Run(target.Name+map[bool]string{false: "/native-icon", true: "/compressed-icon"}[compress], func(t *testing.T) {
				options := iconRegistries(t)
				options.CompressIcons = compress
				originalIcon := codecfixture.NativeSmall().Object()
				want := iconNativeBytes(t, originalIcon, transfer.ExplicitVRLittleEndian, options)
				source := withIcon(codecfixture.NativeSmall().Object(), originalIcon)
				encoded, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, target, options)
				if err != nil {
					t.Fatal(err)
				}
				icon := firstIcon(t, encoded)
				p, err := pixeldata.ExtractView(icon)
				if err != nil || p.Encapsulated != compress {
					t.Fatalf("icon encapsulated=%v, err=%v", p.Encapsulated, err)
				}
				action := pixeldata.IconPixelsPreservedNative
				if compress {
					action = pixeldata.IconPixelsCompressed
				}
				if len(report.Icons) != 1 || report.Icons[0].Action != action || report.Icons[0].MetadataIncomplete || report.Icons[0].Path.String() != "(0088,0200)[0]" {
					t.Fatalf("report=%+v", report.Icons)
				}
				// The native representation of the same icon is byte exact.
				if got := iconNativeBytes(t, icon, target, options); !bytes.Equal(got, want) {
					t.Fatal("icon samples changed")
				}
				var part10 bytes.Buffer
				if err := object.WriteFile(&part10, &object.File{Dataset: encoded, TransferSyntax: target}); err != nil {
					t.Fatal(err)
				}
				reopened, err := object.ReadFile(bytes.NewReader(part10.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				if got := iconNativeBytes(t, firstIcon(t, reopened.Dataset), target, options); !bytes.Equal(got, want) {
					t.Fatal("written icon samples changed")
				}
				native, back, err := pixeldata.TranscodeDataSet(context.Background(), reopened.Dataset, target, transfer.ExplicitVRLittleEndian, options)
				if err != nil {
					t.Fatal(err)
				}
				if back.Icons[0].Action != map[bool]pixeldata.IconTranscodeAction{true: pixeldata.IconPixelsDecompressed, false: pixeldata.IconPixelsPreservedNative}[compress] {
					t.Fatalf("back report=%+v", back.Icons)
				}
				if got := iconNativeBytes(t, firstIcon(t, native), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
					t.Fatal("round-trip samples changed")
				}
				if got := iconNativeBytes(t, firstIcon(t, source), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
					t.Fatal("borrowed source changed")
				}
				// Returned nested values must not alias the source.
				returned, _ := firstIcon(t, native).Get(core.TagPixelData)
				raw, _ := returned.RawBytes()
				raw[0] ^= 0xff
				if got := iconNativeBytes(t, firstIcon(t, source), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
					t.Fatal("output aliases source icon")
				}
			})
		}
	}
}

func TestTranscodeIconsEndianAndNestedPaths(t *testing.T) {
	options := iconRegistries(t)
	icon := codecfixture.NativeSmall().Object()
	putUint16(icon, core.NewTag(0x0028, 0x0100), 16)
	putUint16(icon, core.NewTag(0x0028, 0x0101), 16)
	putUint16(icon, core.NewTag(0x0028, 0x0102), 15)
	icon.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4, 5, 6, 7, 8}))
	source := withIcon(codecfixture.NativeSmall().Object(), icon)
	big, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pixeldata.ExtractView(firstIcon(t, big))
	if !bytes.Equal(got.Raw, []byte{2, 1, 4, 3, 6, 5, 8, 7}) || report.Icons[0].Action != pixeldata.IconPixelsPreservedNative {
		t.Fatalf("big icon/report=%v/%+v", got.Raw, report)
	}
	encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), big, transfer.ExplicitVRBigEndian, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = pixeldata.ExtractView(firstIcon(t, encoded))
	if !bytes.Equal(got.Raw, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatal("native icon endian conversion changed samples")
	}
	container := core.NewTag(0x0008, 0x1140)
	nested := codecfixture.NativeSmall().Object()
	nested.Put(core.Element{Header: core.ElementHeader{Tag: container, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{source.ToDataSet()}}})
	// The container is a genuine nested image and has its own top-level pixel
	// data, which remains outside the icon capability and must still fail.
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), nested, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Fatalf("non-icon nested pixels=%v", err)
	}
	child := object.FromElements([]core.Element{}, nil)
	withIcon(child, icon)
	nested.Put(core.Element{Header: core.ElementHeader{Tag: container, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{child.ToDataSet()}}})
	_, report, err = pixeldata.TranscodeDataSet(context.Background(), nested, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
	if err != nil || len(report.Icons) != 1 || report.Icons[0].Path.String() != "(0008,1140)[0]/(0088,0200)[0]" {
		t.Fatalf("nested icon report=%+v err=%v", report, err)
	}
}

func TestTranscodeIconsIncompleteMetadataAndBudgets(t *testing.T) {
	options := iconRegistries(t)
	incomplete := object.FromElements([]core.Element{core.NewRawElement(core.TagPixelData, core.VROB, []byte{9, 10})}, nil)
	source := withIcon(codecfixture.NativeSmall().Object(), incomplete)
	out, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
	if err != nil || len(report.Icons) != 1 || !report.Icons[0].MetadataIncomplete {
		t.Fatalf("incomplete native report=%+v err=%v", report, err)
	}
	got, _ := pixeldata.ExtractView(firstIcon(t, out))
	if !bytes.Equal(got.Raw, []byte{9, 10}) {
		t.Fatal("incomplete native icon was changed")
	}
	options.CompressIcons = true
	if out, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); out != nil || !errors.Is(err, pixeldata.ErrMissingMetadata) {
		t.Fatalf("incomplete icon encode=%v/%v", out, err)
	}
	for _, limits := range []pixeldata.TranscodeLimits{{MaxPixels: 7}, {MaxFrames: 1}, {MaxFragments: 1}, {MaxOutputBytes: 1}} {
		options = iconRegistries(t)
		options.CompressIcons = true
		options.Limits = limits
		valid := withIcon(codecfixture.NativeSmall().Object(), codecfixture.NativeSmall().Object())
		if out, _, err := pixeldata.TranscodeDataSet(context.Background(), valid, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); out != nil || (!errors.Is(err, pixeldata.ErrTranscodeResourceLimit) && !errors.Is(err, pixeldata.ErrDecompressResourceLimit)) {
			t.Fatalf("limits=%+v err=%v", limits, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, _, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.RLELossless, iconRegistries(t)); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
	bad := codecfixture.NativeSmall().Object()
	bad.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{1}))
	if out, _, err := pixeldata.TranscodeDataSet(context.Background(), withIcon(codecfixture.NativeSmall().Object(), bad), transfer.ExplicitVRLittleEndian, transfer.RLELossless, iconRegistries(t)); out != nil || !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("truncated icon=%v", err)
	}
}

func TestTranscodeIconsPathFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	sourcePath, destination := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "destination.dcm")
	icon := codecfixture.NativeSmall().Object()
	icon.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}})
	main := codecfixture.NativeSmall().Object()
	options := iconRegistries(t)
	encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), main, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	validIcon, _, err := pixeldata.TranscodeDataSet(context.Background(), codecfixture.NativeSmall().Object(), transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	encoded.Put(core.Element{Header: core.ElementHeader{Tag: iconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{validIcon.ToDataSet(), icon.ToDataSet()}}})
	err = object.WriteFile(f, &object.File{Dataset: encoded, TransferSyntax: transfer.RLELossless})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("fixture write=%v/%v", err, closeErr)
	}
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pixeldata.TranscodePath(context.Background(), sourcePath, destination, transfer.ExplicitVRLittleEndian, options); err == nil {
		t.Fatal("malformed compressed icon accepted")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "previous" {
		t.Fatalf("destination=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary files leaked: count=%d err=%v", len(entries), err)
	}
}

// Existing upstream fixtures are opt-in and never copied into the repository.
func TestTranscodeIconsGDCMFixtures(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to an existing GDCM data directory")
	}
	for _, name := range []string{"KODAK_CompressedIcon.dcm", "simpleImageWithIcon.dcm", "CT-SIEMENS-Icone-With-PaletteColor.dcm"} {
		t.Run(name, func(t *testing.T) {
			options := iconRegistries(t)
			file, err := object.OpenFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			originalIcon := firstIcon(t, file.Dataset)
			want := iconNativeBytes(t, originalIcon, file.TransferSyntax, options)
			native, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.ExplicitVRLittleEndian, options)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []transfer.Syntax{transfer.JPEGLSLossless, transfer.RLELossless} {
				for _, compress := range []bool{false, true} {
					options.CompressIcons = compress
					converted, report, err := pixeldata.TranscodeFile(context.Background(), native, target, options)
					if name == "simpleImageWithIcon.dcm" && compress {
						if converted != nil || !errors.Is(err, pixeldata.ErrMissingMetadata) {
							t.Fatalf("incomplete icon encode=%v", err)
						}
						recoveryOptions := options
						recoveryOptions.Recovery.DefaultImagePixelModule = true
						converted, report, err = pixeldata.TranscodeFile(context.Background(), native, target, recoveryOptions)
						if err != nil || len(report.Icons) != 1 || !slices.Contains(report.Icons[0].DecodeDiagnostics, pixeldata.DiagnosticDefaultSamplesPerPixel) {
							t.Fatalf("opt-in icon recovery report=%+v err=%v", report, err)
						}
						if got := iconNativeBytes(t, firstIcon(t, converted.Dataset), target, recoveryOptions); !bytes.Equal(got, want) {
							t.Fatal("recovered icon samples changed")
						}
						back, _, err := pixeldata.TranscodeFile(context.Background(), converted, transfer.ExplicitVRLittleEndian, recoveryOptions)
						if err != nil || !bytes.Equal(iconNativeBytes(t, firstIcon(t, back.Dataset), transfer.ExplicitVRLittleEndian, recoveryOptions), want) {
							t.Fatalf("recovered icon round trip err=%v", err)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(report.Icons) != 1 || report.Icons[0].MetadataIncomplete != (name == "simpleImageWithIcon.dcm") {
						t.Fatalf("report=%+v", report)
					}
					if got := iconNativeBytes(t, firstIcon(t, converted.Dataset), target, options); !bytes.Equal(got, want) {
						t.Fatal("fixture icon samples changed")
					}
					back, _, err := pixeldata.TranscodeFile(context.Background(), converted, transfer.ExplicitVRLittleEndian, options)
					if err != nil {
						t.Fatal(err)
					}
					if got := iconNativeBytes(t, firstIcon(t, back.Dataset), transfer.ExplicitVRLittleEndian, options); !bytes.Equal(got, want) {
						t.Fatal("fixture round-trip icon changed")
					}
					// Endian and palette attributes must belong to the icon, not
					// inherit the main image's 16-bit sample width.
					if m, err := pixeldata.ExtractMetadata(firstIcon(t, back.Dataset)); err == nil && m.BitsAllocated != 8 {
						t.Fatal("icon inherited main sample width")
					}
				}
			}
		})
	}
}

// Deferred values were already outside the in-memory transcoder's contract.
// A rejected request must leave the source provider available for replay; the
// path adapter reopens/materializes the source when its streaming path sees SQ.
func TestTranscodeIconsDeferredSourceRejectedWithoutLoss(t *testing.T) {
	main, icon := codecfixture.NativeSmall().Object(), codecfixture.NativeSmall().Object()
	blob := core.NewTag(0x0029, 0x1010)
	main.Put(core.NewRawElement(blob, core.VROB, bytes.Repeat([]byte{7}, 64)))
	icon.Put(core.NewRawElement(blob, core.VROB, bytes.Repeat([]byte{9}, 64)))
	withIcon(main, icon)
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: main, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.dcm")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, readOptions := range []object.ReadFileOptions{
		{DeferPixelData: true},
		{InlineValueBytesThreshold: 1, DeferNestedValues: true, DeferValueTags: []core.Tag{blob, core.TagPixelData}},
	} {
		file, err := object.OpenFileWithOptions(path, readOptions)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless} {
			options := iconRegistries(t)
			if out, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, target, options); out != nil || !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("deferred DataSet target=%s out=%v err=%v", target.Name, out != nil, err)
			}
			if out, _, err := pixeldata.TranscodeFile(context.Background(), file, target, options); out != nil || !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("deferred File target=%s out=%v err=%v", target.Name, out != nil, err)
			}
		}
		var replay bytes.Buffer
		if err := object.WriteFile(&replay, file); err != nil || !bytes.Equal(replay.Bytes(), encoded.Bytes()) {
			t.Fatalf("deferred source lost replay after rejection: %v", err)
		}
		file.Close()
	}
	for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless} {
		destination := filepath.Join(filepath.Dir(path), target.Name+".dcm")
		if _, err := pixeldata.TranscodePath(context.Background(), path, destination, target, iconRegistries(t)); err != nil {
			t.Fatalf("path materialized fallback %s: %v", target.Name, err)
		}
		converted, err := object.OpenFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if got := iconNativeBytes(t, firstIcon(t, converted.Dataset), target, iconRegistries(t)); !bytes.Equal(got, codecfixture.NativeSmall().ExpectedFrames[0]) {
			t.Fatal("path fallback changed icon")
		}
		converted.Close()
	}
}

type iconCountingEncoder struct {
	pixeldata.FrameEncoder
	calls int
}

func (e *iconCountingEncoder) EncodeFrame(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	e.calls++
	return e.FrameEncoder.EncodeFrame(ctx, frame, m)
}

func TestTranscodeIconsLegacyRepairRejectsSameSyntaxLossyEncoder(t *testing.T) {
	options := iconRegistries(t)
	encoder, err := jpeg.NewEncoder(100)
	if err != nil {
		t.Fatal(err)
	}
	options.EncoderRegistry = pixeldata.NewMemoryEncoderRegistry()
	if err := options.EncoderRegistry.RegisterEncoder(jpeg.UID, encoder); err != nil {
		t.Fatal(err)
	}
	options.AllowLossy = true
	native := codecfixture.NativeSmall().Object()
	m, err := pixeldata.ExtractMetadata(native)
	if err != nil {
		t.Fatal(err)
	}
	// This constant is reconstructed exactly, so pixel-exact verification
	// alone cannot enforce the prohibition on newly lossy icon encodings.
	native.Put(core.NewRawElement(core.TagPixelData, core.VROB, bytes.Repeat([]byte{128}, int(m.FrameSize()))))
	icon, _, err := pixeldata.TranscodeDataSet(context.Background(), native, transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, options)
	if err != nil {
		t.Fatal(err)
	}
	source := withIcon(object.FromElements(icon.Elements(), nil), icon)
	spy := &iconCountingEncoder{FrameEncoder: encoder}
	options.EncoderRegistry = pixeldata.NewMemoryEncoderRegistry()
	if err := options.EncoderRegistry.RegisterEncoder(jpeg.UID, spy); err != nil {
		t.Fatal(err)
	}
	options.CompressIcons = true
	kept, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.JPEGBaseline, transfer.JPEGBaseline, options)
	if err != nil || kept == nil || len(report.Icons) != 1 || report.Icons[0].Action != pixeldata.IconPixelsPreservedEncapsulated || spy.calls != 0 {
		t.Fatalf("same-syntax preservation: calls=%d report=%+v err=%v", spy.calls, report, err)
	}
	// A shared producer policy may carry this RLE-specific tolerance while
	// transcoding JPEG. Any requested legacy repair selects the decode route.
	options.Recovery.RLEReversedByteSegments = true
	out, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.JPEGBaseline, transfer.JPEGBaseline, options)
	if out != nil || !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) || spy.calls != 0 {
		t.Fatalf("lossy icon encoder reached: output=%v calls=%d err=%v", out != nil, spy.calls, err)
	}
}

func TestTranscodeIconsEquivalentSyntaxPolicyAndNil(t *testing.T) {
	options := iconRegistries(t)
	options.CompressIcons = true
	encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), withIcon(codecfixture.NativeSmall().Object(), codecfixture.NativeSmall().Object()), transfer.ExplicitVRLittleEndian, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	options.EncoderRegistry = nil
	kept, report, err := pixeldata.TranscodeDataSet(context.Background(), encoded, transfer.RLELossless, transfer.RLELossless, options)
	if err != nil || kept == nil || !report.PixelDataPreserved || report.Icons[0].Action != pixeldata.IconPixelsPreservedEncapsulated {
		t.Fatalf("equivalent compressed icon report=%+v err=%v", report, err)
	}
	options.CompressIcons = false
	nativeIcons, report, err := pixeldata.TranscodeDataSet(context.Background(), encoded, transfer.RLELossless, transfer.RLELossless, options)
	if err != nil || !report.PixelDataPreserved || report.Icons[0].Action != pixeldata.IconPixelsDecompressed {
		t.Fatalf("equivalent main/native icon report=%+v err=%v", report, err)
	}
	if p, err := pixeldata.ExtractView(firstIcon(t, nativeIcons)); err != nil || p.Encapsulated {
		t.Fatal("default icon remained encapsulated on same-syntax main")
	}
	if out, _, err := pixeldata.TranscodeDataSet(context.Background(), nil, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); out != nil || !errors.Is(err, pixeldata.ErrMissingMetadata) {
		t.Fatalf("nil input=%v", err)
	}
}

func TestTranscodeIconsRejectDuplicateTagsBeforeFacade(t *testing.T) {
	for _, kind := range []string{"rows", "pixel-data", "icon-sequence"} {
		t.Run(kind, func(t *testing.T) {
			icon := codecfixture.NativeSmall().Object().ToDataSet()
			duplicateTag := core.NewTag(0x0028, 0x0010)
			if kind == "pixel-data" {
				duplicateTag = core.TagPixelData
			}
			if kind != "icon-sequence" {
				for _, element := range icon.Elements {
					if element.Tag() == duplicateTag {
						icon.Elements = append(icon.Elements, element)
						break
					}
				}
			} else {
				sequence := core.Element{Header: core.ElementHeader{Tag: iconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{codecfixture.NativeSmall().Object().ToDataSet()}}}
				icon.Elements = append(icon.Elements, sequence, sequence)
			}
			main := codecfixture.NativeSmall().Object()
			main.Put(core.Element{Header: core.ElementHeader{Tag: iconSequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon}}})
			options := iconRegistries(t)
			if out, _, err := pixeldata.TranscodeDataSet(context.Background(), main, transfer.ExplicitVRLittleEndian, transfer.RLELossless, options); out != nil || !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("duplicate %s accepted: %v", kind, err)
			}
			dir := t.TempDir()
			sourcePath, destination := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "destination.dcm")
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, &object.File{Dataset: main, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sourcePath, encoded.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := pixeldata.TranscodePath(context.Background(), sourcePath, destination, transfer.RLELossless, options); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("duplicate path err=%v", err)
			}
			if got, err := os.ReadFile(destination); err != nil || string(got) != "previous" {
				t.Fatal("duplicate input replaced destination")
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
				t.Fatal("duplicate input leaked temporary output")
			}
		})
	}
}
