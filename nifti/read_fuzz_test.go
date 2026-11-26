package nifti_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

const fuzzSeedsEnvironment = "DICOM_GO_NIFTI_WRITE_FUZZ_SEEDS"

// readFuzzSeeds are synthetic headers covering both versions, both byte
// orders, pairs, extensions, every transform combination, non-finite values
// and malformed fields. testdata/fuzz/FuzzReadHeader holds the same inputs
// (regenerate with DICOM_GO_NIFTI_WRITE_FUZZ_SEEDS=1).
func readFuzzSeeds(tb testing.TB) map[string][]byte {
	quaternion, rotation := axisAngleQuaternion(render.Vec3{X: 1, Y: -2, Z: 0.5}, 1.1)
	// Round the trigonometric results to float32 so the committed NIfTI-2
	// (float64) seeds do not change with last-bit differences in math.Sin and
	// math.Cos between Go toolchains.
	for i := range quaternion {
		quaternion[i] = float64(float32(quaternion[i]))
	}
	for i := range rotation {
		for j := range rotation[i] {
			rotation[i][j] = float64(float32(rotation[i][j]))
		}
	}
	spacing := [3]float64{0.8, 0.8, 1.5}
	offset := [3]float64{-90, 120, -60}
	oblique := defaultSynth()
	oblique.pixdim = [8]float64{-1, spacing[0], spacing[1], spacing[2], 1, 1, 1, 1}
	oblique.quatern, oblique.qoffset = quaternion, offset
	oblique.srow = sformRows(rotation, spacing, offset)
	values := []float64{1, 2, 3, 4}

	seeds := map[string][]byte{}
	add := func(name string, spec synthHeader, voxels []float64) {
		seeds[name] = synthFile(spec, voxels)
	}
	both := oblique
	both.qformCode, both.sformCode = 1, 1
	add("nifti1-both", both, values)
	qform := oblique
	qform.qformCode = 1
	add("nifti1-qform-only", qform, values)
	sform := oblique
	sform.sformCode = 2
	add("nifti1-sform-only", sform, values)
	codes := both
	codes.sformCode = 4
	codes.srow[0][3] += 10
	add("nifti1-different-codes", codes, values)
	sheared := sform
	sheared.srow[0][1] += 0.3
	add("nifti1-sheared-sform", sheared, values)
	tiny := sform
	tiny.srow = sformRows(rotation, [3]float64{1e-4, 1e-4, 2e-4}, offset)
	add("nifti1-tiny-voxels", tiny, values)
	nan := both
	nan.datatype = nifti.DatatypeFloat32
	nan.slope = math.NaN()
	add("nifti1-float-nan", nan, []float64{math.NaN(), math.Inf(1), -0.5, 3})
	nanTransform := both
	nanTransform.quatern[0] = math.NaN()
	nanTransform.srow[1][1] = math.Inf(-1)
	add("nifti1-nan-transform", nanTransform, values)
	big := both
	big.bigEndian = true
	big.datatype = nifti.DatatypeInt16
	add("nifti1-big-endian", big, values)
	v2 := both
	v2.version = 2
	v2.datatype = nifti.DatatypeFloat64
	add("nifti2-both", v2, values)
	v2big := v2
	v2big.bigEndian = true
	v2big.dims = []int64{2, 1, 1, 2}
	add("nifti2-big-endian-4d", v2big, values)
	extended := both
	extended.extension = append([]byte{16, 0, 0, 0, 4, 0, 0, 0}, make([]byte, 8)...)
	add("nifti1-extension", extended, values)
	unsupported := both
	unsupported.datatype = 32
	add("nifti1-complex", unsupported, values)
	huge := both
	huge.dims = []int64{32767, 32767, 32767}
	add("nifti1-huge-dims", huge, values)
	offsetZero := both
	offsetZero.voxOffset = 0
	add("nifti1-vox-offset-zero", offsetZero, values)
	paired := both
	paired.paired = true
	seeds["nifti1-paired-header"] = synthHeaderBytes(paired)
	corrupted := v2
	corrupted.magic = "n+2\x00\n\x1a\n\x00"
	add("nifti2-newline-corruption", corrupted, values)
	seeds["gzip-nifti1"] = gzipBytes(tb, seeds["nifti1-both"])
	seeds["truncated-nifti2"] = seeds["nifti2-both"][:300]
	return seeds
}

