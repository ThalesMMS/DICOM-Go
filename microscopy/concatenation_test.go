package microscopy

import (
	"bytes"
	"errors"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestPyramidAssemblesTiledFullConcatenationOutOfOrder(t *testing.T) {
	firstRef := InstanceRef{"2.25.100", "2.25.101", "2.25.200"}
	secondRef := InstanceRef{"2.25.100", "2.25.101", "2.25.201"}
	first := slideConcatenationFixture(t, firstRef, 0, 8, nil)
	second := slideConcatenationFixture(t, secondRef, 8, 16, nil)

	var pyramid Pyramid
	if err := pyramid.Add(second); err != nil {
		t.Fatal(err)
	}
	before := pyramid.Levels[0]
	if err := pyramid.ValidateConcatenations(); !errors.Is(err, ErrInvalidSlide) {
		t.Fatalf("incomplete leading span = %v", err)
	}
	// The first member's frames are outside the only retained SOP at this point.
	if tiles := before.TilesForViewport(before.MatrixBounds(), "Zeta", 0); len(tiles) != 0 {
		t.Fatalf("partial level invented %d absent tiles", len(tiles))
	}

	if err := pyramid.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := pyramid.ValidateConcatenations(); err != nil {
		t.Fatal(err)
	}
	if len(pyramid.Levels) != 1 {
		t.Fatalf("concatenation members were published as %d resolutions", len(pyramid.Levels))
	}
	level, ok := pyramid.LevelForScale(1)
	if !ok || level.ID != before.ID || !level.ImplicitTiling || len(level.Tiles) != 0 ||
		level.Source != firstRef || level.FrameOffset != 0 || level.FrameCount != 24 {
		t.Fatalf("incorrect combined lazy level: %+v", level)
	}
	if old := before.TilesForViewport(before.MatrixBounds(), "Zeta", 0); len(old) != 0 {
		t.Fatalf("retained level observed a later insertion: %+v", old)
	}
	for pathIndex, path := range []string{"Zeta", "Alpha"} {
		for plane := 0; plane < 2; plane++ {
			tiles := level.TilesForViewport(level.MatrixBounds(), path, plane)
			if len(tiles) != 6 {
				t.Fatalf("path %s plane %d = %d tiles, want 6", path, plane, len(tiles))
			}
			for _, tile := range tiles {
				ordinal := pathIndex*12 + plane*6 + (tile.Row/2)*3 + tile.Column/2
				wantRef, wantLocal := firstRef, ordinal+1
				if ordinal >= 8 {
					wantRef, wantLocal = secondRef, ordinal-8+1
				}
				// The fixture encodes the origin as 3 μm and the slice spacing as 0.006 mm.
				if tile.Source != wantRef || tile.FrameNumber != wantLocal ||
					math.Abs(tile.ZOffsetMillimeter-(0.003+float64(plane)*0.006)) > 1e-10 {
					t.Fatalf("ordinal %d -> %+v, want source %s local frame %d", ordinal, tile, wantRef.SOPInstanceUID, wantLocal)
				}
			}
		}
	}
}

func TestPyramidRejectsInconsistentTiledFullMembersBeforePublication(t *testing.T) {
	firstRef := InstanceRef{"2.25.100", "2.25.101", "2.25.200"}
	secondRef := InstanceRef{"2.25.100", "2.25.101", "2.25.201"}
	for _, test := range []struct {
		name   string
		ref    InstanceRef
		offset int
		frames int
		edit   func(*object.Object)
	}{
		{"overlap", secondRef, 7, 17, nil},
		{"gap", secondRef, 9, 15, nil},
		{"duplicate ordinal", secondRef, 0, 8, nil},
		{"duplicate SOP", firstRef, 8, 16, nil},
		{"different series", InstanceRef{"2.25.100", "2.25.300", "2.25.201"}, 8, 16, nil},
		{"different path order", secondRef, 8, 16, func(dataset *object.Object) {
			dataset.Put(derivedio.Seq(tagOpticalPathSequence,
				derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "Alpha")),
				derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "Zeta")),
			))
		}},
		{"different orientation", secondRef, 8, 16, func(dataset *object.Object) {
			dataset.Put(derivedio.DS(tagImageOrientationSlide, 0, 1, 0, -1, 0, 0))
		}},
		{"different plane depth", secondRef, 8, 16, func(dataset *object.Object) {
			dataset.Put(derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(
				derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(
					derivedio.DS(tagPixelSpacing, 0.003, 0.001),
					derivedio.DS(core.NewTag(0x0018, 0x0088), 0.009),
				)),
			)))
		}},
		{"different declared count", secondRef, 8, 8, func(dataset *object.Object) {
			dataset.Put(derivedio.US(tagInConcatenationTotalNumber, 3))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var pyramid Pyramid
			first := slideConcatenationFixture(t, firstRef, 0, 8, nil)
			conflicting := slideConcatenationFixture(t, test.ref, test.offset, test.frames, test.edit)
			if err := pyramid.Add(first); err != nil {
				t.Fatal(err)
			}
			if err := pyramid.Add(conflicting); !errors.Is(err, ErrInvalidSlide) {
				t.Fatalf("conflict admitted: %v", err)
			}
			level := pyramid.Levels[0]
			if len(pyramid.Levels) != 1 || level.Source != firstRef || level.FrameCount != 8 ||
				len(level.TilesForViewport(image.Rect(0, 0, 5, 3), "", -1)) != 8 {
				t.Fatalf("failed addition changed the published level: %+v", level)
			}
			if err := pyramid.Add(slideConcatenationFixture(t, secondRef, 8, 16, nil)); err != nil {
				t.Fatal(err)
			}
			if err := pyramid.ValidateConcatenations(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPyramidValidatesTiledFullCoverageWithoutDeclaredPartCount(t *testing.T) {
	firstRef := InstanceRef{"2.25.100", "2.25.101", "2.25.200"}
	secondRef := InstanceRef{"2.25.100", "2.25.101", "2.25.201"}
	for _, test := range []struct {
		name     string
		frames   int
		complete bool
	}{{"trailing positions missing", 14, false}, {"exact full coverage", 16, true}} {
		t.Run(test.name, func(t *testing.T) {
			unknownTotal := func(dataset *object.Object) {
				dataset.Remove(tagInConcatenationTotalNumber)
			}
			var pyramid Pyramid
			for _, part := range []Instance{
				slideConcatenationFixture(t, secondRef, 8, test.frames, unknownTotal),
				slideConcatenationFixture(t, firstRef, 0, 8, unknownTotal),
			} {
				if err := pyramid.Add(part); err != nil {
					t.Fatal(err)
				}
			}
			if err := pyramid.ValidateConcatenations(); test.complete && err != nil ||
				!test.complete && !errors.Is(err, ErrInvalidSlide) {
				t.Fatalf("validation = %v, complete=%v", err, test.complete)
			}
			level := pyramid.Levels[0]
			if tiles := level.TilesForViewport(level.MatrixBounds(), "", -1); len(tiles) != 8+test.frames {
				t.Fatalf("materialized %d instead of %d admitted tiles", len(tiles), 8+test.frames)
			}
		})
	}
}

func TestPyramidFillsOnlyAdmittedSpansWhenMiddleMemberArrivesLast(t *testing.T) {
	var pyramid Pyramid
	for _, test := range []struct {
		ref    string
		number uint16
		offset int
		frames int
	}{{"2.25.203", 3, 15, 9}, {"2.25.201", 1, 0, 7}, {"2.25.202", 2, 7, 8}} {
		member := slideConcatenationFixture(t, InstanceRef{"2.25.100", "2.25.101", test.ref}, test.offset, test.frames,
			func(dataset *object.Object) {
				dataset.Put(derivedio.US(tagInConcatenationNumber, test.number))
				dataset.Put(derivedio.US(tagInConcatenationTotalNumber, 3))
			})
		if err := pyramid.Add(member); err != nil {
			t.Fatal(err)
		}
		if test.number != 2 {
			if err := pyramid.ValidateConcatenations(); !errors.Is(err, ErrInvalidSlide) {
				t.Fatalf("missing middle member was ignored: %v", err)
			}
		}
	}
	if err := pyramid.ValidateConcatenations(); err != nil {
		t.Fatal(err)
	}
	level := pyramid.Levels[0]
	tiles := level.TilesForViewport(level.MatrixBounds(), "Alpha", 0)
	if len(tiles) != 6 {
		t.Fatalf("plane spans yielded %d tiles", len(tiles))
	}
	for _, tile := range tiles {
		ordinal := 12 + (tile.Row/2)*3 + tile.Column/2
		wantSOP, wantFrame := "2.25.202", ordinal-7+1
		if ordinal >= 15 {
			wantSOP, wantFrame = "2.25.203", ordinal-15+1
		}
		if tile.Source.SOPInstanceUID != wantSOP || tile.FrameNumber != wantFrame {
			t.Fatalf("global frame %d -> %+v, want %s frame %d", ordinal+1, tile, wantSOP, wantFrame)
		}
	}
}

func slideConcatenationFixture(t *testing.T, ref InstanceRef, offset, frames int, edit func(*object.Object)) Instance {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(encodeSlidePart(t, ref, offset, frames)))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if edit != nil {
		edit(file.Dataset)
	}
	instance, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}
