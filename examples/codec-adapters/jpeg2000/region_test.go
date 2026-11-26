package jpeg2000

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
)

func corpusCodestream(t testing.TB, fixture string) []byte {
	t.Helper()
	path, err := codecfixture.CorpusFilePath(libraryRoot, "jpeg2000/core/"+fixture+".j2k")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCoreRegionMatchesCroppedDecode decodes regions of corpus codestreams,
// tiled or not, subsampled and at reduced resolution, and compares every
// component with the cropped decode of the whole image.
func TestCoreRegionMatchesCroppedDecode(t *testing.T) {
	for _, fixture := range []string{
		"jpeg2000-core-u12-tiles", "jpeg2000-core-rgb8-rpcl-precincts", "jpeg2000-core-u12-pcrl-offsets",
		"jpeg2000-core-u8-subsampled", "jpeg2000-core-u8-vsc-reset-irreversible",
	} {
		stream, err := j2kcore.ParseCodestream(corpusCodestream(t, fixture))
		if err != nil {
			t.Fatal(err)
		}
		for _, reduce := range []int{0, 1} {
			full, err := j2kcore.DecodeContextOptions(context.Background(), stream, j2kcore.DefaultLimits(), j2kcore.DecodeOptions{Reduce: reduce})
			if err != nil {
				t.Fatal(err)
			}
			w, h := full.Width, full.Height
			for _, region := range []image.Rectangle{
				image.Rect(0, 0, w, h), image.Rect(0, 0, 1, 1), image.Rect(w/3, h/4, w/3+w/2, h/4+h/3),
				image.Rect(w-5, h-3, w, h), image.Rect(w/2-1, 0, w/2+1, h),
			} {
				got, err := j2kcore.DecodeContextOptions(context.Background(), stream, j2kcore.DefaultLimits(), j2kcore.DecodeOptions{Reduce: reduce, Region: region})
				if err != nil {
					t.Fatalf("%s r%d %v: %v", fixture, reduce, region, err)
				}
				if got.Width != region.Dx() || got.Height != region.Dy() {
					t.Fatalf("%s r%d %v: %dx%d image", fixture, reduce, region, got.Width, got.Height)
				}
				for c := range full.Components {
					for y := 0; y < region.Dy(); y++ {
						for x := 0; x < region.Dx(); x++ {
							want := full.Components[c][(region.Min.Y+y)*w+region.Min.X+x]
							if v := got.Components[c][y*region.Dx()+x]; v != want {
								t.Fatalf("%s r%d %v component %d at %d,%d: %d, want %d", fixture, reduce, region, c, x, y, v, want)
							}
						}
					}
				}
			}
			if _, err := j2kcore.DecodeContextOptions(context.Background(), stream, j2kcore.DefaultLimits(), j2kcore.DecodeOptions{Reduce: reduce, Region: image.Rect(0, 0, w+1, h)}); err == nil {
				t.Fatalf("%s: region outside the image decoded", fixture)
			}
		}
	}
}

// TestFrameReaderRegionWithCoreCodec reads regions of DICOM JPEG 2000 frames
// through pixeldata.FrameReader and the core codec's region decoder.
func TestFrameReaderRegionWithCoreCodec(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := RegisterCore(registry, CoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"693_J2KR.dcm", "US1_J2KI.dcm"} {
		path := filepath.Join("..", "..", "..", "pixeldata", "codecfixture", "testdata", "codecfull", "pydicom", name)
		reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: registry})
		if err != nil {
			t.Fatal(err)
		}
		frame, metadata, err := reader.Frame(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		region := image.Rect(int(metadata.Columns)/4, int(metadata.Rows)/3, int(metadata.Columns)/2, int(metadata.Rows)/2)
		got, regionMetadata, err := reader.Region(context.Background(), 0, region)
		if err != nil {
			t.Fatal(err)
		}
		step := int(metadata.SamplesPerPixel) * metadata.BytesPerSample()
		var want []byte
		for y := region.Min.Y; y < region.Max.Y; y++ {
			want = append(want, frame[(y*int(metadata.Columns)+region.Min.X)*step:(y*int(metadata.Columns)+region.Max.X)*step]...)
		}
		if !bytes.Equal(got, want) || int(regionMetadata.Columns) != region.Dx() {
			t.Fatalf("%s: region differs from the cropped frame", name)
		}
		reader.Close()
	}
	// Other backends ask for a whole decode.
	if _, err := New().DecodeFrameRegion(context.Background(), nil, pixeldata.Metadata{}, image.Rect(0, 0, 1, 1)); !errors.Is(err, pixeldata.ErrRegionUnsupported) {
		t.Fatalf("pure-Go backend: %v", err)
	}
}

// BenchmarkCoreRegion compares a one-tile region of the tiled corpus
// codestream with the whole decode.
func BenchmarkCoreRegion(b *testing.B) {
	stream, err := j2kcore.ParseCodestream(corpusCodestream(b, "jpeg2000-core-u12-tiles"))
	if err != nil {
		b.Fatal(err)
	}
	full, err := j2kcore.DecodeContext(context.Background(), stream, j2kcore.DefaultLimits())
	if err != nil {
		b.Fatal(err)
	}
	x0, y0, x1, y1 := stream.TileBounds(0)
	region := image.Rect(x0, y0, x1, y1).Intersect(image.Rect(0, 0, full.Width, full.Height))
	for _, c := range []struct {
		name    string
		options j2kcore.DecodeOptions
	}{{"whole", j2kcore.DecodeOptions{}}, {"one-tile-region", j2kcore.DecodeOptions{Region: region}}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := j2kcore.DecodeContextOptions(context.Background(), stream, j2kcore.DefaultLimits(), c.options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Logf("%d tiles, region %v of %dx%d", stream.NumTiles(), region, full.Width, full.Height)
}
