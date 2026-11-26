package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestWriteParametricMapPreservesCodedUnitsAndCalibratedFloatValues(t *testing.T) {
	file := quantitativeParametricMap(t)
	var output bytes.Buffer
	report, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, nifti.DefaultOptions())
	if err != nil {
		t.Fatalf("WriteFiles(Parametric Map) error = %v", err)
	}
	if report.Datatype != nifti.DatatypeFloat64 || report.ScalingPolicy != nifti.ScalingApplyFloat64 {
		t.Fatalf("encoding = datatype %d scaling %v", report.Datatype, report.ScalingPolicy)
	}
	if report.Sidecar.Units != (nifti.CodedUnits{Code: "mm", Scheme: "UCUM"}) {
		t.Fatalf("coded units = %+v", report.Sidecar.Units)
	}
	if report.Sidecar.Quantity != (nifti.CodedUnits{Code: "ADC", Scheme: "DCM"}) {
		t.Fatalf("coded quantity = %+v", report.Sidecar.Quantity)
	}
	voxels := output.Bytes()[352:]
	if len(voxels) != 16 {
		t.Fatalf("voxel bytes = %d, want 16", len(voxels))
	}
	got := []float64{
		math.Float64frombits(binary.LittleEndian.Uint64(voxels[:8])),
		math.Float64frombits(binary.LittleEndian.Uint64(voxels[8:])),
	}
	if got[0] != 3 || got[1] != 5 {
		t.Fatalf("calibrated voxels = %v, want [3 5]", got)
	}
}

func TestWriteParametricMapRejectsMixedQuantitiesWithSameUnits(t *testing.T) {
	first := quantitativeParametricMap(t)
	second := quantitativeParametricMap(t, "T1")
	var output bytes.Buffer
	_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{first, second}, nifti.DefaultOptions())
	var exportErr *nifti.ExportError
	if !errors.As(err, &exportErr) || exportErr.Code != nifti.CodePixels {
		t.Fatalf("WriteFiles(mixed quantities) error = %T %v, want CodePixels", err, err)
	}
	if output.Len() != 0 {
		t.Fatalf("mixed quantities wrote %d bytes", output.Len())
	}
}

func TestWriteParametricMapEnforcesCombinedPayloadAndFrameBufferLimit(t *testing.T) {
	file := quantitativeParametricMap(t)
	path := writePart10File(t, "parametric-map.dcm", file)
	options := nifti.DefaultOptions()
	// The fixture has eight payload bytes and needs one 8-byte value buffer plus
	// one 8-byte encoded buffer for each one-pixel frame.
	options.Limits.MaxInMemorySourceBytes = 23
	tests := []struct {
		name   string
		source nifti.Source
	}{
		{name: "borrowed file", source: nifti.NewFilesSource([]*object.File{file})},
		{name: "path source", source: nifti.NewPathSource([]string{path})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := nifti.Write(context.Background(), &output, test.source, options)
			var exportErr *nifti.ExportError
			if !errors.As(err, &exportErr) || exportErr.Code != nifti.CodeLimit || !errors.Is(err, nifti.ErrLimitExceeded) {
				t.Fatalf("Write() error = %T %v, want CodeLimit/ErrLimitExceeded", err, err)
			}
			if output.Len() != 0 {
				t.Fatalf("limited Parametric Map wrote %d bytes", output.Len())
			}
		})
	}
}

