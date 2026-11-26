package microscopy

import (
	"errors"
	"image"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagNumberOfFocalPlanes        = core.NewTag(0x0048, 0x0013)
	tagDistanceBetweenFocalPlanes = core.NewTag(0x0048, 0x0014)
)

func TestTiledFullVolumeRequiresCompleteLayout(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*object.Object)
	}{
		{"missing organization", func(ds *object.Object) { ds.Remove(tagDimensionOrganizationType) }},
		{"sparse without positions", func(ds *object.Object) { ds.Put(derivedio.CS(tagDimensionOrganizationType, "TILED_SPARSE")) }},
		{"short standalone", func(ds *object.Object) { ds.SetIS(tagNumberOfFrames, 5) }},
		{"long standalone", func(ds *object.Object) { ds.SetIS(tagNumberOfFrames, 7) }},
		{"declared path count disagrees", func(ds *object.Object) { ds.Put(derivedio.UL(tagNumberOfOpticalPaths, 2)) }},
		{"huge grid does not overflow", func(ds *object.Object) {
			ds.Put(derivedio.UL(tagTotalPixelMatrixColumns, math.MaxUint32))
			ds.Put(derivedio.UL(tagTotalPixelMatrixRows, math.MaxUint32))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := tiledGeometryFixture(6, 1)
			test.change(file.Dataset)
			if got, err := Read(file); !errors.Is(err, ErrInvalidSlide) || len(got.Level.Tiles) != 0 || got.Level.ImplicitTiling {
				t.Fatalf("Read = %+v, %v; want no positioned tiles and ErrInvalidSlide", got, err)
			}
		})
	}
	file := tiledGeometryFixture(6, 1)
	got, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	tiles := got.Level.TilesForViewport(image.Rect(4, 2, 5, 3), "", 0)
	if len(tiles) != 1 || tiles[0].FrameNumber != 6 || tiles[0].Bounds() != image.Rect(4, 2, 5, 3) {
		t.Fatalf("right lower edge = %+v; want one clipped tile at local frame 6", tiles)
	}
}

func TestTiledFullUsesEncodedFocalPlaneSpacing(t *testing.T) {
	for _, test := range []struct {
		name string
		z    []float64
	}{
		{"encoded separately", []float64{3, 3.006}},
		{"focus stacking still single encoded plane", []float64{3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := tiledGeometryFixture(6*len(test.z), len(test.z))
			ds := file.Dataset
			if len(test.z) == 1 {
				ds.Remove(tagTotalPixelMatrixFocalPlanes)
			}
			ds.Put(derivedio.US(tagNumberOfFocalPlanes, 20))
			ds.Put(derivedio.FL(tagDistanceBetweenFocalPlanes, 19))
			got, err := Read(file)
			if err != nil {
				t.Fatal(err)
			}
			if got.Level.Origin.Z != 3 || !slices.Equal(got.FocalPlanes, test.z) || !slices.Equal(got.Level.FocalPlaneOffsets, test.z) {
				t.Fatalf("origin=%+v planes=%+v level offsets=%+v, want Z=3 mm and planes %v", got.Level.Origin, got.FocalPlanes, got.Level.FocalPlaneOffsets, test.z)
			}
			for plane, z := range test.z {
				tiles := got.Level.TilesForViewport(image.Rect(0, 0, 2, 2), "A", plane)
				if len(tiles) != 1 || tiles[0].FrameNumber != plane*6+1 || tiles[0].ZOffsetMillimeter != z {
					t.Fatalf("plane %d tiles=%+v; want frame %d at %.3f mm", plane, tiles, plane*6+1, z)
				}
			}
		})
	}
	for _, test := range []struct {
		name    string
		spacing []core.Element
	}{
		{"absent", nil},
		{"nonpositive", []core.Element{derivedio.DS(tagSpacingBetweenSlices, 0)}},
		{"negative", []core.Element{derivedio.DS(tagSpacingBetweenSlices, -0.006)}},
		{"multiple", []core.Element{derivedio.DS(tagSpacingBetweenSlices, 0.006, 0.007)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := tiledGeometryFixture(12, 2)
			file.Dataset.SetSequence(tagSharedFunctionalGroups, tiledGeometryMeasures(test.spacing...))
			file.Dataset.Put(derivedio.FL(tagDistanceBetweenFocalPlanes, 6))
			if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
				t.Fatalf("Read = %v; want invalid encoded plane spacing", err)
			}
		})
	}
}

