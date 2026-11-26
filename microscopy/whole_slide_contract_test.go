package microscopy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// PS3.3 C.7.6.17.3 defines spatial order across the entire concatenation.
// Retrieving an individual tile instead uses a one-based frame within its SOP.
func TestTiledFullConcatenationKeepsLocalFrameIdentity(t *testing.T) {
	const split = 8
	const frames = 24
	firstRef := InstanceRef{"2.25.100", "2.25.101", "2.25.102"}
	secondRef := InstanceRef{"2.25.100", "2.25.101", "2.25.103"}
	sources := make(map[InstanceRef][]byte)
	all := make([]Tile, 0, frames)
	keys := make(map[TileKey]bool)
	for _, part := range []struct {
		ref           InstanceRef
		offset, count int
	}{{firstRef, 0, split}, {secondRef, split, frames - split}} {
		encoded := encodeSlidePart(t, part.ref, part.offset, part.count)
		sources[part.ref] = encoded
		file, err := object.ReadFile(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		instance, err := Read(file)
		if err != nil {
			t.Fatal(err)
		}
		level := instance.Level
		if !level.ImplicitTiling || len(level.Tiles) != 0 || level.FrameOffset != part.offset {
			t.Fatalf("incorrect lazy tile metadata for %s: %+v", part.ref.SOPInstanceUID, level)
		}
		for _, tile := range level.TilesForViewport(level.MatrixBounds(), "", -1) {
			if keys[tile.Key()] {
				t.Fatalf("duplicate tile key %+v", tile.Key())
			}
			keys[tile.Key()] = true
			all = append(all, tile)
		}
	}
	if len(all) != frames {
		t.Fatalf("%d tiles across both SOP instances, want %d", len(all), frames)
	}
	type tilePosition struct {
		origin image.Point
		path   string
		plane  int
	}
	lookup := make(map[tilePosition]Tile)
	for _, tile := range all {
		position := tilePosition{tile.Bounds().Min, tile.OpticalPath, tile.FocalPlane}
		if _, exists := lookup[position]; exists {
			t.Fatalf("two frames at the same TILED_FULL position: %+v", position)
		}
		lookup[position] = tile
	}
	source := LocalTileSource{Open: func(_ context.Context, ref InstanceRef) (*object.File, error) {
		encoded, ok := sources[ref]
		if !ok {
			return nil, fmt.Errorf("unexpected source identity %+v", ref)
		}
		return object.ReadFile(bytes.NewReader(encoded))
	}}
	// The coordinate of each expected tile includes the partial right and lower edges.
	spatial := []image.Rectangle{
		image.Rect(0, 0, 2, 2), image.Rect(2, 0, 4, 2), image.Rect(4, 0, 5, 2),
		image.Rect(0, 2, 2, 3), image.Rect(2, 2, 4, 3), image.Rect(4, 2, 5, 3),
	}
	for _, block := range []struct {
		path              string
		plane, firstFrame int
	}{{"Zeta", 0, 1}, {"Zeta", 1, 7}, {"Alpha", 0, 13}, {"Alpha", 1, 19}} {
		for index, bounds := range spatial {
			global := block.firstFrame + index
			t.Run(fmt.Sprintf("global_frame_%02d", global), func(t *testing.T) {
				tile := lookup[tilePosition{bounds.Min, block.path, block.plane}]
				wantRef, wantLocal := firstRef, global
				if global > split {
					wantRef, wantLocal = secondRef, global-split
				}
				if tile.Source != wantRef || tile.FrameNumber != wantLocal || tile.Bounds() != bounds {
					t.Fatalf("tile = %+v, want %+v local frame %d at %v", tile, wantRef, wantLocal, bounds)
				}
				pixels, err := source.FetchTile(context.Background(), tile)
				if err != nil {
					t.Fatal(err)
				}
				// Distinct frame values are separated by ten codes. Tolerate one code
				// of grayscale rounding while detecting an adjacent-frame substitution.
				got := int(color.GrayModel.Convert(pixels.At(0, 0)).(color.Gray).Y)
				if want := global * 10; got < want-1 || got > want+1 {
					t.Fatalf("decoded level = %d, want %d ± 1", got, want)
				}
			})
		}
	}
}

func encodeSlidePart(t *testing.T, ref InstanceRef, offset, frames int) []byte {
	t.Helper()
	pixels := make([]byte, 0, 4*frames)
	for local := 1; local <= frames; local++ {
		value := byte((offset + local) * 10)
		pixels = append(pixels, value, value, value, value)
	}
	dataset := nativeMicroscopyPixelFixture(pixels).Dataset
	partNumber := uint16(1)
	if offset > 0 {
		partNumber = 2
	}
	for _, element := range []core.Element{
		derivedio.UI(tagSOPInstanceUID, ref.SOPInstanceUID),
		derivedio.UI(tagStudyInstanceUID, ref.StudyInstanceUID),
		derivedio.UI(tagSeriesInstanceUID, ref.SeriesInstanceUID),
		derivedio.UI(core.NewTag(0x0020, 0x9161), "2.25.104"), // Concatenation UID.
		derivedio.US(core.NewTag(0x0020, 0x9162), partNumber), // In-concatenation Number.
		derivedio.US(core.NewTag(0x0020, 0x9163), 2),          // In-concatenation Total Number.
		derivedio.Strings(core.NewTag(0x0008, 0x0008), core.VRCS, []string{"ORIGINAL", "PRIMARY", "VOLUME", "NONE"}),
		derivedio.CS(tagDimensionOrganizationType, "TILED_FULL"),
		derivedio.UL(tagConcatenationFrameOffset, uint32(offset)),
		derivedio.IS(tagNumberOfFrames, frames),
		derivedio.UL(tagTotalPixelMatrixColumns, 5),
		derivedio.UL(tagTotalPixelMatrixRows, 3),
		derivedio.UL(tagTotalPixelMatrixFocalPlanes, 2),
		derivedio.DS(tagImageOrientationSlide, 1, 0, 0, 0, 1, 0),
		derivedio.Seq(tagTotalPixelMatrixOriginSequence, derivedio.DataSet(
			derivedio.DS(tagXOffsetSlide, 1),
			derivedio.DS(tagYOffsetSlide, 2),
			derivedio.DS(tagZOffsetSlide, 3),
		)),
		derivedio.Seq(tagOpticalPathSequence,
			derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "Zeta")),
			derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "Alpha")),
		),
		derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(
			derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(
				derivedio.DS(tagPixelSpacing, 0.003, 0.001),
				derivedio.DS(core.NewTag(0x0018, 0x0088), 0.006),
			)),
		)),
	} {
		dataset.Put(element)
	}
	file, err := derivedio.File(VLWholeSlideMicroscopyImageStorage, ref.SOPInstanceUID, dataset)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestSlideViewportSelectsExactTilesAcrossBoundsAndModes(t *testing.T) {
	// Both levels occupy the same 5×8 matrix. The sparse acquisition reverses
	// frame positions and omits both the upper-left corner and several edge tiles.
	implicit := Level{
		MatrixWidth: 5, MatrixHeight: 8, TileWidth: 2, TileHeight: 3,
		FrameCount: 9, ImplicitTiling: true,
	}
	file := microscopyFixture(t, "2.25.105", 5, 8, 2, 3, []frameFixture{
		{column: 5, row: 7, path: "IHC", z: 0.002},
		{column: 3, row: 4, path: "H&E"},
		{column: 2, row: 4, path: "H&E"},
		{column: 1, row: 7, path: "IHC", z: 0.002},
	})
	file.Dataset.Put(derivedio.CS(tagDimensionOrganizationType, "TILED_SPARSE"))
	partial, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		level        Level
		rect         image.Rectangle
		path         string
		plane, limit int
		frames       []int
	}{
		{"center_horizontal_strip", implicit, image.Rect(0, 4, 5, 5), "", 0, 0, []int{4, 5, 6}},
		{"center_vertical_strip", implicit, image.Rect(2, 0, 3, 8), "", 0, 0, []int{2, 5, 8}},
		{"top_left_clipped", implicit, image.Rect(-2, -3, 3, 2), "", 0, 0, []int{1, 2}},
		{"bottom_right_clipped", implicit, image.Rect(3, 5, 12, 15), "", 0, 0, []int{5, 6, 8, 9}},
		{"right_boundary_is_exclusive", implicit, image.Rect(5, 0, 6, 8), "", 0, 0, nil},
		{"bottom_boundary_is_exclusive", implicit, image.Rect(0, 8, 5, 9), "", 0, 0, nil},
		{"unknown_optical_path", implicit, implicit.MatrixBounds(), "missing", 0, 0, nil},
		{"unknown_focal_plane", implicit, implicit.MatrixBounds(), "", 1, 0, nil},
		{"cap_keeps_center_tile", implicit, implicit.MatrixBounds(), "", 0, 1, []int{5}},
		{"sparse_hole", partial.Level, image.Rect(0, 0, 2, 3), "", -1, 0, nil},
		{"sparse_overlap_retains_both_frames", partial.Level, image.Rect(2, 3, 3, 4), "H&E", 0, 0, []int{2, 3}},
		{"sparse_other_path_other_plane", partial.Level, partial.Level.MatrixBounds(), "IHC", 1, 0, []int{1, 4}},
		{"sparse_selected_path_excludes_wrong_plane", partial.Level, partial.Level.MatrixBounds(), "IHC", 0, 0, nil},
		{"sparse_edge_uses_source_frame_number", partial.Level, image.Rect(4, 6, 5, 8), "", -1, 1, []int{1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var actual []int
			for _, tile := range test.level.TilesForViewportLimit(test.rect, test.path, test.plane, test.limit) {
				if !tile.Bounds().Overlaps(test.rect) || !tile.Bounds().In(test.level.MatrixBounds()) {
					t.Fatalf("tile %v outside requested %v or matrix %v", tile.Bounds(), test.rect, test.level.MatrixBounds())
				}
				actual = append(actual, tile.FrameNumber)
			}
			slices.Sort(actual)
			if !slices.Equal(actual, test.frames) {
				t.Errorf("got frames %v, want %v", actual, test.frames)
			}
		})
	}
}