func TestWriteParametricMapCarriesNaNInfinityAndPaddingThroughNIfTIRead(t *testing.T) {
	// Two 1x2 frames of stored values: measured, NaN | +Inf, padding. The
	// Float Pixel Padding pair marks the stored range [-2000, -1000].
	file := parametricMapFixture(t, "ADC", 2, []float32{1, float32(math.NaN()), float32(math.Inf(1)), -1500},
		derivedio.FL(core.NewTag(0x0028, 0x0122), -1000),
		derivedio.FL(core.NewTag(0x0028, 0x0124), -2000),
	)
	path := writePart10File(t, "padded-parametric-map.dcm", file)
	var output bytes.Buffer
	report, err := nifti.Write(context.Background(), &output, nifti.NewPathSource([]string{path}), nifti.DefaultOptions())
	if err != nil {
		t.Fatalf("Write(padded Parametric Map) error = %v", err)
	}
	if report.UnmeasuredVoxels != (nifti.UnmeasuredVoxels{Padding: 1, NaN: 1, Infinite: 1}) {
		t.Fatalf("UnmeasuredVoxels = %+v, want one of each kind", report.UnmeasuredVoxels)
	}
	wantWarnings := []string{nifti.WarningParametricPaddingAsNaN, nifti.WarningParametricNaNPreserved, nifti.WarningParametricInfinitePreserved}
	if strings.Join(report.Warnings, ",") != strings.Join(wantWarnings, ",") ||
		strings.Join(report.Sidecar.Warnings, ",") != strings.Join(wantWarnings, ",") {
		t.Fatalf("warnings = %v sidecar %v, want %v", report.Warnings, report.Sidecar.Warnings, wantWarnings)
	}

	volume, err := nifti.Read(context.Background(), bytes.NewReader(output.Bytes()), nifti.ReadOptions{})
	if err != nil {
		t.Fatalf("nifti.Read(exported map) error = %v", err)
	}
	if volume.Dimensions != [4]int{2, 1, 2, 1} || volume.Header.Datatype != nifti.DatatypeFloat64 {
		t.Fatalf("reopened volume = dims %v datatype %d", volume.Dimensions, volume.Header.Datatype)
	}
	got := make([]float64, volume.Len())
	for index := range got {
		got[index] = volume.Value(index)
	}
	if got[0] != 3 || !math.IsNaN(got[1]) || !math.IsInf(got[2], 1) || !math.IsNaN(got[3]) {
		t.Fatalf("reopened voxels = %v, want [3 NaN +Inf NaN]", got)
	}
}

func TestWriteParametricMapCarriesOutsideMappingGapAsNaN(t *testing.T) {
	// The gap at 15 is a finite stored value. Keep it distinct from the
	// stored NaN and from padding even though all three serialize as NaN.
	file := parametricMapFixture(t, "ADC", 3, []float32{1, 15, 20, 25, float32(math.NaN()), -99},
		derivedio.FL(core.NewTag(0x0028, 0x0122), -99),
		derivedio.FL(core.NewTag(0x0028, 0x0124), -99),
	)
	mappingTag := core.NewTag(0x0040, 0x9096)
	items, ok := file.Dataset.GetSequence(mappingTag)
	if !ok || len(items) != 1 {
		t.Fatal("fixture requires one initial mapping item")
	}
	first, last := core.NewTag(0x0040, 0x9214), core.NewTag(0x0040, 0x9213)
	low := items[0]
	high := object.FromDataSet(low.ToDataSet(), nil)
	low.Put(derivedio.FD(first, 0))
	low.Put(derivedio.FD(last, 10))
	high.Put(derivedio.FD(first, 20))
	high.Put(derivedio.FD(last, 30))
	high.Put(derivedio.FD(core.NewTag(0x0040, 0x9225), 1))
	high.Put(derivedio.FD(core.NewTag(0x0040, 0x9224), 81))
	file.Dataset.Put(derivedio.Seq(mappingTag, high.ToDataSet(), low.ToDataSet()))

	path := writePart10File(t, "parametric-map-with-gap.dcm", file)
	var output bytes.Buffer
	report, err := nifti.Write(context.Background(), &output, nifti.NewPathSource([]string{path}), nifti.DefaultOptions())
	if err != nil {
		t.Fatalf("Write(Parametric Map with a gap): %v", err)
	}
	if report.UnmeasuredVoxels != (nifti.UnmeasuredVoxels{Padding: 1, NaN: 1, OutsideMapping: 1}) {
		t.Fatalf("UnmeasuredVoxels = %+v, want separate counts for padding, stored NaN and the gap", report.UnmeasuredVoxels)
	}
	wantWarnings := []string{nifti.WarningParametricPaddingAsNaN, nifti.WarningParametricNaNPreserved, nifti.WarningParametricOutsideMappingAsNaN}
	if strings.Join(report.Warnings, ",") != strings.Join(wantWarnings, ",") ||
		strings.Join(report.Sidecar.Warnings, ",") != strings.Join(wantWarnings, ",") {
		t.Fatalf("warnings = %v sidecar %v, want %v", report.Warnings, report.Sidecar.Warnings, wantWarnings)
	}

	volume, err := nifti.Read(context.Background(), bytes.NewReader(output.Bytes()), nifti.ReadOptions{})
	if err != nil {
		t.Fatalf("Read(Parametric Map with a gap): %v", err)
	}
	if volume.Dimensions != [4]int{3, 1, 2, 1} || volume.Header.Datatype != nifti.DatatypeFloat64 {
		t.Fatalf("reopened volume = dims %v datatype %d", volume.Dimensions, volume.Header.Datatype)
	}
	for index, want := range []float64{3, math.NaN(), 101, 106, math.NaN(), math.NaN()} {
		got := volume.Value(index)
		if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("reopened voxel %d = %v, want %v", index, got, want)
		}
	}
}

