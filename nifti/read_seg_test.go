package nifti_test

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/seg"
)

const segTestSeriesUID = "1.2.826.0.1.3680043.10.543.1019.1"

// segReferenceSeries is a synthetic axial series: 4 columns (0.5 mm), 3 rows
// (0.75 mm), 3 slices 2.5 mm apart, supplied in shuffled order.
func segReferenceSeries() []nifti.SEGReferenceImage {
	var references []nifti.SEGReferenceImage
	for _, slice := range []int{2, 0, 1} {
		references = append(references, nifti.SEGReferenceImage{
			Geometry: render.SliceGeometry{
				Origin:     render.Vec3{X: -20, Y: 30, Z: 10 + 2.5*float64(slice)},
				RowDir:     render.Vec3{X: 1},
				ColDir:     render.Vec3{Y: 1},
				Normal:     render.Vec3{Z: 1},
				ColSpacing: 0.5,
				RowSpacing: 0.75,
				Rows:       3,
				Columns:    4,
			},
			Image: seg.ReferencedImage{
				SeriesInstanceUID: segTestSeriesUID,
				SOPClassUID:       ctStorageUID,
				SOPInstanceUID:    fmt.Sprintf("%s.%d", segTestSeriesUID, 100+slice),
			},
		})
	}
	return references
}

// segReferenceLabels is the ground truth on the DICOM grid, indexed
// [slice][row][column].
func segReferenceLabels() [3][3][4]int {
	var labels [3][3][4]int
	labels[0][0][1], labels[0][1][1], labels[0][1][2] = 1, 1, 2
	labels[1][2][3] = 5
	labels[2][0][0], labels[2][2][3] = 2, 1
	return labels
}

// labelmapFile writes a labelmap whose voxel (i, j, k) lies at
// indexToLPS*(i, j, k); dicomIndex maps a voxel to its DICOM grid index
// (column, row, slice), or ok=false when it lies outside the series.
func labelmapFile(t *testing.T, indexToLPS render.GeometryAffine, dims [3]int, datatype int16, dicomIndex func(i, j, k int) ([3]int, bool)) []byte {
	t.Helper()
	truth := segReferenceLabels()
	values := make([]float64, dims[0]*dims[1]*dims[2])
	for k := 0; k < dims[2]; k++ {
		for j := 0; j < dims[1]; j++ {
			for i := 0; i < dims[0]; i++ {
				if target, ok := dicomIndex(i, j, k); ok {
					values[i+dims[0]*(j+dims[1]*k)] = float64(truth[target[2]][target[1]][target[0]])
				}
			}
		}
	}
	spec := defaultSynth()
	spec.dims = []int64{int64(dims[0]), int64(dims[1]), int64(dims[2])}
	spec.datatype = datatype
	spec.sformCode = 1
	for row := 0; row < 3; row++ {
		sign := 1.0
		if row < 2 {
			sign = -1
		}
		for column := 0; column < 4; column++ {
			spec.srow[row][column] = sign * indexToLPS[4*row+column]
		}
	}
	for axis := 0; axis < 3; axis++ {
		spec.pixdim[axis+1] = math.Sqrt(indexToLPS[axis]*indexToLPS[axis] + indexToLPS[4+axis]*indexToLPS[4+axis] + indexToLPS[8+axis]*indexToLPS[8+axis])
	}
	return synthFile(spec, values)
}

func segOptions() nifti.SEGOptions {
	return nifti.SEGOptions{
		SOPInstanceUID:      "1.2.826.0.1.3680043.10.543.1019.2",
		StudyInstanceUID:    "1.2.826.0.1.3680043.10.543.1019.3",
		SeriesInstanceUID:   "1.2.826.0.1.3680043.10.543.1019.4",
		FrameOfReferenceUID: testFORUID,
		References:          segReferenceSeries(),
	}
}

type segFrameKey struct {
	segment int
	sop     string
}

