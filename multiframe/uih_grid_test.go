package multiframe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func uihFixture(images int, block uint8, descending bool) *object.File {
	gridColumns := 1
	for gridColumns*gridColumns < images {
		gridColumns++
	}
	gridRows := (images + gridColumns - 1) / gridColumns
	rows, columns := gridRows*3, gridColumns*2
	pixels := make([]byte, rows*columns*2)
	for y := range rows {
		for x := range columns {
			tile := y/3*gridColumns + x/2
			value := uint16(tile*1000 + (y%3)*32 + x%2)
			binary.LittleEndian.PutUint16(pixels[(y*columns+x)*2:], value)
		}
	}
	var items []core.DataSet
	for k := range images {
		z := float64(k) * 2
		if descending {
			z = -z
		}
		items = append(items, derivedio.DataSet(
			derivedio.DS(tagImagePositionPatient, 10+.8*z, 20-.6*z, 30),
			derivedio.DS(tagImageOrientationPatient, .6, .8, 0, 0, 0, 1),
		))
	}
	private := func(offset uint8) core.Tag { return core.NewTag(0x0065, uint16(block)<<8|uint16(offset)) }
	dataset := object.FromElements([]core.Element{
		stringsElement(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY", "M", "VFRAME"}),
		ui(tagSOPClassUID, MRImageStorageUID), ui(tagSOPInstanceUID, "1.2.826.0.1.3680043.10.1.1318.1"),
		ui(tagStudyInstanceUID, "1.2.826.0.1.3680043.10.1.1318.2"), ui(tagSeriesInstanceUID, "1.2.826.0.1.3680043.10.1.1318.3"),
		str(tagModality, core.VRCS, "MR"), str(tagManufacturer, core.VRLO, "UIH"),
		str(tagSliceThickness, core.VRDS, "1.5"), str(tagSpacingBetweenSlices, core.VRDS, "2"),
		derivedio.DS(tagImagePositionPatient, -100, -200, -300), // grid corner must not replace item IPPs
		derivedio.DS(tagImageOrientationPatient, .6, .8, 0, 0, 0, 1),
		us(tagSamplesPerPixel, 1), str(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
		us(tagRows, uint16(rows)), us(tagColumns, uint16(columns)), derivedio.DS(tagPixelSpacing, 1.25, .75),
		us(tagBitsAllocated, 16), us(tagBitsStored, 16), us(tagHighBit, 15), us(tagPixelRepresentation, 0),
		str(core.NewTag(0x0065, uint16(block)), core.VRLO, uihCreator),
		derivedio.DS(private(0x50), float64(images)), derivedio.DS(private(0x14), -.8, .6, 0),
		derivedio.Seq(private(0x51), items...),
		core.NewRawElement(tagPixelData, core.VROW, pixels),
	}, std.Dictionary)
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
}

func uihOptions() UIHGridOptions {
	return UIHGridOptions{SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1318.4", SOPInstanceUIDForSlice: func(k int) (string, error) {
		return fmt.Sprintf("1.2.826.0.1.3680043.10.1.1318.5.%d", k+1), nil
	}}
}

func closeUIHFiles(files []*object.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func TestSplitUIHGridPixelsGeometryAndOwnership(t *testing.T) {
	for _, images := range []int{1, 4, 5} {
		for _, descending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%v", images, descending), func(t *testing.T) {
				source := uihFixture(images, 0x25, descending) // creator block is not fixed at 10
				before := source.Dataset.ToDataSet()
				var sourceWireBefore bytes.Buffer
				if err := object.WriteFile(&sourceWireBefore, source); err != nil {
					t.Fatal(err)
				}
				outputs, report, err := SplitUIHGrid(context.Background(), source, uihOptions())
				if err != nil {
					t.Fatal(err)
				}
				defer closeUIHFiles(outputs)
				if len(outputs) != images || report.Geometry.Rows != 3 || report.Geometry.Columns != 2 || report.OutputBytes <= 0 {
					t.Fatalf("outputs/report: %d %+v", len(outputs), report)
				}
				for k, output := range outputs {
					frames, err := pixeldata.ExtractNativeFramesView(output.Dataset)
					if err != nil || len(frames.Data) != 1 || frames.Metadata.Rows != 3 || frames.Metadata.Columns != 2 {
						t.Fatalf("frame %d: %v %+v", k, err, frames.Metadata)
					}
					for y := range 3 {
						for x := range 2 {
							got := binary.LittleEndian.Uint16(frames.Data[0][(y*2+x)*2:])
							if got != uint16(k*1000+y*32+x) {
								t.Fatalf("tile %d pixel %d,%d=%d", k, x, y, got)
							}
						}
					}
					position, _ := output.Dataset.GetFloats(tagImagePositionPatient)
					z := float64(k) * 2
					if descending {
						z = -z
					}
					want := []float64{10 + .8*z, 20 - .6*z, 30}
					for i := range want {
						if math.Abs(position[i]-want[i]) > 1e-9 {
							t.Fatalf("position %v want %v", position, want)
						}
					}
					orientation, _ := output.Dataset.GetFloats(tagImageOrientationPatient)
					if !reflect.DeepEqual(orientation, []float64{.6, .8, 0, 0, 0, 1}) {
						t.Fatalf("orientation %v", orientation)
					}
					for _, e := range output.Dataset.Elements() {
						if e.Tag().IsPrivate() {
							t.Fatalf("private metadata retained: %s", e.Tag())
						}
					}
					imageType, _ := output.Dataset.GetStrings(tagImageType)
					if imageType[0] != "DERIVED" || imageType[1] != "SECONDARY" || strings.Contains(strings.Join(imageType, "\\"), "VFRAME") {
						t.Fatalf("image type %v", imageType)
					}
					refs, ok := output.Dataset.GetSequence(tagSourceImageSequence)
					if !ok || len(refs) != 1 {
						t.Fatalf("source references %+v", refs)
					}
					refUID, _ := refs[0].GetString(tagReferencedSOPInstanceUID)
					if refUID != "1.2.826.0.1.3680043.10.1.1318.1" {
						t.Fatal(refUID)
					}
					var wire bytes.Buffer
					if err := object.WriteFile(&wire, output); err != nil {
						t.Fatal(err)
					}
					roundtrip, err := object.ReadFile(&wire)
					if err != nil {
						t.Fatal(err)
					}
					_ = roundtrip.Close()
				}
				if !reflect.DeepEqual(before, source.Dataset.ToDataSet()) {
					t.Fatal("source changed")
				}
				var sourceWireAfter bytes.Buffer
				if err := object.WriteFile(&sourceWireAfter, source); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(sourceWireBefore.Bytes(), sourceWireAfter.Bytes()) {
					t.Fatal("source wire changed")
				}
				// Outputs survive closing/mutating the source and do not share pixels.
				_ = source.Close()
				srcPixels, _ := source.Dataset.Get(tagPixelData)
				srcPixels.Value.(core.RawValue)[0] = 255
				outPixels, _ := outputs[0].Dataset.Get(tagPixelData)
				if outPixels.Value.(core.RawValue)[0] != 0 {
					t.Fatal("pixels alias source")
				}
				outPixels.Value.(core.RawValue)[0] = 254
				if images > 1 {
					next, _ := outputs[1].Dataset.Get(tagPixelData)
					if next.Value.(core.RawValue)[0] == 254 {
						t.Fatal("outputs alias")
					}
					manufacturer, _ := outputs[0].Dataset.Get(tagManufacturer)
					manufacturer.Value.(core.StringValue)[0] = "edited"
					if text, _ := outputs[1].Dataset.GetString(tagManufacturer); text != "UIH" {
						t.Fatal("metadata aliases another output")
					}
					if text, _ := source.Dataset.GetString(tagManufacturer); text != "UIH" {
						t.Fatal("metadata aliases source")
					}
				}
			})
		}
	}
}

