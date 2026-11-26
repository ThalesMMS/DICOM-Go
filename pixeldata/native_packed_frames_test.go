package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestNativeOneBitFramesAcrossByteSeams(t *testing.T) {
	ctx := context.Background()
	wantFrames := [][]byte{{0x05}, {0x01}, {0x07}}
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
		vr     core.VR
		wire   []byte
	}{
		{"little endian OB", transfer.ExplicitVRLittleEndian, core.VROB, []byte{0xcd, 0xff}},
		{"little endian OW", transfer.ExplicitVRLittleEndian, core.VROW, []byte{0xcd, 0xff}},
		{"implicit little endian", transfer.ImplicitVRLittleEndian, core.VROW, []byte{0xcd, 0xff}},
		{"big endian OB", transfer.ExplicitVRBigEndian, core.VROB, []byte{0xcd, 0xff}},
		{"big endian OW", transfer.ExplicitVRBigEndian, core.VROW, []byte{0xff, 0xcd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := nativePackedTestFile(t, 3, 3, tc.syntax, tc.vr, tc.wire)
			if findings := pixeldata.MetadataValidationRule().ValidateDataSet(ctx, validation.DataSetContext{
				DataSet:   core.DataSet{Elements: file.Dataset.Elements()},
				ByteOrder: tc.syntax.ByteOrder,
			}); len(findings) != 0 {
				t.Fatalf("valid two-byte Pixel Data rejected: %v", findings)
			}
			for _, extract := range []func(*object.Object) (*pixeldata.NativeFrames, error){
				pixeldata.ExtractNativeFrames, pixeldata.ExtractNativeFramesView,
			} {
				got, err := extract(file.Dataset)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Data, wantFrames) || got.Metadata.BitsAllocated != 1 || got.Metadata.NumberOfFrames != 3 {
					t.Fatalf("extracted %+v, want three packed frames %x", got, wantFrames)
				}
			}

			pixel, err := pixeldata.ExtractView(file.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := pixeldata.DecodeFramesContext(ctx, tc.syntax.UID, pixel, file.Dataset)
			if err != nil || !reflect.DeepEqual(decoded.Data, wantFrames) {
				t.Fatalf("decoded %+v, %v, want %x", decoded, err, wantFrames)
			}

			// A byte ceiling equal to the continuous value admits the
			// result, even though the standalone frame buffers use 3 bytes.
			out, err := pixeldata.DecompressFileContext(ctx, file, pixeldata.DecompressOptions{
				Limits: pixeldata.DecompressLimits{MaxNativeBytes: 2},
			})
			if err != nil {
				t.Fatal(err)
			}
			roundTrip := roundTripDecompressedFile(t, out)
			if got := rawPixelData(t, roundTrip.Dataset); !bytes.Equal(got, []byte{0xcd, 0x01}) {
				t.Fatalf("round-trip Pixel Data = %x, want continuous little-endian cd01", got)
			}
			native, err := pixeldata.ExtractNativeFramesView(roundTrip.Dataset)
			if err != nil || !reflect.DeepEqual(native.Data, wantFrames) {
				t.Fatalf("round-trip frames = %+v, %v, want %x", native, err, wantFrames)
			}
			if got := rawPixelData(t, file.Dataset); !bytes.Equal(got, tc.wire) {
				t.Fatalf("source Pixel Data changed from %x to %x", tc.wire, got)
			}
			out, err = pixeldata.DecompressFileContext(ctx, file, pixeldata.DecompressOptions{
				Limits: pixeldata.DecompressLimits{MaxNativeBytes: 1},
			})
			var limit *pixeldata.DecompressResourceLimitError
			if out != nil || !errors.As(err, &limit) || limit.Limit != "MaxNativeBytes" || limit.Actual != 2 || limit.Maximum != 1 {
				t.Fatalf("one-byte limit: file=%v, error=%v", out, err)
			}
		})
	}
}

func TestNativeOneBitGlobalPaddingAndValidation(t *testing.T) {
	ctx := context.Background()
	syntax := transfer.ExplicitVRLittleEndian
	// Three six-bit frames have 18 significant bits. The last six bits and
	// final byte in this synthetic Part 10 value are padding.
	file := nativePackedTestFile(t, 6, 3, syntax, core.VROB, []byte{0x55, 0xf1, 0xfe, 0x9e})
	wantFrames := [][]byte{{0x15}, {0x05}, {0x2f}}
	opts := validation.Options{DataSetRules: []validation.DataSetRuleRegistration{
		{Name: "pixel-metadata", Rule: pixeldata.MetadataValidationRule()},
	}}
	result, err := validation.ValidateDataSet(ctx, core.DataSet{Elements: file.Dataset.Elements()}, opts)
	if err != nil || result.Report.Count(validation.CodePixelMetadata) != 0 {
		t.Fatalf("wire length rejected: findings=%+v, error=%v", result.Report.Findings, err)
	}
	frames, err := pixeldata.ExtractNativeFramesView(file.Dataset)
	if err != nil || !reflect.DeepEqual(frames.Data, wantFrames) {
		t.Fatalf("frames = %+v, %v, want %x", frames, err, wantFrames)
	}
	out, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rawPixelData(t, out.Dataset); !bytes.Equal(got, []byte{0x55, 0xf1, 0x02}) {
		t.Fatalf("decoded payload = %x, want 55f102", got)
	}
	if got := rawPixelData(t, roundTripDecompressedFile(t, out).Dataset); !bytes.Equal(got, []byte{0x55, 0xf1, 0x02, 0}) {
		t.Fatalf("re-encoded payload = %x, want 55f10200", got)
	}

	short := nativePackedTestFile(t, 6, 3, syntax, core.VROB, []byte{0x55, 0xf1})
	if out, err := pixeldata.ExtractNativeFramesView(short.Dataset); out != nil || !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("truncated native value: frames=%v error=%v", out, err)
	}
	bad, err := validation.ValidateDataSet(ctx, core.DataSet{Elements: short.Dataset.Elements()}, opts)
	if err != nil || bad.Report.Count(validation.CodePixelMetadata) != 1 {
		t.Fatalf("truncated value: findings=%+v, error=%v", bad.Report.Findings, err)
	}
}

func TestNativeOneBitWordLanesAcrossMultiByteFrames(t *testing.T) {
	file := nativePackedTestFile(t, 11, 3, transfer.ExplicitVRBigEndian, core.VROW,
		[]byte{0xed, 0xa5, 0x9e, 0x31, 0x70, 0x15})
	frames, err := pixeldata.ExtractNativeFramesView(file.Dataset)
	want := [][]byte{{0xa5, 0x05}, {0x3d, 0x06}, {0x78, 0x06}}
	if err != nil || !reflect.DeepEqual(frames.Data, want) {
		t.Fatalf("frames = %+v, %v, want %x", frames, err, want)
	}
	out, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rawPixelData(t, roundTripDecompressedFile(t, out).Dataset); !bytes.Equal(got, []byte{0xa5, 0xed, 0x31, 0x9e, 0x01, 0}) {
		t.Fatalf("re-encoded Pixel Data = %x", got)
	}
}

func nativePackedTestFile(t *testing.T, columns uint16, frames int, syntax transfer.Syntax, vr core.VR, raw []byte) *object.File {
	t.Helper()
	elements := append(decompressMetadataElementsWithOrder(syntax.ByteOrder, 1, columns, 1, 1, 0),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames)),
		core.NewRawElement(core.TagPixelData, vr, raw))
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return file
}
