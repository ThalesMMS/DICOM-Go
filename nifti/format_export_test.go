package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
	"github.com/ThalesMMS/dicom-go/object"
)

// Exporting one validated plan as NRRD or MetaImage (#1219): the same pixel
// pass as NIfTI, framed by another header. The files are read back by the
// library's readers and must carry the plan's exact LPS affine and the very
// voxels of the NIfTI export.

// obliqueInt16Series is three 3x4 signed 16-bit slices rotated 30° about the
// slice normal, 0.7 x 0.8 mm pixels, 2.5 mm apart, away from the origin.
func obliqueInt16Series(tb testing.TB, slope, intercept *float64) []*object.File {
	tb.Helper()
	c, s := math.Cos(math.Pi/6), math.Sin(math.Pi/6)
	var files []*object.File
	for k := 2; k >= 0; k-- { // reversed input order
		pixels := make([]byte, 0, 24)
		for index := 0; index < 12; index++ {
			pixels = binary.LittleEndian.AppendUint16(pixels, uint16(int16(k*100+index*37-200)))
		}
		files = append(files, testSlice(tb, imageSpec{
			position:    [3]float64{-12.5, 40.25, -7 + 2.5*float64(k)},
			orientation: [6]float64{c, s, 0, -s, c, 0},
			spacing:     [2]float64{0.8, 0.7},
			rows:        3, columns: 4, bitsAllocated: 16, pixelRepresentation: 1,
			pixels: pixels, slope: slope, intercept: intercept,
		}))
	}
	return files
}

func temporalSeries(tb testing.TB) []*object.File {
	tb.Helper()
	return []*object.File{
		testSlice(tb, imageSpec{position: [3]float64{0, 0, 2}, pixels: []byte{2}, rows: 1, columns: 1, temporalPosition: 1, frameReferenceMilliseconds: float64Pointer(0)}),
		testSlice(tb, imageSpec{position: [3]float64{0, 0, 0}, pixels: []byte{3}, rows: 1, columns: 1, temporalPosition: 2, frameReferenceMilliseconds: float64Pointer(1500)}),
		testSlice(tb, imageSpec{position: [3]float64{0, 0, 0}, pixels: []byte{1}, rows: 1, columns: 1, temporalPosition: 1, frameReferenceMilliseconds: float64Pointer(0)}),
		testSlice(tb, imageSpec{position: [3]float64{0, 0, 2}, pixels: []byte{4}, rows: 1, columns: 1, temporalPosition: 2, frameReferenceMilliseconds: float64Pointer(1500)}),
	}
}