// segFramePixels flattens a document into segment/source-image keyed sets of
// (column, row) pixels.
func segFramePixels(t *testing.T, document *seg.Document) map[segFrameKey][][2]int {
	t.Helper()
	out := map[segFrameKey][][2]int{}
	for _, frame := range document.Frames {
		key := segFrameKey{segment: frame.SegmentNumber, sop: frame.ReferencedSOPInstanceUID}
		if frame.Mask == nil {
			t.Fatalf("frame %+v has no mask", key)
		}
		for row := 0; row < frame.Mask.Rows; row++ {
			for column := 0; column < frame.Mask.Columns; column++ {
				if frame.Mask.Get(column, row) {
					out[key] = append(out[key], [2]int{column, row})
				}
			}
		}
	}
	return out
}

// expectedSEGPixels derives the expected frames from the ground truth,
// keeping pixels accepted by keep (nil keeps all) and numbering segments by
// ascending label, as SEGDocument does without definitions.
func expectedSEGPixels(keep func(column, row, slice int) bool) map[segFrameKey][][2]int {
	truth := segReferenceLabels()
	present := map[int]bool{}
	type labelled struct {
		label, slice, column, row int
	}
	var kept []labelled
	for slice := range truth {
		for row := range truth[slice] {
			for column, label := range truth[slice][row] {
				if label == 0 || (keep != nil && !keep(column, row, slice)) {
					continue
				}
				present[label] = true
				kept = append(kept, labelled{label: label, slice: slice, column: column, row: row})
			}
		}
	}
	var labels []int
	for label := range present {
		labels = append(labels, label)
	}
	sort.Ints(labels)
	numbers := map[int]int{}
	for index, label := range labels {
		numbers[label] = index + 1
	}
	out := map[segFrameKey][][2]int{}
	for _, pixel := range kept {
		key := segFrameKey{segment: numbers[pixel.label], sop: fmt.Sprintf("%s.%d", segTestSeriesUID, 100+pixel.slice)}
		out[key] = append(out[key], [2]int{pixel.column, pixel.row})
	}
	return out
}

func requireSEGPixels(t *testing.T, got, want map[segFrameKey][][2]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	for key, pixels := range want {
		if fmt.Sprint(got[key]) != fmt.Sprint(pixels) {
			t.Fatalf("frame %+v pixels = %v, want %v", key, got[key], pixels)
		}
	}
}

func lpsAffine(columns [3]render.Vec3, origin render.Vec3) render.GeometryAffine {
	return render.GeometryAffine{
		columns[0].X, columns[1].X, columns[2].X, origin.X,
		columns[0].Y, columns[1].Y, columns[2].Y, origin.Y,
		columns[0].Z, columns[1].Z, columns[2].Z, origin.Z,
		0, 0, 0, 1,
	}
}