func TestWriteParametricMapReportsMappingOverflow(t *testing.T) {
	scale := math.Ldexp(1, 1010)
	file := parametricMapFixture(t, "ADC", 1, []float32{1, 1 << 20})
	mappingTag := core.NewTag(0x0040, 0x9096)
	items, ok := file.Dataset.GetSequence(mappingTag)
	if !ok || len(items) != 1 {
		t.Fatal("fixture requires one mapping item")
	}
	items[0].Put(derivedio.FD(core.NewTag(0x0040, 0x9225), scale))
	file.Dataset.Put(derivedio.Seq(mappingTag, items[0].ToDataSet()))

	var output bytes.Buffer
	report, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, nifti.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	wantWarnings := []string{nifti.WarningParametricMappingOverflowAsNaN}
	if report.UnmeasuredVoxels != (nifti.UnmeasuredVoxels{MappingOverflow: 1}) ||
		!slices.Equal(report.Warnings, wantWarnings) || !slices.Equal(report.Sidecar.Warnings, wantWarnings) {
		t.Fatalf("unmeasured = %+v, warnings = %v, sidecar = %v; want one mapping overflow and %q",
			report.UnmeasuredVoxels, report.Warnings, report.Sidecar.Warnings, wantWarnings)
	}

	reopened, err := nifti.Read(context.Background(), bytes.NewReader(output.Bytes()), nifti.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("reopened length = %d; want 2", reopened.Len())
	}
	if reopened.Value(0) != scale || !math.IsNaN(reopened.Value(1)) {
		t.Fatalf("reopened voxels = [%g, %g]; want [%g, NaN]", reopened.Value(0), reopened.Value(1), scale)
	}
}

func TestWriteParametricMapStillRejectsInvalidMappedValues(t *testing.T) {
	// A LUT has no entry for a stored +Inf: that voxel is invalid rather
	// than unmeasured, so the export fails instead of inventing a value.
	file := parametricMapFixture(t, "ADC", 1, []float32{0, float32(math.Inf(1))})
	file.Dataset.Put(derivedio.Seq(core.NewTag(0x0040, 0x9096), derivedio.DataSet(
		derivedio.FD(core.NewTag(0x0040, 0x9214), 0),
		derivedio.FD(core.NewTag(0x0040, 0x9213), 1),
		derivedio.FD(core.NewTag(0x0040, 0x9212), 10, 20),
		derivedio.Seq(core.NewTag(0x0040, 0x08EA), derivedio.DataSet(
			derivedio.SH(core.NewTag(0x0008, 0x0100), "mm"), derivedio.SH(core.NewTag(0x0008, 0x0102), "UCUM"),
		)),
	)))
	var output bytes.Buffer
	_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, nifti.DefaultOptions())
	var exportErr *nifti.ExportError
	if !errors.As(err, &exportErr) || exportErr.Code != nifti.CodePixels || !errors.Is(err, parametricmap.ErrMissingMapping) {
		t.Fatalf("WriteFiles(+Inf through LUT) error = %T %v, want CodePixels wrapping ErrMissingMapping", err, err)
	}
}

