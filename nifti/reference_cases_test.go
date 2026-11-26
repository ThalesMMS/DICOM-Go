package nifti_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

// Compose the file-layout and datatype contracts at the slice presentation
// boundary. Each axis changes the values, so a wrong row, slice or timepoint
// cannot produce a plausible copy of the requested plane.
func TestReferenceNIfTIScalarSliceLayouts(t *testing.T) {
	const columns, rows, slices, times = 3, 2, 2, 2
	const planeSize, voxelCount = columns * rows, columns * rows * slices * times
	datatypeCases := []struct {
		name       string
		datatype   int16
		base, step float64
	}{
		{"uint8", nifti.DatatypeUint8, 193, 1},
		{"int8", nifti.DatatypeInt8, -117, 2},
		{"uint16", nifti.DatatypeUint16, 39005, 951},
		{"int16", nifti.DatatypeInt16, -30701, 2537},
		{"uint32", nifti.DatatypeUint32, 3_300_000_513, 16_777_219},
		{"int32", nifti.DatatypeInt32, -1_500_000_257, 90_000_031},
		{"float32", nifti.DatatypeFloat32, -67.375, 6.3125},
		{"float64", nifti.DatatypeFloat64, -831.15625, 69.078125},
	}
	layouts := []struct {
		name      string
		version   int
		bigEndian bool
		paired    bool
		gzip      bool
	}{
		{name: "nifti1-little-single", version: 1},
		{name: "nifti2-big-single-gzip", version: 2, bigEndian: true, gzip: true},
		{name: "nifti1-big-pair", version: 1, bigEndian: true, paired: true},
		{name: "nifti2-little-pair-gzip", version: 2, paired: true, gzip: true},
	}
	for _, datatype := range datatypeCases {
		for _, layout := range layouts {
			t.Run(datatype.name+"/"+layout.name, func(t *testing.T) {
				ctx := context.Background()
				spec := defaultSynth()
				spec.version, spec.bigEndian, spec.paired = layout.version, layout.bigEndian, layout.paired
				spec.datatype = datatype.datatype
				spec.dims = []int64{columns, rows, slices, times}
				spec.qformCode, spec.slope, spec.inter = 1, .5, -2
				stored := make([]float64, voxelCount)
				for index := range stored {
					stored[index] = datatype.base + float64(index)*datatype.step
				}

				var volume *nifti.Volume
				var err error
				if layout.paired {
					prefix := bytes.Repeat([]byte{0x5a}, 16)
					spec.voxOffset = float64(len(prefix))
					header, voxels := synthHeaderBytes(spec), append(prefix, synthVoxels(spec, stored)...)
					if layout.gzip {
						header, voxels = gzipBytes(t, header), gzipBytes(t, voxels)
					}
					volume, err = nifti.ReadPair(ctx, bytes.NewReader(header), bytes.NewReader(voxels), nifti.ReadOptions{})
				} else {
					data := synthFile(spec, stored)
					if layout.gzip {
						data = gzipBytes(t, data)
					}
					volume, err = nifti.Read(ctx, bytes.NewReader(data), nifti.ReadOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
				if volume.Dimensions != [4]int{columns, rows, slices, times} || volume.Len() != voxelCount ||
					volume.Header.Datatype != datatype.datatype || volume.Header.Version != layout.version ||
					volume.Header.BigEndian != layout.bigEndian || volume.Header.Paired != layout.paired {
					t.Fatalf("read layout dimensions %v, header %+v", volume.Dimensions, volume.Header)
				}

				low, high := stored[0]*.5-2, stored[len(stored)-1]*.5-2
				window := render.WindowLevel{Center: (low + high) / 2, Width: high - low, Function: display.VOILinearExact}
				for timepoint := 0; timepoint < times; timepoint++ {
					for slice := 0; slice < slices; slice++ {
						start := (timepoint*slices + slice) * planeSize
						values := volume.SliceValues(slice, timepoint, []float64{-5})
						if len(values) != planeSize+1 || values[0] != -5 {
							t.Fatalf("slice %d time %d append result %v", slice, timepoint, values)
						}
						img, err := volume.SliceImage(ctx, slice, timepoint, window)
						if err != nil {
							t.Fatal(err)
						}
						if img.Bounds() != image.Rect(0, 0, columns, rows) {
							t.Fatalf("slice bounds %v", img.Bounds())
						}
						for pixel := 0; pixel < planeSize; pixel++ {
							x, y := pixel%columns, pixel/columns
							index := start + pixel
							if got := volume.Index(x, y, slice, timepoint); got != index || volume.Stored(got) != stored[index] {
								t.Fatalf("voxel (%d,%d,%d,%d) has index %d, want %d with stored %v", x, y, slice, timepoint, got, index, stored[index])
							}
							if want := stored[index]*.5 - 2; values[pixel+1] != want {
								t.Fatalf("scaled voxel (%d,%d,%d,%d) = %v, want %v", x, y, slice, timepoint, values[pixel+1], want)
							}
							const intervals = voxelCount - 1
							if got, want := img.GrayAt(x, y).Y, uint8((255*index+intervals/2)/intervals); got != want {
								t.Fatalf("display voxel (%d,%d,%d,%d) = %d, want %d", x, y, slice, timepoint, got, want)
							}
						}
					}
				}
			})
		}
	}
}

func TestReferenceNIfTISliceWorldCoordinates(t *testing.T) {
	for _, fixture := range []struct {
		name           string
		ras            [3][4]float64
		row, col, step render.Vec3
		reversed       bool
	}{
		{
			name: "axial", ras: [3][4]float64{{.625, 0, 0, 15}, {0, 1.25, 0, -23}, {0, 0, 4.5, 41.5}},
			row: render.Vec3{X: -1}, col: render.Vec3{Y: -1}, step: render.Vec3{Z: 4.5},
		},
		{
			name: "coronal", ras: [3][4]float64{{.625, 0, 0, 15}, {0, 0, -4.5, -23}, {0, 1.25, 0, 41.5}},
			row: render.Vec3{X: -1}, col: render.Vec3{Z: 1}, step: render.Vec3{Y: 4.5},
		},
		{
			name: "sagittal-reversed", ras: [3][4]float64{{0, 0, -4.5, 15}, {.625, 0, 0, -23}, {0, 1.25, 0, 41.5}},
			row: render.Vec3{Y: -1}, col: render.Vec3{Z: 1}, step: render.Vec3{X: 4.5}, reversed: true,
		},
		{
			name: "oblique-through-plane", ras: [3][4]float64{{.375, 0, -3.6, 15}, {0, 1.25, 0, -23}, {.5, 0, 2.7, 41.5}},
			row: render.Vec3{X: -.6, Z: .8}, col: render.Vec3{Y: -1}, step: render.Vec3{X: 3.6, Z: 2.7},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			spec := defaultSynth()
			spec.dims = []int64{3, 2, 4}
			spec.sformCode, spec.srow = 1, fixture.ras
			spec.pixdim = [8]float64{1, .625, 1.25, 4.5, 1, 1, 1, 1}
			volume := mustRead(t, synthFile(spec, make([]float64, 24)), nifti.ReadOptions{})
			geometries := volume.SliceGeometries()
			if len(geometries) != 4 || volume.Geometry.LeftHanded != fixture.reversed {
				t.Fatalf("slice count %d left-handed %v", len(geometries), volume.Geometry.LeftHanded)
			}
			const tolerance = 1e-6
			for k, slice := range geometries {
				wantOrigin := (render.Vec3{X: -15, Y: 23, Z: 41.5}).Add(fixture.step.Scale(float64(k)))
				if slice.Columns != 3 || slice.Rows != 2 || !vecNear(slice.Origin, wantOrigin, tolerance) ||
					!vecNear(slice.RowDir, fixture.row, tolerance) || !vecNear(slice.ColDir, fixture.col, tolerance) ||
					math.Abs(slice.ColSpacing-.625) > tolerance || math.Abs(slice.RowSpacing-1.25) > tolerance {
					t.Fatalf("slice %d geometry %+v, want origin %v row %v col %v", k, slice, wantOrigin, fixture.row, fixture.col)
				}
				corner := volume.Geometry.IndexToLPS.TransformPoint(render.Vec3{X: 2, Y: 1, Z: float64(k)})
				wantCorner := wantOrigin.Add(fixture.row.Scale(2 * .625)).Add(fixture.col.Scale(1.25))
				if !vecNear(corner, wantCorner, tolerance) {
					t.Fatalf("slice %d last voxel centre %v, want %v", k, corner, wantCorner)
				}
			}
			stack := volume.VolumeGeometry(nifti.StrictGeometryTolerances())
			if !stack.Regular || stack.ReversedInput != fixture.reversed || math.Abs(stack.MeanSpacing-4.5) > tolerance {
				t.Fatalf("stack regular %v reversed %v spacing %v", stack.Regular, stack.ReversedInput, stack.MeanSpacing)
			}
		})
	}
}

func TestReferenceNIfTIPairCompanionsStayInSourceDirectory(t *testing.T) {
	spec := defaultSynth()
	spec.paired, spec.qformCode = true, 1
	root := t.TempDir()
	for _, fixture := range []struct {
		name     string
		headerGZ bool
		imageGZ  bool
		values   []float64
	}{
		{name: "scan-a", headerGZ: true, values: []float64{7, 11, 13, 19}},
		{name: "scan-b", imageGZ: true, values: []float64{23, 29, 31, 37}},
		{name: "missing-image"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			folder := filepath.Join(root, fixture.name)
			if err := os.Mkdir(folder, 0o700); err != nil {
				t.Fatal(err)
			}
			headerName, imageName := "scalar.hdr", "scalar.img"
			header, voxels := synthHeaderBytes(spec), synthVoxels(spec, fixture.values)
			if fixture.headerGZ {
				headerName, header = headerName+".gz", gzipBytes(t, header)
			}
			if fixture.imageGZ {
				imageName, voxels = imageName+".gz", gzipBytes(t, voxels)
			}
			headerPath, imagePath := filepath.Join(folder, headerName), filepath.Join(folder, imageName)
			if err := os.WriteFile(headerPath, header, 0o600); err != nil {
				t.Fatal(err)
			}
			if fixture.values == nil {
				volume, err := nifti.ReadFile(context.Background(), headerPath, nifti.ReadOptions{})
				requireReadError(t, err, nifti.ReadCodeIO, nifti.ErrInvalidSource)
				if volume != nil {
					t.Fatal("missing local companion published a volume")
				}
				return
			}
			if err := os.WriteFile(imagePath, voxels, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, input := range []string{headerPath, imagePath} {
				volume, err := nifti.ReadFile(context.Background(), input, nifti.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if got := volumeValues(volume); !equalValues(got, fixture.values) {
					t.Fatalf("ReadFile(%s) voxels %v, want %v", filepath.Base(input), got, fixture.values)
				}
			}
		})
	}
}

func TestReferenceNIfTIExportKeepsSliceCalibrationWithGeometry(t *testing.T) {
	const throughPlane = 4.5
	first := [3]float64{15.5, -17.25, 31}
	second := [3]float64{first[0], first[1], first[2] + throughPlane}
	spacing := [2]float64{1.25, .625}
	orientation := [6]float64{.6, .8, 0, -.8, .6, 0}
	files := []*object.File{
		testSlice(t, imageSpec{position: second, spacing: spacing, orientation: orientation, pixels: []byte{7, 13}, slope: float64Pointer(.25), intercept: float64Pointer(.5)}),
		testSlice(t, imageSpec{position: first, spacing: spacing, orientation: orientation, pixels: []byte{3, 11}, slope: float64Pointer(3), intercept: float64Pointer(-17)}),
	}
	ctx := context.Background()
	t.Run("uniform-only-refuses-mixed-calibration", func(t *testing.T) {
		var output bytes.Buffer
		_, err := nifti.WriteFiles(ctx, &output, files, nifti.DefaultOptions())
		var exportErr *nifti.ExportError
		if !errors.As(err, &exportErr) || exportErr.Code != nifti.CodeScaling || !errors.Is(err, nifti.ErrUnsupportedScaling) || output.Len() != 0 {
			t.Fatalf("uniform export error %v with %d output bytes", err, output.Len())
		}
	})
	for _, fixture := range []struct {
		name     string
		policy   nifti.ScalingPolicy
		datatype int16
	}{
		{"apply-float32", nifti.ScalingApplyFloat32, nifti.DatatypeFloat32},
		{"apply-float64", nifti.ScalingApplyFloat64, nifti.DatatypeFloat64},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			options := nifti.DefaultOptions()
			options.Scaling = fixture.policy
			var output bytes.Buffer
			if _, err := nifti.WriteFiles(ctx, &output, files, options); err != nil {
				t.Fatal(err)
			}
			for _, xform := range []nifti.XformPolicy{nifti.XformQForm, nifti.XformSForm} {
				volume := mustRead(t, output.Bytes(), nifti.ReadOptions{Xform: xform})
				if volume.Header.Datatype != fixture.datatype || volume.Scaling.Applied {
					t.Fatalf("datatype %d scaling %+v", volume.Header.Datatype, volume.Scaling)
				}
				wantValues := []float64{-8, 16, 2.25, 3.75}
				wantOrigins := []render.Vec3{{X: first[0], Y: first[1], Z: first[2]}, {X: second[0], Y: second[1], Z: second[2]}}
				if got := volumeValues(volume); !equalValues(got, wantValues) {
					t.Fatalf("xform %v values %v, want %v", xform, got, wantValues)
				}
				const tolerance = 1e-6
				for k, slice := range volume.SliceGeometries() {
					if !vecNear(slice.Origin, wantOrigins[k], tolerance) ||
						!vecNear(slice.RowDir, render.Vec3{X: .6, Y: .8}, tolerance) ||
						!vecNear(slice.ColDir, render.Vec3{X: -.8, Y: .6}, tolerance) ||
						math.Abs(slice.RowSpacing-spacing[0]) > tolerance || math.Abs(slice.ColSpacing-spacing[1]) > tolerance {
						t.Fatalf("xform %v slice %d geometry %+v", xform, k, slice)
					}
				}
			}
		})
	}
}