func TestPlanWriteVoxelsMatchesNIfTIPayloadAndLayout(t *testing.T) {
	plan, err := nifti.PlanVolume(context.Background(), nifti.NewFilesSource(obliqueInt16Series(t, nil, nil)), nifti.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var file, voxels bytes.Buffer
	report, err := plan.Write(context.Background(), &file)
	if err != nil {
		t.Fatal(err)
	}
	voxelReport, err := plan.WriteVoxels(context.Background(), &voxels)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(voxels.Bytes(), file.Bytes()[352:]) {
		t.Fatal("WriteVoxels differs from the NIfTI payload")
	}
	if voxelReport.VoxelOffset != 0 || voxelReport.BytesWritten != int64(voxels.Len()) || voxelReport.Dimensions != report.Dimensions || voxelReport.IndexToRAS != report.IndexToRAS {
		t.Fatalf("voxel report %+v, NIfTI report %+v", voxelReport, report)
	}
	layout := plan.Layout()
	if layout.Dimensions != [4]int{4, 3, 3, 1} || layout.Datatype != nifti.DatatypeInt16 || !layout.IdentityScaling() || layout.NoSpatialTransform {
		t.Fatalf("layout %+v", layout)
	}
	// The NIfTI affine is the layout's, converted to RAS.
	for row := 0; row < 3; row++ {
		sign := 1.0
		if row < 2 {
			sign = -1
		}
		for column := 0; column < 4; column++ {
			if got, want := report.IndexToRAS[4*row+column], sign*layout.IndexToLPS[4*row+column]; got != want {
				t.Fatalf("IndexToRAS[%d,%d] = %v, want %v", row, column, got, want)
			}
		}
	}
	if math.Abs(layout.Spacing[0]-0.7) > 1e-12 || math.Abs(layout.Spacing[1]-0.8) > 1e-12 || math.Abs(layout.Spacing[2]-2.5) > 1e-12 {
		t.Fatalf("spacing %v", layout.Spacing)
	}
}

// readBack decodes an exported NRRD or MetaImage file.
type readBack func(t *testing.T, data []byte) *nifti.Volume

func readNRRD(t *testing.T, data []byte) *nifti.Volume {
	t.Helper()
	image, err := nrrd.Read(context.Background(), bytes.NewReader(data), nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return image.Volume
}

func readMetaImage(t *testing.T, data []byte) *nifti.Volume {
	t.Helper()
	image, err := metaimage.Read(context.Background(), bytes.NewReader(data), metaimage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return image.Volume
}

func TestPlanExportsNRRDAndMetaImageWithIdenticalGeometry(t *testing.T) {
	formats := []struct {
		name string
		// tolerance bounds the affine difference: NRRD stores the affine
		// columns, MetaImage unit directions times spacings.
		tolerance float64
		write     func(context.Context, *bytes.Buffer, *nifti.Plan) (nifti.Report, error)
		read      readBack
	}{
		{"nrrd gzip", 0, func(ctx context.Context, w *bytes.Buffer, p *nifti.Plan) (nifti.Report, error) {
			return nrrd.WritePlan(ctx, w, p, nrrd.WriteOptions{})
		}, readNRRD},
		{"nrrd raw", 0, func(ctx context.Context, w *bytes.Buffer, p *nifti.Plan) (nifti.Report, error) {
			return nrrd.WritePlan(ctx, w, p, nrrd.WriteOptions{Raw: true})
		}, readNRRD},
		{"metaimage", 1e-12, func(ctx context.Context, w *bytes.Buffer, p *nifti.Plan) (nifti.Report, error) {
			return metaimage.WritePlan(ctx, w, p, metaimage.WriteOptions{})
		}, readMetaImage},
		{"metaimage zlib", 1e-12, func(ctx context.Context, w *bytes.Buffer, p *nifti.Plan) (nifti.Report, error) {
			return metaimage.WritePlan(ctx, w, p, metaimage.WriteOptions{Compress: true, TempDir: t.TempDir()})
		}, readMetaImage},
	}
	series := []struct {
		name    string
		files   func(testing.TB) []*object.File
		options func() nifti.Options
	}{
		{"oblique int16", func(tb testing.TB) []*object.File { return obliqueInt16Series(tb, nil, nil) }, nifti.DefaultOptions},
		{"applied float32 scaling", func(tb testing.TB) []*object.File {
			return obliqueInt16Series(tb, float64Pointer(0.5), float64Pointer(-1024))
		}, func() nifti.Options {
			options := nifti.DefaultOptions()
			options.Scaling = nifti.ScalingApplyFloat32
			return options
		}},
		{"temporal", temporalSeries, nifti.DefaultOptions},
	}
	for _, fixture := range patientOrientationFixtures() {
		fixture := fixture
		series = append(series, struct {
			name    string
			files   func(testing.TB) []*object.File
			options func() nifti.Options
		}{fixture.name, func(tb testing.TB) []*object.File { return orientationVolume(tb, fixture) }, nifti.DefaultOptions})
	}
	for _, s := range series {
		for _, format := range formats {
			t.Run(s.name+"/"+format.name, func(t *testing.T) {
				ctx := context.Background()
				plan, err := nifti.PlanVolume(ctx, nifti.NewFilesSource(s.files(t)), s.options())
				if err != nil {
					t.Fatal(err)
				}
				var reference bytes.Buffer
				if _, err := plan.Write(ctx, &reference); err != nil {
					t.Fatal(err)
				}
				want, err := nifti.Read(ctx, &reference, nifti.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				var encoded bytes.Buffer
				report, err := format.write(ctx, &encoded, plan)
				if err != nil {
					t.Fatal(err)
				}
				if report.BytesWritten != int64(encoded.Len()) || report.Dimensions != plan.Layout().Dimensions {
					t.Fatalf("report bytes %d dims %v, file %d bytes", report.BytesWritten, report.Dimensions, encoded.Len())
				}
				got := format.read(t, encoded.Bytes())
				if got.Dimensions != want.Dimensions || got.Header.Datatype != want.Header.Datatype {
					t.Fatalf("dimensions %v datatype %d, want %v %d", got.Dimensions, got.Header.Datatype, want.Dimensions, want.Header.Datatype)
				}
				for i := 0; i < want.Len(); i++ {
					if g, w := got.Stored(i), want.Value(i); g != w && !(math.IsNaN(g) && math.IsNaN(w)) {
						t.Fatalf("voxel %d = %v, want %v", i, g, w)
					}
				}
				exact := plan.Layout().IndexToLPS
				for i := range exact {
					if math.Abs(got.Geometry.IndexToLPS[i]-exact[i]) > format.tolerance {
						t.Fatalf("IndexToLPS %v, want the plan's %v", got.Geometry.IndexToLPS, exact)
					}
					// The NIfTI file stores the same grid in float32.
					if math.Abs(got.Geometry.IndexToLPS[i]-want.Geometry.IndexToLPS[i]) > 1e-5 {
						t.Fatalf("IndexToLPS %v, NIfTI %v", got.Geometry.IndexToLPS, want.Geometry.IndexToLPS)
					}
				}
			})
		}
	}
}

func TestPlanExportsRefuseUnrepresentableScaling(t *testing.T) {
	plan, err := nifti.PlanVolume(context.Background(), nifti.NewFilesSource(obliqueInt16Series(t, float64Pointer(1), float64Pointer(-1024))), nifti.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if layout := plan.Layout(); layout.IdentityScaling() || layout.ScalingIntercept != -1024 {
		t.Fatalf("layout scaling %+v", layout)
	}
	var sink bytes.Buffer
	if _, err := nrrd.WritePlan(context.Background(), &sink, plan, nrrd.WriteOptions{}); !errors.Is(err, nrrd.ErrUnsupported) {
		t.Fatalf("NRRD with scl_inter: %v", err)
	}
	if _, err := metaimage.WritePlan(context.Background(), &sink, plan, metaimage.WriteOptions{}); !errors.Is(err, metaimage.ErrUnsupported) {
		t.Fatalf("MetaImage with scl_inter: %v", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("refused exports wrote %d bytes", sink.Len())
	}
}

func TestPlanExportsPropagateCancellation(t *testing.T) {
	plan, err := nifti.PlanVolume(context.Background(), nifti.NewFilesSource(obliqueInt16Series(t, nil, nil)), nifti.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var sink bytes.Buffer
	for name, write := range map[string]func() error{
		"nrrd": func() error { _, err := nrrd.WritePlan(ctx, &sink, plan, nrrd.WriteOptions{}); return err },
		"metaimage": func() error {
			_, err := metaimage.WritePlan(ctx, &sink, plan, metaimage.WriteOptions{Compress: true, TempDir: t.TempDir()})
			return err
		},
	} {
		var exportErr *nifti.ExportError
		if err := write(); !errors.As(err, &exportErr) || exportErr.Code != nifti.CodeCanceled {
			t.Fatalf("%s: canceled export = %v", name, err)
		}
	}
}