func TestSEGDocumentPlacesLabelmapsOnTheSourceGrid(t *testing.T) {
	x := render.Vec3{X: 0.5}
	y := render.Vec3{Y: 0.75}
	z := render.Vec3{Z: 2.5}
	origin := render.Vec3{X: -20, Y: 30, Z: 10}
	identity := func(i, j, k int) ([3]int, bool) { return [3]int{i, j, k}, true }
	for _, test := range []struct {
		name    string
		affine  render.GeometryAffine
		dims    [3]int
		index   func(i, j, k int) ([3]int, bool)
		axis    [3]int
		flipped [3]bool
		offset  [3]int
		keep    func(column, row, slice int) bool
	}{
		{name: "same grid", affine: lpsAffine([3]render.Vec3{x, y, z}, origin), dims: [3]int{4, 3, 3}, index: identity},
		{
			// A RAS-reoriented copy: columns, rows and slices all run
			// backwards relative to the DICOM series.
			name:   "reoriented",
			affine: lpsAffine([3]render.Vec3{x.Scale(-1), y.Scale(-1), z.Scale(-1)}, origin.Add(x.Scale(3)).Add(y.Scale(2)).Add(z.Scale(2))),
			dims:   [3]int{4, 3, 3},
			index: func(i, j, k int) ([3]int, bool) {
				return [3]int{3 - i, 2 - j, 2 - k}, true
			},
			flipped: [3]bool{true, true, true},
			offset:  [3]int{3, 2, 2},
		},
		{
			name:   "transposed",
			affine: lpsAffine([3]render.Vec3{y, x, z}, origin),
			dims:   [3]int{3, 4, 3},
			index:  func(i, j, k int) ([3]int, bool) { return [3]int{j, i, k}, true },
			axis:   [3]int{1, 0, 2},
		},
		{
			name:   "cropped",
			affine: lpsAffine([3]render.Vec3{x, y, z}, origin.Add(x).Add(z)),
			dims:   [3]int{3, 3, 2},
			index:  func(i, j, k int) ([3]int, bool) { return [3]int{i + 1, j, k + 1}, true },
			offset: [3]int{1, 0, 1},
			// Column 0 and slice 0 lie outside the crop.
			keep: func(column, _, slice int) bool { return column >= 1 && slice >= 1 },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.axis == ([3]int{}) {
				test.axis = [3]int{0, 1, 2}
			}
			volume := mustRead(t, labelmapFile(t, test.affine, test.dims, nifti.DatatypeUint8, test.index), nifti.ReadOptions{})
			document, report, err := volume.SEGDocument(segOptions())
			if err != nil {
				t.Fatalf("SEGDocument() error = %v", err)
			}
			if report.Axis != test.axis || report.Flipped != test.flipped || report.Offset != test.offset {
				t.Fatalf("report = %+v, want axis %v flipped %v offset %v", report, test.axis, test.flipped, test.offset)
			}
			want := expectedSEGPixels(test.keep)
			requireSEGPixels(t, segFramePixels(t, document), want)

			file, err := seg.Write(document)
			if err != nil {
				t.Fatalf("seg.Write() error = %v", err)
			}
			roundTrip, err := seg.Read(file.Dataset)
			if err != nil {
				t.Fatalf("seg.Read() error = %v", err)
			}
			if roundTrip.Rows != 3 || roundTrip.Columns != 4 || roundTrip.FrameOfReferenceUID != testFORUID {
				t.Fatalf("round trip %dx%d FoR %q", roundTrip.Columns, roundTrip.Rows, roundTrip.FrameOfReferenceUID)
			}
			requireSEGPixels(t, segFramePixels(t, roundTrip), want)
			for _, frame := range roundTrip.Frames {
				reference := frame.Geometry.Origin
				if reference.X != -20 || reference.Y != 30 {
					t.Fatalf("frame geometry origin = %v", reference)
				}
			}
		})
	}
}

func TestSEGDocumentUsesSegmentDefinitions(t *testing.T) {
	x, y, z := render.Vec3{X: 0.5}, render.Vec3{Y: 0.75}, render.Vec3{Z: 2.5}
	affine := lpsAffine([3]render.Vec3{x, y, z}, render.Vec3{X: -20, Y: 30, Z: 10})
	identity := func(i, j, k int) ([3]int, bool) { return [3]int{i, j, k}, true }
	volume := mustRead(t, labelmapFile(t, affine, [3]int{4, 3, 3}, nifti.DatatypeInt16, identity), nifti.ReadOptions{})
	options := segOptions()
	options.Segments = map[int]seg.Segment{
		1: {Label: "Liver", AlgorithmType: "AUTOMATIC"},
		2: {Number: 7, Label: "Spleen", AlgorithmType: "AUTOMATIC"},
		5: {Label: "Lesion", AlgorithmType: "AUTOMATIC"},
	}
	document, report, err := volume.SEGDocument(options)
	if err != nil {
		t.Fatalf("SEGDocument() error = %v", err)
	}
	if report.SegmentNumbers[1] != 1 || report.SegmentNumbers[2] != 7 || report.SegmentNumbers[5] != 3 {
		t.Fatalf("segment numbers = %v", report.SegmentNumbers)
	}
	labels := map[int]string{}
	for _, segment := range document.Segments {
		labels[segment.Number] = segment.Label
	}
	if labels[1] != "Liver" || labels[7] != "Spleen" || labels[3] != "Lesion" {
		t.Fatalf("segments = %+v", document.Segments)
	}

	delete(options.Segments, 5)
	_, _, err = volume.SEGDocument(options)
	requireReadError(t, err, nifti.ReadCodeSEG, nifti.ErrSEGInvalidLabel)
}