func TestSparseTilesRequirePositionsAndUseEncodedZ(t *testing.T) {
	empty := derivedio.Object()
	for _, organization := range []string{"", "TILED_SPARSE"} {
		t.Run(organization, func(t *testing.T) {
			file := tiledGeometryFixture(2, 2)
			ds := file.Dataset
			ds.Remove(tagDimensionOrganizationType)
			if organization != "" {
				ds.Put(derivedio.CS(tagDimensionOrganizationType, organization))
			}
			ds.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(5, 3, 3006), empty)
			if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
				t.Fatalf("Read with missing second position = %v", err)
			}
			ds.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(5, 3, 3006), tiledGeometryPosition(1, 1, 3000))
			got, err := Read(file)
			if err != nil {
				t.Fatal(err)
			}
			if got.Level.ImplicitTiling || !slices.Equal(got.FocalPlanes, []float64{3, 3.006}) {
				t.Fatalf("sparse result = %+v", got)
			}
			tile := got.Level.Tiles[0]
			if tile.FrameNumber != 1 || tile.FocalPlane != 1 || tile.ZOffsetMillimeter != 3.006 || tile.Bounds() != image.Rect(4, 2, 5, 3) {
				t.Fatalf("explicit frame = %+v; want first encoded frame at (4,2), plane 1, Z=3.006 mm", tile)
			}
		})
	}
}

func TestTiledFullExplicitPositionMustAgreeWithOrder(t *testing.T) {
	file := tiledGeometryFixture(2, 1)
	file.Dataset.Put(derivedio.UL(tagTotalPixelMatrixColumns, 4))
	file.Dataset.Put(derivedio.UL(tagTotalPixelMatrixRows, 2))
	file.Dataset.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(3, 1, 3000), derivedio.Object())
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("first frame claims second tile: %v", err)
	}
	file.Dataset.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(1, 1, 3006), derivedio.Object())
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("first frame claims a different focal plane: %v", err)
	}
	file.Dataset.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(1, 1, 3000), derivedio.Object())
	got, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	tiles := got.Level.TilesForViewport(image.Rect(2, 0, 4, 2), "A", 0)
	if len(tiles) != 1 || tiles[0].FrameNumber != 2 {
		t.Fatalf("second frame legitimately omits the explicit position: %+v", tiles)
	}
}

func TestTiledFullAncillarySingleFrameMayHaveSmallerExtent(t *testing.T) {
	for _, flavor := range []string{"THUMBNAIL", "LABEL", "OVERVIEW", "LOCALIZER"} {
		t.Run(flavor, func(t *testing.T) {
			file := tiledGeometryFixture(1, 1)
			file.Dataset.Put(derivedio.Strings(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY", flavor, "NONE"}))
			got, err := Read(file)
			if err != nil {
				t.Fatal(err)
			}
			tiles := got.Level.TilesForViewport(got.Level.MatrixBounds(), "A", 0)
			if len(tiles) != 1 || tiles[0].Bounds() != image.Rect(0, 0, 2, 2) {
				t.Fatalf("ancillary frame=%+v; want single frame at its own extent", tiles)
			}
		})
	}
}

func TestTiledFullSharedPathDoesNotShrinkTheGlobalLayout(t *testing.T) {
	file := tiledGeometryFixture(6, 1)
	ds := file.Dataset
	ds.Put(derivedio.UL(tagNumberOfOpticalPaths, 2))
	ds.SetSequence(tagOpticalPathSequence, colorPathItem("A", nil), colorPathItem("B", nil))
	sharedPath := func(id string) *object.Object {
		return derivedio.Object(derivedio.Seq(tagOpticalPathIdentificationSequence, derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, id))))
	}
	ds.SetSequence(tagSharedFunctionalGroups, sharedPath("B"))
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("shared path masked an entirely absent path: %v", err)
	}
	ds.Put(derivedio.UI(tagConcatenationUID, "2.25.4610"))
	ds.Put(derivedio.US(tagInConcatenationNumber, 2))
	ds.Put(derivedio.US(tagInConcatenationTotalNumber, 2))
	ds.Put(derivedio.UL(tagConcatenationFrameOffset, 6))
	got, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Level.OpticalPathIDs, []string{"A", "B"}) ||
		len(got.Level.TilesForViewport(got.Level.MatrixBounds(), "A", 0)) != 0 ||
		len(got.Level.TilesForViewport(got.Level.MatrixBounds(), "B", 0)) != 6 {
		t.Fatalf("shared path lost global layout: %+v", got.Level)
	}
	ds.SetSequence(tagSharedFunctionalGroups, sharedPath("A"))
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("shared path contradicting local frame span: %v", err)
	}
}

