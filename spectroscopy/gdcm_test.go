package spectroscopy

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// The opt-in fixture stays outside the repository. No patient/study/series
// identity, dates, private metadata, or fixture bytes are logged or published.
func TestGDCMSpectroscopyExternal(t *testing.T) {
	path := os.Getenv("DICOMGO_SPECTROSCOPY_GDCM")
	if path == "" {
		t.Skip("set DICOMGO_SPECTROSCOPY_GDCM to the external MR_Spectroscopy_SIEMENS_OF.dcm fixture")
	}
	shape := Dimensions{Frames: 1, VoxelRows: 16, VoxelColumns: 16, PointRows: 1, PointColumns: 1024}
	selected := map[int]uint32{0: 0x4394e1d6, 1: 0x27face04, 2: 0x4367db2b, 3: 0xc350a52d, 262144: 0x433c9f91, 524286: 0xc20184b0, 524287: 0xbedb377f}
	var previous []float32
	for _, deferred := range []bool{false, true} {
		opts := object.ReadFileOptions{MaxElementBytes: 4 << 20, MaxTotalBytes: 8 << 20}
		if deferred {
			opts.InlineValueBytesThreshold = 16 << 10
			opts.DeferValueTags = []core.Tag{tagData}
		}
		file, err := object.OpenFileWithOptions(path, opts)
		if err != nil {
			t.Fatalf("external fixture read failed: %v", err)
		}
		if file.Dataset.Has(core.TagPixelData) {
			_ = file.Close()
			t.Fatal("fixture must prove spectroscopy without Pixel Data")
		}
		element, ok := file.Dataset.Get(tagData)
		if !ok || element.VR() != core.VROF || element.Length() != 2097152 || (element.Value == nil) != deferred {
			_ = file.Close()
			t.Fatal("OF source representation/length mismatch")
		}
		data, readErr := ReadContext(context.Background(), file.Dataset, Options{MaxBytes: 2097152, MaxValues: 524288})
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("signal read/close failed: %v/%v", readErr, closeErr)
		}
		if data.Dimensions != shape || data.Representation != Complex || len(data.Values) != 524288 || data.ColumnAxis.Domain != Time || data.RowAxis != nil {
			t.Fatal("external dimensions/representation/domains mismatch")
		}
		axis := data.ColumnAxis
		if axis.SpectralWidthHz == nil || math.Abs(*axis.SpectralWidthHz-1199.9040076793856) > 1e-9 || axis.TransmitterFrequencyMHz == nil || math.Abs(*axis.TransmitterFrequencyMHz-123.255327) > 1e-9 || axis.ChemicalShiftReferencePPM == nil || math.Abs(*axis.ChemicalShiftReferencePPM-4.7) > 1e-12 {
			t.Fatal("external sampling-axis calibration mismatch")
		}
		if _, err := axis.PPM(0, 0); !errors.Is(err, ErrAxisUnavailable) {
			t.Fatal("TIME fixture must not silently become a frequency spectrum")
		}
		elapsed, err := axis.TimeOffsetSeconds(1023, 0)
		if err != nil || elapsed <= 0 || elapsed >= 1 {
			t.Fatal("incoherent time sampling interval")
		}
		finiteCount, nonzero := 0, 0
		for i, value := range data.Values {
			if finite(float64(value)) {
				finiteCount++
			}
			if value != 0 {
				nonzero++
			}
			if previous != nil && math.Float32bits(value) != math.Float32bits(previous[i]) {
				t.Fatalf("materialized/deferred mismatch at scalar index %d", i)
			}
		}
		if finiteCount != 524288 || nonzero != 524253 {
			t.Fatal("external signal finite/nonzero count mismatch")
		}
		for index, bits := range selected {
			if math.Float32bits(data.Values[index]) != bits {
				t.Fatalf("external selected OF word mismatch at scalar index %d", index)
			}
		}
		point, err := data.Point(0, 15, 15, 0, 1023)
		if err != nil || len(point) != 2 || math.Float32bits(point[0]) != selected[524286] || math.Float32bits(point[1]) != selected[524287] {
			t.Fatal("external terminal voxel/point indexing mismatch")
		}
		previous = data.Values
		t.Logf("deferred=%v frames=1 voxel_grid=16x16 spectral_grid=1x1024 representation=COMPLEX domain=TIME scalar_values=%d finite=%d nonzero=%d OF_bytes=2097152", deferred, len(data.Values), finiteCount, nonzero)
	}
}
