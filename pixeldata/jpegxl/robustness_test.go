package jpegxl

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureCorpus(t testing.TB) [][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		t.Fatal(err)
	}
	xybPaths, err := filepath.Glob(filepath.Join("testdata", "xyb", "*.jxl"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, xybPaths...)
	var corpus [][]byte
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, data)
	}
	return corpus
}

// exercise runs every public entry point on data; errors must be typed and
// nothing may panic.
func exercise(t testing.TB, data []byte) {
	t.Helper()
	typed := func(op string, err error) {
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "internal decoder fault") {
			t.Fatalf("%s: recovered panic: %v", op, err)
		}
		if !errors.Is(err, ErrInvalidCodestream) && !errors.Is(err, ErrUnsupported) &&
			!errors.Is(err, ErrNoReconstructionData) && !errors.Is(err, ErrNotReversible) && !errors.Is(err, ErrLimit) {
			t.Fatalf("%s: untyped error %v", op, err)
		}
	}
	_, err := Decode(data)
	typed("Decode", err)
	_, err = ReconstructJPEG(data)
	typed("ReconstructJPEG", err)
	_, err = RecompressJPEG(data)
	typed("RecompressJPEG", err)
}

// Truncations and bit flips of every fixture fail with typed errors.
func TestCorruptedInputsFailWithTypedErrors(t *testing.T) {
	rng := rand.New(rand.NewSource(1153))
	for _, data := range fixtureCorpus(t) {
		for cut := 0; cut < len(data); cut += 1 + len(data)/64 {
			exercise(t, data[:cut])
		}
		for i := 0; i < 64; i++ {
			mutated := append([]byte(nil), data...)
			for k := 0; k < 1+rng.Intn(3); k++ {
				mutated[rng.Intn(len(mutated))] ^= byte(1 << rng.Intn(8))
			}
			exercise(t, mutated)
		}
	}
}

func FuzzDecoders(f *testing.F) {
	for _, data := range fixtureCorpus(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		exercise(t, data)
	})
}