func TestSlideGeometryCalibratesRotatedAnisotropicCoordinates(t *testing.T) {
	level := Level{
		PixelSpacingX: 0.002, PixelSpacingY: 0.008,
		Origin: SlidePoint{X: 8, Y: -1, Z: 0.5}, Orientation: [6]float64{0, 1, 0, -1, 0, 0},
	}
	for _, test := range []struct {
		name     string
		pixel    image.Point
		plane    float64
		physical SlidePoint
	}{
		{"origin", image.Pt(0, 0), 0, SlidePoint{8, -1, 0.5}},
		{"column_direction", image.Pt(10, 0), 0, SlidePoint{8, -0.98, 0.5}},
		{"row_direction", image.Pt(0, 20), 0, SlidePoint{7.84, -1, 0.5}},
		{"off_axis_other_plane", image.Pt(40, 25), 0.02, SlidePoint{7.8, -0.92, 0.52}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := level.SlideCoordinate(test.pixel, test.plane)
			if err != nil || math.Abs(got.X-test.physical.X) > 1e-10 ||
				math.Abs(got.Y-test.physical.Y) > 1e-10 || math.Abs(got.Z-test.physical.Z) > 1e-10 {
				t.Fatalf("forward = %+v, %v, want %+v", got, err, test.physical)
			}
			x, y, err := level.PixelCoordinate(test.physical)
			if err != nil || math.Abs(x-float64(test.pixel.X)) > 1e-9 || math.Abs(y-float64(test.pixel.Y)) > 1e-9 {
				t.Fatalf("inverse = (%v,%v), %v, want %v", x, y, err, test.pixel)
			}
		})
	}
	for _, test := range []struct {
		name    string
		spacing float64
	}{
		{"missing", 0}, {"negative", -0.002}, {"nan", math.NaN()}, {"infinite", math.Inf(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			uncalibrated := level
			uncalibrated.PixelSpacingX = test.spacing
			if _, err := uncalibrated.SlideCoordinate(image.Pt(10, 20), 0); !errors.Is(err, ErrUncalibratedSlide) {
				t.Errorf("forward error = %v", err)
			}
			if _, _, err := uncalibrated.PixelCoordinate(level.Origin); !errors.Is(err, ErrUncalibratedSlide) {
				t.Errorf("inverse error = %v", err)
			}
			if _, err := (Viewport{MatrixPixelsPerView: 2, ScreenWidth: 250}).ScaleBar(uncalibrated); !errors.Is(err, ErrUncalibratedSlide) {
				t.Errorf("scale bar error = %v", err)
			}
		})
	}
	scale, err := (Viewport{MatrixPixelsPerView: 2, ScreenWidth: 250}).ScaleBar(level)
	if err != nil || math.Abs(scale.Pixels*0.004-scale.Millimeters) > 1e-12 || scale.Label != "500 µm" {
		t.Fatalf("scale = %+v, %v, want calibrated 500 µm bar", scale, err)
	}
}