func TestSEGDocumentRejectsMismatchedGridsAndLabels(t *testing.T) {
	x, y, z := render.Vec3{X: 0.5}, render.Vec3{Y: 0.75}, render.Vec3{Z: 2.5}
	origin := render.Vec3{X: -20, Y: 30, Z: 10}
	identity := func(i, j, k int) ([3]int, bool) { return [3]int{i, j, k}, true }
	rotated := func(v render.Vec3) render.Vec3 { return v.Rotate(render.Vec3{Z: 1}, math.Pi/180) }
	extended := func(i, j, k int) ([3]int, bool) {
		if i >= 4 {
			return [3]int{}, false
		}
		return [3]int{i, j, k}, true
	}

	halfVoxel := mustRead(t, labelmapFile(t, lpsAffine([3]render.Vec3{x, y, z}, origin.Add(render.Vec3{X: 0.25})), [3]int{4, 3, 3}, nifti.DatatypeUint8, identity), nifti.ReadOptions{})
	rotatedGrid := mustRead(t, labelmapFile(t, lpsAffine([3]render.Vec3{rotated(x), rotated(y), z}, origin), [3]int{4, 3, 3}, nifti.DatatypeUint8, identity), nifti.ReadOptions{})
	resampled := mustRead(t, labelmapFile(t, lpsAffine([3]render.Vec3{x.Scale(2), y, z}, origin), [3]int{2, 3, 3}, nifti.DatatypeUint8, identity), nifti.ReadOptions{})
	wider := mustRead(t, labelmapFile(t, lpsAffine([3]render.Vec3{x, y, z}, origin), [3]int{5, 3, 3}, nifti.DatatypeUint8, extended), nifti.ReadOptions{})

	outside := defaultSynth()
	outside.dims = []int64{5, 3, 3}
	outside.sformCode = 1
	outside.srow = [3][4]float64{{-0.5, 0, 0, 20}, {0, -0.75, 0, -30}, {0, 0, 2.5, 10}}
	outsideValues := make([]float64, 45)
	outsideValues[4] = 1
	outsideVolume := mustRead(t, synthFile(outside, outsideValues), nifti.ReadOptions{})

	fractional := outside
	fractional.dims = []int64{4, 3, 3}
	fractional.datatype = nifti.DatatypeFloat32
	fractionalValues := make([]float64, 36)
	fractionalValues[0] = 1.5
	fractionalVolume := mustRead(t, synthFile(fractional, fractionalValues), nifti.ReadOptions{})
	emptyVolume := mustRead(t, synthFile(fractional, make([]float64, 36)), nifti.ReadOptions{})

	noTransform := defaultSynth()
	noTransform.dims = []int64{4, 3, 3}
	noTransformVolume := mustRead(t, synthFile(noTransform, make([]float64, 36)), nifti.ReadOptions{AllowMissingTransform: true})

	irregular := segOptions()
	irregular.References[0].Geometry.Origin.Z += 0.5

	for _, test := range []struct {
		name     string
		volume   *nifti.Volume
		options  nifti.SEGOptions
		sentinel error
	}{
		{name: "half voxel shift", volume: halfVoxel, options: segOptions(), sentinel: nifti.ErrSEGGeometryMismatch},
		{name: "rotated one degree", volume: rotatedGrid, options: segOptions(), sentinel: nifti.ErrSEGGeometryMismatch},
		{name: "different spacing", volume: resampled, options: segOptions(), sentinel: nifti.ErrSEGGeometryMismatch},
		{name: "irregular reference", volume: wider, options: irregular, sentinel: nifti.ErrSEGGeometryMismatch},
		{name: "no references", volume: wider, options: nifti.SEGOptions{}, sentinel: nifti.ErrSEGGeometryMismatch},
		{name: "label outside series", volume: outsideVolume, options: segOptions(), sentinel: nifti.ErrSEGOutsideReference},
		{name: "fractional label", volume: fractionalVolume, options: segOptions(), sentinel: nifti.ErrSEGInvalidLabel},
		{name: "empty labelmap", volume: emptyVolume, options: segOptions(), sentinel: nifti.ErrSEGEmpty},
		{name: "method-1 grid", volume: noTransformVolume, options: segOptions(), sentinel: nifti.ErrSEGGeometryMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := test.volume.SEGDocument(test.options)
			requireReadError(t, err, nifti.ReadCodeSEG, test.sentinel)
		})
	}

	// Zero voxels beyond the series are allowed: only labels must land on it.
	if _, _, err := wider.SEGDocument(segOptions()); err != nil {
		t.Fatalf("zero-padded wider labelmap error = %v", err)
	}
}
