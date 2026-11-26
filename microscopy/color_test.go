package microscopy

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/icc"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

func colorPathItem(identifier string, profile []byte) *object.Object {
	return derivedio.Object(derivedio.SH(tagOpticalPathIdentifier, identifier), derivedio.Raw(tagICCProfile, core.VROB, profile))
}
func TestOpticalPathICCInterleavedFramesOrderAndIndependentNeutralOracle(t *testing.T) {
	linear := icc.AdobeRGB1998
	linear.Gamma = 1
	for _, reversed := range []bool{false, true} {
		file := microscopyFixture(t, "slide", 3, 1, 1, 1, []frameFixture{{column: 1, row: 1, path: "H&E"}, {column: 2, row: 1, path: "IHC"}, {column: 3, row: 1, path: "H&E"}})
		a, b := colorPathItem("H&E", linear.Build()), colorPathItem("IHC", icc.AdobeRGB1998.Build())
		if reversed {
			a, b = b, a
		}
		file.Dataset.SetSequence(tagOpticalPathSequence, a, b)
		i, err := Read(file)
		if err != nil {
			t.Fatal(err)
		}
		resolver := NewColorResolver(nil)
		if err := resolver.Add(i); err != nil {
			t.Fatal(err)
		}
		shared := image.NewNRGBA(image.Rect(0, 0, 1, 1))
		shared.SetNRGBA(0, 0, color.NRGBA{128, 128, 128, 255})
		source := ColorManagedTileSource{Colors: resolver, Source: TileSourceFunc(func(context.Context, Tile) (image.Image, error) { return shared, nil })}
		for frame, want := range []uint8{188, 129, 188} {
			result := resolver.ResolveFrame(i, frame+1)
			if result.Diagnostic.State != ColorConverted {
				t.Fatalf("frame=%d result=%+v", frame+1, result)
			}
			got, err := source.FetchTile(context.Background(), i.Level.Tiles[frame])
			if err != nil {
				t.Fatal(err)
			}
			rgb := color.NRGBAModel.Convert(got.At(0, 0)).(color.NRGBA)
			// For a neutral D50-adapted RGB input the independent IEC sRGB
			// transfer formula gives 188 (linear) and 129 (gamma 563/256).
			// One 8-bit code allows ICC s15Fixed16 colorant quantization.
			for _, v := range []uint8{rgb.R, rgb.G, rgb.B} {
				if int(v)-int(want) > 1 || int(want)-int(v) > 1 {
					t.Fatalf("frame%d=%v want neutral%d", frame+1, rgb, want)
				}
			}
		}
		if shared.NRGBAAt(0, 0) != (color.NRGBA{128, 128, 128, 255}) {
			t.Fatal("source pixels mutated")
		}
		if stats := resolver.CacheStats(); stats.Builds != 2 || stats.Entries != 2 {
			t.Fatalf("tile conversions rebuilt profiles: %+v", stats)
		}
	}
}

func TestOpticalPathICCFullDepthBeforePathAndSharedIdentification(t *testing.T) {
	file := &object.File{Dataset: derivedio.Object(
		derivedio.UI(tagSOPClassUID, VLWholeSlideMicroscopyImageStorage), derivedio.UI(tagSOPInstanceUID, "full"),
		derivedio.UL(tagTotalPixelMatrixColumns, 2), derivedio.UL(tagTotalPixelMatrixRows, 1), derivedio.US(tagColumns, 1), derivedio.US(tagRows, 1),
		derivedio.IS(tagNumberOfFrames, 8), derivedio.CS(tagDimensionOrganizationType, "TILED_FULL"), derivedio.UL(tagTotalPixelMatrixFocalPlanes, 2),
	)}
	file.Dataset.SetSequence(tagOpticalPathSequence, colorPathItem("non-numeric-B", icc.AdobeRGB1998.Build()), colorPathItem("A", icc.AdobeRGB1998.Build()))
	file.Dataset.SetSequence(tagSharedFunctionalGroups, tiledGeometryMeasures(derivedio.DS(tagSpacingBetweenSlices, 0.006)))
	i, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewColorResolver(nil)
	if err := resolver.Add(i); err != nil {
		t.Fatal(err)
	}
	for frame := 1; frame <= 8; frame++ {
		want := "non-numeric-B"
		if frame > 4 {
			want = "A"
		}
		if id, err := i.FrameOpticalPath(frame); err != nil || id != want {
			t.Fatalf("frame%d=%q err%v want%q", frame, id, err, want)
		}
	}
	for pathIndex, id := range []string{"non-numeric-B", "A"} {
		for plane := 0; plane < 2; plane++ {
			tiles := i.Level.TilesForViewport(image.Rect(0, 0, 2, 1), id, plane)
			if len(tiles) != 2 {
				t.Fatalf("tiles=%v", tiles)
			}
			for _, tile := range tiles {
				want := pathIndex*4 + plane*2 + tile.Column + 1
				if tile.FrameNumber != want {
					t.Fatalf("tile=%+v framewant%d", tile, want)
				}
			}
		}
	}
	if resolver.ResolveFrame(i, 1).Transform != resolver.ResolveFrame(i, 8).Transform || resolver.CacheStats().Builds != 1 {
		t.Fatal("identical profiles not shared across paths")
	}
	// Shared identification can refer to a declared path which is not first.
	file.Dataset.Put(derivedio.CS(tagDimensionOrganizationType, "TILED_SPARSE"))
	file.Dataset.SetSequence(tagSharedFunctionalGroups, derivedio.Object(derivedio.Seq(tagOpticalPathIdentificationSequence, derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "A")))))
	file.Dataset.SetSequence(tagPerFrameFunctionalGroups, tiledGeometryPosition(1, 1, 0), tiledGeometryPosition(2, 1, 0))
	file.Dataset.SetIS(tagNumberOfFrames, 2)
	i, err = Read(file)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := i.FrameOpticalPath(2); err != nil || id != "A" {
		t.Fatalf("shared id=%q err=%v", id, err)
	}
}