func TestUIHGridRejectsAmbiguousMetadata(t *testing.T) {
	private := func(offset uint8) core.Tag { return core.NewTag(0x0065, 0x1000|uint16(offset)) }
	tests := []struct {
		name   string
		change func(*object.File)
		want   error
	}{
		{"marker", func(f *object.File) {
			f.Dataset.Put(stringsElement(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY"}))
		}, ErrNotUIHGrid},
		{"missing creator", func(f *object.File) { f.Dataset.Remove(core.NewTag(0x0065, 0x10)) }, ErrNotUIHGrid},
		{"wrong creator", func(f *object.File) { f.Dataset.Put(str(core.NewTag(0x0065, 0x10), core.VRLO, "OTHER")) }, ErrNotUIHGrid},
		{"ambiguous creator", func(f *object.File) { f.Dataset.Put(str(core.NewTag(0x0065, 0x11), core.VRLO, uihCreator)) }, ErrNonconformant},
		{"fractional count", func(f *object.File) { f.Dataset.Put(derivedio.DS(private(0x50), 4.5)) }, ErrNonconformant},
		{"hexadecimal DS", func(f *object.File) { f.Dataset.Put(str(private(0x50), core.VRDS, "0x4p0")) }, ErrNonconformant},
		{"count mismatch", func(f *object.File) { f.Dataset.Put(derivedio.DS(private(0x50), 3)) }, ErrNonconformant},
		{"zero count", func(f *object.File) { f.Dataset.Put(derivedio.DS(private(0x50), 0)) }, ErrNonconformant},
		{"bad grid", func(f *object.File) { f.Dataset.Put(us(tagColumns, 5)) }, ErrNonconformant},
		{"dimension VM", func(f *object.File) {
			f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagRows, VR: core.VRUS}, Value: core.Uint16Value{6, 6}})
		}, ErrNonconformant},
		{"missing normal", func(f *object.File) { f.Dataset.Remove(private(0x14)) }, ErrNonconformant},
		{"wrong normal", func(f *object.File) { f.Dataset.Put(derivedio.DS(private(0x14), 0, 0, 1)) }, ErrNonconformant},
		{"non-unit IOP", func(f *object.File) { f.Dataset.Put(derivedio.DS(tagImageOrientationPatient, 2, 0, 0, 0, 1, 0)) }, ErrNonconformant},
		{"missing IOP", func(f *object.File) { f.Dataset.Remove(tagImageOrientationPatient) }, ErrNonconformant},
		{"spacing", func(f *object.File) { f.Dataset.Put(derivedio.DS(tagPixelSpacing, 0, 1)) }, ErrNonconformant},
		{"slice spacing", func(f *object.File) { f.Dataset.Put(derivedio.DS(tagSpacingBetweenSlices, 3)) }, ErrNonconformant},
		{"missing sequence", func(f *object.File) { f.Dataset.Remove(private(0x51)) }, ErrNonconformant},
		{"wrong class", func(f *object.File) { f.Dataset.Put(ui(tagSOPClassUID, CTImageStorageUID)) }, ErrUnsupportedSOPClass},
		{"functional groups", func(f *object.File) { f.Dataset.Put(derivedio.Seq(tagSharedFunctionalGroupsSequence, core.DataSet{})) }, ErrNonconformant},
		{"grid frames", func(f *object.File) { f.Dataset.Put(str(tagNumberOfFrames, core.VRIS, "2")) }, ErrNonconformant},
		{"overlays", func(f *object.File) { f.Dataset.Put(us(core.NewTag(0x6000, 0x0010), 6)) }, ErrNonconformant},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := uihFixture(4, 0x10, false)
			test.change(f)
			before := f.Dataset.ToDataSet()
			outputs, report, err := SplitUIHGrid(context.Background(), f, uihOptions())
			if !errors.Is(err, test.want) || outputs != nil || !reflect.DeepEqual(report, UIHGridReport{}) {
				t.Fatalf("%v outputs=%v report=%+v", err, outputs, report)
			}
			if !reflect.DeepEqual(before, f.Dataset.ToDataSet()) {
				t.Fatal("source changed on failure")
			}
		})
	}
	for _, kind := range []string{"missing IPP", "duplicate IPP", "nonfinite IPP", "duplicate position", "frame IOP", "frame spacing"} {
		t.Run(kind, func(t *testing.T) {
			f := uihFixture(4, 0x10, false)
			e, _ := f.Dataset.Get(private(0x51))
			s := e.Value.(core.SequenceValue)
			switch kind {
			case "missing IPP":
				s.Items[1].Elements = s.Items[1].Elements[1:]
			case "duplicate IPP":
				s.Items[1].Elements = append(s.Items[1].Elements, s.Items[1].Elements[0])
			case "nonfinite IPP":
				s.Items[1].Elements[0] = stringsElement(tagImagePositionPatient, core.VRDS, []string{"NaN", "0", "0"})
			case "duplicate position":
				s.Items[1].Elements[0] = s.Items[0].Elements[0]
			case "frame IOP":
				s.Items[1].Elements[1] = derivedio.DS(tagImageOrientationPatient, 1, 0, 0, 0, 1, 0)
			case "frame spacing":
				s.Items[1].Elements = append(s.Items[1].Elements, derivedio.DS(tagPixelSpacing, 3, 4))
			}
			e.Value = s
			f.Dataset.Put(e)
			if _, err := UIHGridGeometryOf(f.Dataset, UIHGridOptions{}); !errors.Is(err, ErrNonconformant) {
				t.Fatal(err)
			}
		})
	}
}

