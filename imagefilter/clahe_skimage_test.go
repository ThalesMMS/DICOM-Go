package imagefilter

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// skimageScript runs scikit-image's equalize_adapthist on a raw little-endian
// uint16 image and writes the float64 result.
const skimageScript = `
import sys
import numpy as np
from skimage.exposure import equalize_adapthist
src, dst, h, w, ky, kx, clip, nbins = sys.argv[1:9]
h, w, ky, kx, nbins = int(h), int(w), int(ky), int(kx), int(nbins)
image = np.fromfile(src, dtype='<u2').reshape(h, w)
out = equalize_adapthist(image, kernel_size=(ky, kx), clip_limit=float(clip), nbins=nbins)
out.astype('<f8').tofile(dst)
`

// TestCLAHEMatchesScikitImage compares CLAHE with scikit-image's
// equalize_adapthist on a synthetic image. It is opt-in: set
// DICOMGO_SKIMAGE_PYTHON to a Python interpreter that already has
// scikit-image installed; nothing is installed or downloaded.
//
// The two implementations share Zuiderveld's structure but not every detail:
// scikit-image rescales to 14 bits, uses bins of 1+16384/nbins levels, maps
// each bin with a step (inclusive cumulative count) and redistributes with a
// slightly different residual pass, while this package interpolates inside
// bins. The comparison therefore checks agreement within a tolerance, not
// bit-exactness.
func TestCLAHEMatchesScikitImage(t *testing.T) {
	python := os.Getenv("DICOMGO_SKIMAGE_PYTHON")
	if python == "" {
		t.Skip("set DICOMGO_SKIMAGE_PYTHON to a Python with scikit-image to compare")
	}
	const (
		w, h           = 512, 384
		tilesX, tilesY = 8, 6
		bins           = 256
		clip           = 3.0
	)
	src := syntheticRadiograph(w, h, 65535, 11)
	// scikit-image stretches the input to its own min..max first; pin the
	// extremes so both implementations see the same full-range domain.
	src[0], src[1] = 0, 65535

	dir := t.TempDir()
	in, out, script := filepath.Join(dir, "in.u16"), filepath.Join(dir, "out.f64"), filepath.Join(dir, "clahe.py")
	raw := make([]byte, 2*len(src))
	for i, v := range src {
		binary.LittleEndian.PutUint16(raw[2*i:], v)
	}
	if err := os.WriteFile(in, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(skimageScript), 0o600); err != nil {
		t.Fatal(err)
	}
	// scikit-image's clip_limit is a fraction of the tile pixels per bin;
	// Zuiderveld's normalized limit is a multiple of the mean bin count.
	skClip := clip / bins
	cmd := exec.Command(python, script, in, out, strconv.Itoa(h), strconv.Itoa(w), strconv.Itoa(h/tilesY), strconv.Itoa(w/tilesX), strconv.FormatFloat(skClip, 'g', -1, 64), strconv.Itoa(bins))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scikit-image run failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8*len(src) {
		t.Fatalf("scikit-image wrote %d bytes, want %d", len(data), 8*len(src))
	}

	dst := make([]uint16, len(src))
	if err := CLAHE(context.Background(), dst, src, w, h, CLAHEOptions{TilesX: tilesX, TilesY: tilesY, ClipLimit: clip, Bins: bins}); err != nil {
		t.Fatal(err)
	}
	var sumAbs, maxAbs, sa, sb, saa, sbb, sab float64
	n := float64(len(src))
	for i := range src {
		a := float64(dst[i]) / 65535
		b := math.Float64frombits(binary.LittleEndian.Uint64(data[8*i:]))
		d := math.Abs(a - b)
		sumAbs += d
		maxAbs = math.Max(maxAbs, d)
		sa, sb = sa+a, sb+b
		saa, sbb, sab = saa+a*a, sbb+b*b, sab+a*b
	}
	meanAbs := sumAbs / n
	corr := (sab/n - sa/n*sb/n) / math.Sqrt((saa/n-sa*sa/n/n)*(sbb/n-sb*sb/n/n))
	t.Logf("vs scikit-image equalize_adapthist: mean |Δ|=%.4f max |Δ|=%.4f (of 1.0), Pearson r=%.5f", meanAbs, maxAbs, corr)
	if meanAbs > 0.02 || corr < 0.99 {
		t.Fatalf("CLAHE differs from scikit-image: mean |Δ|=%.4f r=%.5f", meanAbs, corr)
	}
}