func TestOpticalPathICCTypedErrorsNoRootFallbackAndAdmission(t *testing.T) {
	file := microscopyFixture(t, "invalid", 1, 1, 1, 1, []frameFixture{{column: 1, row: 1, path: "H&E"}})
	file.Dataset.SetSequence(tagOpticalPathSequence, colorPathItem("H&E", nil), colorPathItem("IHC", []byte("invalid")))
	i, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewColorResolver(nil)
	if err := resolver.Add(i); err != nil {
		t.Fatal(err)
	}
	if r := resolver.ResolveFrame(i, 1); r.Transform != nil || !errors.Is(r.Diagnostic, ErrMissingOpticalPathProfile) {
		t.Fatalf("missing=%+v", r)
	}
	if r := resolver.ResolveTile(Tile{Source: i.Level.Source, OpticalPath: "IHC"}); r.Transform != nil || !errors.Is(r.Diagnostic, icc.ErrInvalidProfile) {
		t.Fatalf("invalid=%+v", r)
	}
	if r := resolver.ResolveTile(Tile{Source: i.Level.Source, OpticalPath: "ghost"}); !errors.Is(r.Diagnostic, ErrUnknownOpticalPath) {
		t.Fatalf("unknown=%+v", r)
	}
	if r := resolver.ResolveTile(Tile{Source: i.Level.Source}); !errors.Is(r.Diagnostic, ErrMissingOpticalPath) {
		t.Fatalf("missing reference=%+v", r)
	}
	unknown := microscopyFixture(t, "unknown", 1, 1, 1, 1, []frameFixture{{column: 1, row: 1, path: "ghost"}})
	if _, err := Read(unknown); !errors.Is(err, ErrUnknownOpticalPath) {
		t.Fatalf("unknown frame read=%v", err)
	}
	file.Dataset.SetSequence(tagOpticalPathSequence, colorPathItem("H&E", make([]byte, MaxOpticalPathProfileBytes+1)))
	if _, err := Read(file); !errors.Is(err, ErrColorResourceLimit) {
		t.Fatalf("profile resource=%v", err)
	}
	// Same identifier in another instance must not alias the first source.
	other := i
	other.Level.Source.SOPInstanceUID = "other"
	other.OpticalPaths = []OpticalPath{{Identifier: "H&E", ICCProfile: icc.AdobeRGB1998.Build()}}
	if err := resolver.Add(other); err != nil {
		t.Fatal(err)
	}
	if r := resolver.ResolveTile(Tile{Source: other.Level.Source, OpticalPath: "H&E"}); r.Diagnostic.State != ColorConverted {
		t.Fatalf("other source=%+v", r)
	}
}