func quantitativeParametricMap(t *testing.T, quantityValues ...string) *object.File {
	t.Helper()
	quantity := "ADC"
	if len(quantityValues) > 0 {
		quantity = quantityValues[0]
	}
	return parametricMapFixture(t, quantity, 1, []float32{1, 2})
}

// parametricMapFixture builds a two-frame, one-row Parametric Map with the
// given columns and stored Float Pixel Data, mapped by value*2+1.
func parametricMapFixture(t *testing.T, quantity string, columns int, stored []float32, extra ...core.Element) *object.File {
	t.Helper()
	var raw []byte
	for _, value := range stored {
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], math.Float32bits(value))
		raw = append(raw, encoded[:]...)
	}
	mappingTag := core.NewTag(0x0040, 0x9096)
	unitsTag := core.NewTag(0x0040, 0x08EA)
	quantityDefinitionTag := core.NewTag(0x0040, 0x9220)
	conceptNameCodeTag := core.NewTag(0x0040, 0xA043)
	slopeTag := core.NewTag(0x0040, 0x9225)
	interceptTag := core.NewTag(0x0040, 0x9224)
	codeValueTag := core.NewTag(0x0008, 0x0100)
	codeSchemeTag := core.NewTag(0x0008, 0x0102)
	perFrame := []core.DataSet{
		derivedio.DataSet(derivedio.Seq(tagPlanePositionSequence, derivedio.DataSet(derivedio.DS(tagImagePositionPatient, 0, 0, 0)))),
		derivedio.DataSet(derivedio.Seq(tagPlanePositionSequence, derivedio.DataSet(derivedio.DS(tagImagePositionPatient, 0, 0, 2)))),
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, parametricmap.ParametricMapStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, testSeriesUID+".9000"),
		derivedio.UI(derivedio.TagStudyInstanceUID, testSeriesUID+".9102"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, testSeriesUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, testFORUID),
		derivedio.US(derivedio.TagRows, 1), derivedio.US(derivedio.TagColumns, uint16(columns)),
		derivedio.IS(derivedio.TagNumberOfFrames, 2),
		derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(
			derivedio.Seq(tagPlaneOrientationSequence, derivedio.DataSet(derivedio.DS(tagImageOrientationPatient, 1, 0, 0, 0, 1, 0))),
			derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(derivedio.DS(tagPixelSpacing, 1, 1))),
		)),
		derivedio.Seq(tagPerFrameFunctionalGroups, perFrame...),
		derivedio.Seq(mappingTag, derivedio.DataSet(
			derivedio.FD(slopeTag, 2), derivedio.FD(interceptTag, 1),
			derivedio.Seq(unitsTag, derivedio.DataSet(
				derivedio.SH(codeValueTag, "mm"), derivedio.SH(codeSchemeTag, "UCUM"),
			)),
			derivedio.Seq(quantityDefinitionTag, derivedio.DataSet(
				derivedio.Seq(conceptNameCodeTag, derivedio.DataSet(
					derivedio.SH(codeValueTag, quantity), derivedio.SH(codeSchemeTag, "DCM"),
				)),
			)),
		)),
		derivedio.Raw(core.NewTag(0x7FE0, 0x0008), core.VROF, raw),
	}
	dataset := derivedio.Object(append(elements, extra...)...)
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
}
