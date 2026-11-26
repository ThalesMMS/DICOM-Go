package registration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestReadITKDenseMixedComposite(t *testing.T) {
	directory := filepath.Join("testdata", "itk1845")
	manifest, err := os.ReadFile(filepath.Join(directory, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Mixed struct {
			SHA256 string
			Queue  []string
			Points []struct {
				Input, Output [3]float64
				Jacobian      Matrix3
			}
		}
	}
	if err := json.Unmarshal(manifest, &evidence); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "mixed.h5"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(data)
	want := evidence.Mixed
	if hex.EncodeToString(checksum[:]) != want.SHA256 || len(want.Queue) != 58 || len(want.Points) != 4 {
		t.Fatal("unexpected public ITK fixture provenance or shape")
	}
	transform, err := ReadTransformFile(data)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := ReadTransformFileContext(context.Background(), data, TransformFileReadOptions{MaxParameters: 1})
	if limited != nil || err == nil {
		t.Fatalf("dense composite exceeded aggregate parameter budget: %T, %v", limited, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if canceled, err := ReadTransformFileContext(ctx, data, TransformFileReadOptions{}); canceled != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dense import: %T, %v", canceled, err)
	}
	chain, ok := transform.(*Composite)
	if !ok || chain.Len() != len(want.Queue) {
		t.Fatalf("unexpected composite: %T", transform)
	}
	for i, name := range want.Queue {
		component, err := newITKParameters(name+"_double_3_3", 3)
		if err != nil {
			t.Fatal(err)
		}
		reversed := chain.Len() - 1 - i // ITK queue order is the reverse of Go application order.
		if got := chain.Transform(reversed).Kind(); got != component.Kind() {
			t.Errorf("queue %d: got %v, want %v", i, got, component.Kind())
		}
	}
	for i, sample := range want.Points {
		in := render.Vec3{X: sample.Input[0], Y: sample.Input[1], Z: sample.Input[2]}
		point := render.Vec3{X: sample.Output[0], Y: sample.Output[1], Z: sample.Output[2]}
		if delta := chain.TransformPoint(in).Sub(point).Length(); !(delta <= 1e-10) {
			t.Errorf("point %d: %g mm from ITK", i, delta)
		}
		jacobian := chain.JacobianWithRespectToPosition(in)
		for row := range jacobian {
			for column, value := range jacobian[row] {
				if expected := sample.Jacobian[row][column]; !(math.Abs(value-expected) <= 2e-8) {
					t.Errorf("point %d Jacobian[%d,%d]: got %g, ITK centered difference %g", i, row, column, value, expected)
				}
			}
		}
	}
}