func TestSlideCompositionClipsCoverageAndIgnoresDisjointArrivalOrder(t *testing.T) {
	gray := image.NewGray16(image.Rect(9, 10, 12, 12))
	for x, value := range []uint16{0x0A0A, 0x2727, 0x8C8C} {
		gray.SetGray16(9+x, 10, color.Gray16{Y: value})
		gray.SetGray16(9+x, 11, color.Gray16{Y: value + 0x0808})
	}
	rgb := image.NewNRGBA(image.Rect(20, 25, 22, 27))
	red, green := color.NRGBA{R: 178, A: 255}, color.NRGBA{G: 156, A: 255}
	blue, white := color.NRGBA{B: 220, A: 255}, color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	rgb.SetNRGBA(20, 25, red)
	rgb.SetNRGBA(21, 25, green)
	rgb.SetNRGBA(20, 26, blue)
	rgb.SetNRGBA(21, 26, white)
	left := TileLayer{Tile: Tile{Row: 3, Column: 1, Width: 3, Height: 2}, Image: gray, Opacity: 1}
	right := TileLayer{Tile: Tile{Row: 3, Column: 5, Width: 2, Height: 2}, Image: rgb, Opacity: 1}
	viewport := image.Rect(2, 3, 7, 5)
	rows := [][]color.Color{
		{color.Gray{Y: 0x27}, color.Gray{Y: 0x8C}, color.NRGBA{}, red, green},
		{color.Gray{Y: 0x2F}, color.Gray{Y: 0x94}, color.NRGBA{}, blue, white},
	}
	for _, test := range []struct {
		name   string
		layers []TileLayer
	}{
		{"left_arrives_first", []TileLayer{left, right}},
		{"right_arrives_first", []TileLayer{right, left}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ComposeViewport(viewport, test.layers, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds().Size() != viewport.Size() {
				t.Fatalf("composited extent = %v, want %v", got.Bounds().Size(), viewport.Size())
			}
			for y, row := range rows {
				for x, value := range row {
					if want := color.RGBAModel.Convert(value); got.At(x, y) != want {
						t.Errorf("pixel (%d,%d) = %v, want %v", x, y, got.At(x, y), want)
					}
				}
			}
		})
	}
}

func TestSlideReadRejectsInvalidPlacementMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change core.Element
		want   error
	}{
		{"different_storage_class", derivedio.UI(tagSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"), ErrNotTiledMicroscopy},
		{"empty_matrix", derivedio.UL(tagTotalPixelMatrixColumns, 0), ErrInvalidSlide},
		{"empty_tile", derivedio.US(tagRows, 0), ErrInvalidSlide},
		{"mismatched_explicit_frame_count", derivedio.IS(tagNumberOfFrames, 2), ErrInvalidSlide},
		{"excessive_frame_count", derivedio.IS(tagNumberOfFrames, 100_000_001), ErrInvalidSlide},
		{"excessive_concatenation_offset", derivedio.UL(tagConcatenationFrameOffset, 100_000_000), ErrInvalidSlide},
		{"column_outside_matrix", derivedio.UL(tagTotalPixelMatrixColumns, 3), ErrInvalidSlide},
		{"unknown_frame_optical_path", derivedio.Seq(tagOpticalPathSequence,
			derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "IHC"))), ErrUnknownOpticalPath},
		{"duplicate_optical_path", derivedio.Seq(tagOpticalPathSequence,
			derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "H&E")),
			derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "H&E"))), ErrInvalidSlide},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := microscopyFixture(t, "2.25.106", 5, 8, 2, 3, []frameFixture{
				{column: 4, row: 1, path: "H&E"},
			})
			file.Dataset.Put(test.change)
			parsed, err := Read(file)
			if !errors.Is(err, test.want) || parsed.Level.FrameCount != 0 || len(parsed.Level.Tiles) != 0 {
				t.Fatalf("invalid source yielded frame count %d and %d tiles, error %v, want %v", parsed.Level.FrameCount, len(parsed.Level.Tiles), err, test.want)
			}
		})
	}
}