// FuzzReadHeader feeds arbitrary bytes to ParseHeader and Read under small
// limits. Every failure must be a typed *ReadError and every success must
// yield finite geometry and readable voxels.
func FuzzReadHeader(f *testing.F) {
	seeds := readFuzzSeeds(f)
	names := make([]string, 0, len(seeds))
	for name := range seeds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f.Add(seeds[name])
	}
	limits := nifti.ReadLimits{MaxVoxels: 1 << 16, MaxPayloadBytes: 1 << 20, MaxHeaderBytes: 1 << 16}
	optionSets := []nifti.ReadOptions{
		{Limits: limits},
		{Limits: limits, SFormPermissive: true, AllowMissingTransform: true},
		{Limits: limits, Xform: nifti.XformQForm},
		{Limits: limits, Xform: nifti.XformSForm, SFormPermissive: true},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		if header, err := nifti.ParseHeader(data); err == nil {
			if header.Dim[0] < 1 || header.Dim[0] > 7 || (header.Version != 1 && header.Version != 2) {
				t.Fatalf("ParseHeader accepted dim[0] %d version %d", header.Dim[0], header.Version)
			}
			for index := 1; index < len(header.Dim); index++ {
				if header.Dim[index] < 1 {
					t.Fatalf("ParseHeader accepted dim[%d] = %d", index, header.Dim[index])
				}
			}
		} else {
			requireTypedReadError(t, err)
		}
		for _, options := range optionSets {
			volume, err := nifti.Read(context.Background(), bytes.NewReader(data), options)
			if err != nil {
				requireTypedReadError(t, err)
				continue
			}
			checkFuzzedVolume(t, volume, limits)
		}
	})
}

func requireTypedReadError(t *testing.T, err error) {
	t.Helper()
	var readErr *nifti.ReadError
	if !errors.As(err, &readErr) || readErr.Code == "" {
		t.Fatalf("error %v (%T) is not a typed *nifti.ReadError", err, err)
	}
}

func checkFuzzedVolume(t *testing.T, volume *nifti.Volume, limits nifti.ReadLimits) {
	t.Helper()
	geometry := volume.Geometry
	if !geometry.IndexToRAS.Finite() || !geometry.IndexToLPS.Finite() {
		t.Fatalf("non-finite affine %v", geometry.IndexToRAS)
	}
	for _, spacing := range geometry.Spacing {
		if !(spacing > 0) || math.IsInf(spacing, 0) {
			t.Fatalf("spacing %v", geometry.Spacing)
		}
	}
	if geometry.SFormCorrected && !(geometry.SFormDeviationDegrees <= 90) {
		t.Fatalf("deviation %v", geometry.SFormDeviationDegrees)
	}
	count := 1
	for _, dimension := range volume.Dimensions {
		count *= dimension
	}
	if count != volume.Len() || uint64(count) > limits.MaxVoxels {
		t.Fatalf("Len %d, dimensions %v", volume.Len(), volume.Dimensions)
	}
	for index := 0; index < volume.Len(); index++ {
		_ = volume.Value(index)
	}
	for _, slice := range volume.SliceGeometries() {
		if !finiteVector(slice.Origin) || !finiteVector(slice.RowDir) || !finiteVector(slice.ColDir) {
			t.Fatalf("non-finite slice geometry %+v", slice)
		}
	}
}

func finiteVector(v render.Vec3) bool {
	for _, value := range []float64{v.X, v.Y, v.Z} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

// TestWriteReadFuzzSeeds regenerates the committed corpus when
// DICOM_GO_NIFTI_WRITE_FUZZ_SEEDS=1 and otherwise checks it is current.
func TestWriteReadFuzzSeeds(t *testing.T) {
	directory := filepath.Join("testdata", "fuzz", "FuzzReadHeader")
	for name, data := range readFuzzSeeds(t) {
		encoded := []byte(fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", data))
		path := filepath.Join(directory, name)
		if os.Getenv(fuzzSeedsEnvironment) == "1" {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing committed fuzz seed %s (set %s=1 to regenerate): %v", name, fuzzSeedsEnvironment, err)
		}
		if name != "gzip-nifti1" && !bytes.Equal(committed, encoded) {
			t.Fatalf("committed fuzz seed %s is stale (set %s=1 to regenerate)", name, fuzzSeedsEnvironment)
		}
	}
}