func TestOpticalPathICCConcatenationOffsetsAndRetainedTileCacheBound(t *testing.T) {
	file := &object.File{Dataset: derivedio.Object(
		derivedio.UI(tagSOPClassUID, VLWholeSlideMicroscopyImageStorage), derivedio.UI(tagSOPInstanceUID, "2.25.5001"),
		derivedio.UL(tagTotalPixelMatrixColumns, 2), derivedio.UL(tagTotalPixelMatrixRows, 1),
		derivedio.US(tagColumns, 1), derivedio.US(tagRows, 1), derivedio.IS(tagNumberOfFrames, 2),
		derivedio.CS(tagDimensionOrganizationType, "TILED_FULL"), derivedio.UL(tagTotalPixelMatrixFocalPlanes, 2),
		derivedio.UI(tagConcatenationUID, "2.25.5002"),
		derivedio.US(tagInConcatenationNumber, 2), derivedio.US(tagInConcatenationTotalNumber, 2),
		derivedio.UL(tagConcatenationFrameOffset, 6),
	)}
	file.Dataset.SetSequence(tagOpticalPathSequence, colorPathItem("first", nil), colorPathItem("second", nil))
	file.Dataset.SetSequence(tagSharedFunctionalGroups, tiledGeometryMeasures(derivedio.DS(tagSpacingBetweenSlices, 0.006)))
	i, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := i.FrameOpticalPath(1); id != "second" || err != nil {
		t.Fatalf("concat frame=%q err=%v", id, err)
	}
	tiles := i.Level.TilesForViewport(image.Rect(0, 0, 2, 1), "second", 1)
	if len(tiles) != 2 {
		t.Fatalf("concat tiles=%+v", tiles)
	}
	for _, tile := range tiles {
		if tile.FrameNumber != tile.Column+1 {
			t.Fatalf("concat local frame=%+v", tile)
		}
	}
	if tiles := i.Level.TilesForViewport(image.Rect(0, 0, 2, 1), "first", 0); len(tiles) != 0 {
		t.Fatalf("preceding frames emitted=%+v", tiles)
	}
	// Last-pixel subimages retain the entire backing allocation even though Pix
	// has only four bytes left. Cache admission must own a fresh canonical tile.
	backing := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	backing.SetNRGBA(99, 99, color.NRGBA{10, 20, 30, 255})
	tiny := backing.SubImage(image.Rect(99, 99, 100, 100))
	cache := newTileCache(8)
	key := TileKey{FrameNumber: 1}
	cache.put(key, tiny)
	stored, ok := cache.get(key)
	if !ok || stored.Bounds() != image.Rect(0, 0, 1, 1) || cache.stats().Bytes != 4 {
		t.Fatalf("cache retained source backing: bounds=%v stats=%+v", stored.Bounds(), cache.stats())
	}
	backing.SetNRGBA(99, 99, color.NRGBA{200, 200, 200, 255})
	if color.NRGBAModel.Convert(stored.At(0, 0)).(color.NRGBA) != (color.NRGBA{10, 20, 30, 255}) {
		t.Fatal("cache borrowed source pixels")
	}
	sixteen := image.NewRGBA64(image.Rect(0, 0, 1, 1))
	cache.put(TileKey{FrameNumber: 2}, sixteen)
	if stats := cache.stats(); stats.Bytes > 8 || stats.Entries != 1 {
		t.Fatalf("16-bit cache bound=%+v", stats)
	}
}

func TestOpticalPathICCSharedSparseAndUnsupportedAndResolverQuota(t *testing.T) {
	file := microscopyFixture(t, "shared", 2, 1, 1, 1, []frameFixture{{column: 1, row: 1, path: "H&E"}, {column: 2, row: 1, path: "H&E"}})
	file.Dataset.SetSequence(tagSharedFunctionalGroups, derivedio.Object(derivedio.Seq(tagOpticalPathIdentificationSequence, derivedio.DataSet(derivedio.SH(tagOpticalPathIdentifier, "IHC")))))
	file.Dataset.SetSequence(tagPerFrameFunctionalGroups,
		derivedio.Object(derivedio.Seq(tagPlanePositionSlideSequence, derivedio.DataSet(derivedio.SL(tagColumnPositionTotalMatrix, 1), derivedio.SL(tagRowPositionTotalMatrix, 1)))),
		derivedio.Object(derivedio.Seq(tagPlanePositionSlideSequence, derivedio.DataSet(derivedio.SL(tagColumnPositionTotalMatrix, 2), derivedio.SL(tagRowPositionTotalMatrix, 1)))))
	unsupported := icc.AdobeRGB1998.Build()
	copy(unsupported[16:20], "GRAY")
	file.Dataset.SetSequence(tagOpticalPathSequence, colorPathItem("H&E", icc.AdobeRGB1998.Build()), colorPathItem("IHC", unsupported))
	i, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewColorResolver(nil)
	if err := resolver.Add(i); err != nil {
		t.Fatal(err)
	}
	for frame := 1; frame <= 2; frame++ {
		r := resolver.ResolveFrame(i, frame)
		if r.Diagnostic.OpticalPath != "IHC" || r.Diagnostic.State != ColorUnsupportedProfile || !errors.Is(r.Diagnostic, icc.ErrUnsupportedProfile) {
			t.Fatalf("shared result=%+v", r)
		}
	}
	limited := NewColorResolver(icc.NewTransformCache(icc.CacheOptions{MaxBytes: 64 << 10}))
	if err := limited.Add(i); err != nil {
		t.Fatal(err)
	}
	if r := limited.ResolveTile(Tile{Source: i.Level.Source, OpticalPath: "H&E"}); r.Diagnostic.State != ColorResourceLimited {
		t.Fatalf("cache refusal=%+v", r)
	}
	// Resolver admission uses the aggregate quota before copying/publishing any
	// source, regardless of compressed profile validity.
	resolver.mu.Lock()
	resolver.bytes = MaxColorProfileBytes
	resolver.mu.Unlock()
	next := i
	next.Level.Source.SOPInstanceUID = "refused"
	if err := resolver.Add(next); !errors.Is(err, ErrColorResourceLimit) {
		t.Fatalf("aggregate refusal=%v", err)
	}
	if r := resolver.ResolveTile(Tile{Source: next.Level.Source, OpticalPath: "H&E"}); !errors.Is(r.Diagnostic, ErrUnknownOpticalPath) {
		t.Fatalf("partial admission=%+v", r)
	}
}
