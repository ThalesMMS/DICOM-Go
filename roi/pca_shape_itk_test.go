package roi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type pcaITKReceipt struct {
	Schema                                          int
	Engine, Version, OracleSHA256, ExecutableSHA256 string
	Model                                           PCAShapeModelData
	Pose                                            PCAShapePose
	Points                                          [][3]float64
	Cases                                           []struct{ CoefficientsSD, Values []float64 }
}

func TestPCAShapeFrozenITKRepresentation(t *testing.T) {
	f, e := os.Open("testdata/pca_shape/itk.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	var receipt pcaITKReceipt
	if e = json.NewDecoder(r).Decode(&receipt); e != nil {
		t.Fatal(e)
	}
	if receipt.Schema != 1 || receipt.Engine != "ITK" || receipt.Version != "5.4.6" || len(receipt.Cases) != 3 || len(receipt.Points) != 5 || len(receipt.OracleSHA256) != 64 || len(receipt.ExecutableSHA256) != 64 {
		t.Fatal("invalid frozen oracle receipt")
	}
	source, e := os.ReadFile("testdata/pca_shape/oracle.cpp")
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(source)
	if fmt.Sprintf("%x", hash) != receipt.OracleSHA256 {
		t.Fatal("ITK fixture oracle source hash mismatch")
	}
	m, e := NewPCAShapeModel(nil, receipt.Model, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range receipt.Cases {
		got, e := m.Evaluate(nil, c.CoefficientsSD, receipt.Pose, receipt.Points, PCARejectOutside, PCAShapeLimits{})
		if e != nil {
			t.Fatal(e)
		}
		if len(got.FieldMM) != len(c.Values) {
			t.Fatal("oracle length")
		}
		for i, want := range c.Values {
			if math.Abs(got.FieldMM[i]-want) > 1e-10 {
				t.Fatalf("frozen ITK point %d", i)
			}
		}
	}
}

// Opt-in SDK qualification; default tests never find, build or execute ITK.
func TestPCAShapeITKRepresentationAndPose(t *testing.T) {
	exe := os.Getenv("DICOM_GO_PCA_SHAPE_ITK_ORACLE")
	if exe == "" {
		t.Skip("DICOM_GO_PCA_SHAPE_ITK_ORACLE not set")
	}
	f := pcaNumpyFixture(t)
	d := PCAShapeModelData{Version: 1, ID: "independent-numpy-model", Provenance: "project-authored registered synthetic spheres; NumPy SVD", Convention: PCAShapeConvention, Geometry: f.Geometry, TrainingSamples: len(f.Training), MeanMM: f.MeanMM, Components: f.Components, StandardDeviationsMM: f.StandardDeviationsMM}
	m, e := NewPCAShapeModel(nil, d, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	pose := IdentityPCAShapePose()
	angle := .23
	pose.Rotation = [9]float64{math.Cos(angle), -math.Sin(angle), 0, math.Sin(angle), math.Cos(angle), 0, 0, 0, 1}
	pose.TranslationMM = [3]float64{.3, -.2, .4}
	var points [][3]float64
	for _, idx := range [][3]float64{{1, 1, 1}, {2.25, 3.5, 1.75}, {5.4, 4.1, 2.2}, {9.5, 7.1, 5.2}, {5, 4, 3}} {
		var canonical [3]float64
		canonical = d.Geometry.OriginMM
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				canonical[a] += d.Geometry.Direction[3*a+b] * idx[b] * d.Geometry.SpacingMM[b]
			}
		}
		var world [3]float64
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				world[a] += pose.Rotation[3*b+a] * (canonical[b] - pose.TranslationMM[b])
			}
		}
		points = append(points, world)
	}
	receipt := pcaITKReceipt{Schema: 1, Engine: "ITK", Version: "5.4.6", Model: d, Pose: pose, Points: points}
	for _, entry := range []struct {
		path        string
		destination *string
	}{{"testdata/pca_shape/oracle.cpp", &receipt.OracleSHA256}, {exe, &receipt.ExecutableSHA256}} {
		b, e := os.ReadFile(entry.path)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(b)
		*entry.destination = fmt.Sprintf("%x", h)
	}
	for _, coeff := range [][]float64{make([]float64, len(d.Components)), f.HeldOut[0].CoefficientsSD, f.HeldOut[1].CoefficientsSD} {
		input := bytes.NewBuffer(nil)
		write := func(v any) {
			t.Helper()
			if e := binary.Write(input, binary.LittleEndian, v); e != nil {
				t.Fatal(e)
			}
		}
		write(uint32(0x31414350))
		for _, n := range d.Geometry.Dimensions {
			write(uint32(n))
		}
		write(uint32(len(d.Components)))
		write(uint32(len(points)))
		write(d.Geometry.SpacingMM)
		write(d.Geometry.OriginMM)
		write(d.Geometry.Direction)
		write(pose.Rotation)
		write(pose.TranslationMM)
		write(d.StandardDeviationsMM)
		write(coeff)
		write(d.MeanMM)
		for _, c := range d.Components {
			write(c)
		}
		write(points)
		path := filepath.Join(t.TempDir(), "model-input.bin")
		if e = os.WriteFile(path, input.Bytes(), 0o600); e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, exe, path)
		output, e := command.CombinedOutput()
		cancel()
		if e != nil {
			t.Fatalf("native oracle %v: %s", e, output)
		}
		var native struct {
			Engine, Version string
			Values          []float64
		}
		if e = json.Unmarshal(output, &native); e != nil {
			t.Fatal(e)
		}
		if native.Engine != "ITK" || native.Version != "5.4.6" || len(native.Values) != len(points) {
			t.Fatalf("oracle identity %s", output)
		}
		got, e := m.Evaluate(nil, coeff, pose, points, PCARejectOutside, PCAShapeLimits{})
		if e != nil {
			t.Fatal(e)
		}
		maximum := 0.0
		for i, want := range native.Values {
			delta := math.Abs(got.FieldMM[i] - want)
			maximum = math.Max(maximum, delta)
			if delta > 1e-10 {
				t.Fatalf("ITK point %d %g != %g", i, got.FieldMM[i], want)
			}
		}
		t.Logf("ITK %s: %d physically posed/off-grid points, max delta=%g mm", native.Version, len(points), maximum)
		receipt.Cases = append(receipt.Cases, struct{ CoefficientsSD, Values []float64 }{CoefficientsSD: coeff, Values: native.Values})
	}
	if path := os.Getenv("DICOM_GO_PCA_SHAPE_ITK_RECEIPT"); path != "" {
		f, e := os.Create(path)
		if e != nil {
			t.Fatal(e)
		}
		w := gzip.NewWriter(f)
		encodeErr := json.NewEncoder(w).Encode(receipt)
		closeErr := w.Close()
		fileErr := f.Close()
		if encodeErr != nil || closeErr != nil || fileErr != nil {
			t.Fatalf("receipt %v %v %v", encodeErr, closeErr, fileErr)
		}
	}
}
