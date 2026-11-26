package jpeg2000

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

// reducedCases are corpus codestreams decoded with resolution levels dropped.
// The references in testdata/reduced are opj_decompress -r output (planar,
// little-endian, each component at its own size), regenerated with
// DICOM_GO_J2K_UPDATE_REDUCED=1 and DICOM_GO_OPENJPEG_DECOMPRESS set.
var reducedCases = []struct {
	fixture  string
	reduce   int
	maxError int32
}{
	{"jpeg2000-core-u12-tiles", 1, 0},
	{"jpeg2000-core-u12-tiles", 2, 0},
	{"jpeg2000-core-rgb8-rpcl-precincts", 1, 0},
	{"jpeg2000-core-rgb8-rpcl-precincts", 3, 0},
	{"jpeg2000-core-u12-pcrl-offsets", 2, 0},
	{"jpeg2000-core-u8-subsampled", 1, 0},
	// Irreversible: decoder-to-decoder rounding, as at full resolution.
	{"jpeg2000-core-u8-vsc-reset-irreversible", 2, 1},
}

func TestDecodeCoreReducedMatchesOpenJPEG(t *testing.T) {
	update := os.Getenv("DICOM_GO_J2K_UPDATE_REDUCED") == "1"
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if update && decompress == "" {
		t.Fatal("DICOM_GO_J2K_UPDATE_REDUCED needs DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	for _, tc := range reducedCases {
		t.Run(fmt.Sprintf("%s-r%d", tc.fixture, tc.reduce), func(t *testing.T) {
			path, err := codecfixture.CorpusFilePath(libraryRoot, "jpeg2000/core/"+tc.fixture+".j2k")
			if err != nil {
				t.Fatal(err)
			}
			codestream, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			reference := filepath.Join("testdata", "reduced", fmt.Sprintf("%s-r%d.rawl", tc.fixture, tc.reduce))
			if update {
				runOpenJPEG(t, decompress, "-i", path, "-o", reference, "-r", fmt.Sprint(tc.reduce))
			}
			raw, err := os.ReadFile(reference)
			if err != nil {
				t.Fatal(err)
			}
			image, err := DecodeCoreReduced(context.Background(), codestream, tc.reduce, CoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// Every fixture here samples its components alike, so the output
			// grid is each component's own.
			worst := int32(0)
			for c := range image.Components {
				var want []int32
				want, raw, err = readPlane(raw, image.Columns*image.Rows, image.Depths[c], image.Signed[c])
				if err != nil {
					t.Fatal(err)
				}
				for i, v := range want {
					diff := image.Components[c][i] - v
					if diff < 0 {
						diff = -diff
					}
					worst = max(worst, diff)
				}
			}
			if len(raw) != 0 {
				t.Fatalf("the reference holds %d more bytes than a %dx%d image", len(raw), image.Columns, image.Rows)
			}
			if worst > tc.maxError {
				t.Fatalf("differs from OpenJPEG -r %d by %d", tc.reduce, worst)
			}
		})
	}
}

func TestDecodeCoreReducedRefusesTooManyLevels(t *testing.T) {
	path, err := codecfixture.CorpusFilePath(libraryRoot, "jpeg2000/core/jpeg2000-core-u12-tiles.j2k")
	if err != nil {
		t.Fatal(err)
	}
	codestream, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Three resolutions: two decomposition levels can be dropped, not three.
	if _, err := DecodeCoreReduced(context.Background(), codestream, 3, CoreOptions{}); !errors.Is(err, ErrInvalidReduction) {
		t.Fatalf("reduce 3: %v", err)
	}
	if _, err := DecodeCoreReduced(context.Background(), codestream, 1, CoreOptions{Limits: CoreLimits{MaxInputBytes: 10}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("input limit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecodeCoreReduced(ctx, codestream, 1, CoreOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