func TestTiledFullRequiresUnambiguousPixelMeasures(t *testing.T) {
	file := tiledGeometryFixture(4, 2)
	ds := file.Dataset
	ds.Put(derivedio.UL(tagTotalPixelMatrixColumns, 4))
	ds.Put(derivedio.UL(tagTotalPixelMatrixRows, 2))
	measures := tiledGeometryMeasures(derivedio.DS(tagSpacingBetweenSlices, 0.006))
	ds.Remove(tagSharedFunctionalGroups)
	ds.SetSequence(tagPerFrameFunctionalGroups, measures, measures, measures, measures)
	got, err := Read(file)
	if err != nil || !slices.Equal(got.FocalPlanes, []float64{3, 3.006}) {
		t.Fatalf("consistent per-frame spacing: %v, %v", got.FocalPlanes, err)
	}
	different := tiledGeometryMeasures(derivedio.DS(tagSpacingBetweenSlices, 0.007))
	ds.SetSequence(tagPerFrameFunctionalGroups, measures, different, measures, measures)
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("inconsistent per-frame spacing: %v", err)
	}
	ds.SetSequence(tagSharedFunctionalGroups, measures)
	if _, err := Read(file); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("contradictory per-frame and shared spacing: %v", err)
	}
}

func tiledGeometryFixture(frames, planes int) *object.File {
	ds := derivedio.Object(
		derivedio.UI(tagSOPClassUID, VLWholeSlideMicroscopyImageStorage),
		derivedio.UI(tagSOPInstanceUID, "2.25.4601"),
		derivedio.UI(tagStudyInstanceUID, "2.25.4602"),
		derivedio.UI(tagSeriesInstanceUID, "2.25.4603"),
		derivedio.Strings(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY", "VOLUME", "NONE"}),
		derivedio.CS(tagDimensionOrganizationType, "TILED_FULL"),
		derivedio.UL(tagTotalPixelMatrixColumns, 5), derivedio.UL(tagTotalPixelMatrixRows, 3),
		derivedio.US(tagColumns, 2), derivedio.US(tagRows, 2),
		derivedio.IS(tagNumberOfFrames, frames),
		derivedio.UL(tagTotalPixelMatrixFocalPlanes, uint32(planes)),
		derivedio.Seq(tagOpticalPathSequence, derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "A"))),
		derivedio.Seq(tagTotalPixelMatrixOriginSequence, derivedio.DataSet(
			derivedio.DS(tagXOffsetSlide, 1), derivedio.DS(tagYOffsetSlide, 2), derivedio.DS(tagZOffsetSlide, 3000),
		)),
		derivedio.DS(tagImageOrientationSlide, 1, 0, 0, 0, 1, 0),
	)
	ds.SetSequence(tagSharedFunctionalGroups, tiledGeometryMeasures(derivedio.DS(tagSpacingBetweenSlices, 0.006)))
	return &object.File{Dataset: ds}
}

func tiledGeometryMeasures(attrs ...core.Element) *object.Object {
	measures := append([]core.Element{derivedio.DS(tagPixelSpacing, 0.003, 0.001)}, attrs...)
	return derivedio.Object(derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(measures...)))
}

func tiledGeometryPosition(column, row int32, zMicrometers float64) *object.Object {
	return derivedio.Object(derivedio.Seq(tagPlanePositionSlideSequence, derivedio.DataSet(
		derivedio.SL(tagColumnPositionTotalMatrix, column),
		derivedio.SL(tagRowPositionTotalMatrix, row),
		derivedio.DS(tagZOffsetSlide, zMicrometers),
	)))
}