func TestSplitUIHGridLimitsCancellationAndIdentity(t *testing.T) {
	for _, limits := range []Limits{{MaxFrames: 3}, {MaxPixels: 10}, {MaxElements: 5}, {MaxDepth: 1}, {MaxInputBytes: 4}, {MaxOutputBytes: 8}} {
		f := uihFixture(4, 0x10, false)
		opts := uihOptions()
		opts.Limits = limits
		outputs, report, err := SplitUIHGrid(context.Background(), f, opts)
		if !errors.Is(err, ErrResourceLimit) || outputs != nil || !reflect.DeepEqual(report, UIHGridReport{}) {
			t.Fatalf("limits %+v: %v %+v", limits, err, report)
		}
	}
	for _, kind := range []string{"nil callback", "same series", "series equals source SOP", "duplicate UID", "same UID", "SOP equals new series", "SOP equals source series", "callback error", "cancel callback"} {
		t.Run(kind, func(t *testing.T) {
			opts := uihOptions()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := ErrInvalidOptions
			switch kind {
			case "nil callback":
				opts.SOPInstanceUIDForSlice = nil
			case "same series":
				opts.SeriesInstanceUID = "1.2.826.0.1.3680043.10.1.1318.3"
			case "series equals source SOP":
				opts.SeriesInstanceUID = "1.2.826.0.1.3680043.10.1.1318.1"
			case "duplicate UID":
				opts.SOPInstanceUIDForSlice = func(int) (string, error) { return "1.2.3.4", nil }
			case "same UID":
				opts.SOPInstanceUIDForSlice = func(int) (string, error) { return "1.2.826.0.1.3680043.10.1.1318.1", nil }
			case "SOP equals new series":
				opts.SOPInstanceUIDForSlice = func(int) (string, error) { return opts.SeriesInstanceUID, nil }
			case "SOP equals source series":
				opts.SOPInstanceUIDForSlice = func(int) (string, error) { return "1.2.826.0.1.3680043.10.1.1318.3", nil }
			case "callback error":
				want = errors.New("identity failed")
				opts.SOPInstanceUIDForSlice = func(int) (string, error) { return "", want }
			case "cancel callback":
				want = context.Canceled
				old := opts.SOPInstanceUIDForSlice
				opts.SOPInstanceUIDForSlice = func(k int) (string, error) { cancel(); return old(k) }
			}
			outputs, report, err := SplitUIHGrid(ctx, uihFixture(4, 0x10, false), opts)
			if !errors.Is(err, want) || outputs != nil || !reflect.DeepEqual(report, UIHGridReport{}) {
				t.Fatalf("%v %v %+v", err, outputs, report)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := SplitUIHGrid(ctx, uihFixture(4, 0x10, false), uihOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSplitUIHGridCompressedSource(t *testing.T) {
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	source := uihFixture(5, 0x10, false)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	compressed, _, err := pixeldata.TranscodeFile(context.Background(), source, transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	opts := uihOptions()
	opts.DecoderRegistry = registry
	outputs, _, err := SplitUIHGrid(context.Background(), compressed, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeUIHFiles(outputs)
	last, _ := outputs[4].Dataset.Get(tagPixelData)
	if got := binary.LittleEndian.Uint16(last.Value.(core.RawValue)); got != 4000 {
		t.Fatalf("last tile %d", got)
	}
	opts.DecoderRegistry = nil
	if outputs, _, err := SplitUIHGrid(context.Background(), compressed, opts); err == nil || outputs != nil {
		t.Fatal("missing codec accepted")
	}
}

func TestUIHGridBoundsStructuralCopiesBeforeClone(t *testing.T) {
	f := uihFixture(4, 0x10, false)
	// A public SQ with empty items costs nodes even when it has no children.
	f.Dataset.Put(derivedio.Seq(core.NewTag(0x0008, 0x1110), make([]core.DataSet, 100)...))
	opts := uihOptions()
	opts.Limits.MaxElements = 200
	outputs, report, err := SplitUIHGrid(context.Background(), f, opts)
	if !errors.Is(err, ErrResourceLimit) || outputs != nil || !reflect.DeepEqual(report, UIHGridReport{}) {
		t.Fatalf("%v %+v", err, report)
	}
	f = uihFixture(4, 0x10, false)
	f.Dataset.Put(core.NewRawElement(tagImageOrientationPatient, core.VRDS, bytes.Repeat([]byte{'1'}, 10000)))
	if _, err := UIHGridGeometryOf(f.Dataset, UIHGridOptions{}); !errors.Is(err, ErrNonconformant) {
		t.Fatal(err)
	}
	f = uihFixture(4, 0x10, false)
	if _, err := UIHGridGeometryOf(f.Dataset, UIHGridOptions{Limits: Limits{MaxOutputBytes: 72*4 - 1}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("geometry allocation budget:", err)
	}
	f.Dataset.Put(str(core.NewTag(0x0065, 0x1050), core.VRDS, "9.22337203685e18"))
	if _, err := UIHGridGeometryOf(f.Dataset, UIHGridOptions{Limits: Limits{MaxFrames: int(^uint(0) >> 1)}}); err == nil {
		t.Fatal("huge count accepted")
	}
}

func TestSplitUIHGridImplicitVRAndExplicitIrregularPositions(t *testing.T) {
	source := uihFixture(5, 0x61, false)
	source.TransferSyntax = transfer.ImplicitVRLittleEndian
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, source); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&wire)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	count, _ := parsed.Dataset.Get(core.NewTag(0x0065, 0x6150))
	if count.VR() != core.VRUN {
		t.Fatalf("private DS fixture VR=%s", count.VR())
	}
	outputs, _, err := SplitUIHGrid(context.Background(), parsed, uihOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer closeUIHFiles(outputs)
	if len(outputs) != 5 {
		t.Fatal(len(outputs))
	}
	// Without advertised spacing, actual irregular per-item positions survive.
	source = uihFixture(4, 0x10, false)
	source.Dataset.Remove(tagSpacingBetweenSlices)
	e, _ := source.Dataset.Get(core.NewTag(0x0065, 0x1051))
	sq := e.Value.(core.SequenceValue)
	sq.Items[3].Elements[0] = derivedio.DS(tagImagePositionPatient, 18, 14, 30)
	e.Value = sq
	source.Dataset.Put(e)
	outputs, report, err := SplitUIHGrid(context.Background(), source, uihOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer closeUIHFiles(outputs)
	if report.Geometry.Positions[3] != [3]float64{18, 14, 30} || outputs[3].Dataset.Has(tagSpacingBetweenSlices) {
		t.Fatal("irregular geometry replaced or spacing invented")
	}
}

func TestSplitUIHGridLateFailureIsAtomicAndConcurrentRequests(t *testing.T) {
	f := uihFixture(5, 0x10, false)
	outputs, report, err := SplitUIHGrid(context.Background(), f, uihOptions())
	if err != nil {
		t.Fatal(err)
	}
	closeUIHFiles(outputs)
	opts := uihOptions()
	opts.Limits.MaxOutputBytes = report.OutputBytes - 1
	outputs, report, err = SplitUIHGrid(context.Background(), f, opts)
	var conversion *ConversionError
	if !errors.Is(err, ErrResourceLimit) || !errors.As(err, &conversion) || conversion.Stage != "output" || outputs != nil || !reflect.DeepEqual(report, UIHGridReport{}) {
		t.Fatalf("late error %v stage=%+v report=%+v", err, conversion, report)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(block int) {
			defer wg.Done()
			files, _, err := SplitUIHGrid(context.Background(), uihFixture(5, uint8(0x20+block), false), uihOptions())
			if err != nil {
				t.Error(err)
				return
			}
			defer closeUIHFiles(files)
			pixel, _ := files[4].Dataset.Get(tagPixelData)
			if binary.LittleEndian.Uint16(pixel.Value.(core.RawValue)) != 4000 {
				t.Error("concurrent pixels differ")
			}
		}(i)
	}
	wg.Wait()
}
