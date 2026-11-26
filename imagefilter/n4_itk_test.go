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
	"time"
)

// The fixture of testdata/upstream/itk/n4 is ITK's N4 (commit 35b196e1) run
// by dicom-go-validation/scripts/itk_n4 on the 32 × 32 × 16 phantom below,
// without shrinking, three fitting levels of at most 50 iterations: the
// input, its mask and ITK's log bias field, little-endian, x fastest.

var itkN4Dims = [3]int{32, 32, 16}

const itkN4Levels, itkN4Iterations = 3, 50

func itkN4Dir() string { return filepath.Join("..", "testdata", "upstream", "itk", "n4") }

func itkN4Phantom() (*Volume, []bool) {
	in, _, _ := n4Phantom(itkN4Dims, n4SurfaceCoil, 4)
	in.SpacingMM = [3]float64{1, 1, 1}
	mask := make([]bool, len(in.Data))
	for i, v := range in.Data {
		mask[i] = v > 0
	}
	return in, mask
}

func readFloat32s(t *testing.T, path string, n int) []float32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4*n {
		t.Fatalf("%s: %d bytes, want %d", path, len(data), 4*n)
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
	}
	return out
}

func writeFloat32s(t *testing.T, path string, values []float32) {
	t.Helper()
	data := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// compareWithITKN4 runs dicom-go's N4 as the fixture ran ITK's and compares
// the log bias fields.
func compareWithITKN4(t *testing.T, in *Volume, mask []bool, itk []float32) {
	t.Helper()
	iterations := make([]int, itkN4Levels)
	for i := range iterations {
		iterations[i] = itkN4Iterations
	}
	result, err := N4BiasFieldCorrection(context.Background(), in, N4Options{Mask: mask, ShrinkFactor: 1, Iterations: iterations})
	if err != nil {
		t.Fatal(err)
	}
	var worst, sum float64
	var n int
	for i, inside := range mask {
		if !inside {
			continue
		}
		d := math.Abs(float64(result.LogBiasField.Data[i]) - float64(itk[i]))
		worst = math.Max(worst, d)
		sum += d
		n++
	}
	t.Logf("log bias field against ITK: mean difference %.2e, worst %.2e (levels %+v)", sum/float64(n), worst, result.Levels)
	// ITK computes in float32; the fields agree to about 1e-5.
	if worst > 1e-4 {
		t.Fatalf("log bias field differs from ITK's by up to %.2e", worst)
	}
}

// TestN4MatchesITK compares against the committed ITK fixture.
func TestN4MatchesITK(t *testing.T) {
	n := itkN4Dims[0] * itkN4Dims[1] * itkN4Dims[2]
	input := readFloat32s(t, filepath.Join(itkN4Dir(), "phantom.f32"), n)
	maskBytes, err := os.ReadFile(filepath.Join(itkN4Dir(), "mask.u8"))
	if err != nil || len(maskBytes) != n {
		t.Fatalf("mask: %d bytes, %v", len(maskBytes), err)
	}
	mask := make([]bool, n)
	for i, b := range maskBytes {
		mask[i] = b != 0
	}
	in := &Volume{Dims: itkN4Dims, SpacingMM: [3]float64{1, 1, 1}, Data: input}
	compareWithITKN4(t, in, mask, readFloat32s(t, filepath.Join(itkN4Dir(), "logbias.f32"), n))
}

// TestN4AgainstITKTool runs ITK's N4 when ITK_N4_TOOL names the n4gen tool
// of dicom-go-validation/scripts/itk_n4; ITK_N4_FIXTURE_OUT, when set, also
// receives the fixture files.
func TestN4AgainstITKTool(t *testing.T) {
	tool := os.Getenv("ITK_N4_TOOL")
	if tool == "" {
		t.Skip("ITK_N4_TOOL not set")
	}
	dir := os.Getenv("ITK_N4_FIXTURE_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	in, mask := itkN4Phantom()
	writeFloat32s(t, filepath.Join(dir, "phantom.f32"), in.Data)
	maskBytes := make([]byte, len(mask))
	for i, inside := range mask {
		if inside {
			maskBytes[i] = 1
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "mask.u8"), maskBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, filepath.Join(dir, "phantom.f32"), filepath.Join(dir, "mask.u8"),
		strconv.Itoa(itkN4Dims[0]), strconv.Itoa(itkN4Dims[1]), strconv.Itoa(itkN4Dims[2]),
		strconv.Itoa(itkN4Levels), strconv.Itoa(itkN4Iterations), filepath.Join(dir, "logbias.f32")).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, out)
	}
	t.Logf("ITK: %s", out)
	compareWithITKN4(t, in, mask, readFloat32s(t, filepath.Join(dir, "logbias.f32"), len(in.Data)))
}
